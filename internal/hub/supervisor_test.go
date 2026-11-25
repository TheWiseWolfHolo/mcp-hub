package hub

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestHelperProcess acts as a lightweight standalone child process when invoked
// by supervisor / service / gateway tests with GO_WANT_HELPER_PROCESS=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	mode := os.Getenv("HELPER_MODE")
	switch mode {
	case "sleep":
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case "crash":
		if msg := os.Getenv("HELPER_STDERR_MSG"); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(42)

	case "exit0":
		if msg := os.Getenv("HELPER_STDERR_MSG"); msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
		os.Exit(0)

	case "stdio-mcp":
		br := bufio.NewReader(os.Stdin)
		for {
			line, err := br.ReadString('\n')
			if len(strings.TrimSpace(line)) > 0 {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Name      string         `json:"name"`
						Arguments map[string]any `json:"arguments"`
					} `json:"params"`
				}
				if json.Unmarshal([]byte(line), &req) == nil && len(req.ID) > 0 {
					var resp any
					switch req.Method {
					case "initialize":
						resp = map[string]any{
							"jsonrpc": "2.0",
							"id":      req.ID,
							"result": map[string]any{
								"protocolVersion": "2025-03-26",
								"serverInfo":      map[string]any{"name": "mock-stdio", "version": "1.0.0"},
								"capabilities":    map[string]any{"tools": map[string]any{}},
							},
						}
					case "tools/list":
						resp = map[string]any{
							"jsonrpc": "2.0",
							"id":      req.ID,
							"result": map[string]any{
								"tools": []map[string]any{
									{"name": "echo_tool", "annotations": map[string]any{"readOnlyHint": true}},
									{"name": "delete_all", "annotations": map[string]any{"readOnlyHint": false}},
								},
							},
						}
					case "tools/call":
						msg, _ := req.Params.Arguments["msg"].(string)
						resp = map[string]any{
							"jsonrpc": "2.0",
							"id":      req.ID,
							"result": map[string]any{
								"content": []map[string]any{{"type": "text", "text": "echo:" + msg}},
							},
						}
					default:
						resp = map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"ok": true}}
					}
					out, _ := json.Marshal(resp)
					_, _ = os.Stdout.Write(append(out, '\n'))
				}
			}
			if err != nil {
				os.Exit(0)
			}
		}

	case "server":
		addr := os.Getenv("HELPER_LISTEN_ADDR")
		expectedShutdownTok := os.Getenv("TEST_SECRET")
		shutdownMarker := os.Getenv("HELPER_SHUTDOWN_MARKER")
		var reloadCount atomic.Int32

		ln, err := net.Listen("tcp", addr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "helper listen %q: %v\n", addr, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "helper server ready pid=%d secret=%s\n", os.Getpid(), expectedShutdownTok)

		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		})
		mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusOK, map[string]any{
				"pid":           os.Getpid(),
				"secret":        os.Getenv("TEST_SECRET"),
				"version_tag":   os.Getenv("HELPER_VERSION_TAG"),
				"http_proxy":    os.Getenv("HTTP_PROXY"),
				"https_proxy":   os.Getenv("HTTPS_PROXY"),
				"no_proxy":      os.Getenv("NO_PROXY"),
				"inbound_auth":  r.Header.Get("Authorization"),
				"inbound_host":  r.Host,
				"request_count": 1,
			})
		})
		mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				flusher, ok := w.(http.Flusher)
				if !ok {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprintf(w, "data: pid-%d\n\n", os.Getpid())
				flusher.Flush()
				<-r.Context().Done()
				return
			}
			sess := r.Header.Get("Mcp-Session-Id")
			if sess == "" {
				sess = fmt.Sprintf("sess-%d", os.Getpid())
			}
			w.Header().Set("Mcp-Session-Id", sess)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"pid":%d,"session":%q}}`, os.Getpid(), sess)
		})
		mux.HandleFunc("/admin/reload", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if expectedShutdownTok != "" && !VerifyBearer(r, expectedShutdownTok) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			n := reloadCount.Add(1)
			writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "count": n, "pid": os.Getpid()})
		})
		done := make(chan struct{})
		mux.HandleFunc("/admin/shutdown", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if !VerifyBearer(r, expectedShutdownTok) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if shutdownMarker != "" {
				_ = os.WriteFile(shutdownMarker, []byte(fmt.Sprintf("shutdown-pid-%d", os.Getpid())), 0o600)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-done:
			default:
				close(done)
			}
		})

		srv := &http.Server{Handler: mux}
		go func() {
			<-done
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
		_ = srv.Serve(ln)
		os.Exit(0)
	}
}

func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func TestSupervisor_EndToEndLifecycle(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9999")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9999")

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	controlTokPath := filepath.Join(dir, "control.token")
	svcSecretPath := filepath.Join(dir, "svc.token")
	proxyTokPath := filepath.Join(dir, "proxy.token")
	shutdownMarker := filepath.Join(dir, "graceful.marker")

	if err := os.WriteFile(controlTokPath, []byte("ctrl-secret-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svcSecretPath, []byte("svc-secret-456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyTokPath, []byte("px-secret-789\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	svcPort := freeLocalPort(t)
	svcAddr := fmt.Sprintf("127.0.0.1:%d", svcPort)

	cfg := &Config{
		ControlAddr:      "127.0.0.1:0",
		ControlTokenFile: controlTokPath,
		LogDir:           logDir,
		LogMaxBytes:      1024 * 1024,
		Services: []ServiceConfig{
			{
				Name:    "demo-svc",
				Command: exe,
				Args:    []string{"-test.run=^TestHelperProcess$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "server",
					"HELPER_LISTEN_ADDR":     svcAddr,
					"HELPER_SHUTDOWN_MARKER": shutdownMarker,
				},
				EnvFiles: map[string]string{
					"TEST_SECRET": svcSecretPath,
				},
				HealthURL:         "http://" + svcAddr + "/healthz",
				HealthIntervalSec: 1,
				ShutdownURL:       "http://" + svcAddr + "/admin/shutdown",
				ShutdownTokenFile: svcSecretPath,
				ReloadURL:         "http://" + svcAddr + "/admin/reload",
				ReloadTokenFile:   svcSecretPath,
				Description:       "Demo MCP service",
				Tags:              []string{"demo"},
			},
		},
		Proxies: []ProxyConfig{
			{
				Name:       "demo-proxy",
				ListenAddr: "127.0.0.1:0",
				TargetURL:  "http://" + svcAddr,
				TokenFile:  proxyTokPath,
				HealthPath: "/healthz",
				ClientPath: "/env",
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

	ctrlClient := NewClient(sup.ControlAddr(), "ctrl-secret-123")

	// 1. Wait until unauthenticated /healthz reports demo-svc running + healthy.
	var firstPID int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		hz, _, err := ctrlClient.Healthz(ctx)
		if err == nil && hz.OK {
			s := hz.Services["demo-svc"]
			if s.Running && s.Healthy && s.PID > 0 && s.Port == svcPort && s.State == "running" {
				firstPID = s.PID
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if firstPID == 0 {
		st, _, _ := ctrlClient.Status(ctx)
		t.Fatalf("demo-svc did not become healthy in time; status=%+v", st)
	}

	// Verify /status schemaVersion=1, secret redaction in /logs/demo-svc, /catalog, /reload/demo-svc, and TraySnapshot.
	st, _, err := ctrlClient.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != 1 || st.Version != Version || !st.Services["demo-svc"].HasReload {
		t.Fatalf("unexpected /status: %+v", st)
	}

	logsRes, _, err := ctrlClient.Logs(ctx, "demo-svc", 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, ln := range logsRes.Lines {
		if strings.Contains(ln.Text, "svc-secret-456") {
			t.Fatalf("secret leaked in RingLog! line=%q", ln.Text)
		}
	}

	reloadSvcRes, _, err := ctrlClient.ReloadService(ctx, "demo-svc")
	if err != nil || !reloadSvcRes.OK || !strings.Contains(string(reloadSvcRes.Result), `"reloaded":true`) {
		t.Fatalf("ReloadService failed: res=%+v err=%v", reloadSvcRes, err)
	}

	catRes, _, err := ctrlClient.Catalog(ctx)
	if err != nil || len(catRes.Entries) != 1 || catRes.Entries[0].ViaProxy != "demo-proxy" || !catRes.Entries[0].Capabilities.Reload {
		t.Fatalf("unexpected Catalog: %+v err=%v", catRes, err)
	}

	tray := sup.TraySnapshot()
	if tray.State != "ok" || len(tray.Services) != 1 || !tray.Services[0].HasReload {
		t.Fatalf("unexpected TraySnapshot: %+v", tray)
	}

	// 2. Call through proxy & establish a stateful MCP session + open SSE stream.
	proxyAddr, ok := sup.ProxyAddr("demo-proxy")
	if !ok {
		t.Fatal("missing demo-proxy")
	}
	httpClient := NewDirectClient(5 * time.Second)

	reqEnv, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+proxyAddr+"/env", nil)
	reqEnv.Header.Set("Authorization", "Bearer px-secret-789")
	respEnv, err := httpClient.Do(reqEnv)
	if err != nil {
		t.Fatal(err)
	}
	bodyEnv, _ := io.ReadAll(respEnv.Body)
	respEnv.Body.Close()
	var envInfo map[string]any
	_ = json.Unmarshal(bodyEnv, &envInfo)
	if envInfo["secret"] != "svc-secret-456" || envInfo["http_proxy"] != "" || envInfo["inbound_auth"] != "" {
		t.Fatalf("unexpected /env info: %+v", envInfo)
	}

	// Create stateful session on epoch 1.
	reqSess, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+proxyAddr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	reqSess.Header.Set("Authorization", "Bearer px-secret-789")
	reqSess.Header.Set("Content-Type", "application/json")
	respSess, err := httpClient.Do(reqSess)
	if err != nil {
		t.Fatal(err)
	}
	sessID := respSess.Header.Get("Mcp-Session-Id")
	respSess.Body.Close()
	if sessID == "" {
		t.Fatal("expected Mcp-Session-Id from upstream")
	}

	// Open an SSE stream through proxy; restarting the service MUST abort this SSE stream via EpochContext!
	sseReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+proxyAddr+"/mcp", nil)
	sseReq.Header.Set("Authorization", "Bearer px-secret-789")
	sseResp, err := NewDirectClient(0).Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	sseClosed := make(chan struct{})
	go func() {
		defer sseResp.Body.Close()
		_, _ = io.Copy(io.Discard, sseResp.Body)
		close(sseClosed)
	}()

	// 3. Restart service with ?wait=healthy; verify graceful shutdown marker, new PID, SSE abort, and stale-session 404!
	restartRes, _, err := ctrlClient.RestartWithWait(ctx, "demo-svc", true)
	if err != nil {
		t.Fatalf("Client.RestartWithWait: %v", err)
	}
	if restartRes.Service.PID == 0 || restartRes.Service.PID == firstPID || !restartRes.Service.Healthy {
		t.Fatalf("expected new healthy PID after restart (old=%d, new=%+v)", firstPID, restartRes.Service)
	}
	select {
	case <-sseClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("proxy SSE stream was not canceled by EpochContext on service restart")
	}

	markerBytes, err := os.ReadFile(shutdownMarker)
	if err != nil || !strings.Contains(string(markerBytes), fmt.Sprintf("shutdown-pid-%d", firstPID)) {
		t.Fatalf("unexpected shutdown marker: %v %q", err, string(markerBytes))
	}

	// Sending the OLD Mcp-Session-Id from epoch 1 MUST return 404 session_expired!
	reqStale, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+proxyAddr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	reqStale.Header.Set("Authorization", "Bearer px-secret-789")
	reqStale.Header.Set("Content-Type", "application/json")
	reqStale.Header.Set("Mcp-Session-Id", sessID)
	respStale, err := httpClient.Do(reqStale)
	if err != nil {
		t.Fatal(err)
	}
	staleBody, _ := io.ReadAll(respStale.Body)
	respStale.Body.Close()
	if respStale.StatusCode != http.StatusNotFound || !strings.Contains(string(staleBody), "session_expired") {
		t.Fatalf("stale session status = %d body=%s, want 404 session_expired", respStale.StatusCode, string(staleBody))
	}

	// 4. Test OnSuspend / OnResume hooks cleanly, Client.FollowLogs SSE, then POST /stop.
	sup.OnSuspend()
	sup.OnResume()

	followCtx, followCancel := context.WithTimeout(ctx, 3*time.Second)
	var followed []LogLine
	_ = ctrlClient.FollowLogs(followCtx, "demo-svc", 5, func(ln LogLine) error {
		followed = append(followed, ln)
		followCancel()
		return context.Canceled
	})
	followCancel()
	if len(followed) == 0 {
		t.Errorf("expected Client.FollowLogs to receive at least 1 log line")
	}

	if _, err := ctrlClient.Stop(ctx); err != nil {
		t.Fatalf("Client.Stop: %v", err)
	}
	select {
	case <-sup.stopCh:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor stopCh was not closed after POST /stop")
	}
	sup.Shutdown()
}

func TestServiceRunner_LazyWakeIdleSleepAndManualStop(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	svcPort := freeLocalPort(t)
	svcAddr := fmt.Sprintf("127.0.0.1:%d", svcPort)
	dir := t.TempDir()
	proxyTok := filepath.Join(dir, "px.token")
	if err := os.WriteFile(proxyTok, []byte("lazy-px-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := ServiceConfig{
		Name:      "lazy-svc",
		StartMode: "lazy",
		Command:   exe,
		Args:      []string{"-test.run=^TestHelperProcess$"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "server",
			"HELPER_LISTEN_ADDR":     svcAddr,
		},
		HealthURL:         "http://" + svcAddr + "/healthz",
		HealthIntervalSec: 1,
	}

	runner := NewServiceRunner(cfg, filepath.Join(dir, "logs"), 64*1024, nil)
	runner.idleSleep = 250 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.Launch(ctx)
	defer func() {
		cancel()
		runner.Wait()
	}()

	ps := NewProxyServer(ProxyConfig{
		Name:            "lazy-px",
		ListenAddr:      "127.0.0.1:0",
		TargetURL:       "http://" + svcAddr,
		TokenFile:       proxyTok,
		UpstreamService: "lazy-svc",
		ClientPath:      "/env",
	}, nil)
	ps.SetServiceLookup(func(name string) *ServiceRunner {
		if name == "lazy-svc" {
			return runner
		}
		return nil
	})
	if err := ps.Start(); err != nil {
		t.Fatalf("ProxyServer.Start: %v", err)
	}
	defer func() {
		shutCtx, sc := context.WithTimeout(context.Background(), 2*time.Second)
		defer sc()
		_ = ps.Shutdown(shutCtx)
	}()

	client := NewDirectClient(5 * time.Second)

	// 1. Starts in "sleeping"; proxy /healthz returns 200 sleeping WITHOUT waking it!
	time.Sleep(50 * time.Millisecond)
	if st := runner.State(); st != "sleeping" {
		t.Fatalf("initial lazy state = %q, want sleeping", st)
	}
	hzResp, err := client.Get("http://" + ps.ListenAddr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	hzBody, _ := io.ReadAll(hzResp.Body)
	hzResp.Body.Close()
	if hzResp.StatusCode != http.StatusOK || !strings.Contains(string(hzBody), `"sleeping"`) {
		t.Fatalf("sleeping proxy /healthz = %d %s", hzResp.StatusCode, string(hzBody))
	}
	if runner.State() != "sleeping" {
		t.Fatalf("/healthz woke up sleeping service!")
	}

	// 2. Concurrent requests through proxy singleflight-wake the service cleanly (both succeed 200, Restarts == 0)!
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest(http.MethodGet, "http://"+ps.ListenAddr()+"/env", nil)
			req.Header.Set("Authorization", "Bearer lazy-px-secret")
			resp, err := client.Do(req)
			if err != nil {
				errs <- err
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status=%d", resp.StatusCode)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent lazy-wake request failed: %v", err)
	}
	if st := runner.Snapshot(); st.State != "running" || st.Restarts != 0 {
		t.Fatalf("after first singleflight wake: state=%q restarts=%d, want running / 0", st.State, st.Restarts)
	}

	// 3. After idleSleep (250ms) with no active requests/SSE/sessions, runner returns to "sleeping"!
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && runner.State() != "sleeping" {
		time.Sleep(30 * time.Millisecond)
	}
	if runner.State() != "sleeping" {
		t.Fatalf("expected service to idle-sleep back to sleeping, got %q", runner.State())
	}

	// 3b. Re-waking after idle sleep increments WakeCount and keeps Restarts == 0 and CrashRestarts == 0!
	reqWake2, _ := http.NewRequest(http.MethodGet, "http://"+ps.ListenAddr()+"/env", nil)
	reqWake2.Header.Set("Authorization", "Bearer lazy-px-secret")
	respWake2, err := client.Do(reqWake2)
	if err != nil {
		t.Fatal(err)
	}
	respWake2.Body.Close()
	if respWake2.StatusCode != http.StatusOK {
		t.Fatalf("second lazy wake status = %d, want 200", respWake2.StatusCode)
	}
	if st := runner.Snapshot(); st.State != "running" || st.WakeCount != 2 || st.Restarts != 0 || st.CrashRestarts != 0 {
		t.Fatalf("after idle-sleep + re-wake: state=%q wakeCount=%d restarts=%d crashRestarts=%d, want running/2/0/0",
			st.State, st.WakeCount, st.Restarts, st.CrashRestarts)
	}

	// 4. Manual Stop prevents auto-wake (proxy returns 503 service_stopped).
	if err := runner.Stop(ctx); err != nil {
		t.Fatalf("runner.Stop: %v", err)
	}
	reqStopped, _ := http.NewRequest(http.MethodGet, "http://"+ps.ListenAddr()+"/env", nil)
	reqStopped.Header.Set("Authorization", "Bearer lazy-px-secret")
	respStopped, err := client.Do(reqStopped)
	if err != nil {
		t.Fatal(err)
	}
	stoppedBody, _ := io.ReadAll(respStopped.Body)
	respStopped.Body.Close()
	if respStopped.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(stoppedBody), "service_stopped") {
		t.Fatalf("stopped service proxy status = %d %s, want 503 service_stopped", respStopped.StatusCode, string(stoppedBody))
	}
}

func TestServiceRunner_CircuitBreakerDegradedAndExit0RedactedCrashTail(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("circuit breaker opens after maxConsecutiveFailures and resets on Start", func(t *testing.T) {
		cfg := ServiceConfig{
			Name:                   "crasher-cb",
			Command:                exe,
			Args:                   []string{"-test.run=^TestHelperProcess$"},
			MaxConsecutiveFailures: 3,
			Env: map[string]string{
				"GO_WANT_HELPER_PROCESS": "1",
				"HELPER_MODE":            "crash",
				"HELPER_STDERR_MSG":      "fatal crash Bearer topsecret-token-9999",
			},
		}
		redactor := NewRedactor()
		redactor.SetSecrets([]string{"topsecret-token-9999"})
		runner := NewServiceRunner(cfg, t.TempDir(), 64*1024, nil)
		runner.SetRedactor(redactor)
		runner.backoff = []time.Duration{20 * time.Millisecond}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner.Launch(ctx)
		defer func() {
			cancel()
			runner.Wait()
		}()

		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			if runner.State() == "degraded" {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		st := runner.Snapshot()
		if st.State != "degraded" || st.DegradedAt == "" {
			t.Fatalf("expected degraded state with DegradedAt, got %+v", st)
		}
		if st.LastExitCode != 42 {
			t.Errorf("LastExitCode = %d, want 42", st.LastExitCode)
		}
		if len(st.CrashTail) == 0 {
			t.Errorf("expected frozen CrashTail lines")
		}
		for _, ln := range st.CrashTail {
			if strings.Contains(ln, "topsecret-token-9999") {
				t.Errorf("secret leaked in CrashTail: %q", ln)
			}
			if !strings.Contains(ln, RedactedPlaceholder) {
				t.Errorf("expected redacted placeholder in CrashTail: %q", ln)
			}
		}

		// EnsureRunning fails closed when degraded.
		if err := runner.EnsureRunning(ctx); err != ErrServiceDegraded {
			t.Fatalf("EnsureRunning while degraded = %v, want ErrServiceDegraded", err)
		}

		// Restarts stop incrementing once degraded.
		restartsAtTrip := st.Restarts
		time.Sleep(100 * time.Millisecond)
		if got := runner.Snapshot().Restarts; got != restartsAtTrip {
			t.Fatalf("service kept restarting while degraded (%d -> %d)", restartsAtTrip, got)
		}
	})

	t.Run("unexpected exit 0 records unexpected exit 0", func(t *testing.T) {
		cfg := ServiceConfig{
			Name:                   "exit0-svc",
			Command:                exe,
			Args:                   []string{"-test.run=^TestHelperProcess$"},
			MaxConsecutiveFailures: 2,
			Env: map[string]string{
				"GO_WANT_HELPER_PROCESS": "1",
				"HELPER_MODE":            "exit0",
			},
		}
		runner := NewServiceRunner(cfg, t.TempDir(), 64*1024, nil)
		runner.backoff = []time.Duration{20 * time.Millisecond}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		runner.Launch(ctx)
		defer func() {
			cancel()
			runner.Wait()
		}()

		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if runner.State() == "degraded" {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		st := runner.Snapshot()
		if st.LastExitCode != 0 || st.LastError != "unexpected exit 0" {
			t.Fatalf("expected LastExitCode=0 LastError='unexpected exit 0', got code=%d err=%q", st.LastExitCode, st.LastError)
		}
	})
}

func TestSupervisor_HotReload(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	cfgPath := filepath.Join(dir, "config.json")
	ctrlTokPath := filepath.Join(dir, "ctrl.token")
	pxTokPath := filepath.Join(dir, "px.token")
	envSecretPath := filepath.Join(dir, "env.secret")

	if err := os.WriteFile(ctrlTokPath, []byte("ctrl-reload-tok-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pxTokPath, []byte("px-reload-tok-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envSecretPath, []byte("env-secret-v1"), 0o600); err != nil {
		t.Fatal(err)
	}

	portKeep := freeLocalPort(t)
	portChange := freeLocalPort(t)
	portAdded := freeLocalPort(t)
	portProxy := freeLocalPort(t)
	portCtrl := freeLocalPort(t)

	writeCfg := func(c *Config) {
		t.Helper()
		raw, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(cfgPath, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfgV1 := &Config{
		ControlAddr:      fmt.Sprintf("127.0.0.1:%d", portCtrl),
		ControlTokenFile: ctrlTokPath,
		LogDir:           logDir,
		Services: []ServiceConfig{
			{
				Name:    "svc-keep",
				Command: exe,
				Args:    []string{"-test.run=^TestHelperProcess$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "server",
					"HELPER_LISTEN_ADDR":     fmt.Sprintf("127.0.0.1:%d", portKeep),
				},
				HealthURL:         fmt.Sprintf("http://127.0.0.1:%d/healthz", portKeep),
				HealthIntervalSec: 1,
			},
			{
				Name:    "svc-change",
				Command: exe,
				Args:    []string{"-test.run=^TestHelperProcess$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "server",
					"HELPER_LISTEN_ADDR":     fmt.Sprintf("127.0.0.1:%d", portChange),
					"HELPER_VERSION_TAG":     "v1",
				},
				EnvFiles:          map[string]string{"TEST_SECRET": envSecretPath},
				HealthURL:         fmt.Sprintf("http://127.0.0.1:%d/healthz", portChange),
				HealthIntervalSec: 1,
			},
		},
		Proxies: []ProxyConfig{
			{
				Name:       "px-keep",
				ListenAddr: fmt.Sprintf("127.0.0.1:%d", portProxy),
				TargetURL:  fmt.Sprintf("http://127.0.0.1:%d", portKeep),
				TokenFile:  pxTokPath,
				ClientPath: "/env",
			},
		},
	}
	writeCfg(cfgV1)

	loadedV1, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	sup, err := NewSupervisor(loadedV1, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Shutdown()

	ctrlClient := NewClient(sup.ControlAddr(), "ctrl-reload-tok-1")
	waitHealthy := func(names ...string) map[string]ServiceStatus {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			st, _, err := ctrlClient.Status(ctx)
			if err == nil {
				allUp := true
				for _, n := range names {
					s, ok := st.Services[n]
					if !ok || !s.Running || !s.Healthy || s.PID <= 0 {
						allUp = false
						break
					}
				}
				if allUp {
					return st.Services
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatalf("services %v did not become healthy in time", names)
		return nil
	}

	svcsV1 := waitHealthy("svc-keep", "svc-change")
	keepPID := svcsV1["svc-keep"].PID
	changePID := svcsV1["svc-change"].PID

	// 1. Fail-closed reload on invalid config: live services stay completely untouched!
	if err := os.WriteFile(cfgPath, []byte(`{"controlAddr":"0.0.0.0:18470"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ctrlClient.ReloadHub(ctx); err == nil {
		t.Fatal("expected ReloadHub to reject invalid config")
	}

	// Binding a replacement fails before retiring any live endpoint or applying the new token.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	nextControlToken := filepath.Join(dir, "replacement-control.token")
	if err := os.WriteFile(nextControlToken, []byte("replacement-control-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBlocked := *cfgV1
	cfgBlocked.ControlTokenFile = nextControlToken
	cfgBlocked.Proxies = append([]ProxyConfig(nil), cfgV1.Proxies...)
	cfgBlocked.Proxies[0].ListenAddr = occupied.Addr().String()
	writeCfg(&cfgBlocked)
	if _, _, err := ctrlClient.ReloadHub(ctx); err == nil {
		t.Fatal("expected occupied replacement address to reject reload")
	}
	oldAddr, ok := sup.ProxyAddr("px-keep")
	reqOld, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+oldAddr+"/env", nil)
	reqOld.Header.Set("Authorization", "Bearer px-reload-tok-1")
	respOld, err := NewDirectClient(5 * time.Second).Do(reqOld)
	if err != nil {
		t.Fatal(err)
	}
	respOld.Body.Close()
	if !ok || oldAddr != cfgV1.Proxies[0].ListenAddr || respOld.StatusCode != http.StatusOK {
		t.Fatalf("old proxy after failed reload: addr=%q status=%d", oldAddr, respOld.StatusCode)
	}
	if got := waitHealthy("svc-keep", "svc-change"); got["svc-keep"].PID != keepPID || got["svc-change"].PID != changePID {
		t.Fatal("failed listener preparation disturbed service processes")
	}

	// 2. Valid hot reload: update envSecretPath (triggers svc-change restart), add svc-added, rotate proxy token!
	if err := os.WriteFile(envSecretPath, []byte("env-secret-v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pxTokPath, []byte("px-reload-tok-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgV2 := *cfgV1
	cfgV2.Services = append(append([]ServiceConfig(nil), cfgV1.Services...), ServiceConfig{
		Name:    "svc-added",
		Command: exe,
		Args:    []string{"-test.run=^TestHelperProcess$"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "server",
			"HELPER_LISTEN_ADDR":     fmt.Sprintf("127.0.0.1:%d", portAdded),
		},
		HealthURL:         fmt.Sprintf("http://127.0.0.1:%d/healthz", portAdded),
		HealthIntervalSec: 1,
	})
	writeCfg(&cfgV2)

	reloadRes, _, err := ctrlClient.ReloadHub(ctx)
	if err != nil {
		t.Fatalf("ReloadHub: %v", err)
	}
	if !reloadRes.OK {
		t.Fatalf("expected reload OK: %+v", reloadRes)
	}

	svcsV2 := waitHealthy("svc-keep", "svc-change", "svc-added")
	if svcsV2["svc-keep"].PID != keepPID {
		t.Errorf("unchanged svc-keep PID changed! %d -> %d", keepPID, svcsV2["svc-keep"].PID)
	}
	if svcsV2["svc-change"].PID == changePID {
		t.Errorf("expected svc-change (envFiles content changed) to restart with new PID, stayed %d", changePID)
	}

	// Proxy accepts new token immediately without restarting listener.
	reqPx, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/env", portProxy), nil)
	reqPx.Header.Set("Authorization", "Bearer px-reload-tok-2")
	respPx, err := NewDirectClient(5 * time.Second).Do(reqPx)
	if err != nil {
		t.Fatal(err)
	}
	respPx.Body.Close()
	if respPx.StatusCode != http.StatusOK {
		t.Fatalf("proxy new token after hot reload = %d, want 200", respPx.StatusCode)
	}
}

func TestSupervisor_ShutdownAfterFailedStartDoesNotDeadlock(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		ControlAddr:                "127.0.0.1:0",
		AllowInsecureNoControlAuth: true,
		LogDir:                     filepath.Join(dir, "logs"),
		Services: []ServiceConfig{
			{Name: "never-started", Command: "noop"},
		},
		Proxies: []ProxyConfig{
			{
				Name:       "broken-proxy",
				ListenAddr: "127.0.0.1:0",
				TargetURL:  "http://127.0.0.1:18486",
				TokenFile:  filepath.Join(dir, "missing.token"),
			},
		},
	}

	sup, err := NewSupervisor(cfg, filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	if err := sup.Start(context.Background()); err == nil {
		sup.Shutdown()
		t.Fatal("expected Start to fail on missing proxy token file")
	}

	done := make(chan struct{})
	go func() {
		sup.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Supervisor.Shutdown deadlocked after failed Start!")
	}
}

func TestServiceRunner_CrashBackoff(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := ServiceConfig{
		Name:    "crasher",
		Command: exe,
		Args:    []string{"-test.run=^TestHelperProcess$"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "crash",
		},
	}
	runner := NewServiceRunner(cfg, t.TempDir(), 64*1024, nil)
	runner.backoff = []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond}

	idx := 0
	wantSeq := []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond}
	for i, want := range wantSeq {
		if got := runner.nextBackoff(&idx); got != want {
			t.Fatalf("step %d: backoff = %s, want %s", i, got, want)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner.Launch(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st := runner.Snapshot()
		if st.Restarts >= 2 && st.LastExitCode == 42 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	st := runner.Snapshot()
	cancel()
	runner.Wait()

	if st.Restarts < 2 {
		t.Fatalf("expected crasher to restart at least twice, got restarts=%d lastError=%q", st.Restarts, st.LastError)
	}
	if st.LastExitCode != 42 {
		t.Errorf("LastExitCode = %d, want 42", st.LastExitCode)
	}
}

func TestServiceRunner_TCPHealthProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	runner := NewServiceRunner(ServiceConfig{
		Name:      "tcp-svc",
		Command:   "noop",
		HealthURL: "tcp://" + addr,
	}, t.TempDir(), 64*1024, nil)

	if !runner.probeHealthOnce(context.Background()) {
		_ = ln.Close()
		t.Fatal("expected tcp:// health probe to succeed while listener is open")
	}
	_ = ln.Close()
	if runner.probeHealthOnce(context.Background()) {
		t.Fatal("expected tcp:// health probe to fail after listener closed")
	}
}

func TestServiceRunner_ShutdownURLFailureTerminatesImmediately(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int32
	badShutdownSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badShutdownSrv.Close()

	cfg := ServiceConfig{
		Name:        "sleeper",
		Command:     exe,
		Args:        []string{"-test.run=^TestHelperProcess$"},
		ShutdownURL: badShutdownSrv.URL + "/admin/shutdown",
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "sleep",
		},
	}

	runner := NewServiceRunner(cfg, t.TempDir(), 64*1024, nil)
	if runner.shutdownTimeout != 5*time.Second {
		t.Fatalf("default shutdownTimeout = %s, want 5s", runner.shutdownTimeout)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runner.Launch(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := runner.Snapshot(); st.Running && st.PID > 0 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !runner.Snapshot().Running {
		cancel()
		runner.Wait()
		t.Fatal("sleeper child did not start in time")
	}

	start := time.Now()
	cancel()
	runner.Wait()
	elapsed := time.Since(start)

	if hits.Load() != 1 {
		t.Errorf("shutdownUrl hits = %d, want 1", hits.Load())
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("shutdown took %s after non-2xx shutdownUrl; expected immediate TerminateAndClose without waiting 5s", elapsed)
	}
}

func TestSupervisor_ShutdownCancelsOpenSSEConcurrently(t *testing.T) {
	const token = "sse-shutdown-secret"
	var canceled atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: open\n\n")
		flusher.Flush()
		<-r.Context().Done()
		canceled.Add(1)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "proxy.token")
	if err := os.WriteFile(tokPath, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{
		ControlAddr:                "127.0.0.1:0",
		AllowInsecureNoControlAuth: true,
		LogDir:                     filepath.Join(dir, "logs"),
		Proxies: []ProxyConfig{
			{
				Name:       "sse-a",
				ListenAddr: "127.0.0.1:0",
				TargetURL:  upstream.URL,
				TokenFile:  tokPath,
			},
			{
				Name:       "sse-b",
				ListenAddr: "127.0.0.1:0",
				TargetURL:  upstream.URL,
				TokenFile:  tokPath,
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
		t.Fatalf("Start: %v", err)
	}

	client := NewDirectClient(0)
	openStream := func(proxyName string) *http.Response {
		t.Helper()
		addr, ok := sup.ProxyAddr(proxyName)
		if !ok {
			t.Fatalf("missing proxy %s", proxyName)
		}
		req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/sse", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("connect %s SSE: %v", proxyName, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("%s SSE status = %d, want 200", proxyName, resp.StatusCode)
		}
		buf := make([]byte, len("data: open\n\n"))
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("%s read initial SSE chunk: %v", proxyName, err)
		}
		return resp
	}

	respA := openStream("sse-a")
	defer respA.Body.Close()
	respB := openStream("sse-b")
	defer respB.Body.Close()

	start := time.Now()
	sup.Shutdown()
	elapsed := time.Since(start)

	if elapsed >= 1*time.Second {
		t.Fatalf("Supervisor.Shutdown with 2 open SSE streams took %s; expected immediate BaseContext cancellation (<1s)", elapsed)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && canceled.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := canceled.Load(); got != 2 {
		t.Errorf("upstream SSE handlers canceled = %d, want 2", got)
	}
}

func TestServiceRunner_V03HealthBackoffAggregationAndCoalesceStop(t *testing.T) {
	// 1. aggregateMCPMetrics preserves latest non-empty LastTool independent of slice order.
	mA := NewMCPMetrics()
	mB := NewMCPMetrics()
	recA, finA := mA.WrapResponseWriter(httptest.NewRecorder(), MCPPeekInfo{
		IsJSONRPC: true, Method: "tools/call", HasToolsCall: true, ToolName: "read_doc",
		ToolCalls: []MCPToolCallRef{{ID: json.RawMessage("1"), Name: "read_doc"}},
	}, 1, nil)
	recA.WriteHeader(http.StatusOK)
	finA()
	time.Sleep(10 * time.Millisecond)
	recB, finB := mB.WrapResponseWriter(httptest.NewRecorder(), MCPPeekInfo{
		IsJSONRPC: true, Method: "ping",
	}, 1, nil)
	recB.WriteHeader(http.StatusOK)
	finB()

	aggAB := aggregateMCPMetrics([]*MCPMetrics{mA, mB})
	aggBA := aggregateMCPMetrics([]*MCPMetrics{mB, mA})
	if aggAB.LastTool != "read_doc" || aggBA.LastTool != "read_doc" {
		t.Fatalf("aggregateMCPMetrics lost LastTool across ordering: AB=%q BA=%q", aggAB.LastTool, aggBA.LastTool)
	}

	// 2. pollHealth stops hammering every 75ms after startTimeout and records redacted LastHealthError.
	var probes atomic.Int32
	var healthyFlag atomic.Bool
	unhealthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if healthyFlag.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthySrv.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var stateChanges atomic.Int32
	runner := NewServiceRunner(ServiceConfig{
		Name:      "health-slowdown",
		StartMode: "eager",
		Command:   exe,
		Args:      []string{"-test.run=^TestHelperProcess$"},
		Env:       map[string]string{"GO_WANT_HELPER_PROCESS": "1", "HELPER_MODE": "sleep"},
		HealthURL: unhealthySrv.URL,
	}, t.TempDir(), DefaultLogMaxBytes, nil)
	runner.startTimeout = 120 * time.Millisecond
	runner.healthInterval = 400 * time.Millisecond
	runner.startingPollTick = 40 * time.Millisecond
	runner.SetOnStateChange(func() { stateChanges.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		runner.Wait()
	}()
	runner.Launch(ctx)

	time.Sleep(250 * time.Millisecond)
	probesAfterTimeout := probes.Load()
	stFail := runner.Snapshot()
	if stFail.LastHealthCheckAt == "" || !strings.Contains(stFail.LastHealthError, "503") {
		t.Fatalf("expected LastHealthCheckAt and HTTP 503 LastHealthError, got %+v", stFail)
	}
	time.Sleep(350 * time.Millisecond)
	if delta := probes.Load() - probesAfterTimeout; delta > 3 {
		t.Fatalf("pollHealth kept hammering after startTimeout: %d probes in 350ms (healthInterval=400ms)", delta)
	}

	healthyFlag.Store(true)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runner.Snapshot().Healthy {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	stOK := runner.Snapshot()
	if !stOK.Healthy || stOK.LastHealthError != "" {
		t.Fatalf("expected Healthy=true and cleared LastHealthError, got %+v", stOK)
	}
	if stateChanges.Load() == 0 {
		t.Fatalf("expected OnStateChange to fire on transitions")
	}

	// 3. coalesceQueuedRestarts must NEVER reorder svcOpStop behind a restart launch!
	shutdownStarted := make(chan struct{})
	releaseShutdown := make(chan struct{})
	var shutdownHits atomic.Int32
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/shutdown":
			if shutdownHits.Add(1) == 1 {
				close(shutdownStarted)
				<-releaseShutdown
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ctrlSrv.Close()

	runnerStop := NewServiceRunner(ServiceConfig{
		Name:        "restart-then-stop",
		StartMode:   "eager",
		Command:     exe,
		Args:        []string{"-test.run=^TestHelperProcess$"},
		Env:         map[string]string{"GO_WANT_HELPER_PROCESS": "1", "HELPER_MODE": "sleep"},
		HealthURL:   ctrlSrv.URL + "/health",
		ShutdownURL: ctrlSrv.URL + "/shutdown",
	}, t.TempDir(), DefaultLogMaxBytes, nil)
	runnerStop.shutdownTimeout = 2 * time.Second

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer func() {
		cancel2()
		runnerStop.Wait()
	}()
	runnerStop.Launch(ctx2)
	if err := runnerStop.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	initialPID := runnerStop.Snapshot().PID

	restartDone := make(chan error, 1)
	go func() {
		restartDone <- runnerStop.Restart(context.Background(), true)
	}()
	select {
	case <-shutdownStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for restart shutdown phase")
	}

	// Queue Stop while restart's stopProcess is still running!
	stopDone := make(chan error, 1)
	go func() {
		stopDone <- runnerStop.Stop(context.Background())
	}()
	time.Sleep(50 * time.Millisecond)
	close(releaseShutdown)

	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop during restart failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for Stop during restart")
	}
	select {
	case err := <-restartDone:
		if !errors.Is(err, ErrServiceStopped) {
			t.Fatalf("expected Restart canceled by queued Stop with ErrServiceStopped, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Restart error")
	}
	stStopped := runnerStop.Snapshot()
	if stStopped.State != "stopped" || stStopped.Running || stStopped.PID != 0 || stStopped.Restarts != 0 {
		t.Fatalf("queued Stop was reordered behind restart launch! initialPID=%d status=%+v", initialPID, stStopped)
	}
}

type fakeDashboardHandler struct {
	nonce string
}

func (f *fakeDashboardHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "dashboard-ok")
}

func (f *fakeDashboardHandler) MintBootstrapToken() (string, time.Time) {
	return f.nonce, time.Now().Add(time.Minute)
}

func TestSupervisor_V03BulkControlsValidateTrayEventsAndClient(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	cfgPath := filepath.Join(dir, "config.json")
	ctrlTokPath := filepath.Join(dir, "ctrl.token")
	if err := os.WriteFile(ctrlTokPath, []byte("sup-v03-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	portHTTP := freeLocalPort(t)
	cfg := &Config{
		ControlAddr:      "127.0.0.1:0",
		ControlTokenFile: ctrlTokPath,
		LogDir:           logDir,
		Services: []ServiceConfig{
			{
				Name:      "lazy-http",
				StartMode: "lazy",
				Command:   exe,
				Args:      []string{"-test.run=^TestHelperProcess$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "server",
					"HELPER_LISTEN_ADDR":     fmt.Sprintf("127.0.0.1:%d", portHTTP),
				},
				HealthURL:         fmt.Sprintf("http://127.0.0.1:%d/healthz", portHTTP),
				HealthIntervalSec: 1,
			},
			{
				Name:    "lazy-stdio",
				Mode:    "stdio-bridge",
				Command: exe,
				Args:    []string{"-test.run=^TestHelperProcess$"},
				Env: map[string]string{
					"GO_WANT_HELPER_PROCESS": "1",
					"HELPER_MODE":            "stdio-mcp",
				},
			},
		},
	}
	rawCfg, _ := json.MarshalIndent(cfg, "", "  ")
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
		t.Fatalf("Start: %v", err)
	}
	defer sup.Shutdown()

	// 1. CreateDashboardLaunchURL is auto-mounted on Start, fails closed if cleared, and honors custom handlers.
	autoLaunchURL, err := sup.CreateDashboardLaunchURL()
	if err != nil || !strings.Contains(autoLaunchURL, "/ui?bootstrap=") {
		t.Fatalf("CreateDashboardLaunchURL after Start = %q err=%v", autoLaunchURL, err)
	}
	sup.SetDashboardHandler(nil)
	if _, err := sup.CreateDashboardLaunchURL(); !errors.Is(err, ErrDashboardNotMounted) {
		t.Fatalf("CreateDashboardLaunchURL after clearing = %v, want ErrDashboardNotMounted", err)
	}
	sup.SetDashboardHandler(&fakeDashboardHandler{nonce: "nonce-xyz-123"})
	launchURL, err := sup.CreateDashboardLaunchURL()
	if err != nil || !strings.Contains(launchURL, "/ui?bootstrap=nonce-xyz-123") {
		t.Fatalf("CreateDashboardLaunchURL = %q err=%v", launchURL, err)
	}

	// 2. TraySnapshot reports "sleeping" when all services sleep, and populates new fields!
	time.Sleep(50 * time.Millisecond)
	traySleep := sup.TraySnapshot()
	if traySleep.State != "sleeping" || traySleep.SleepingCount != 2 || traySleep.RunningCount != 0 || traySleep.Version != Version || traySleep.ControlAddr == "" {
		t.Fatalf("unexpected sleeping TraySnapshot: %+v", traySleep)
	}

	// 3. NewClient has no 40s http.Client.Timeout trap; NewClientWithTimeout sets explicit timeout.
	ctrlClient := NewClient(sup.ControlAddr(), "sup-v03-secret")
	if ctrlClient.http.Timeout != 0 {
		t.Fatalf("NewClient http.Timeout = %s, want 0 (caller ctx deadline)", ctrlClient.http.Timeout)
	}
	shortClient := NewClientWithTimeout(sup.ControlAddr(), "sup-v03-secret", 7*time.Second)
	if shortClient.http.Timeout != 7*time.Second {
		t.Fatalf("NewClientWithTimeout http.Timeout = %s, want 7s", shortClient.http.Timeout)
	}

	// 4. SubscribeEvents receives live events on Wake/Sleep/Stop/StartAll/RestartAll!
	evCh, unsub := sup.SubscribeEvents()
	defer unsub()

	if res, _, err := ctrlClient.WakeService(ctx, "lazy-stdio"); err != nil || !res.OK || res.Service.State != "running" {
		t.Fatalf("WakeService(lazy-stdio): res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.SleepService(ctx, "lazy-stdio"); err != nil || !res.OK || res.Service.State != "sleeping" {
		t.Fatalf("SleepService(lazy-stdio): res=%+v err=%v", res, err)
	}

	// Stop one service while the other sleeps -> TraySnapshot must be "degraded" and tooltip MUST show BOTH sleep and stopped!
	if res, _, err := ctrlClient.StopService(ctx, "lazy-http"); err != nil || !res.OK || res.Service.State != "stopped" {
		t.Fatalf("StopService(lazy-http): res=%+v err=%v", res, err)
	}
	trayMixed := sup.TraySnapshot()
	if trayMixed.State != "degraded" || trayMixed.SleepingCount != 1 || trayMixed.StoppedCount != 1 {
		t.Fatalf("unexpected mixed sleeping+stopped TraySnapshot: %+v", trayMixed)
	}
	if !strings.Contains(trayMixed.Tooltip, "1 sleep") || !strings.Contains(trayMixed.Tooltip, "1 stopped") {
		t.Fatalf("tooltip hid stopped count while sleeping exists: %q", trayMixed.Tooltip)
	}

	// StartAll, RestartAll, StopAll via Control API.
	if res, _, err := ctrlClient.StartAll(ctx); err != nil || !res.OK || len(res.Services) != 2 {
		t.Fatalf("StartAll: res=%+v err=%v", res, err)
	}
	if res, _, err := ctrlClient.RestartAll(ctx, true); err != nil || !res.OK || !res.Services["lazy-http"].Service.Healthy {
		t.Fatalf("RestartAll: res=%+v err=%v", res, err)
	}
	// StatusResponse exposes HubPrivateMB, HubWorkingSetMB, Goroutines.
	st, _, err := ctrlClient.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Goroutines <= 0 {
		t.Fatalf("expected Goroutines > 0, got %+v", st)
	}
	// ValidateConfig dry-run preview via Control API.
	valRes, _, err := ctrlClient.ValidateConfig(ctx)
	if err != nil || !valRes.OK || len(valRes.UnchangedServices) != 2 {
		t.Fatalf("ValidateConfig: res=%+v err=%v", valRes, err)
	}
	if res, _, err := ctrlClient.StopAll(ctx); err != nil || !res.OK {
		t.Fatalf("StopAll: res=%+v err=%v", res, err)
	}

	// Verify SubscribeEvents received events.
	select {
	case ev := <-evCh:
		if ev.Type == "" || ev.Timestamp == "" {
			t.Fatalf("unexpected HubEvent: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("expected HubEvent on SubscribeEvents channel")
	}
}

func TestInferServiceTokenFile(t *testing.T) {
	t.Run("prefers shutdownTokenFile over structured TOKENS_FILE json", func(t *testing.T) {
		svc := ServiceConfig{
			Name: "search-toolkit",
			Env: map[string]string{
				"SEARCH_TOOLKIT_HTTP_TOKENS_FILE": "D:/ProgramData/SearchToolkit/tokens.json",
			},
			ShutdownTokenFile: "D:/ProgramData/McpHub/secrets/search-toolkit-full.token",
			ReloadTokenFile:   "D:/ProgramData/McpHub/secrets/search-toolkit-reload.token",
		}
		if got := InferServiceTokenFile(svc); got != svc.ShutdownTokenFile {
			t.Fatalf("InferServiceTokenFile() = %q, want %q", got, svc.ShutdownTokenFile)
		}
	})

	t.Run("falls back to reloadTokenFile before env token files", func(t *testing.T) {
		svc := ServiceConfig{
			Name: "search-toolkit",
			Env: map[string]string{
				"SEARCH_TOOLKIT_HTTP_TOKENS_FILE": "D:/ProgramData/SearchToolkit/tokens.json",
				"APP_TOKEN_FILE":                  "D:/ProgramData/McpHub/secrets/app.token",
			},
			ReloadTokenFile: "D:/ProgramData/McpHub/secrets/reload.token",
		}
		if got := InferServiceTokenFile(svc); got != svc.ReloadTokenFile {
			t.Fatalf("InferServiceTokenFile() = %q, want %q", got, svc.ReloadTokenFile)
		}
	})

	t.Run("skips structured _TOKENS_FILE and .json policy files", func(t *testing.T) {
		svc := ServiceConfig{
			Name: "policy-only",
			Env: map[string]string{
				"SEARCH_TOOLKIT_HTTP_TOKENS_FILE": "D:/ProgramData/SearchToolkit/tokens.json",
				"CUSTOM_TOKEN_FILE":               "D:/ProgramData/SearchToolkit/policy.json",
			},
			EnvFiles: map[string]string{
				"MULTI_TOKENS_FILE": "D:/ProgramData/SearchToolkit/more.json",
			},
		}
		if got := InferServiceTokenFile(svc); got != "" {
			t.Fatalf("expected empty token file when only structured policy files exist, got %q", got)
		}
	})

	t.Run("deterministic sorted selection across multiple env and envFiles entries", func(t *testing.T) {
		svc := ServiceConfig{
			Name: "multi-env",
			Env: map[string]string{
				"Z_TOKEN_FILE": "D:/secrets/z.token",
				"A_TOKEN_FILE": "D:/secrets/a.token",
				"M_TOKENS_FILE": "D:/secrets/m.json",
			},
		}
		for i := 0; i < 20; i++ {
			if got := InferServiceTokenFile(svc); got != "D:/secrets/a.token" {
				t.Fatalf("iteration %d: got %q, want D:/secrets/a.token", i, got)
			}
		}

		svcEnvFiles := ServiceConfig{
			Name: "envfiles-only",
			EnvFiles: map[string]string{
				"Z_AUTH_TOKEN": "D:/secrets/z-env.token",
				"A_AUTH_TOKEN": "D:/secrets/a-env.token",
			},
		}
		for i := 0; i < 20; i++ {
			if got := InferServiceTokenFile(svcEnvFiles); got != "D:/secrets/a-env.token" {
				t.Fatalf("iteration %d: got %q, want D:/secrets/a-env.token", i, got)
			}
		}
	})
}
