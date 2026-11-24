package webui

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu           sync.Mutex
	controlToken string
	allowNoAuth  bool
	secretWord   string
	statusJSON   []byte
	catalogJSON  []byte
	healthzJSON  []byte
	logs         map[string][]LogEntry
	logSubs      map[string][]chan LogEntry
	eventSubs    []chan Event
	probeFn      func(ctx context.Context, service string, wake bool) (ToolsProbeResult, error)
	reloadErr    error
	lastAction   string
	lastWait     bool
}

func newFakeBackend(token string) *fakeBackend {
	return &fakeBackend{
		controlToken: token,
		secretWord:   "SUPER_SECRET_TOKEN_999",
		statusJSON:   []byte(`{"schemaVersion":1,"ok":true,"version":"0.3.0","services":{"docs-mcp":{"name":"docs-mcp","running":true,"healthy":true,"state":"running"}}}`),
		catalogJSON:  []byte(`{"schemaVersion":1,"ok":true,"entries":[{"name":"docs-mcp","mode":"http","clientUrl":"http://127.0.0.1:18471/s/docs/mcp","auth":{"type":"bearer","tokenFile":"tokens/docs.token"}}]}`),
		healthzJSON:  []byte(`{"ok":true,"services":{"docs-mcp":{"running":true,"healthy":true}}}`),
		logs: map[string][]LogEntry{
			"hub": {
				{Seq: 1, Timestamp: "2026-04-01T10:00:00Z", Stream: "hub", Line: "starting mcp-hub"},
				{Seq: 2, Timestamp: "2026-04-01T10:00:01Z", Stream: "hub", Line: "token=SUPER_SECRET_TOKEN_999"},
			},
			"docs-mcp": {
				{Seq: 10, Timestamp: "2026-04-01T10:00:02Z", Stream: "stdout", Line: "listening on :18473"},
				{Seq: 11, Timestamp: "2026-04-01T10:00:03Z", Stream: "stderr", Line: "WARN cache miss"},
			},
		},
		logSubs: make(map[string][]chan LogEntry),
	}
}

func (f *fakeBackend) VerifyControlToken(token string) bool {
	if f.controlToken == "" {
		return false
	}
	a := sha256.Sum256([]byte(token))
	b := sha256.Sum256([]byte(f.controlToken))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (f *fakeBackend) AllowNoAuth() bool { return f.allowNoAuth }

func (f *fakeBackend) StatusJSON() ([]byte, error)  { return f.statusJSON, nil }
func (f *fakeBackend) CatalogJSON() ([]byte, error) { return f.catalogJSON, nil }
func (f *fakeBackend) HealthzJSON() ([]byte, error) { return f.healthzJSON, nil }

func (f *fakeBackend) LogsTail(name string, lines int) ([]LogEntry, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	list, ok := f.logs[name]
	if !ok {
		return nil, false, nil
	}
	if lines > 0 && len(list) > lines {
		list = list[len(list)-lines:]
	}
	out := make([]LogEntry, len(list))
	copy(out, list)
	return out, true, nil
}

func (f *fakeBackend) SubscribeLogs(name string) (<-chan LogEntry, func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.logs[name]; !ok {
		return nil, nil, false
	}
	ch := make(chan LogEntry, 16)
	f.logSubs[name] = append(f.logSubs[name], ch)
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		subs := f.logSubs[name]
		for i, c := range subs {
			if c == ch {
				f.logSubs[name] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
	}, true
}

func (f *fakeBackend) pushLog(name string, e LogEntry) {
	f.mu.Lock()
	f.logs[name] = append(f.logs[name], e)
	subs := append([]chan LogEntry(nil), f.logSubs[name]...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

func (f *fakeBackend) SubscribeEvents() (<-chan Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan Event, 16)
	f.eventSubs = append(f.eventSubs, ch)
	return ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, c := range f.eventSubs {
			if c == ch {
				f.eventSubs = append(f.eventSubs[:i], f.eventSubs[i+1:]...)
				break
			}
		}
	}
}

func (f *fakeBackend) pushEvent(ev Event) {
	f.mu.Lock()
	subs := append([]chan Event(nil), f.eventSubs...)
	f.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (f *fakeBackend) recordAction(action string, wait bool) {
	f.mu.Lock()
	f.lastAction = action
	f.lastWait = wait
	f.mu.Unlock()
}

func (f *fakeBackend) StartService(_ context.Context, name string) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("start:"+name, false)
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) StopService(_ context.Context, name string) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("stop:"+name, false)
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) RestartService(_ context.Context, name string, waitHealthy bool) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("restart:"+name, waitHealthy)
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) ReloadService(_ context.Context, name string) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("reload:"+name, false)
	if f.reloadErr != nil {
		return nil, true, f.reloadErr
	}
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) SleepService(_ context.Context, name string) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("sleep:"+name, false)
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) WakeService(_ context.Context, name string) ([]byte, bool, error) {
	if name == "missing" {
		return nil, false, nil
	}
	f.recordAction("wake:"+name, false)
	return []byte(`{"ok":true,"name":"` + name + `"}`), true, nil
}

func (f *fakeBackend) StartAll(_ context.Context) ([]byte, error) {
	f.recordAction("start-all", false)
	return []byte(`{"ok":true}`), nil
}

func (f *fakeBackend) StopAll(_ context.Context) ([]byte, error) {
	f.recordAction("stop-all", false)
	return []byte(`{"ok":true}`), nil
}

func (f *fakeBackend) RestartAll(_ context.Context) ([]byte, error) {
	f.recordAction("restart-all", false)
	return []byte(`{"ok":true}`), nil
}

func (f *fakeBackend) ReloadHub(_ context.Context) ([]byte, error) {
	f.recordAction("reload-hub", false)
	return []byte(`{"ok":true,"unchangedServices":["docs-mcp"]}`), nil
}

func (f *fakeBackend) ProbeTools(ctx context.Context, service string, wake bool) (ToolsProbeResult, error) {
	if f.probeFn != nil {
		return f.probeFn(ctx, service, wake)
	}
	ro := true
	return ToolsProbeResult{
		Service:   service,
		Endpoint:  "http://127.0.0.1:18471/s/" + service + "/mcp",
		LatencyMs: 24,
		Tools: []ToolInfo{
			{
				Name:         "search_docs",
				Description:  "Search documentation (secret SUPER_SECRET_TOKEN_999)",
				ReadOnlyHint: &ro,
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`),
			},
		},
	}, nil
}

func (f *fakeBackend) ClientSnippet(service, client string) (string, error) {
	if service == "missing" {
		return "", errors.New("service not found")
	}
	return `{"client":"` + client + `","service":"` + service + `","note":"SUPER_SECRET_TOKEN_999"}`, nil
}

func (f *fakeBackend) Redact(s string) string {
	if f.secretWord == "" {
		return s
	}
	return strings.ReplaceAll(s, f.secretWord, "[REDACTED]")
}

func doReq(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func bootstrapAuth(t *testing.T, h *Handler, host string) (*http.Cookie, string) {
	t.Helper()
	nonce, _ := h.MintBootstrapToken()
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/?bootstrap="+nonce, nil)
	req.Host = host
	req.Header.Set("Sec-Fetch-Site", "none")
	rr := doReq(h, req)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap expected 303, got %d", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/ui/" {
		t.Fatalf("bootstrap Location = %q, want /ui/", loc)
	}
	var sessionCookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == sessionCookieName {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatalf("missing %s cookie", sessionCookieName)
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode || sessionCookie.Path != "/ui" {
		t.Fatalf("unexpected cookie attributes: %+v", sessionCookie)
	}

	// Fetch CSRF token from /ui/api/session
	sReq := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/api/session", nil)
	sReq.Host = host
	sReq.Header.Set("Sec-Fetch-Site", "same-origin")
	sReq.AddCookie(sessionCookie)
	sRR := doReq(h, sReq)
	if sRR.Code != http.StatusOK {
		t.Fatalf("GET /ui/api/session expected 200, got %d: %s", sRR.Code, sRR.Body.String())
	}
	var payload struct {
		OK        bool   `json:"ok"`
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(sRR.Body.Bytes(), &payload); err != nil || !payload.OK || len(payload.CSRFToken) != 64 {
		t.Fatalf("invalid session payload: %s", sRR.Body.String())
	}
	return sessionCookie, payload.CSRFToken
}

func TestHostAllowlistAndPreflight(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	for _, badHost := range []string{"evil.example:18470", "127.0.0.1:9999", "192.168.1.5:18470", ""} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/", nil)
		req.Host = badHost
		rr := doReq(h, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("host %q expected 403, got %d", badHost, rr.Code)
		}
	}

	for _, goodHost := range []string{"127.0.0.1:18470", "localhost:18470", "[::1]:18470"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/", nil)
		req.Host = goodHost
		rr := doReq(h, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("host %q expected 200 unlock page, got %d", goodHost, rr.Code)
		}
		if got := rr.Header().Get("Content-Security-Policy"); got != cspPolicy {
			t.Fatalf("CSP mismatch: %q", got)
		}
		if rr.Header().Get("X-Content-Type-Options") != "nosniff" ||
			rr.Header().Get("Referrer-Policy") != "no-referrer" ||
			rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("missing security headers: %+v", rr.Header())
		}
	}

	optReq := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:18470/ui/api/status", nil)
	optReq.Host = "127.0.0.1:18470"
	if rr := doReq(h, optReq); rr.Code != http.StatusForbidden {
		t.Fatalf("OPTIONS expected 403, got %d", rr.Code)
	}
}

func TestBootstrapNonceLifecycle(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	h.sec.now = func() time.Time { return now }

	// Mint nonce and verify HEAD does not consume it.
	nonce, exp := h.MintBootstrapToken()
	if len(nonce) != 64 || !exp.Equal(now.Add(bootstrapTTL)) {
		t.Fatalf("unexpected nonce len=%d exp=%v", len(nonce), exp)
	}
	headReq := httptest.NewRequest(http.MethodHead, "http://127.0.0.1:18470/ui/?bootstrap="+nonce, nil)
	headReq.Host = "127.0.0.1:18470"
	if rr := doReq(h, headReq); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD bootstrap expected 405, got %d", rr.Code)
	}

	// First GET /ui?bootstrap=<nonce> succeeds and redirects 303 to /ui/ with cookie.
	req1 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui?bootstrap="+nonce, nil)
	req1.Host = "127.0.0.1:18470"
	req1.Header.Set("Sec-Fetch-Site", "none")
	rr1 := doReq(h, req1)
	if rr1.Code != http.StatusSeeOther || rr1.Header().Get("Location") != "/ui/" {
		t.Fatalf("first bootstrap got %d loc=%q", rr1.Code, rr1.Header().Get("Location"))
	}
	if len(rr1.Result().Cookies()) == 0 {
		t.Fatalf("expected session cookie on first bootstrap")
	}

	// Replay fails (single-use).
	req2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/?bootstrap="+nonce, nil)
	req2.Host = "127.0.0.1:18470"
	rr2 := doReq(h, req2)
	if rr2.Code != http.StatusSeeOther || rr2.Header().Get("Location") != "/ui/?auth=expired" {
		t.Fatalf("replayed bootstrap got %d loc=%q", rr2.Code, rr2.Header().Get("Location"))
	}
	if len(rr2.Result().Cookies()) != 0 {
		t.Fatalf("replayed bootstrap must not set cookie")
	}

	// Expired nonce fails after 60s TTL.
	expNonce, _ := h.MintBootstrapToken()
	now = now.Add(bootstrapTTL + time.Second)
	reqExp := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/?bootstrap="+expNonce, nil)
	reqExp.Host = "127.0.0.1:18470"
	rrExp := doReq(h, reqExp)
	if rrExp.Code != http.StatusSeeOther || rrExp.Header().Get("Location") != "/ui/?auth=expired" {
		t.Fatalf("expired bootstrap got %d loc=%q", rrExp.Code, rrExp.Header().Get("Location"))
	}

	// Max 16 active nonces cap evicts oldest.
	nonces := make([]string, 20)
	for i := range nonces {
		nonces[i], _ = h.MintBootstrapToken()
	}
	if h.sec.consumeBootstrapToken(nonces[0]) {
		t.Fatalf("oldest evicted nonce should be rejected")
	}
	if !h.sec.consumeBootstrapToken(nonces[19]) {
		t.Fatalf("newest nonce should still be valid")
	}
}

func TestMintSessionBearerEndpoint(t *testing.T) {
	fb := newFakeBackend("my-control-token")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	// Unauthorized without Bearer
	r1 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/session", nil)
	r1.Host = "127.0.0.1:18470"
	if rr := doReq(h, r1); rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without bearer, got %d", rr.Code)
	}

	// Rejected if browser Origin header is present
	r2 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/session", nil)
	r2.Host = "127.0.0.1:18470"
	r2.Header.Set("Authorization", "Bearer my-control-token")
	r2.Header.Set("Origin", "http://127.0.0.1:18470")
	if rr := doReq(h, r2); rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 with Origin header, got %d", rr.Code)
	}

	// Succeeds with valid Bearer and no browser headers
	r3 := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/session", nil)
	r3.Host = "127.0.0.1:18470"
	r3.Header.Set("Authorization", "Bearer my-control-token")
	rr3 := doReq(h, r3)
	if rr3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr3.Code, rr3.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Nonce string `json:"nonce"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(rr3.Body.Bytes(), &resp); err != nil || !resp.OK || len(resp.Nonce) != 64 || !strings.Contains(resp.URL, resp.Nonce) {
		t.Fatalf("unexpected /ui/session response: %s", rr3.Body.String())
	}
}

func TestLoginRateLimitAndLogout(t *testing.T) {
	fb := newFakeBackend("correct-token")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	now := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	h.sec.now = func() time.Time { return now }

	postLogin := func(tok string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"token": tok})
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/api/login", bytes.NewReader(body))
		req.Host = "127.0.0.1:18470"
		req.Header.Set("Origin", "http://127.0.0.1:18470")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		return doReq(h, req)
	}

	for i := 0; i < loginMaxFailures; i++ {
		if rr := postLogin("wrong-token"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i+1, rr.Code)
		}
	}
	// 6th attempt rate-limited
	rrLimit := postLogin("correct-token")
	if rrLimit.Code != http.StatusTooManyRequests || rrLimit.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", rrLimit.Code)
	}

	// Advance past rate-limit window -> login succeeds
	now = now.Add(loginWindow + time.Second)
	rrOK := postLogin("correct-token")
	if rrOK.Code != http.StatusOK {
		t.Fatalf("expected 200 after window, got %d: %s", rrOK.Code, rrOK.Body.String())
	}
	var loginResp struct {
		OK        bool   `json:"ok"`
		CSRFToken string `json:"csrfToken"`
	}
	_ = json.Unmarshal(rrOK.Body.Bytes(), &loginResp)
	cookie := rrOK.Result().Cookies()[0]

	// Authenticated GET /ui/ serves dashboard HTML with embedded CSRF meta
	idxReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/", nil)
	idxReq.Host = "127.0.0.1:18470"
	idxReq.AddCookie(cookie)
	idxRR := doReq(h, idxReq)
	if idxRR.Code != http.StatusOK || !strings.Contains(idxRR.Body.String(), loginResp.CSRFToken) {
		t.Fatalf("expected authenticated HTML with CSRF token, got %d", idxRR.Code)
	}

	// Logout clears session & cookie
	outReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/api/logout", nil)
	outReq.Host = "127.0.0.1:18470"
	outReq.Header.Set("Origin", "http://127.0.0.1:18470")
	outReq.Header.Set("Sec-Fetch-Site", "same-origin")
	outReq.Header.Set(csrfHeaderName, loginResp.CSRFToken)
	outReq.AddCookie(cookie)
	outRR := doReq(h, outReq)
	if outRR.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d", outRR.Code)
	}
	if outRR.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout expected MaxAge=-1 cookie")
	}

	// Old cookie is now invalid
	stReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/api/status", nil)
	stReq.Host = "127.0.0.1:18470"
	stReq.Header.Set("Sec-Fetch-Site", "same-origin")
	stReq.AddCookie(cookie)
	if rr := doReq(h, stReq); rr.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout API expected 401, got %d", rr.Code)
	}
}

func TestCSRFFetchSiteAndOriginGuards(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	cookie, csrf := bootstrapAuth(t, h, "127.0.0.1:18470")

	// GET /ui/api/status rejects cross-site and same-site Sec-Fetch-Site
	for _, badSFS := range []string{"cross-site", "same-site", "weird"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/api/status", nil)
		req.Host = "127.0.0.1:18470"
		req.Header.Set("Sec-Fetch-Site", badSFS)
		req.AddCookie(cookie)
		if rr := doReq(h, req); rr.Code != http.StatusForbidden {
			t.Fatalf("GET with Sec-Fetch-Site=%q expected 403, got %d", badSFS, rr.Code)
		}
	}

	// GET /ui/api/status allows same-origin and none
	for _, okSFS := range []string{"same-origin", "none"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/api/status", nil)
		req.Host = "127.0.0.1:18470"
		req.Header.Set("Sec-Fetch-Site", okSFS)
		req.AddCookie(cookie)
		if rr := doReq(h, req); rr.Code != http.StatusOK {
			t.Fatalf("GET with Sec-Fetch-Site=%q expected 200, got %d", okSFS, rr.Code)
		}
	}

	// POST /ui/api/reload security matrix
	cases := []struct {
		name     string
		origin   string
		sfs      string
		csrf     string
		cookie   *http.Cookie
		wantCode int
	}{
		{"missing origin", "", "same-origin", csrf, cookie, http.StatusForbidden},
		{"wrong origin", "http://evil.com", "same-origin", csrf, cookie, http.StatusForbidden},
		{"https origin mismatch", "https://127.0.0.1:18470", "same-origin", csrf, cookie, http.StatusForbidden},
		{"missing Sec-Fetch-Site", "http://127.0.0.1:18470", "", csrf, cookie, http.StatusForbidden},
		{"cross-site Sec-Fetch-Site", "http://127.0.0.1:18470", "cross-site", csrf, cookie, http.StatusForbidden},
		{"none Sec-Fetch-Site", "http://127.0.0.1:18470", "none", csrf, cookie, http.StatusForbidden},
		{"missing cookie", "http://127.0.0.1:18470", "same-origin", csrf, nil, http.StatusUnauthorized},
		{"missing csrf", "http://127.0.0.1:18470", "same-origin", "", cookie, http.StatusForbidden},
		{"wrong csrf", "http://127.0.0.1:18470", "same-origin", strings.Repeat("a", 64), cookie, http.StatusForbidden},
		{"valid request", "http://127.0.0.1:18470", "same-origin", csrf, cookie, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/ui/api/reload", nil)
			req.Host = "127.0.0.1:18470"
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.sfs != "" {
				req.Header.Set("Sec-Fetch-Site", tc.sfs)
			}
			if tc.csrf != "" {
				req.Header.Set(csrfHeaderName, tc.csrf)
			}
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			rr := doReq(h, req)
			if rr.Code != tc.wantCode {
				t.Fatalf("%s: got %d (%s), want %d", tc.name, rr.Code, rr.Body.String(), tc.wantCode)
			}
		})
	}
}

func TestSessionCapAndNoAuthMode(t *testing.T) {
	fb := newFakeBackend("")
	fb.allowNoAuth = true
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	// AllowNoAuth auto-mints a session cookie on GET /ui/ so POST still has CSRF protection
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/", nil)
	req.Host = "127.0.0.1:18470"
	rr := doReq(h, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("noauth GET /ui/ expected 200, got %d", rr.Code)
	}
	if len(rr.Result().Cookies()) == 0 {
		t.Fatalf("noauth GET /ui/ must set session cookie for CSRF")
	}

	// Verify session cap at 8 concurrent sessions
	for i := 0; i < 12; i++ {
		h.sec.createSession()
	}
	if got := h.ActiveSessionCount(); got != maxSessions {
		t.Fatalf("ActiveSessionCount = %d, want %d", got, maxSessions)
	}
}

func TestAllAPIEndpointsAndRedaction(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	cookie, csrf := bootstrapAuth(t, h, "127.0.0.1:18470")

	getAPI := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470"+path, nil)
		req.Host = "127.0.0.1:18470"
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.AddCookie(cookie)
		return doReq(h, req)
	}
	postAPI := func(path string, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470"+path, rdr)
		req.Host = "127.0.0.1:18470"
		req.Header.Set("Origin", "http://127.0.0.1:18470")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set(csrfHeaderName, csrf)
		req.AddCookie(cookie)
		return doReq(h, req)
	}

	// Status, catalog, healthz
	for _, ep := range []string{"/ui/api/status", "/ui/api/catalog", "/ui/api/healthz"} {
		if rr := getAPI(ep); rr.Code != http.StatusOK {
			t.Fatalf("GET %s failed: %d %s", ep, rr.Code, rr.Body.String())
		}
	}

	// Logs tail & redaction
	rrLogs := getAPI("/ui/api/logs/hub?lines=10")
	if rrLogs.Code != http.StatusOK {
		t.Fatalf("GET logs/hub failed: %d", rrLogs.Code)
	}
	if strings.Contains(rrLogs.Body.String(), fb.secretWord) || !strings.Contains(rrLogs.Body.String(), "[REDACTED]") {
		t.Fatalf("logs not redacted: %s", rrLogs.Body.String())
	}
	if rr := getAPI("/ui/api/logs/nonexistent"); rr.Code != http.StatusNotFound {
		t.Fatalf("missing log expected 404, got %d", rr.Code)
	}
	if rr := getAPI("/ui/api/logs/bad..name"); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid log name expected 400, got %d", rr.Code)
	}

	// Snippets for all 7 clients + redaction
	for _, client := range []string{"claude", "codex", "cursor", "vscode", "agy", "opencode", "devin"} {
		rr := getAPI("/ui/api/snippet?service=docs-mcp&client=" + client)
		if rr.Code != http.StatusOK {
			t.Fatalf("snippet %s failed: %d %s", client, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), fb.secretWord) {
			t.Fatalf("snippet leaked secret for %s", client)
		}
	}
	if rr := getAPI("/ui/api/snippet?service=docs-mcp&client=unknown"); rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown snippet client expected 400, got %d", rr.Code)
	}

	// Service & global actions
	for _, act := range []struct {
		path   string
		body   string
		expect string
		wait   bool
	}{
		{"/ui/api/services/docs-mcp/start", "", "start:docs-mcp", false},
		{"/ui/api/services/docs-mcp/stop", "", "stop:docs-mcp", false},
		{"/ui/api/services/docs-mcp/restart?wait=healthy", "", "restart:docs-mcp", true},
		{"/ui/api/services/docs-mcp/restart", `{"waitHealthy":true}`, "restart:docs-mcp", true},
		{"/ui/api/services/docs-mcp/reload", "", "reload:docs-mcp", false},
		{"/ui/api/services/docs-mcp/sleep", "", "sleep:docs-mcp", false},
		{"/ui/api/services/docs-mcp/wake", "", "wake:docs-mcp", false},
		{"/ui/api/services/start-all", "", "start-all", false},
		{"/ui/api/services/stop-all", "", "stop-all", false},
		{"/ui/api/services/restart-all", "", "restart-all", false},
		{"/ui/api/reload", "", "reload-hub", false},
	} {
		rr := postAPI(act.path, act.body)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %s got %d: %s", act.path, rr.Code, rr.Body.String())
		}
		if fb.lastAction != act.expect || fb.lastWait != act.wait {
			t.Fatalf("POST %s recorded action=%q wait=%v, want %q %v", act.path, fb.lastAction, fb.lastWait, act.expect, act.wait)
		}
	}

	// ErrReloadNotSupported maps to 400; missing service maps to 404
	fb.reloadErr = ErrReloadNotSupported
	if rr := postAPI("/ui/api/services/docs-mcp/reload", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("ErrReloadNotSupported expected 400, got %d", rr.Code)
	}
	if rr := postAPI("/ui/api/services/missing/start", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("missing service expected 404, got %d", rr.Code)
	}

	// Probe tools + redaction
	rrProbe := postAPI("/ui/api/probe-tools", `{"service":"docs-mcp","wake":false}`)
	if rrProbe.Code != http.StatusOK {
		t.Fatalf("probe-tools got %d: %s", rrProbe.Code, rrProbe.Body.String())
	}
	if strings.Contains(rrProbe.Body.String(), fb.secretWord) || !strings.Contains(rrProbe.Body.String(), "search_docs") {
		t.Fatalf("unexpected probe-tools body: %s", rrProbe.Body.String())
	}
}

func TestSSELogsAndEventsStreaming(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:0", fb)
	defer h.Close()

	srv := httptest.NewServer(h)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	h.SetBoundAddr(host)

	cookie, _ := bootstrapAuth(t, h, host)

	// 1. SSE Logs with dedupe and live push
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/api/logs/docs-mcp?follow=1&lines=10", nil)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(cookie)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE logs request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected SSE logs response: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	var gotLines []string
	// Push duplicate seq=11 (must be deduped) and new seq=12 (must arrive redacted)
	go func() {
		time.Sleep(40 * time.Millisecond)
		fb.pushLog("docs-mcp", LogEntry{Seq: 11, Timestamp: "2026-04-01T10:00:03Z", Stream: "stderr", Line: "duplicate"})
		fb.pushLog("docs-mcp", LogEntry{Seq: 12, Timestamp: "2026-04-01T10:00:04Z", Stream: "stdout", Line: "live SUPER_SECRET_TOKEN_999"})
	}()

	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			gotLines = append(gotLines, line)
			if strings.Contains(line, `"seq":12`) {
				cancel()
				break
			}
		}
	}
	joined := strings.Join(gotLines, "\n")
	if strings.Contains(joined, "duplicate") {
		t.Fatalf("SSE failed to deduplicate seq=11: %s", joined)
	}
	if strings.Contains(joined, fb.secretWord) || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("SSE live line not redacted: %s", joined)
	}

	// 2. SSE Events stream
	evCtx, evCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer evCancel()

	evReq, _ := http.NewRequestWithContext(evCtx, http.MethodGet, srv.URL+"/ui/api/events", nil)
	evReq.Header.Set("Sec-Fetch-Site", "same-origin")
	evReq.AddCookie(cookie)
	evResp, err := http.DefaultClient.Do(evReq)
	if err != nil {
		t.Fatalf("SSE events failed: %v", err)
	}
	defer evResp.Body.Close()

	go func() {
		time.Sleep(40 * time.Millisecond)
		fb.pushEvent(Event{Type: "service_change", Service: "docs-mcp", Message: "reloaded SUPER_SECRET_TOKEN_999", Timestamp: "2026-04-01T10:00:05Z"})
	}()

	evScanner := bufio.NewScanner(evResp.Body)
	var sawReady, sawServiceEv bool
	for evScanner.Scan() {
		ln := evScanner.Text()
		if ln == "event: ready" {
			sawReady = true
		}
		if strings.HasPrefix(ln, "data: ") && strings.Contains(ln, "docs-mcp") {
			if strings.Contains(ln, fb.secretWord) {
				t.Fatalf("SSE event leaked secret: %s", ln)
			}
			sawServiceEv = true
			evCancel()
			break
		}
	}
	if !sawReady || !sawServiceEv {
		t.Fatalf("SSE events missing expected frames: ready=%v serviceEv=%v", sawReady, sawServiceEv)
	}
}

func TestEmbeddedAssetsStrictCSPAndNoCDNs(t *testing.T) {
	fb := newFakeBackend("control-secret")
	h := NewHandler("127.0.0.1:18470", fb)
	defer h.Close()

	for _, asset := range []string{"app.css", "app.js", "unlock.js", "favicon.svg"} {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/assets/"+asset, nil)
		req.Host = "127.0.0.1:18470"
		rr := doReq(h, req)
		if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
			t.Fatalf("asset %s failed: %d", asset, rr.Code)
		}
		body := rr.Body.String()
		if strings.Contains(body, "https://cdn") || strings.Contains(body, "unpkg.com") || strings.Contains(body, "fonts.googleapis.com") {
			t.Fatalf("asset %s must not reference external CDNs", asset)
		}
	}

	// Path traversal blocked
	badReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/ui/assets/../index.html", nil)
	badReq.Host = "127.0.0.1:18470"
	if rr := doReq(h, badReq); rr.Code != http.StatusNotFound {
		t.Fatalf("path traversal expected 404, got %d", rr.Code)
	}

	// Verify HTML templates have zero inline scripts, zero inline styles, zero CDN URLs
	for _, name := range []string{"assets/index.html", "assets/unlock.html"} {
		raw, err := assetsFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		s := string(raw)
		if strings.Contains(s, "<style") || strings.Contains(s, "style=") {
			t.Fatalf("%s must not contain inline styles under style-src 'self'", name)
		}
		if strings.Contains(s, "<script>") {
			t.Fatalf("%s must not contain inline scripts under script-src 'self'", name)
		}
		if strings.Contains(s, "https://") || strings.Contains(s, "http://") {
			t.Fatalf("%s must not contain external URLs", name)
		}
	}
}
