package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

func TestControlHandler_HealthzAndBearerAuth(t *testing.T) {
	const adminToken = "control-admin-secret-999"
	var restartedName string
	var restartedWait bool
	var startedName, stoppedName, reloadedName string
	var stopCalls atomic.Int32
	stopDone := make(chan struct{})
	ring := NewRingLog(32, 1024)
	ring.Append("stdout", "boot line 1")
	ring.Append("stderr", "warn line 2")

	hooks := ControlHooks{
		Healthz: func() HealthzResponse {
			return HealthzResponse{
				OK: true,
				Services: map[string]ServiceHealthSummary{
					"playwright": {
						Running:  true,
						Healthy:  true,
						Restarts: 2,
						PID:      4321,
						Port:     18486,
						State:    "running",
						Mode:     "http",
					},
				},
			}
		},
		Status: func() StatusResponse {
			return StatusResponse{
				SchemaVersion: 1,
				OK:            true,
				Version:       Version,
				PID:           1111,
				ControlAddr:   "127.0.0.1:18470",
				Services: map[string]ServiceStatus{
					"playwright": {
						Name:     "playwright",
						Running:  true,
						Healthy:  true,
						Restarts: 2,
						PID:      4321,
						Port:     18486,
						State:    "running",
					},
				},
				Proxies: map[string]ProxyStatus{
					"playwright-proxy": {
						Name:       "playwright-proxy",
						ListenAddr: "127.0.0.1:18476",
						TargetURL:  "http://127.0.0.1:18486",
						Running:    true,
					},
				},
			}
		},
		Catalog: func() CatalogResponse {
			return CatalogResponse{
				SchemaVersion: 1,
				OK:            true,
				Entries: []CatalogEntry{{
					Name:      "playwright",
					Mode:      "http",
					ClientURL: "http://127.0.0.1:18476/mcp",
					State:     "running",
					Healthy:   true,
				}},
			}
		},
		GetRingLog: func(name string) (*RingLog, bool) {
			if name == "playwright" || name == "hub" {
				return ring, true
			}
			return nil, false
		},
		StartService: func(_ context.Context, name string) (ServiceStatus, bool, error) {
			if name != "playwright" {
				return ServiceStatus{}, false, nil
			}
			startedName = name
			return ServiceStatus{Name: name, Running: true, State: "running"}, true, nil
		},
		StopService: func(_ context.Context, name string) (ServiceStatus, bool, error) {
			if name != "playwright" {
				return ServiceStatus{}, false, nil
			}
			stoppedName = name
			return ServiceStatus{Name: name, Running: false, State: "stopped"}, true, nil
		},
		Restart: func(_ context.Context, name string, waitHealthy bool) (ServiceStatus, bool, error) {
			if name != "playwright" {
				return ServiceStatus{}, false, nil
			}
			restartedName = name
			restartedWait = waitHealthy
			return ServiceStatus{
				Name:     name,
				Running:  true,
				Healthy:  waitHealthy,
				Restarts: 3,
				PID:      9999,
				Port:     18486,
			}, true, nil
		},
		ReloadHub: func(_ context.Context) (ReloadHubResponse, error) {
			return ReloadHubResponse{OK: true, ReloadedGateway: true}, nil
		},
		ReloadService: func(_ context.Context, name string) (json.RawMessage, bool, error) {
			if name == "no-reload" {
				return nil, true, ErrReloadNotSupported
			}
			if name != "playwright" {
				return nil, false, nil
			}
			reloadedName = name
			return json.RawMessage(`{"reloaded":true}`), true, nil
		},
		Stop: func() {
			if stopCalls.Add(1) == 1 {
				close(stopDone)
			}
		},
	}

	h := NewControlHandler(adminToken, "127.0.0.1:18470", hooks)

	t.Run("GET /healthz is unauthenticated, allows Sec-Fetch-Site: none, matches contract", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/healthz", nil)
		req.Header.Set("Sec-Fetch-Site", "none")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var out HealthzResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode /healthz: %v", err)
		}
		if !out.OK {
			t.Errorf("ok = false, want true")
		}
		svc, ok := out.Services["playwright"]
		if !ok || !svc.Running || !svc.Healthy || svc.Restarts != 2 || svc.PID != 4321 || svc.Port != 18486 {
			t.Fatalf("unexpected service summary: %+v", svc)
		}
	})

	t.Run("GET /status rejects missing/invalid Bearer and browser Sec-Fetch-Site: none", func(t *testing.T) {
		for _, auth := range []string{"", "Bearer bad", "Basic abc"} {
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/status", nil)
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("auth=%q: status = %d, want 401", auth, rec.Code)
			}
		}

		reqBrowser := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/status", nil)
		reqBrowser.Header.Set("Authorization", "Bearer "+adminToken)
		reqBrowser.Header.Set("Sec-Fetch-Site", "none")
		recBrowser := httptest.NewRecorder()
		h.ServeHTTP(recBrowser, reqBrowser)
		if recBrowser.Code != http.StatusForbidden {
			t.Fatalf("Sec-Fetch-Site: none on /status = %d, want 403", recBrowser.Code)
		}
	})

	t.Run("GET /status and /catalog succeed with valid Bearer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/status", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var out StatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode /status: %v", err)
		}
		if out.SchemaVersion != 1 || out.PID != 1111 || len(out.Proxies) != 1 {
			t.Errorf("unexpected status payload: %+v", out)
		}

		reqCat := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/catalog", nil)
		reqCat.Header.Set("Authorization", "Bearer "+adminToken)
		recCat := httptest.NewRecorder()
		h.ServeHTTP(recCat, reqCat)
		if recCat.Code != http.StatusOK {
			t.Fatalf("catalog status = %d, want 200", recCat.Code)
		}
		var cat CatalogResponse
		if err := json.Unmarshal(recCat.Body.Bytes(), &cat); err != nil {
			t.Fatalf("decode /catalog: %v", err)
		}
		if cat.SchemaVersion != 1 || len(cat.Entries) != 1 || cat.Entries[0].Name != "playwright" {
			t.Errorf("unexpected catalog: %+v", cat)
		}
	})

	t.Run("GET /logs/<name> JSON and validation", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/logs/playwright?lines=1", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("logs status = %d, want 200", rec.Code)
		}
		var out LogsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Lines) != 1 || out.Lines[0].Text != "warn line 2" {
			t.Fatalf("unexpected logs payload: %+v", out)
		}

		reqBad := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/logs/..bad", nil)
		reqBad.Header.Set("Authorization", "Bearer "+adminToken)
		recBad := httptest.NewRecorder()
		h.ServeHTTP(recBad, reqBad)
		if recBad.Code != http.StatusBadRequest {
			t.Fatalf("invalid log name status = %d, want 400", recBad.Code)
		}
	})

	t.Run("POST /start, /stop/<svc>, /restart/<svc>?wait=healthy, /reload, /reload/<svc>", func(t *testing.T) {
		reqStart := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/start/playwright", nil)
		reqStart.Header.Set("Authorization", "Bearer "+adminToken)
		recStart := httptest.NewRecorder()
		h.ServeHTTP(recStart, reqStart)
		if recStart.Code != http.StatusOK || startedName != "playwright" {
			t.Fatalf("start status=%d name=%q", recStart.Code, startedName)
		}

		reqStopSvc := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/stop/playwright", nil)
		reqStopSvc.Header.Set("Authorization", "Bearer "+adminToken)
		recStopSvc := httptest.NewRecorder()
		h.ServeHTTP(recStopSvc, reqStopSvc)
		if recStopSvc.Code != http.StatusOK || stoppedName != "playwright" {
			t.Fatalf("stop svc status=%d name=%q", recStopSvc.Code, stoppedName)
		}

		reqRestart := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/restart/playwright?wait=healthy", nil)
		reqRestart.Header.Set("Authorization", "Bearer "+adminToken)
		recRestart := httptest.NewRecorder()
		h.ServeHTTP(recRestart, reqRestart)
		if recRestart.Code != http.StatusOK || restartedName != "playwright" || !restartedWait {
			t.Fatalf("restart status=%d name=%q wait=%v", recRestart.Code, restartedName, restartedWait)
		}

		reqReloadHub := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/reload", nil)
		reqReloadHub.Header.Set("Authorization", "Bearer "+adminToken)
		recReloadHub := httptest.NewRecorder()
		h.ServeHTTP(recReloadHub, reqReloadHub)
		if recReloadHub.Code != http.StatusOK {
			t.Fatalf("reload hub status=%d", recReloadHub.Code)
		}

		reqReloadSvc := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/reload/playwright", nil)
		reqReloadSvc.Header.Set("Authorization", "Bearer "+adminToken)
		recReloadSvc := httptest.NewRecorder()
		h.ServeHTTP(recReloadSvc, reqReloadSvc)
		if recReloadSvc.Code != http.StatusOK || reloadedName != "playwright" {
			t.Fatalf("reload svc status=%d name=%q", recReloadSvc.Code, reloadedName)
		}

		reqReloadUnsupported := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/reload/no-reload", nil)
		reqReloadUnsupported.Header.Set("Authorization", "Bearer "+adminToken)
		recReloadUnsupported := httptest.NewRecorder()
		h.ServeHTTP(recReloadUnsupported, reqReloadUnsupported)
		if recReloadUnsupported.Code != http.StatusBadRequest {
			t.Fatalf("unsupported reload status=%d, want 400", recReloadUnsupported.Code)
		}

		// Oversized POST body (>64 KiB) returns 413.
		reqBig := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/reload", strings.NewReader(strings.Repeat("x", controlMaxPostBodyBytes+1)))
		reqBig.Header.Set("Authorization", "Bearer "+adminToken)
		recBig := httptest.NewRecorder()
		h.ServeHTTP(recBig, reqBig)
		if recBig.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("oversized POST status=%d, want 413", recBig.Code)
		}
	})

	t.Run("POST /stop requires Bearer and triggers stop callback", func(t *testing.T) {
		reqUnauth := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/stop", nil)
		recUnauth := httptest.NewRecorder()
		h.ServeHTTP(recUnauth, reqUnauth)
		if recUnauth.Code != http.StatusUnauthorized {
			t.Fatalf("unauth /stop status = %d, want 401", recUnauth.Code)
		}

		reqOK := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/stop", nil)
		reqOK.Header.Set("Authorization", "Bearer "+adminToken)
		recOK := httptest.NewRecorder()
		h.ServeHTTP(recOK, reqOK)
		if recOK.Code != http.StatusOK {
			t.Fatalf("POST /stop status = %d, want 200", recOK.Code)
		}
		select {
		case <-stopDone:
		case <-time.After(time.Second):
			t.Fatal("Stop hook was not triggered")
		}
	})

	t.Run("bad Host, Origin, OPTIONS rejected with 403", func(t *testing.T) {
		reqHost := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/healthz", nil)
		reqHost.Host = "attacker.example:18470"
		recHost := httptest.NewRecorder()
		h.ServeHTTP(recHost, reqHost)
		if recHost.Code != http.StatusForbidden {
			t.Fatalf("bad host status = %d, want 403", recHost.Code)
		}

		reqOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18470/healthz", nil)
		reqOrigin.Header.Set("Origin", "https://attacker.example")
		recOrigin := httptest.NewRecorder()
		h.ServeHTTP(recOrigin, reqOrigin)
		if recOrigin.Code != http.StatusForbidden {
			t.Fatalf("origin status = %d, want 403", recOrigin.Code)
		}

		reqOpt := httptest.NewRequest(http.MethodOptions, "http://127.0.0.1:18470/status", nil)
		recOpt := httptest.NewRecorder()
		h.ServeHTTP(recOpt, reqOpt)
		if recOpt.Code != http.StatusForbidden {
			t.Fatalf("OPTIONS status = %d, want 403", recOpt.Code)
		}
	})
}

func TestControlServer_FailClosedAndLogsSSEAndTokenGrace(t *testing.T) {
	// 1. Fail-closed when tokenFile is empty and allowNoAuth is false.
	csFail := NewControlServer("127.0.0.1:0", "", false, ControlHooks{}, nil)
	if err := csFail.Start(); err == nil {
		_ = csFail.Shutdown(context.Background())
		t.Fatal("expected ControlServer.Start to fail closed without tokenFile")
	}

	// 2. Real ControlServer with .next grace token and SSE /logs/<name>?follow=1.
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "control.token")
	if err := os.WriteFile(tokPath, []byte("primary-ctrl-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokPath+".next", []byte("grace-ctrl-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ring := NewRingLog(32, 1024)
	ring.Append("hub", "initial-line")

	cs := NewControlServer("127.0.0.1:0", tokPath, false, ControlHooks{
		GetRingLog: func(name string) (*RingLog, bool) {
			if name == "hub" {
				return ring, true
			}
			return nil, false
		},
		ReloadHub: func(_ context.Context) (ReloadHubResponse, error) {
			return ReloadHubResponse{}, configValidationError{errors.New("bad config")}
		},
	}, nil)
	if err := cs.Start(); err != nil {
		t.Fatalf("ControlServer.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cs.Shutdown(ctx)
	}()

	// Both primary and .next tokens work on /status.
	for _, tok := range []string{"primary-ctrl-token", "grace-ctrl-token"} {
		req, _ := http.NewRequest(http.MethodGet, "http://"+cs.ListenAddr()+"/status", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := NewDirectClient(5 * time.Second).Do(req)
		if err != nil {
			t.Fatalf("GET /status with %s: %v", tok, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /status with %s = %d, want 200", tok, resp.StatusCode)
		}
	}

	// Validation error on /reload returns HTTP 400.
	reqBadReload, _ := http.NewRequest(http.MethodPost, "http://"+cs.ListenAddr()+"/reload", nil)
	reqBadReload.Header.Set("Authorization", "Bearer primary-ctrl-token")
	respBadReload, err := NewDirectClient(5 * time.Second).Do(reqBadReload)
	if err != nil {
		t.Fatal(err)
	}
	respBadReload.Body.Close()
	if respBadReload.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid config reload status = %d, want 400", respBadReload.StatusCode)
	}

	// Stream SSE from /logs/hub?lines=10&follow=1.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reqSSE, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+cs.ListenAddr()+"/logs/hub?lines=10&follow=1", nil)
	reqSSE.Header.Set("Authorization", "Bearer primary-ctrl-token")
	respSSE, err := NewDirectClient(0).Do(reqSSE)
	if err != nil {
		t.Fatalf("GET /logs/hub?follow=1: %v", err)
	}
	defer respSSE.Body.Close()
	if respSSE.StatusCode != http.StatusOK {
		t.Fatalf("logs SSE status = %d, want 200", respSSE.StatusCode)
	}
	if ct := respSSE.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(respSSE.Body)
	readEvent := func() LogLine {
		t.Helper()
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("read SSE line: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(line, "data: ") {
				var out LogLine
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &out); err != nil {
					t.Fatalf("unmarshal SSE LogLine: %v", err)
				}
				return out
			}
		}
	}

	ev1 := readEvent()
	if ev1.Text != "initial-line" {
		t.Fatalf("first SSE line = %q, want initial-line", ev1.Text)
	}

	ring.Append("hub", "live-streamed-line")
	ev2 := readEvent()
	if ev2.Text != "live-streamed-line" {
		t.Fatalf("second SSE line = %q, want live-streamed-line", ev2.Text)
	}
	if err := os.WriteFile(tokPath, []byte("grace-ctrl-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tokPath + ".next"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ReloadTokens(tokPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("expected revoked log stream to close before client timeout: %v", err)
	}
}

func TestControlServer_V03EndpointsSSELastEventIDEventsAndUIMount(t *testing.T) {
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "control.token")
	if err := os.WriteFile(tokPath, []byte("v03-primary-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ring := NewRingLog(32, 1024)
	ring.Append("hub", "line-1")
	ring.Append("hub", "line-2")
	ring.Append("hub", "line-3")

	evCh := make(chan HubEvent, 8)
	var slept, woken string
	var startAllCalled, stopAllCalled, restartAllCalled, validateCalled atomic.Int32

	cs := NewControlServer("127.0.0.1:0", tokPath, false, ControlHooks{
		GetRingLog: func(name string) (*RingLog, bool) {
			if name == "hub" {
				return ring, true
			}
			return nil, false
		},
		SleepService: func(_ context.Context, name string) (ServiceStatus, bool, error) {
			if name != "demo" {
				return ServiceStatus{}, false, nil
			}
			slept = name
			return ServiceStatus{Name: name, State: "sleeping"}, true, nil
		},
		WakeService: func(_ context.Context, name string) (ServiceStatus, bool, error) {
			if name != "demo" {
				return ServiceStatus{}, false, nil
			}
			woken = name
			return ServiceStatus{Name: name, State: "running", Running: true, Healthy: true}, true, nil
		},
		StartAll: func(_ context.Context) map[string]ServiceActionResponse {
			startAllCalled.Add(1)
			return map[string]ServiceActionResponse{"demo": {OK: true, Name: "demo", Service: ServiceStatus{Name: "demo", State: "running"}}}
		},
		StopAll: func(_ context.Context) map[string]ServiceActionResponse {
			stopAllCalled.Add(1)
			return map[string]ServiceActionResponse{"demo": {OK: true, Name: "demo", Service: ServiceStatus{Name: "demo", State: "stopped"}}}
		},
		RestartAll: func(_ context.Context, waitHealthy bool) map[string]RestartResponse {
			restartAllCalled.Add(1)
			return map[string]RestartResponse{"demo": {OK: true, Name: "demo", Service: ServiceStatus{Name: "demo", State: "running", Healthy: waitHealthy}}}
		},
		ValidateConfig: func() (ReloadHubResponse, error) {
			validateCalled.Add(1)
			return ReloadHubResponse{OK: true, AddedServices: []string{"new-svc"}}, nil
		},
		SubscribeEvents: func() (<-chan HubEvent, func()) {
			return evCh, func() {}
		},
		CreateDashboardSession: func() (string, time.Time, error) {
			return "http://127.0.0.1:18470/ui/?bootstrap=test-nonce", time.Now().Add(time.Minute), nil
		},
	}, nil)

	// Mount optional UI handler before Start.
	cs.SetUIHandler("/ui", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-UI-Mounted", "1")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ui:"+r.URL.Path)
	}))

	if err := cs.Start(); err != nil {
		t.Fatalf("ControlServer.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = cs.Shutdown(ctx)
	}()

	base := "http://" + cs.ListenAddr()
	client := NewDirectClient(5 * time.Second)

	// 1. UI mount allows browser navigation on /ui and /ui/* while rejecting non-loopback Host,
	// and keeps /status strictly behind CheckLoopbackGuard + Bearer auth!
	reqUI, _ := http.NewRequest(http.MethodGet, base+"/ui/index.html", nil)
	reqUI.Header.Set("Sec-Fetch-Site", "none")
	respUI, err := client.Do(reqUI)
	if err != nil {
		t.Fatal(err)
	}
	uiBody, _ := io.ReadAll(respUI.Body)
	respUI.Body.Close()
	if respUI.StatusCode != http.StatusOK || string(uiBody) != "ui:/ui/index.html" {
		t.Fatalf("GET /ui/index.html = %d %q, want 200 ui:/ui/index.html", respUI.StatusCode, string(uiBody))
	}

	reqBadHost, _ := http.NewRequest(http.MethodGet, base+"/ui/", nil)
	reqBadHost.Host = "evil.example:18470"
	respBadHost, err := client.Do(reqBadHost)
	if err != nil {
		t.Fatal(err)
	}
	respBadHost.Body.Close()
	if respBadHost.StatusCode != http.StatusForbidden {
		t.Fatalf("bad host on /ui/ = %d, want 403", respBadHost.StatusCode)
	}

	reqStatusBrowser, _ := http.NewRequest(http.MethodGet, base+"/status", nil)
	reqStatusBrowser.Header.Set("Authorization", "Bearer v03-primary-token")
	reqStatusBrowser.Header.Set("Sec-Fetch-Site", "none")
	respSB, err := client.Do(reqStatusBrowser)
	if err != nil {
		t.Fatal(err)
	}
	respSB.Body.Close()
	if respSB.StatusCode != http.StatusForbidden {
		t.Fatalf("browser Sec-Fetch-Site on /status = %d, want 403", respSB.StatusCode)
	}

	// POST /ui/session stays behind Bearer auth.
	reqSessNoAuth, _ := http.NewRequest(http.MethodPost, base+"/ui/session", nil)
	respSessNoAuth, err := client.Do(reqSessNoAuth)
	if err != nil {
		t.Fatal(err)
	}
	respSessNoAuth.Body.Close()
	if respSessNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth POST /ui/session = %d, want 401", respSessNoAuth.StatusCode)
	}

	// 2. Test new Control API endpoints via hub.Client.
	ctx := context.Background()
	ctrlClient := NewClient(cs.ListenAddr(), "v03-primary-token")
	if res, _, err := ctrlClient.SleepService(ctx, "demo"); err != nil || !res.OK || slept != "demo" {
		t.Fatalf("SleepService: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.WakeService(ctx, "demo"); err != nil || !res.OK || woken != "demo" {
		t.Fatalf("WakeService: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.StartAll(ctx); err != nil || !res.OK || startAllCalled.Load() != 1 {
		t.Fatalf("StartAll: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.StopAll(ctx); err != nil || !res.OK || stopAllCalled.Load() != 1 {
		t.Fatalf("StopAll: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.RestartAll(ctx, true); err != nil || !res.OK || restartAllCalled.Load() != 1 || !res.Services["demo"].Service.Healthy {
		t.Fatalf("RestartAll: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.ValidateConfig(ctx); err != nil || !res.OK || validateCalled.Load() != 1 {
		t.Fatalf("ValidateConfig: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.CreateDashboardSession(ctx); err != nil || !res.OK || !strings.Contains(res.URL, "bootstrap=test-nonce") {
		t.Fatalf("CreateDashboardSession: res=%+v err=%v", res, err)
	}

	// 3. Logs SSE Last-Event-ID resumes strictly after Seq=1 and emits id: header.
	sseCtx, sseCancel := context.WithTimeout(ctx, 5*time.Second)
	defer sseCancel()
	reqResume, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, base+"/logs/hub?lines=10&follow=1", nil)
	reqResume.Header.Set("Authorization", "Bearer v03-primary-token")
	reqResume.Header.Set("Last-Event-ID", "1")
	respResume, err := NewDirectClient(0).Do(reqResume)
	if err != nil {
		t.Fatal(err)
	}
	defer respResume.Body.Close()
	brLogs := bufio.NewReader(respResume.Body)
	readLogSSE := func() LogLine {
		t.Helper()
		for {
			ln, err := brLogs.ReadString('\n')
			if err != nil {
				t.Fatalf("read resumed log SSE: %v", err)
			}
			ln = strings.TrimRight(ln, "\r\n")
			if strings.HasPrefix(ln, "data: ") {
				var out LogLine
				if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &out); err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
	}
	got2 := readLogSSE()
	got3 := readLogSSE()
	if got2.Seq != 2 || got2.Text != "line-2" || got3.Seq != 3 || got3.Text != "line-3" {
		t.Fatalf("Last-Event-ID=1 resumed lines = (%+v, %+v), want seq 2 & 3", got2, got3)
	}
	sseCancel()

	// 4. GET /events SSE streams live events and closes immediately on token revocation!
	evCtx, evCancel := context.WithTimeout(ctx, 5*time.Second)
	defer evCancel()
	reqEv, _ := http.NewRequestWithContext(evCtx, http.MethodGet, base+"/events", nil)
	reqEv.Header.Set("Authorization", "Bearer v03-primary-token")
	respEv, err := NewDirectClient(0).Do(reqEv)
	if err != nil {
		t.Fatal(err)
	}
	defer respEv.Body.Close()
	if respEv.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status=%d, want 200", respEv.StatusCode)
	}
	brEv := bufio.NewReader(respEv.Body)
	readHubEv := func() HubEvent {
		t.Helper()
		for {
			ln, err := brEv.ReadString('\n')
			if err != nil {
				t.Fatalf("read /events SSE: %v", err)
			}
			ln = strings.TrimRight(ln, "\r\n")
			if strings.HasPrefix(ln, "data: ") {
				var out HubEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &out); err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
	}
	if ready := readHubEv(); ready.Type != "ready" {
		t.Fatalf("first /events frame = %+v, want ready", ready)
	}
	evCh <- HubEvent{Seq: 1, Type: "service.state", Service: "demo", State: "running", Timestamp: time.Now().UTC().Format(time.RFC3339)}
	if ev := readHubEv(); ev.Type != "service.state" || ev.Service != "demo" {
		t.Fatalf("live /events frame = %+v, want service.state demo", ev)
	}

	// Rotate away v03-primary-token -> /events SSE stream MUST terminate before evCtx timeout!
	if err := os.WriteFile(tokPath, []byte("v03-rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ReloadTokens(tokPath, false); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(brEv); err != nil {
		t.Fatalf("expected revoked /events stream to close cleanly, got %v", err)
	}
}
