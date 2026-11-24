package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebUIAdapter_BootstrapAuthCSRFControlSecurityAndProbe(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	cfgPath := filepath.Join(dir, "config.json")
	ctrlTokPath := filepath.Join(dir, "control.token")
	routeTokPath := filepath.Join(dir, "route.token")

	const (
		ctrlSecret  = "webui-ctrl-secret-v03-991122"
		routeSecret = "webui-route-secret-v03-887766"
	)
	if err := os.WriteFile(ctrlTokPath, []byte(ctrlSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(routeTokPath, []byte(routeSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Stateful paginated Streamable HTTP MCP mock server (page 1 JSON, page 2 SSE, DELETE cleanup).
	var (
		deleteHits atomic.Int32
		initHits   atomic.Int32
	)
	mcpMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			if r.Header.Get("Mcp-Session-Id") == "probe-sess-42" {
				deleteHits.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		case http.MethodPost:
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					Cursor string `json:"cursor"`
				} `json:"params"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch req.Method {
			case "initialize":
				initHits.Add(1)
				w.Header().Set("Mcp-Session-Id", "probe-sess-42")
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"mock-mcp","version":"1.0.0"}}}`)
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			case "tools/list":
				if req.Params.Cursor == "" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"zeta_write","description":"Mutates state","annotations":{"readOnlyHint":false},"inputSchema":{"type":"object","properties":{"id":{"type":"string"}}}}],"nextCursor":"page-2"}}`, string(req.ID))
					return
				}
				// Page 2 returned over SSE to verify SSE framing support.
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[{\"name\":\"alpha_read\",\"description\":\"Reads docs\",\"annotations\":{\"readOnlyHint\":true},\"inputSchema\":{\"type\":\"object\"}}]}}\n\n", string(req.ID))
			default:
				w.WriteHeader(http.StatusBadRequest)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer mcpMock.Close()

	cfg := &Config{
		ControlAddr:      "127.0.0.1:0",
		ControlTokenFile: ctrlTokPath,
		LogDir:           logDir,
		Services: []ServiceConfig{
			{
				Name:      "lazy-stdio",
				Mode:      "stdio-bridge",
				StartMode: "lazy",
				Command:   exe,
				Args:      []string{"-test.run=^TestHelperStdioMCP$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
				},
			},
		},
		Gateway: &GatewayConfig{
			ListenAddr: "127.0.0.1:0",
			Routes: []GatewayRouteConfig{
				{
					Slug:      "lazy-stdio",
					Service:   "lazy-stdio",
					TokenFile: routeTokPath,
				},
				{
					Slug:      "mock-mcp",
					TargetURL: mcpMock.URL,
					TokenFile: routeTokPath,
				},
			},
		},
	}
	rawCfg, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, rawCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	sup, err := NewSupervisor(cfg, cfgPath)
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Supervisor.Start: %v", err)
	}
	defer sup.Shutdown()

	ctrlAddr := sup.ControlAddr()
	baseURL := "http://" + ctrlAddr
	noRedirectClient := NewDirectClient(10 * time.Second)
	noRedirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	// 1. POST /ui/session requires Bearer auth and blocks browser Origin / Sec-Fetch-Site.
	for _, tc := range []struct {
		name     string
		method   string
		auth     string
		origin   string
		fetch    string
		wantCode int
	}{
		{"missing bearer", http.MethodPost, "", "", "", http.StatusUnauthorized},
		{"wrong bearer", http.MethodPost, "Bearer wrong-token", "", "", http.StatusUnauthorized},
		{"browser origin rejected", http.MethodPost, "Bearer " + ctrlSecret, baseURL, "", http.StatusForbidden},
		{"browser sec-fetch-site rejected", http.MethodPost, "Bearer " + ctrlSecret, "", "none", http.StatusForbidden},
		{"wrong method GET", http.MethodGet, "Bearer " + ctrlSecret, "", "", http.StatusMethodNotAllowed},
	} {
		t.Run("session_guard/"+tc.name, func(t *testing.T) {
			req, _ := http.NewRequestWithContext(ctx, tc.method, baseURL+"/ui/session", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			if tc.fetch != "" {
				req.Header.Set("Sec-Fetch-Site", tc.fetch)
			}
			resp, err := noRedirectClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}

	// 2. Mint one-time bootstrap URL via Client.CreateDashboardSession -> GET /ui?bootstrap=... sets HttpOnly SameSite=Strict cookie + 303.
	ctrlClient := NewClient(ctrlAddr, ctrlSecret)
	sessRes, _, err := ctrlClient.CreateDashboardSession(ctx)
	if err != nil {
		t.Fatalf("CreateDashboardSession: %v", err)
	}
	if !sessRes.OK || !strings.HasPrefix(sessRes.URL, baseURL+"/ui?bootstrap=") || sessRes.ExpiresAt == "" {
		t.Fatalf("unexpected CreateDashboardSession response: %+v", sessRes)
	}

	reqBoot, _ := http.NewRequestWithContext(ctx, http.MethodGet, sessRes.URL, nil)
	reqBoot.Header.Set("Sec-Fetch-Site", "none")
	respBoot, err := noRedirectClient.Do(reqBoot)
	if err != nil {
		t.Fatalf("GET bootstrap: %v", err)
	}
	_ = respBoot.Body.Close()
	if respBoot.StatusCode != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want 303", respBoot.StatusCode)
	}
	if loc := respBoot.Header.Get("Location"); loc != "/ui/" {
		t.Fatalf("bootstrap Location = %q, want /ui/", loc)
	}
	var sessionCookie *http.Cookie
	for _, c := range respBoot.Cookies() {
		if c.Name == "mcphub_ui" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("missing mcphub_ui session cookie on bootstrap 303 response")
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode || sessionCookie.Path != "/ui" {
		t.Fatalf("weak session cookie attributes: HttpOnly=%v SameSite=%v Path=%q", sessionCookie.HttpOnly, sessionCookie.SameSite, sessionCookie.Path)
	}

	// Single-use guarantee: replaying the same bootstrap URL must fail closed and set NO cookie.
	reqReplay, _ := http.NewRequestWithContext(ctx, http.MethodGet, sessRes.URL, nil)
	respReplay, err := noRedirectClient.Do(reqReplay)
	if err != nil {
		t.Fatal(err)
	}
	_ = respReplay.Body.Close()
	if respReplay.StatusCode != http.StatusSeeOther || respReplay.Header.Get("Location") != "/ui/?auth=expired" {
		t.Fatalf("replayed bootstrap = %d %q, want 303 /ui/?auth=expired", respReplay.StatusCode, respReplay.Header.Get("Location"))
	}
	if len(respReplay.Cookies()) != 0 {
		t.Fatalf("replayed bootstrap unexpectedly set cookies: %+v", respReplay.Cookies())
	}

	// 3. Load /ui/ HTML with session cookie and extract CSRF token.
	reqUI, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/ui/", nil)
	reqUI.AddCookie(sessionCookie)
	respUI, err := noRedirectClient.Do(reqUI)
	if err != nil {
		t.Fatal(err)
	}
	uiHTML, _ := io.ReadAll(respUI.Body)
	_ = respUI.Body.Close()
	if respUI.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ status = %d, want 200", respUI.StatusCode)
	}
	if csp := respUI.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("missing strict CSP header on /ui/: %q", csp)
	}
	csrfMatch := regexp.MustCompile(`meta name="mcphub-csrf" content="([0-9a-f]{64})"`).FindSubmatch(uiHTML)
	if len(csrfMatch) != 2 {
		t.Fatalf("could not find mcphub-csrf meta token in /ui/ HTML")
	}
	csrfToken := string(csrfMatch[1])

	uiGet := func(path string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
		req.AddCookie(sessionCookie)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body
	}

	uiPost := func(path, jsonBody, origin, fetchSite, csrf string) (*http.Response, []byte) {
		t.Helper()
		var rd io.Reader
		if jsonBody != "" {
			rd = strings.NewReader(jsonBody)
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, rd)
		req.AddCookie(sessionCookie)
		if jsonBody != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if fetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", fetchSite)
		}
		if csrf != "" {
			req.Header.Set("X-McpHub-CSRF", csrf)
		}
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, body
	}

	// 4. GET /ui/api/status, /catalog, /healthz, /logs/hub succeed with session cookie.
	respSt, bodySt := uiGet("/ui/api/status")
	if respSt.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/api/status = %d body=%s", respSt.StatusCode, string(bodySt))
	}
	var st StatusResponse
	if err := json.Unmarshal(bodySt, &st); err != nil || st.Version != Version || st.Services["lazy-stdio"].State != "sleeping" {
		t.Fatalf("unexpected /ui/api/status payload: err=%v st=%+v", err, st)
	}
	if respCat, bodyCat := uiGet("/ui/api/catalog"); respCat.StatusCode != http.StatusOK || !strings.Contains(string(bodyCat), "lazy-stdio") {
		t.Fatalf("GET /ui/api/catalog = %d body=%s", respCat.StatusCode, string(bodyCat))
	}
	if respHz, bodyHz := uiGet("/ui/api/healthz"); respHz.StatusCode != http.StatusOK || !strings.Contains(string(bodyHz), `"ok":true`) {
		t.Fatalf("GET /ui/api/healthz = %d body=%s", respHz.StatusCode, string(bodyHz))
	}
	if respLog, bodyLog := uiGet("/ui/api/logs/hub?lines=10"); respLog.StatusCode != http.StatusOK || !strings.Contains(string(bodyLog), `"ok":true`) {
		t.Fatalf("GET /ui/api/logs/hub = %d body=%s", respLog.StatusCode, string(bodyLog))
	}

	// 5. POST /ui/api/reload enforces Origin, Sec-Fetch-Site, and CSRF token.
	if resp, body := uiPost("/ui/api/reload", "", baseURL, "same-origin", ""); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "forbidden_csrf") {
		t.Fatalf("POST /ui/api/reload missing CSRF = %d body=%s, want 403 forbidden_csrf", resp.StatusCode, string(body))
	}
	if resp, body := uiPost("/ui/api/reload", "", "http://evil.example.com", "same-origin", csrfToken); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "forbidden_origin") {
		t.Fatalf("POST /ui/api/reload bad Origin = %d body=%s, want 403 forbidden_origin", resp.StatusCode, string(body))
	}
	if resp, body := uiPost("/ui/api/reload", "", baseURL, "cross-site", csrfToken); resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "forbidden_fetch_site") {
		t.Fatalf("POST /ui/api/reload bad Sec-Fetch-Site = %d body=%s, want 403 forbidden_fetch_site", resp.StatusCode, string(body))
	}
	if resp, body := uiPost("/ui/api/reload", "", baseURL, "same-origin", csrfToken); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("POST /ui/api/reload valid = %d body=%s, want 200 ok", resp.StatusCode, string(body))
	}

	// 6. Live SSE /ui/api/events receives ready + mapped service events on wake/sleep.
	evCtx, evCancel := context.WithTimeout(ctx, 8*time.Second)
	defer evCancel()
	reqEv, _ := http.NewRequestWithContext(evCtx, http.MethodGet, baseURL+"/ui/api/events", nil)
	reqEv.AddCookie(sessionCookie)
	reqEv.Header.Set("Sec-Fetch-Site", "same-origin")
	respEv, err := NewDirectClient(0).Do(reqEv)
	if err != nil {
		t.Fatalf("GET /ui/api/events: %v", err)
	}
	defer respEv.Body.Close()
	if respEv.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/api/events status = %d, want 200", respEv.StatusCode)
	}
	brEv := bufio.NewReader(respEv.Body)
	readEventName := func() string {
		t.Helper()
		for {
			ln, err := brEv.ReadString('\n')
			if err != nil {
				t.Fatalf("read /ui/api/events SSE: %v", err)
			}
			ln = strings.TrimRight(ln, "\r\n")
			if strings.HasPrefix(ln, "event: ") {
				return strings.TrimPrefix(ln, "event: ")
			}
		}
	}
	if first := readEventName(); first != "ready" {
		t.Fatalf("first /ui/api/events event = %q, want ready", first)
	}

	// 7. ProbeTools with wake=false skips sleeping lazy-stdio WITHOUT waking it!
	respSkip, bodySkip := uiPost("/ui/api/probe-tools", `{"service":"lazy-stdio","wake":false}`, baseURL, "same-origin", csrfToken)
	if respSkip.StatusCode != http.StatusOK {
		t.Fatalf("probe-tools wake=false status = %d body=%s", respSkip.StatusCode, string(bodySkip))
	}
	var skipOut struct {
		OK      bool   `json:"ok"`
		Skipped bool   `json:"skipped"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(bodySkip, &skipOut); err != nil || !skipOut.OK || !skipOut.Skipped || skipOut.Reason != "service is sleeping" {
		t.Fatalf("unexpected probe-tools wake=false payload: %s", string(bodySkip))
	}
	if stAfterSkip := sup.Status().Services["lazy-stdio"]; stAfterSkip.State != "sleeping" || stAfterSkip.WakeCount != 0 {
		t.Fatalf("probe-tools wake=false unexpectedly woke lazy-stdio: %+v", stAfterSkip)
	}

	// 8. ProbeTools with wake=true wakes lazy-stdio, probes tools, and emits live SSE events!
	respWakeProbe, bodyWakeProbe := uiPost("/ui/api/probe-tools", `{"service":"lazy-stdio","wake":true}`, baseURL, "same-origin", csrfToken)
	if respWakeProbe.StatusCode != http.StatusOK {
		t.Fatalf("probe-tools wake=true status = %d body=%s", respWakeProbe.StatusCode, string(bodyWakeProbe))
	}
	if strings.Contains(string(bodyWakeProbe), routeSecret) || strings.Contains(string(bodyWakeProbe), ctrlSecret) {
		t.Fatalf("probe-tools leaked raw token secret: %s", string(bodyWakeProbe))
	}
	var wakeProbeOut struct {
		OK      bool `json:"ok"`
		Skipped bool `json:"skipped"`
		Tools   []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(bodyWakeProbe, &wakeProbeOut); err != nil || !wakeProbeOut.OK || wakeProbeOut.Skipped || len(wakeProbeOut.Tools) == 0 {
		t.Fatalf("unexpected probe-tools wake=true payload: %s", string(bodyWakeProbe))
	}
	// Confirm SSE /ui/api/events delivered a mapped service event.
	gotEv := readEventName()
	if gotEv == "" || gotEv == "ready" {
		t.Fatalf("expected mapped live event on /ui/api/events, got %q", gotEv)
	}
	evCancel()

	// 9. ProbeTools on paginated stateful HTTP MCP route verifies pagination, SSE page 2, readOnlyHint, schema, and DELETE cleanup.
	respMockProbe, bodyMockProbe := uiPost("/ui/api/probe-tools", `{"service":"mock-mcp","wake":false}`, baseURL, "same-origin", csrfToken)
	if respMockProbe.StatusCode != http.StatusOK {
		t.Fatalf("probe-tools mock-mcp status = %d body=%s", respMockProbe.StatusCode, string(bodyMockProbe))
	}
	if strings.Contains(string(bodyMockProbe), routeSecret) {
		t.Fatalf("mock-mcp probe leaked route token secret")
	}
	var mockOut struct {
		OK    bool `json:"ok"`
		Tools []struct {
			Name         string          `json:"name"`
			Description  string          `json:"description"`
			ReadOnlyHint *bool           `json:"readOnlyHint"`
			InputSchema  json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(bodyMockProbe, &mockOut); err != nil || !mockOut.OK || len(mockOut.Tools) != 2 {
		t.Fatalf("unexpected paginated probe-tools payload: %s", string(bodyMockProbe))
	}
	if mockOut.Tools[0].Name != "alpha_read" || mockOut.Tools[0].ReadOnlyHint == nil || !*mockOut.Tools[0].ReadOnlyHint {
		t.Fatalf("tool[0] = %+v, want alpha_read readOnlyHint=true", mockOut.Tools[0])
	}
	if mockOut.Tools[1].Name != "zeta_write" || mockOut.Tools[1].ReadOnlyHint == nil || *mockOut.Tools[1].ReadOnlyHint {
		t.Fatalf("tool[1] = %+v, want zeta_write readOnlyHint=false", mockOut.Tools[1])
	}
	if initHits.Load() != 1 || deleteHits.Load() != 1 {
		t.Fatalf("expected 1 initialize and 1 DELETE session cleanup, got init=%d delete=%d", initHits.Load(), deleteHits.Load())
	}

	// 10. UI service actions (sleep, wake, unsupported reload -> 400).
	if resp, body := uiPost("/ui/api/services/lazy-stdio/sleep", "", baseURL, "same-origin", csrfToken); resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"sleeping"`) {
		t.Fatalf("UI sleep = %d body=%s", resp.StatusCode, string(body))
	}
	if resp, body := uiPost("/ui/api/services/lazy-stdio/reload", "", baseURL, "same-origin", csrfToken); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("UI unsupported reload = %d body=%s, want 400", resp.StatusCode, string(body))
	}

	// 11. Client snippets via HTTP (/ui/api/snippet) and direct adapter for all 9 clients never embed raw tokens.
	for _, clientName := range []string{"claude", "codex"} {
		respSnip, bodySnip := uiGet("/ui/api/snippet?service=lazy-stdio&client=" + clientName)
		if respSnip.StatusCode != http.StatusOK {
			t.Fatalf("GET /ui/api/snippet?client=%s = %d %s", clientName, respSnip.StatusCode, string(bodySnip))
		}
		var snipResp struct {
			OK      bool   `json:"ok"`
			Snippet string `json:"snippet"`
		}
		if err := json.Unmarshal(bodySnip, &snipResp); err != nil || !snipResp.OK {
			t.Fatalf("decode /ui/api/snippet?client=%s: err=%v body=%s", clientName, err, string(bodySnip))
		}
		if !strings.Contains(snipResp.Snippet, "Bearer <TOKEN_FROM: ") || strings.Contains(snipResp.Snippet, RedactedPlaceholder) || strings.Contains(snipResp.Snippet, routeSecret) {
			t.Fatalf("unexpected /ui/api/snippet?client=%s snippet:\n%s", clientName, snipResp.Snippet)
		}
	}
	adapter := newSupervisorWebUIBackend(sup)
	for _, tc := range []struct {
		client string
		needle string
	}{
		{"claude", `"type": "http"`},
		{"codex", "[mcp_servers.lazy-stdio]"},
		{"cursor", `"mcpServers"`},
		{"vscode", `"servers"`},
		{"agy", `"serverUrl"`},
		{"opencode", `"type": "remote"`},
		{"devin", `"mcpServers"`},
		{"windsurf", `"serverUrl"`},
		{"gemini", `"httpUrl"`},
	} {
		snip, err := adapter.ClientSnippet("lazy-stdio", tc.client)
		if err != nil {
			t.Fatalf("ClientSnippet(%q): %v", tc.client, err)
		}
		if !strings.Contains(snip, tc.needle) {
			t.Errorf("ClientSnippet(%q) missing %q:\n%s", tc.client, tc.needle, snip)
		}
		if !strings.Contains(snip, "<TOKEN_FROM:") {
			t.Errorf("ClientSnippet(%q) missing <TOKEN_FROM:> placeholder:\n%s", tc.client, snip)
		}
		if strings.Contains(snip, routeSecret) || strings.Contains(snip, ctrlSecret) {
			t.Errorf("ClientSnippet(%q) leaked raw token secret!", tc.client)
		}
	}

	// 12. Existing /healthz, /status, /catalog security invariants remain 100% untouched!
	// /healthz unauthenticated succeeds.
	if resp, err := noRedirectClient.Get(baseURL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz unauth: resp=%v err=%v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
	// /status and /catalog reject unauthenticated AND UI-cookie-only requests (UI cookie never bypasses Bearer auth!).
	for _, p := range []string{"/status", "/catalog"} {
		reqCookieOnly, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+p, nil)
		reqCookieOnly.AddCookie(sessionCookie)
		resp, err := noRedirectClient.Do(reqCookieOnly)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("GET %s with UI cookie only = %d, want 401", p, resp.StatusCode)
		}
	}
	// /status rejects browser Origin and Sec-Fetch-Site even with valid Bearer token.
	for _, hdr := range [][2]string{{"Origin", baseURL}, {"Sec-Fetch-Site", "none"}} {
		reqBad, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/status", nil)
		reqBad.Header.Set("Authorization", "Bearer "+ctrlSecret)
		reqBad.Header.Set(hdr[0], hdr[1])
		resp, err := noRedirectClient.Do(reqBad)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET /status with %s=%s = %d, want 403", hdr[0], hdr[1], resp.StatusCode)
		}
	}
	// /status and /catalog succeed with valid CLI Bearer token.
	if _, _, err := ctrlClient.Status(ctx); err != nil {
		t.Fatalf("Client.Status with Bearer: %v", err)
	}
	if _, _, err := ctrlClient.Catalog(ctx); err != nil {
		t.Fatalf("Client.Catalog with Bearer: %v", err)
	}
}
