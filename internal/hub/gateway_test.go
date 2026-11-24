package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGatewayServer_HTTPRoutingIsolationAndSSEFiltering(t *testing.T) {
	var alphaPath, betaPath string
	var alphaHits, betaHits atomic.Int32

	alphaUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		alphaHits.Add(1)
		alphaPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools/list"`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"safe_read\",\"annotations\":{\"readOnlyHint\":true}},{\"name\":\"danger_drop\",\"annotations\":{\"readOnlyHint\":false}}]}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"server":"alpha","path":"` + r.URL.Path + `"}}`))
	}))
	defer alphaUp.Close()

	betaUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		betaHits.Add(1)
		betaPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server":"beta"}`))
	}))
	defer betaUp.Close()

	dir := t.TempDir()
	tokA := filepath.Join(dir, "alpha.token")
	tokB := filepath.Join(dir, "beta.token")
	tokA2 := filepath.Join(dir, "alpha-v2.token")
	if err := os.WriteFile(tokA, []byte("alpha-token-1111"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokB, []byte("beta-token-2222"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokA2, []byte("alpha-token-3333"), 0o600); err != nil {
		t.Fatal(err)
	}

	gwCfg := GatewayConfig{
		ListenAddr: "127.0.0.1:0",
		Routes: []GatewayRouteConfig{
			{
				Slug:        "alpha",
				TargetURL:   alphaUp.URL,
				HealthPath:  "/healthz",
				Description: "Alpha HTTP route",
				Tokens: []RouteTokenConfig{
					{Name: "ro", TokenFile: tokA, ReadOnlyOnly: true},
				},
			},
			{
				Slug:      "beta",
				TargetURL: betaUp.URL,
				TokenFile: tokB,
			},
		},
	}

	gw := NewGatewayServer(gwCfg, nil, nil, nil, NewRedactor(), nil)
	if err := gw.Start(); err != nil {
		t.Fatalf("GatewayServer.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = gw.Shutdown(ctx)
	}()

	client := NewDirectClient(5 * time.Second)
	base := "http://" + gw.ListenAddr()

	// 1. Unauthenticated /healthz and /s/alpha/healthz succeed on loopback.
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	respH, err := client.Get(base + "/s/alpha/healthz")
	if err != nil {
		t.Fatal(err)
	}
	respH.Body.Close()
	if respH.StatusCode != http.StatusOK {
		t.Fatalf("GET /s/alpha/healthz = %d, want 200", respH.StatusCode)
	}

	// 2. Per-route token isolation: beta token rejected on alpha route!
	reqCross, _ := http.NewRequest(http.MethodPost, base+"/s/alpha/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	reqCross.Header.Set("Authorization", "Bearer beta-token-2222")
	reqCross.Header.Set("Content-Type", "application/json")
	respCross, err := client.Do(reqCross)
	if err != nil {
		t.Fatal(err)
	}
	respCross.Body.Close()
	if respCross.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cross-route token status = %d, want 401", respCross.StatusCode)
	}

	// 3. Prefix stripping and SSE tools/list filtering on /s/alpha/mcp.
	reqList, _ := http.NewRequest(http.MethodPost, base+"/s/alpha/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	reqList.Header.Set("Authorization", "Bearer alpha-token-1111")
	reqList.Header.Set("Content-Type", "application/json")
	respList, err := client.Do(reqList)
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.ReadAll(respList.Body)
	respList.Body.Close()
	if respList.StatusCode != http.StatusOK {
		t.Fatalf("alpha tools/list status = %d, want 200", respList.StatusCode)
	}
	if alphaPath != "/mcp" {
		t.Errorf("stripped upstream path = %q, want /mcp", alphaPath)
	}
	if !strings.Contains(string(listBody), "safe_read") || strings.Contains(string(listBody), "danger_drop") {
		t.Fatalf("SSE tools/list not filtered: %s", string(listBody))
	}

	// 4. Bare /s/beta strips to "/".
	reqBeta, _ := http.NewRequest(http.MethodPost, base+"/s/beta", strings.NewReader(`{}`))
	reqBeta.Header.Set("Authorization", "Bearer beta-token-2222")
	respBeta, err := client.Do(reqBeta)
	if err != nil {
		t.Fatal(err)
	}
	respBeta.Body.Close()
	if respBeta.StatusCode != http.StatusOK || betaPath != "/" {
		t.Fatalf("bare /s/beta status=%d path=%q", respBeta.StatusCode, betaPath)
	}

	// 5. Unknown slug -> 404.
	reqMissing, _ := http.NewRequest(http.MethodPost, base+"/s/nope/mcp", strings.NewReader(`{}`))
	reqMissing.Header.Set("Authorization", "Bearer alpha-token-1111")
	respMissing, err := client.Do(reqMissing)
	if err != nil {
		t.Fatal(err)
	}
	respMissing.Body.Close()
	if respMissing.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown slug status = %d, want 404", respMissing.StatusCode)
	}

	// 6. Loopback guard: OPTIONS / Origin / Sec-Fetch-Site rejected with 403.
	reqOpt, _ := http.NewRequest(http.MethodOptions, base+"/s/alpha/mcp", nil)
	respOpt, err := client.Do(reqOpt)
	if err != nil {
		t.Fatal(err)
	}
	respOpt.Body.Close()
	if respOpt.StatusCode != http.StatusForbidden {
		t.Fatalf("OPTIONS status = %d, want 403", respOpt.StatusCode)
	}

	// 7. Hot-reload gateway routes atomically without dropping listener.
	gwCfg.Routes[0].Tokens = []RouteTokenConfig{{Name: "ro", TokenFile: tokA2, ReadOnlyOnly: true}}
	if err := gw.ReloadConfig(gwCfg, nil); err != nil {
		t.Fatalf("GatewayServer.ReloadConfig: %v", err)
	}

	reqOldTok, _ := http.NewRequest(http.MethodPost, base+"/s/alpha/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	reqOldTok.Header.Set("Authorization", "Bearer alpha-token-1111")
	reqOldTok.Header.Set("Content-Type", "application/json")
	respOldTok, err := client.Do(reqOldTok)
	if err != nil {
		t.Fatal(err)
	}
	respOldTok.Body.Close()
	if respOldTok.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token after gateway reload = %d, want 401", respOldTok.StatusCode)
	}

	reqNewTok, _ := http.NewRequest(http.MethodPost, base+"/s/alpha/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	reqNewTok.Header.Set("Authorization", "Bearer alpha-token-3333")
	reqNewTok.Header.Set("Content-Type", "application/json")
	respNewTok, err := client.Do(reqNewTok)
	if err != nil {
		t.Fatal(err)
	}
	respNewTok.Body.Close()
	if respNewTok.StatusCode != http.StatusOK {
		t.Fatalf("new token after gateway reload = %d, want 200", respNewTok.StatusCode)
	}

	st := gw.Status()
	if !st.Running || len(st.Routes) != 2 || st.Routes[0].Metrics == nil || st.Routes[0].Metrics.TotalRequests < 2 {
		t.Fatalf("unexpected gateway status: %+v", st)
	}
}

func TestGatewayServer_StdioBridgeIntegration(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ctrlTok := filepath.Join(dir, "ctrl.token")
	bridgeTok := filepath.Join(dir, "bridge.token")
	if err := os.WriteFile(ctrlTok, []byte("ctrl-bridge-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridgeTok, []byte("gw-bridge-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		ControlAddr:      "127.0.0.1:0",
		ControlTokenFile: ctrlTok,
		LogDir:           filepath.Join(dir, "logs"),
		Services: []ServiceConfig{
			{
				Name:        "mock-stdio",
				Mode:        "stdio-bridge",
				Command:     exe,
				Args:        []string{"-test.run=^TestHelperProcess$"},
				Description: "Mock STDIO MCP tool server",
				Tags:        []string{"stdio", "test"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "stdio-mcp",
				},
			},
		},
		Gateway: &GatewayConfig{
			ListenAddr: "127.0.0.1:0",
			Routes: []GatewayRouteConfig{
				{
					Slug:    "mock",
					Service: "mock-stdio",
					Tokens: []RouteTokenConfig{
						{
							Name:      "scoped",
							TokenFile: bridgeTok,
							DenyTools: []string{"delete_*"},
						},
					},
				},
			},
		},
	}

	sup, err := NewSupervisor(cfg, filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Supervisor.Start: %v", err)
	}
	defer sup.Shutdown()

	gwAddr, ok := sup.GatewayAddr()
	if !ok {
		t.Fatal("gateway not started")
	}
	client := NewDirectClient(10 * time.Second)

	// 1. Bridge starts in sleeping state; /healthz reports OK WITHOUT waking it!
	hzResp, err := client.Get("http://" + gwAddr + "/s/mock/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hzBody, _ := io.ReadAll(hzResp.Body)
	hzResp.Body.Close()
	if hzResp.StatusCode != http.StatusOK || !strings.Contains(string(hzBody), `"sleeping"`) {
		t.Fatalf("bridge /healthz = %d %s, want 200 sleeping", hzResp.StatusCode, string(hzBody))
	}

	// Control /healthz also counts sleeping bridge as OK!
	ctrlClient := NewClient(sup.ControlAddr(), "ctrl-bridge-secret")
	hubHZ, _, err := ctrlClient.Healthz(ctx)
	if err != nil {
		t.Fatalf("Client.Healthz: %v", err)
	}
	if !hubHZ.OK || hubHZ.Services["mock-stdio"].State != "sleeping" || hubHZ.Services["mock-stdio"].Mode != "stdio-bridge" {
		t.Fatalf("unexpected hub /healthz with sleeping bridge: %+v", hubHZ)
	}

	// 2. Denied tools/call rejected before waking the sleeping bridge!
	postBridge := func(payload string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+gwAddr+"/s/mock", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer gw-bridge-secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /s/mock: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	deniedResp, deniedBody := postBridge(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_all","arguments":{}}}`)
	if deniedResp.StatusCode != http.StatusOK || !strings.Contains(string(deniedBody), "-32601") {
		t.Fatalf("expected -32601 tool denial, got %d: %s", deniedResp.StatusCode, string(deniedBody))
	}
	if st, _, _ := ctrlClient.Status(ctx); st.Services["mock-stdio"].State != "sleeping" {
		t.Fatalf("bridge woke up on denied tool call! state=%s", st.Services["mock-stdio"].State)
	}

	// 3. Allowed tools/list wakes the bridge, filters out delete_all, and reports running + metrics in /status and /catalog!
	listResp, listBody := postBridge(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listResp.StatusCode, string(listBody))
	}
	if !strings.Contains(string(listBody), "echo_tool") || strings.Contains(string(listBody), "delete_all") {
		t.Fatalf("unexpected filtered bridge tools/list: %s", string(listBody))
	}

	callResp, callBody := postBridge(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo_tool","arguments":{"msg":"hi"}}}`)
	if callResp.StatusCode != http.StatusOK || !strings.Contains(string(callBody), "echo:hi") {
		t.Fatalf("echo_tool call failed: %d %s", callResp.StatusCode, string(callBody))
	}

	st, _, err := ctrlClient.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	svcSt := st.Services["mock-stdio"]
	if svcSt.State != "running" || svcSt.Mode != "stdio-bridge" || svcSt.PID <= 0 || svcSt.Metrics == nil || svcSt.Metrics.TotalRequests < 2 {
		t.Fatalf("unexpected running bridge status: %+v", svcSt)
	}

	cat, _, err := ctrlClient.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Entries) != 1 || !cat.Entries[0].Capabilities.StdioBridge || cat.Entries[0].ViaGateway != "mock" {
		b, _ := json.Marshal(cat)
		t.Fatalf("unexpected catalog for bridge: %s", string(b))
	}
}

func TestGatewayServer_MissingLinkedServiceIsStoppedNotExternal(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "gw.token")
	if err := os.WriteFile(tok, []byte("gw-missing-svc-tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	gw := NewGatewayServer(GatewayConfig{
		ListenAddr: "127.0.0.1:0",
		Routes: []GatewayRouteConfig{
			{
				Slug:      "missing-svc",
				Service:   "vanished-http-svc",
				TargetURL: "http://127.0.0.1:18499",
				TokenFile: tok,
			},
			{
				Slug:      "ext-route",
				TargetURL: "http://127.0.0.1:18498",
				TokenFile: tok,
			},
		},
	}, func(string) *ServiceRunner { return nil }, nil, nil, NewRedactor(), nil)
	if err := gw.Start(); err != nil {
		t.Fatalf("GatewayServer.Start: %v", err)
	}
	defer gw.Shutdown(context.Background())

	sum := gw.HealthSummary()
	if sum.Routes["missing-svc"] {
		t.Fatalf("expected missing linked service route to report unhealthy, got %+v", sum.Routes)
	}
	if !sum.Routes["ext-route"] {
		t.Fatalf("expected external route without service to report ok, got %+v", sum.Routes)
	}
	st := gw.Status()
	if st.Routes[0].State != "stopped" {
		t.Fatalf("missing linked service state = %q, want stopped", st.Routes[0].State)
	}
	if st.Routes[1].State != "external" {
		t.Fatalf("external route state = %q, want external", st.Routes[1].State)
	}
}
