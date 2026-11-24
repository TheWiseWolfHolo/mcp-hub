package hub

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyHandler_AuthHostOriginAndHeaderStripping(t *testing.T) {
	const token = "proxy-secret-token-xyz"
	var (
		upstreamHost    string
		upstreamAuth    string
		upstreamSession string
		upstreamHeaders http.Header
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHost = r.Host
		upstreamAuth = r.Header.Get("Authorization")
		upstreamSession = r.Header.Get("Mcp-Session-Id")
		upstreamHeaders = r.Header.Clone()
		w.Header().Set("Mcp-Session-Id", "sess-resp-42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer upstream.Close()

	cfg := ProxyConfig{
		Name:       "playwright",
		ListenAddr: "127.0.0.1:18476",
		TargetURL:  upstream.URL,
	}
	handler, err := NewProxyHandler(cfg, token, cfg.ListenAddr, nil)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}

	t.Run("missing bearer returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); got != "Bearer" {
			t.Errorf("WWW-Authenticate = %q, want Bearer", got)
		}
	})

	t.Run("wrong bearer returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer wrong-token")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("bad host, origin, OPTIONS, and Sec-Fetch-Site return 403", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
		req.Host = "evil.rebind.test:18476"
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("bad host status = %d, want 403", rec.Code)
		}

		reqO := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
		reqO.Header.Set("Authorization", "Bearer "+token)
		reqO.Header.Set("Origin", "https://evil.example")
		recO := httptest.NewRecorder()
		handler.ServeHTTP(recO, reqO)
		if recO.Code != http.StatusForbidden {
			t.Fatalf("origin status = %d, want 403", recO.Code)
		}

		reqOpt := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:18476/mcp", nil)
		recOpt := httptest.NewRecorder()
		handler.ServeHTTP(recOpt, reqOpt)
		if recOpt.Code != http.StatusForbidden {
			t.Fatalf("OPTIONS status = %d, want 403", recOpt.Code)
		}

		reqFetch := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
		reqFetch.Header.Set("Authorization", "Bearer "+token)
		reqFetch.Header.Set("Sec-Fetch-Site", "none")
		recFetch := httptest.NewRecorder()
		handler.ServeHTTP(recFetch, reqFetch)
		if recFetch.Code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site: none on /mcp status = %d, want 403", recFetch.Code)
		}
	})

	t.Run("valid bearer forwards cleanly and strips proxy Authorization + dangerous forwarding headers", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:18476/mcp", strings.NewReader(`{"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Mcp-Session-Id", "client-sess-99")
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Port", "443")
		req.Header.Set("Forwarded", "for=1.2.3.4;host=evil.example")
		req.Header.Set("X-Real-IP", "1.2.3.4")
		req.Header.Set("Cookie", "session=leak")
		req.Header.Set("Proxy-Authorization", "Basic leak")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if upstreamAuth != "" {
			t.Errorf("expected proxy Authorization header to be stripped upstream, got %q", upstreamAuth)
		}
		for _, stripped := range []string{
			"X-Forwarded-Host", "X-Forwarded-Proto", "X-Forwarded-Port",
			"Forwarded", "X-Real-IP", "Cookie", "Proxy-Authorization",
		} {
			if got := upstreamHeaders.Get(stripped); got != "" {
				t.Errorf("expected header %s to be stripped upstream, got %q", stripped, got)
			}
		}
		if upstreamSession != "client-sess-99" {
			t.Errorf("upstream Mcp-Session-Id = %q, want client-sess-99", upstreamSession)
		}
		if got := rec.Header().Get("Mcp-Session-Id"); got != "sess-resp-42" {
			t.Errorf("response Mcp-Session-Id = %q, want sess-resp-42", got)
		}
		if !strings.HasPrefix(upstream.URL, "http://"+upstreamHost) {
			t.Errorf("upstream Host = %q, want target host from %q", upstreamHost, upstream.URL)
		}
	})
}

func TestProxyHandler_HealthzModesAndSingleflightCache(t *testing.T) {
	var statusCode atomic.Int32
	statusCode.Store(http.StatusOK)
	var hits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			hits.Add(1)
			time.Sleep(20 * time.Millisecond)
			w.WriteHeader(int(statusCode.Load()))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	t.Run("HTTP healthPath probe with 1s singleflight cache", func(t *testing.T) {
		h, err := NewProxyHandler(ProxyConfig{
			Name:       "p1",
			ListenAddr: "127.0.0.1:18476",
			TargetURL:  upstream.URL,
			HealthPath: "/health",
		}, "tok", "127.0.0.1:18476", nil)
		if err != nil {
			t.Fatal(err)
		}
		rt := h.(*proxyRuntime)

		// Concurrent burst hits upstream only ONCE due to singleflight + 1s cache.
		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18476/healthz", nil)
				req.Header.Set("Sec-Fetch-Site", "none") // allowed on GET /healthz
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("concurrent healthz status = %d, want 200", rec.Code)
				}
			}()
		}
		wg.Wait()
		if got := hits.Load(); got != 1 {
			t.Fatalf("expected 1 singleflight upstream health probe, got %d", got)
		}

		// After invalidation (or TTL expiry), unhealthy upstream returns 503.
		statusCode.Store(http.StatusInternalServerError)
		rt.upstream.healthCache.invalidate()
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18476/healthz", nil)
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 when upstream unhealthy", rec2.Code)
		}
	})

	t.Run("TCP fallback probe when healthPath omitted", func(t *testing.T) {
		tcpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		h, err := NewProxyHandler(ProxyConfig{
			Name:       "p2",
			ListenAddr: "127.0.0.1:18476",
			TargetURL:  tcpUpstream.URL,
		}, "tok", "127.0.0.1:18476", nil)
		if err != nil {
			t.Fatal(err)
		}
		rt := h.(*proxyRuntime)

		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18476/healthz", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 when TCP port is open", rec.Code)
		}

		tcpUpstream.Close()
		rt.upstream.healthCache.invalidate()
		rec2 := httptest.NewRecorder()
		h.ServeHTTP(rec2, req)
		if rec2.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503 when TCP port is closed", rec2.Code)
		}
	})
}

func TestProxyServer_SSEChunkStreamingAndInPlaceReload(t *testing.T) {
	const tokenV1 = "sse-stream-secret-v1"
	const tokenV2 = "sse-stream-secret-v2"
	proceedChunk2 := make(chan struct{})
	upstreamClosed := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		_, _ = fmt.Fprint(w, "data: chunk-1\n\n")
		flusher.Flush()

		select {
		case <-proceedChunk2:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "data: chunk-2\n\n")
		flusher.Flush()
		<-r.Context().Done()
		close(upstreamClosed)
	}))
	defer upstream.Close()

	tokenPath := filepath.Join(t.TempDir(), "proxy.token")
	if err := os.WriteFile(tokenPath, []byte(tokenV1), 0o600); err != nil {
		t.Fatal(err)
	}

	pcfg := ProxyConfig{
		Name:       "sse-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		TokenFile:  tokenPath,
		ClientPath: "/ping",
	}
	ps := NewProxyServer(pcfg, nil)
	if err := ps.Start(); err != nil {
		t.Fatalf("ProxyServer.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ps.ListenAddr()+"/sse", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tokenV1)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := NewDirectClient(0).Do(req)
	if err != nil {
		t.Fatalf("GET /sse: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	line1Ch := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		line1Ch <- line
	}()

	select {
	case line1 := <-line1Ch:
		if strings.TrimSpace(line1) != "data: chunk-1" {
			t.Fatalf("chunk 1 = %q, want 'data: chunk-1'", line1)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE chunk 1")
	}

	// Stage the new token while keeping the existing stream authorized in the grace period.
	if err := os.WriteFile(tokenPath+".next", []byte(tokenV2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ps.ReloadConfig(pcfg); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	client := NewDirectClient(3 * time.Second)
	reqNew, _ := http.NewRequest(http.MethodGet, "http://"+ps.ListenAddr()+"/ping", nil)
	reqNew.Header.Set("Authorization", "Bearer "+tokenV2)
	respNew, err := client.Do(reqNew)
	if err != nil {
		t.Fatal(err)
	}
	respNew.Body.Close()
	if respNew.StatusCode != http.StatusOK {
		t.Fatalf("new token after reload status = %d, want 200", respNew.StatusCode)
	}

	// The stream stays live while its token is still in the rule set.
	close(proceedChunk2)
	_, _ = reader.ReadString('\n')
	line2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read chunk 2 after in-place reload: %v", err)
	}
	if strings.TrimSpace(line2) != "data: chunk-2" {
		t.Fatalf("chunk 2 = %q, want 'data: chunk-2'", line2)
	}
	if err := os.WriteFile(tokenPath, []byte(tokenV2), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokenPath + ".next"); err != nil {
		t.Fatal(err)
	}
	if err := ps.ReloadConfig(pcfg); err != nil {
		t.Fatalf("commit token rotation: %v", err)
	}
	select {
	case <-upstreamClosed:
	case <-ctx.Done():
		t.Fatal("revoked stream stayed open")
	}
	reqOld, _ := http.NewRequest(http.MethodGet, "http://"+ps.ListenAddr()+"/ping", nil)
	reqOld.Header.Set("Authorization", "Bearer "+tokenV1)
	respOld, err := client.Do(reqOld)
	if err != nil {
		t.Fatal(err)
	}
	respOld.Body.Close()
	if respOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after commit = %d, want 401", respOld.StatusCode)
	}
}

func TestProxyServer_ToolFilteringJSONAndSSE(t *testing.T) {
	var upstreamCallHits atomic.Int32
	var lastAcceptEncoding string
	var sendSSE atomic.Bool

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAcceptEncoding = r.Header.Get("Accept-Encoding")
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools/list"`) {
			toolsPayload := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"read_item","annotations":{"readOnlyHint":true}},{"name":"write_item","annotations":{"readOnlyHint":false}},{"name":"secret_admin"}]}}`
			if sendSSE.Load() {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", toolsPayload)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(toolsPayload))
			return
		}
		upstreamCallHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ran"}]}}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	roTokPath := filepath.Join(dir, "ro.token")
	fullTokPath := filepath.Join(dir, "full.token")
	if err := os.WriteFile(roTokPath, []byte("readonly-tok-1111"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullTokPath, []byte("full-tok-2222"), 0o600); err != nil {
		t.Fatal(err)
	}

	ps := NewProxyServer(ProxyConfig{
		Name:       "acl-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		Tokens: []RouteTokenConfig{
			{Name: "ro", TokenFile: roTokPath, ReadOnlyOnly: true},
			{Name: "full", TokenFile: fullTokPath, DenyTools: []string{"secret_*"}},
		},
	}, nil)
	if err := ps.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
	}()

	client := NewDirectClient(5 * time.Second)
	postMCP := func(tok, payload string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		return resp
	}

	// 1. JSON tools/list filtered for readOnlyOnly token; Accept-Encoding: identity forced upstream.
	resp := postMCP("readonly-tok-1111", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if lastAcceptEncoding != "identity" {
		t.Errorf("upstream Accept-Encoding = %q, want identity", lastAcceptEncoding)
	}
	if !strings.Contains(string(body), "read_item") || strings.Contains(string(body), "write_item") || strings.Contains(string(body), "secret_admin") {
		t.Fatalf("filtered JSON tools/list unexpected: %s", string(body))
	}

	// 2. SSE tools/list filtered for full token (denies secret_*).
	sendSSE.Store(true)
	respSSE := postMCP("full-tok-2222", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	bodySSE, _ := io.ReadAll(respSSE.Body)
	respSSE.Body.Close()
	if !strings.Contains(string(bodySSE), "event: message") || !strings.Contains(string(bodySSE), "write_item") || strings.Contains(string(bodySSE), "secret_admin") {
		t.Fatalf("filtered SSE tools/list unexpected: %s", string(bodySSE))
	}

	// 3. Denied tools/call rejected pre-flight with -32601 without reaching upstream!
	respDenied := postMCP("readonly-tok-1111", `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"write_item","arguments":{}}}`)
	deniedBody, _ := io.ReadAll(respDenied.Body)
	respDenied.Body.Close()
	if respDenied.StatusCode != http.StatusOK || !strings.Contains(string(deniedBody), "-32601") {
		t.Fatalf("expected JSON-RPC -32601 denial, got %d: %s", respDenied.StatusCode, string(deniedBody))
	}
	if upstreamCallHits.Load() != 0 {
		t.Fatalf("denied tools/call leaked to upstream (%d hits)", upstreamCallHits.Load())
	}

	// 4. Allowed readOnlyHint=true tool passes through to upstream!
	respOK := postMCP("readonly-tok-1111", `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"read_item","arguments":{}}}`)
	respOK.Body.Close()
	if respOK.StatusCode != http.StatusOK || upstreamCallHits.Load() != 1 {
		t.Fatalf("expected read_item call to reach upstream, status=%d hits=%d", respOK.StatusCode, upstreamCallHits.Load())
	}
}

func TestProxyServer_UpstreamAuthModes(t *testing.T) {
	const (
		proxyToken    = "client-facing-proxy-secret"
		upstreamToken = "upstream-docs-secret-999"
	)

	var lastUpstreamAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastUpstreamAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	proxyTokPath := filepath.Join(dir, "proxy.token")
	upstreamTokPath := filepath.Join(dir, "upstream.token")
	if err := os.WriteFile(proxyTokPath, []byte(proxyToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(upstreamTokPath, []byte(upstreamToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	client := NewDirectClient(5 * time.Second)

	t.Run("strips Authorization when UpstreamTokenFile unset", func(t *testing.T) {
		lastUpstreamAuth = "sentinel"
		ps := NewProxyServer(ProxyConfig{
			Name:       "playwright-mode",
			ListenAddr: "127.0.0.1:0",
			TargetURL:  upstream.URL,
			TokenFile:  proxyTokPath,
		}, nil)
		if err := ps.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = ps.Shutdown(ctx)
		}()

		req, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+proxyToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if lastUpstreamAuth != "" {
			t.Fatalf("upstream Authorization = %q, want empty string", lastUpstreamAuth)
		}
	})

	t.Run("injects upstream Bearer when UpstreamTokenFile set", func(t *testing.T) {
		lastUpstreamAuth = ""
		ps := NewProxyServer(ProxyConfig{
			Name:              "upstream-auth-mode",
			ListenAddr:        "127.0.0.1:0",
			TargetURL:         upstream.URL,
			TokenFile:         proxyTokPath,
			UpstreamTokenFile: upstreamTokPath,
		}, nil)
		if err := ps.Start(); err != nil {
			t.Fatalf("Start: %v", err)
		}
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = ps.Shutdown(ctx)
		}()

		req, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+proxyToken)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		want := "Bearer " + upstreamToken
		if lastUpstreamAuth != want {
			t.Fatalf("upstream Authorization = %q, want %q", lastUpstreamAuth, want)
		}
	})

	t.Run("Start fails fast if UpstreamTokenFile is unreadable", func(t *testing.T) {
		ps := NewProxyServer(ProxyConfig{
			Name:              "bad-upstream-token",
			ListenAddr:        "127.0.0.1:0",
			TargetURL:         upstream.URL,
			TokenFile:         proxyTokPath,
			UpstreamTokenFile: filepath.Join(dir, "does-not-exist.token"),
		}, nil)
		if err := ps.Start(); err == nil {
			_ = ps.Shutdown(context.Background())
			t.Fatal("expected Start error when UpstreamTokenFile does not exist")
		}
	})
}

func TestTargetHostPort(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"http://127.0.0.1:18486", "127.0.0.1:18486"},
		{"http://127.0.0.1", "127.0.0.1:80"},
		{"https://localhost", "localhost:443"},
		{"http://[::1]:18486", "[::1]:18486"},
		{"http://[::1]", "[::1]:80"},
		{"https://[::1]", "[::1]:443"},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("url.Parse(%q): %v", tc.raw, err)
		}
		if got := targetHostPort(u); got != tc.want {
			t.Errorf("targetHostPort(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestProxyHandler_MaxBytesCapAndFastFail413(t *testing.T) {
	const token = "maxbytes-secret"
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	tokenPath := filepath.Join(t.TempDir(), "proxy.token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProxyServer(ProxyConfig{
		Name:       "cap-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		TokenFile:  tokenPath,
	}, nil)
	if err := ps.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
	}()

	// 1. Fast-fail 413 via ContentLength without touching upstream!
	handler, err := NewProxyHandler(ProxyConfig{
		Name:       "fastfail",
		ListenAddr: "127.0.0.1:18476",
		TargetURL:  upstream.URL,
	}, token, "127.0.0.1:18476", nil)
	if err != nil {
		t.Fatal(err)
	}
	reqFast := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18476/mcp", strings.NewReader(`{}`))
	reqFast.ContentLength = maxProxyRequestBodyBytes + 1
	reqFast.Header.Set("Authorization", "Bearer "+token)
	recFast := httptest.NewRecorder()
	handler.ServeHTTP(recFast, reqFast)
	if recFast.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("fast-fail status = %d, want 413", recFast.Code)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("fast-fail 413 must not hit upstream, got %d hits", upstreamHits.Load())
	}

	// 2. Chunked streaming > 32 MiB returns 413 without allocating 32 MiB in RAM.
	body := io.LimitReader(zeroReader{}, maxProxyRequestBodyBytes+1)
	req, err := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := NewDirectClient(10 * time.Second).Do(req)
	if err != nil {
		t.Fatalf("Do oversized request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 413 Request Entity Too Large (body=%s)", resp.StatusCode, string(respBody))
	}
}

func TestProxyServer_ResumeInvalidatesHealthCacheAndStaleSessionDeletesMetric(t *testing.T) {
	var healthStatus atomic.Int32
	healthStatus.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(int(healthStatus.Load()))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	tokPath := filepath.Join(t.TempDir(), "proxy.token")
	if err := os.WriteFile(tokPath, []byte("resume-tok-1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	ps := NewProxyServer(ProxyConfig{
		Name:       "resume-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		TokenFile:  tokPath,
		HealthPath: "/healthz",
	}, nil)
	if err := ps.Start(); err != nil {
		t.Fatal(err)
	}
	defer ps.Shutdown(context.Background())

	client := NewDirectClient(3 * time.Second)
	resp1, err := client.Get("http://" + ps.ListenAddr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("initial /healthz = %d, want 200", resp1.StatusCode)
	}

	// Flip upstream to 503; ResumeFromSuspend must invalidate healthCache immediately.
	healthStatus.Store(http.StatusServiceUnavailable)
	ps.ResumeFromSuspend()
	resp2, err := client.Get("http://" + ps.ListenAddr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-resume /healthz = %d, want 503 (cache not invalidated)", resp2.StatusCode)
	}

	// Stale session 404 must delete the session from MCPMetrics instead of leaking ActiveSessions.
	runner := NewServiceRunner(ServiceConfig{Name: "epoch-svc", Command: "noop"}, t.TempDir(), DefaultLogMaxBytes, nil)
	runner.mu.Lock()
	runner.status.State = "running"
	runner.epoch = 2
	runner.epochCtx = context.Background()
	runner.mu.Unlock()
	ps.SetServiceLookup(func(string) *ServiceRunner { return runner })
	if err := ps.ReloadConfig(ProxyConfig{
		Name:            "resume-proxy",
		ListenAddr:      "127.0.0.1:0",
		TargetURL:       upstream.URL,
		TokenFile:       tokPath,
		HealthPath:      "/healthz",
		UpstreamService: "epoch-svc",
	}); err != nil {
		t.Fatal(err)
	}
	ps.MCPMetrics().TrackSessionEpoch("stale-sess-1", 1)
	if got := ps.MCPMetrics().ActiveSessionCount(); got != 1 {
		t.Fatalf("expected 1 tracked session, got %d", got)
	}
	reqStale, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	reqStale.Header.Set("Authorization", "Bearer resume-tok-1234")
	reqStale.Header.Set("Mcp-Session-Id", "stale-sess-1")
	respStale, err := client.Do(reqStale)
	if err != nil {
		t.Fatal(err)
	}
	respStale.Body.Close()
	if respStale.StatusCode != http.StatusNotFound {
		t.Fatalf("stale session status = %d, want 404", respStale.StatusCode)
	}
	if got := ps.MCPMetrics().ActiveSessionCount(); got != 0 {
		t.Fatalf("stale session leaked in MCPMetrics: ActiveSessionCount=%d, want 0", got)
	}
}
