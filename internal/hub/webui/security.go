package webui

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName  = "mcphub_ui"
	csrfHeaderName     = "X-McpHub-CSRF"
	bootstrapTTL       = 60 * time.Second
	maxBootstrapNonces = 16
	sessionTTL         = 12 * time.Hour
	maxSessions        = 8
	loginWindow        = 60 * time.Second
	loginMaxFailures   = 5
	maxPostBodyBytes   = 64 << 10 // 64 KiB
	cspPolicy          = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
)

var validNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func isValidName(name string) bool {
	return validNameRe.MatchString(name) && !strings.Contains(name, "..")
}

var validSnippetClients = map[string]struct{}{
	"claude":   {},
	"codex":    {},
	"cursor":   {},
	"vscode":   {},
	"agy":      {},
	"opencode": {},
	"devin":    {},
}

type bootstrapNonce struct {
	digest    [32]byte
	expiresAt time.Time
}

type uiSession struct {
	idDigest   [32]byte
	csrfDigest [32]byte
	csrfToken  string
	lastUsed   time.Time
	expiresAt  time.Time
	revoked    chan struct{}
}

type sessionView struct {
	csrfToken string
	expiresAt time.Time
	revoked   <-chan struct{}
}

type securityState struct {
	mu           sync.Mutex
	allowedHosts map[string]struct{}
	nonces       []bootstrapNonce
	sessions     []*uiSession
	loginFails   []time.Time
	now          func() time.Time
}

func newSecurityState(boundAddr string) *securityState {
	return &securityState{
		allowedHosts: buildAllowedHostSet(defaultAllowedHosts(boundAddr)),
		now:          time.Now,
	}
}

func (s *securityState) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *securityState) setBoundAddr(addr string) {
	s.mu.Lock()
	s.allowedHosts = buildAllowedHostSet(defaultAllowedHosts(addr))
	s.mu.Unlock()
}

func (s *securityState) setAllowedHosts(hosts []string) {
	s.mu.Lock()
	s.allowedHosts = buildAllowedHostSet(hosts)
	s.mu.Unlock()
}

func (s *securityState) isHostAllowed(host string) bool {
	norm := strings.ToLower(strings.TrimSpace(host))
	if norm == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.allowedHosts) == 0 {
		return false
	}
	_, ok := s.allowedHosts[norm]
	return ok
}

func defaultAllowedHosts(listenAddr string) []string {
	seen := make(map[string]struct{}, 4)
	var out []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return
		}
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	trimmed := strings.TrimSpace(listenAddr)
	if host, port, err := net.SplitHostPort(trimmed); err == nil && port != "" && port != "0" {
		if isLoopbackHost(host) {
			add(trimmed)
			add("127.0.0.1:" + port)
			add("localhost:" + port)
			add("[::1]:" + port)
		}
	}
	return out
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func buildAllowedHostSet(hosts []string) map[string]struct{} {
	set := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		norm := strings.ToLower(strings.TrimSpace(h))
		if norm != "" {
			set[norm] = struct{}{}
		}
	}
	return set
}

func randomHex256() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("webui: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (s *securityState) mintBootstrapToken() (string, time.Time) {
	nonce := randomHex256()
	digest := sha256.Sum256([]byte(nonce))

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneNoncesLocked(now)
	for len(s.nonces) >= maxBootstrapNonces {
		s.nonces = s.nonces[1:]
	}
	exp := now.Add(bootstrapTTL)
	s.nonces = append(s.nonces, bootstrapNonce{
		digest:    digest,
		expiresAt: exp,
	})
	return nonce, exp.UTC()
}

func (s *securityState) consumeBootstrapToken(candidate string) bool {
	candidate = strings.TrimSpace(candidate)
	if len(candidate) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(candidate))

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneNoncesLocked(now)

	matched := -1
	for i := range s.nonces {
		if subtle.ConstantTimeCompare(digest[:], s.nonces[i].digest[:]) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return false
	}
	s.nonces = append(s.nonces[:matched], s.nonces[matched+1:]...)
	return true
}

func (s *securityState) pruneNoncesLocked(now time.Time) {
	dst := s.nonces[:0]
	for _, n := range s.nonces {
		if now.Before(n.expiresAt) {
			dst = append(dst, n)
		}
	}
	s.nonces = dst
}

func (s *securityState) createSession() (sessionID string, view sessionView) {
	sessionID = randomHex256()
	csrfToken := randomHex256()
	idDigest := sha256.Sum256([]byte(sessionID))
	csrfDigest := sha256.Sum256([]byte(csrfToken))

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneSessionsLocked(now)
	for len(s.sessions) >= maxSessions {
		oldestIdx := 0
		for i := 1; i < len(s.sessions); i++ {
			if s.sessions[i].lastUsed.Before(s.sessions[oldestIdx].lastUsed) {
				oldestIdx = i
			}
		}
		evicted := s.sessions[oldestIdx]
		s.sessions = append(s.sessions[:oldestIdx], s.sessions[oldestIdx+1:]...)
		close(evicted.revoked)
	}

	exp := now.Add(sessionTTL)
	sess := &uiSession{
		idDigest:   idDigest,
		csrfDigest: csrfDigest,
		csrfToken:  csrfToken,
		lastUsed:   now,
		expiresAt:  exp,
		revoked:    make(chan struct{}),
	}
	s.sessions = append(s.sessions, sess)
	return sessionID, sessionView{
		csrfToken: csrfToken,
		expiresAt: exp.UTC(),
		revoked:   sess.revoked,
	}
}

func (s *securityState) lookupSession(r *http.Request) (sessionView, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || len(c.Value) != 64 {
		return sessionView{}, false
	}
	digest := sha256.Sum256([]byte(c.Value))

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneSessionsLocked(now)

	matched := -1
	for i := range s.sessions {
		if subtle.ConstantTimeCompare(digest[:], s.sessions[i].idDigest[:]) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return sessionView{}, false
	}
	sess := s.sessions[matched]
	sess.lastUsed = now
	sess.expiresAt = now.Add(sessionTTL)
	return sessionView{
		csrfToken: sess.csrfToken,
		expiresAt: sess.expiresAt.UTC(),
		revoked:   sess.revoked,
	}, true
}

func (s *securityState) verifyCSRF(r *http.Request) (sessionView, bool, bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || len(c.Value) != 64 {
		return sessionView{}, false, false
	}
	idDigest := sha256.Sum256([]byte(c.Value))

	csrfVals := r.Header.Values(csrfHeaderName)
	var csrfDigest [32]byte
	hasCSRF := len(csrfVals) == 1 && len(strings.TrimSpace(csrfVals[0])) == 64
	if hasCSRF {
		csrfDigest = sha256.Sum256([]byte(strings.TrimSpace(csrfVals[0])))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneSessionsLocked(now)

	matched := -1
	for i := range s.sessions {
		if subtle.ConstantTimeCompare(idDigest[:], s.sessions[i].idDigest[:]) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return sessionView{}, false, false
	}
	sess := s.sessions[matched]
	if !hasCSRF || subtle.ConstantTimeCompare(csrfDigest[:], sess.csrfDigest[:]) != 1 {
		return sessionView{}, true, false
	}
	sess.lastUsed = now
	sess.expiresAt = now.Add(sessionTTL)
	return sessionView{
		csrfToken: sess.csrfToken,
		expiresAt: sess.expiresAt.UTC(),
		revoked:   sess.revoked,
	}, true, true
}

func (s *securityState) deleteSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || len(c.Value) != 64 {
		return false
	}
	digest := sha256.Sum256([]byte(c.Value))

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	s.pruneSessionsLocked(now)

	matched := -1
	for i := range s.sessions {
		if subtle.ConstantTimeCompare(digest[:], s.sessions[i].idDigest[:]) == 1 && matched < 0 {
			matched = i
		}
	}
	if matched < 0 {
		return false
	}
	removed := s.sessions[matched]
	s.sessions = append(s.sessions[:matched], s.sessions[matched+1:]...)
	close(removed.revoked)
	return true
}

func (s *securityState) activeSessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneSessionsLocked(s.nowTime())
	return len(s.sessions)
}

func (s *securityState) closeAllSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		close(sess.revoked)
	}
	s.sessions = nil
	s.nonces = nil
}

func (s *securityState) pruneSessionsLocked(now time.Time) {
	dst := s.sessions[:0]
	for _, sess := range s.sessions {
		if now.Before(sess.expiresAt) {
			dst = append(dst, sess)
		} else {
			close(sess.revoked)
		}
	}
	s.sessions = dst
}

func (s *securityState) checkLoginRateLimit() (allowed bool, retryAfterSec int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.nowTime()
	cutoff := now.Add(-loginWindow)
	dst := s.loginFails[:0]
	for _, ts := range s.loginFails {
		if ts.After(cutoff) {
			dst = append(dst, ts)
		}
	}
	s.loginFails = dst
	if len(s.loginFails) >= loginMaxFailures {
		wait := s.loginFails[0].Add(loginWindow).Sub(now)
		sec := int(math.Ceil(wait.Seconds()))
		if sec < 1 {
			sec = 1
		}
		return false, sec
	}
	return true, 0
}

func (s *securityState) recordLoginFailure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginFails = append(s.loginFails, s.nowTime())
}

func (s *securityState) clearLoginFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginFails = nil
}

func setSecurityHeaders(w http.ResponseWriter, noStore bool) {
	h := w.Header()
	h.Set("Content-Security-Policy", cspPolicy)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if noStore {
		h.Set("Cache-Control", "no-store")
	}
}

func setSessionCookie(w http.ResponseWriter, sessionID string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionID,
		Path:     "/ui",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/ui",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func checkGetFetchMetadata(r *http.Request) (bool, string) {
	if originVals := r.Header.Values("Origin"); len(originVals) > 0 {
		if !isExactSameOrigin(r) {
			return false, "forbidden_origin"
		}
	}
	for _, raw := range r.Header.Values("Sec-Fetch-Site") {
		for _, part := range strings.Split(raw, ",") {
			val := strings.ToLower(strings.TrimSpace(part))
			if val == "" {
				continue
			}
			if val != "same-origin" && val != "none" {
				return false, "forbidden_fetch_site"
			}
		}
	}
	return true, ""
}

func checkPostFetchMetadata(r *http.Request) (bool, string) {
	if !isExactSameOrigin(r) {
		return false, "forbidden_origin"
	}
	sfsVals := r.Header.Values("Sec-Fetch-Site")
	if len(sfsVals) != 1 || !strings.EqualFold(strings.TrimSpace(sfsVals[0]), "same-origin") {
		return false, "forbidden_fetch_site"
	}
	return true, ""
}

func isExactSameOrigin(r *http.Request) bool {
	originVals := r.Header.Values("Origin")
	if len(originVals) != 1 {
		return false
	}
	expected := "http://" + strings.ToLower(strings.TrimSpace(r.Host))
	return strings.EqualFold(strings.TrimSpace(originVals[0]), expected)
}

func hasAnyBrowserOriginOrFetchSite(r *http.Request) (bool, string) {
	for _, v := range r.Header.Values("Origin") {
		if strings.TrimSpace(v) != "" {
			return true, "forbidden_origin"
		}
	}
	for _, raw := range r.Header.Values("Sec-Fetch-Site") {
		if strings.TrimSpace(raw) != "" {
			return true, "forbidden_fetch_site"
		}
	}
	return false, ""
}

func extractBearerToken(r *http.Request) (string, bool) {
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	setSecurityHeaders(w, true)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	setSecurityHeaders(w, true)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": code})
}
