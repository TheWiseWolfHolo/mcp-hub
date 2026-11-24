package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// NewDirectTransport returns an http.Transport that never consults system proxy
// environment variables and does not impose ResponseHeaderTimeout (safe for SSE).
func NewDirectTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewDirectClient returns an http.Client with no proxy and no redirect following.
func NewDirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewDirectTransport(),
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func makeAllowedHostSet(hosts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm != "" {
			set[norm] = struct{}{}
		}
	}
	return set
}

func isHostAllowed(host string, allowed map[string]struct{}) bool {
	if len(allowed) == 0 {
		return false
	}
	_, ok := allowed[strings.ToLower(strings.TrimSpace(host))]
	return ok
}

func hasNonEmptyOrigin(r *http.Request) bool {
	for _, v := range r.Header.Values("Origin") {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// isDisallowedBrowserFetch inspects Sec-Fetch-Site headers to block browser-initiated
// requests (cross-site, same-site, same-origin). Direct browser navigations ("none")
// are rejected unless allowDirectNav is true (e.g. GET /healthz).
func isDisallowedBrowserFetch(r *http.Request, allowDirectNav bool) bool {
	for _, raw := range r.Header.Values("Sec-Fetch-Site") {
		for _, part := range strings.Split(raw, ",") {
			val := strings.ToLower(strings.TrimSpace(part))
			if val == "" {
				continue
			}
			switch val {
			case "none":
				if !allowDirectNav {
					return true
				}
			case "cross-site", "same-site", "same-origin":
				return true
			default:
				// Fail closed on any unrecognized browser fetch metadata value.
				return true
			}
		}
	}
	return false
}

// CheckLoopbackGuard validates Host allowlist, rejects browser Origin headers,
// rejects OPTIONS preflights, and enforces Sec-Fetch-Site restrictions.
// When a check fails, it writes a 403 JSON error response and returns false.
func CheckLoopbackGuard(w http.ResponseWriter, r *http.Request, allowedSet map[string]struct{}, allowDirectNav bool) bool {
	if !isHostAllowed(r.Host, allowedSet) {
		writeJSONError(w, http.StatusForbidden, "forbidden_host")
		return false
	}
	if hasNonEmptyOrigin(r) {
		writeJSONError(w, http.StatusForbidden, "forbidden_origin")
		return false
	}
	if r.Method == http.MethodOptions {
		writeJSONError(w, http.StatusForbidden, "forbidden_preflight")
		return false
	}
	if isDisallowedBrowserFetch(r, allowDirectNav) {
		writeJSONError(w, http.StatusForbidden, "forbidden_fetch_site")
		return false
	}
	return true
}

func extractBearerToken(r *http.Request) (string, bool) {
	if r == nil {
		return "", false
	}
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// VerifyBearer checks a single Authorization: Bearer <token> header in constant time
// by comparing SHA-256 digests so token length is never leaked via timing.
func VerifyBearer(r *http.Request, expectedToken string) bool {
	if expectedToken == "" {
		return false
	}
	got, ok := extractBearerToken(r)
	if !ok {
		return false
	}
	gotSum := sha256.Sum256([]byte(got))
	expSum := sha256.Sum256([]byte(expectedToken))
	return subtle.ConstantTimeCompare(gotSum[:], expSum[:]) == 1
}

// TokenRule describes one authorized Bearer token digest and its optional tool ACL scope.
type TokenRule struct {
	Name         string
	Digest       [32]byte
	AllowTools   []string
	DenyTools    []string
	ReadOnlyOnly bool
}

// NewTokenRule builds a TokenRule by hashing secret with SHA-256 and copying ACL slices.
func NewTokenRule(name, secret string, allow, deny []string, readOnly bool) TokenRule {
	return TokenRule{
		Name:         name,
		Digest:       sha256.Sum256([]byte(secret)),
		AllowTools:   append([]string(nil), allow...),
		DenyTools:    append([]string(nil), deny...),
		ReadOnlyOnly: readOnly,
	}
}

// TokenStore manages an atomically swappable set of Bearer TokenRules.
type TokenStore struct {
	rules atomic.Pointer[[]TokenRule]
}

// NewTokenStore creates an empty TokenStore.
func NewTokenStore() *TokenStore {
	return &TokenStore{}
}

// SetSingle replaces all rules with a single token rule (or clears the store if secret is empty).
func (s *TokenStore) SetSingle(name, secret string, allow, deny []string, readOnly bool) {
	if s == nil {
		return
	}
	if secret == "" {
		s.SetRules(nil)
		return
	}
	s.SetRules([]TokenRule{NewTokenRule(name, secret, allow, deny, readOnly)})
}

// LoadTokenFileWithGrace reads path via ReadSecretFile and, if "<path>.next" exists,
// loads it as a second valid token for zero-downtime rotation. On any error, the store
// is left unchanged.
func (s *TokenStore) LoadTokenFileWithGrace(name, path string, allow, deny []string, readOnly bool) (loadedSecrets []string, err error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return nil, errors.New("secret file path is empty")
	}
	cleanPath := filepath.Clean(trimmed)
	primary, err := ReadSecretFile(cleanPath)
	if err != nil {
		return nil, err
	}
	rules := []TokenRule{NewTokenRule(name, primary, allow, deny, readOnly)}
	loadedSecrets = []string{primary}

	nextPath := cleanPath + ".next"
	if _, statErr := os.Stat(nextPath); statErr == nil {
		nextSecret, readErr := ReadSecretFile(nextPath)
		if readErr != nil {
			return nil, fmt.Errorf("grace token file %q: %w", nextPath, readErr)
		}
		if nextSecret != primary {
			rules = append(rules, NewTokenRule(name, nextSecret, allow, deny, readOnly))
			loadedSecrets = append(loadedSecrets, nextSecret)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("stat grace token file %q: %w", nextPath, statErr)
	}

	if s != nil {
		s.SetRules(rules)
	}
	return loadedSecrets, nil
}

// SetRules atomically replaces the store's rules with a deep copy of rules.
func (s *TokenStore) SetRules(rules []TokenRule) {
	if s == nil {
		return
	}
	if len(rules) == 0 {
		s.rules.Store(nil)
		return
	}
	copied := make([]TokenRule, len(rules))
	for i, r := range rules {
		copied[i] = TokenRule{
			Name:         r.Name,
			Digest:       r.Digest,
			AllowTools:   append([]string(nil), r.AllowTools...),
			DenyTools:    append([]string(nil), r.DenyTools...),
			ReadOnlyOnly: r.ReadOnlyOnly,
		}
	}
	s.rules.Store(&copied)
}

// Empty reports whether the store currently holds no token rules.
func (s *TokenStore) Empty() bool {
	if s == nil {
		return true
	}
	rp := s.rules.Load()
	return rp == nil || len(*rp) == 0
}

// Match verifies the request's Bearer token against all stored rules in constant time
// (scanning every rule without early return) and returns the matching TokenRule.
func (s *TokenStore) Match(r *http.Request) (TokenRule, bool) {
	if s == nil || r == nil {
		return TokenRule{}, false
	}
	rp := s.rules.Load()
	if rp == nil || len(*rp) == 0 {
		return TokenRule{}, false
	}
	got, ok := extractBearerToken(r)
	if !ok {
		return TokenRule{}, false
	}
	gotSum := sha256.Sum256([]byte(got))
	rules := *rp
	matchedIdx := -1
	for i := range rules {
		if subtle.ConstantTimeCompare(gotSum[:], rules[i].Digest[:]) == 1 && matchedIdx < 0 {
			matchedIdx = i
		}
	}
	if matchedIdx < 0 {
		return TokenRule{}, false
	}
	matched := rules[matchedIdx]
	matched.AllowTools = append([]string(nil), matched.AllowTools...)
	matched.DenyTools = append([]string(nil), matched.DenyTools...)
	return matched, true
}

// Verify reports whether r carries a Bearer token matching any rule in the store.
func (s *TokenStore) Verify(r *http.Request) bool {
	_, ok := s.Match(r)
	return ok
}

// Token lifetimes allow a listener reload to keep streams whose route and rule are
// unchanged and cancel streams authorized by rules that no longer apply.
type tokenLifetime struct {
	rule   TokenRule
	ctx    context.Context
	cancel context.CancelFunc
}

type tokenLifetimes map[[32]byte]*tokenLifetime

func (s *TokenStore) lifetimes(previous tokenLifetimes) tokenLifetimes {
	if rules := s.rules.Load(); rules != nil {
		return lifetimesForRules(*rules, previous)
	}
	return nil
}

func lifetimesForRules(rules []TokenRule, previous tokenLifetimes) tokenLifetimes {
	active := make(tokenLifetimes, len(rules))
	for _, rule := range rules {
		if active[rule.Digest] != nil {
			continue
		}
		old := previous[rule.Digest]
		if old != nil && old.rule.ReadOnlyOnly == rule.ReadOnlyOnly &&
			slices.Equal(old.rule.AllowTools, rule.AllowTools) && slices.Equal(old.rule.DenyTools, rule.DenyTools) {
			active[rule.Digest] = old
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		active[rule.Digest] = &tokenLifetime{rule: rule, ctx: ctx, cancel: cancel}
	}
	return active
}

func (old tokenLifetimes) retire(next tokenLifetimes) {
	for digest, lifetime := range old {
		if lifetime != next[digest] {
			lifetime.cancel()
		}
	}
}

func (active tokenLifetimes) bind(r *http.Request, rule TokenRule) (*http.Request, func()) {
	lifetime := active[rule.Digest]
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(lifetime.ctx, cancel)
	if lifetime.ctx.Err() != nil {
		cancel()
	}
	return r.WithContext(ctx), func() {
		stop()
		cancel()
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
