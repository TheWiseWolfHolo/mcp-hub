package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// TestHelperRegressionBridge is a subprocess helper for H1 (stdin wedge), L1 (server ping),
// and M5 (delayed exit / exclusive lock overlap) tests.
func TestHelperRegressionBridge(t *testing.T) {
	if os.Getenv("GO_WANT_REGRESSION_HELPER") != "1" {
		return
	}
	mode := os.Getenv("REGRESSION_MODE")
	lockPath := os.Getenv("REGRESSION_LOCK_PATH")

	var lockFile *os.File
	if lockPath != "" {
		// Create exclusive sentinel file; fail if a previous child is still holding it!
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintf(os.Stderr, "exclusive lock conflict: %v\n", err)
			os.Exit(9)
		}
		lockFile = f
		defer func() {
			_ = lockFile.Close()
			_ = os.Remove(lockPath)
		}()
	}

	writeLine := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = os.Stdout.Write(append(b, '\n'))
	}

	br := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
			}
			if json.Unmarshal(trimmed, &msg) == nil {
				switch msg.Method {
				case "initialize":
					writeLine(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result": map[string]any{
							"protocolVersion": "2025-03-26",
							"capabilities":    map[string]any{"tools": map[string]any{}},
							"serverInfo":      map[string]any{"name": "regression-helper", "pid": os.Getpid()},
						},
					})
				case "notifications/initialized":
					if mode == "wedge_stdin" {
						// Stop reading stdin completely so a large request fills the OS pipe buffer and blocks Write!
						time.Sleep(60 * time.Second)
						os.Exit(0)
					}
				case "tools/call":
					if mode == "server_ping" {
						// Send server->client ping and wait for {} result before answering client!
						writeLine(map[string]any{
							"jsonrpc": "2.0",
							"id":      "srv-ping-1",
							"method":  "ping",
						})
						pingReplyLine, perr := br.ReadBytes('\n')
						if perr != nil {
							os.Exit(2)
						}
						var pr struct {
							ID     json.RawMessage `json:"id"`
							Result map[string]any  `json:"result"`
							Error  json.RawMessage `json:"error"`
						}
						_ = json.Unmarshal(bytes.TrimSpace(pingReplyLine), &pr)
						okPing := pr.Result != nil && len(bytes.TrimSpace(pr.Error)) == 0
						writeLine(map[string]any{
							"jsonrpc": "2.0",
							"id":      msg.ID,
							"result":  map[string]any{"pingOK": okPing},
						})
						continue
					}
					writeLine(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result":  map[string]any{"pid": os.Getpid()},
					})
				}
			}
		}
		if err != nil {
			if mode == "slow_exit_lock" {
				// Hold lock for 250ms after stdin closes before exiting cleanly!
				time.Sleep(250 * time.Millisecond)
			}
			if lockFile != nil {
				_ = lockFile.Close()
				_ = os.Remove(lockPath)
			}
			os.Exit(0)
		}
	}
}

func TestSecurity_C1C2_StrictToolFilterProxyAndBridge(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "scoped.token")
	if err := os.WriteFile(tokPath, []byte("scoped-secret-token-1234"), 0o600); err != nil {
		t.Fatal(err)
	}

	ps := NewProxyServer(ProxyConfig{
		Name:       "strict-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		Tokens: []RouteTokenConfig{
			{
				Name:      "scoped",
				TokenFile: tokPath,
				DenyTools: []string{"denied*", "fs/delete_[0-9]"},
			},
		},
	}, nil)
	if err := ps.Start(); err != nil {
		t.Fatalf("ProxyServer.Start: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ps.Shutdown(ctx)
	}()

	client := NewDirectClient(5 * time.Second)
	postProxy := func(ct, ce, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer scoped-secret-token-1234")
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if ce != "" {
			req.Header.Set("Content-Encoding", ce)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST proxy: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(b)
	}

	// C1a: 20 KiB padded tools/call (> 16 KiB peek buffer) MUST be blocked!
	pad20K := strings.Repeat("A", 20*1024)
	paddedCall := fmt.Sprintf(`{"pad":"%s","jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"denied_tool"}}`, pad20K)
	resp, body := postProxy("application/json", "", paddedCall)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "-32601") {
		t.Fatalf("C1 20KiB padded call not denied: status=%d body=%s", resp.StatusCode, body)
	}

	// C1b: Batch with denied tool after 20 KiB padding MUST be blocked!
	paddedBatch := fmt.Sprintf(`[{"jsonrpc":"2.0","id":1,"method":"ping","pad":"%s"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"denied_batch"}}]`, pad20K)
	resp, body = postProxy("application/json", "", paddedBatch)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "-32601") {
		t.Fatalf("C1 padded batch not denied: status=%d body=%s", resp.StatusCode, body)
	}

	// C1c: Empty / missing tool name MUST fail closed (HTTP 400 / -32600)!
	for _, payload := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":""}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":["denied"]}`,
	} {
		resp, body = postProxy("application/json", "", payload)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "-32600") {
			t.Fatalf("C1 empty/missing tool name (%s) not rejected: status=%d body=%s", payload, resp.StatusCode, body)
		}
	}

	// C1d: Malformed Content-Type, non-JSON Content-Type, missing Content-Type, and Content-Encoding MUST return 415!
	for _, tc := range []struct {
		name string
		ct   string
		ce   string
	}{
		{"malformed ct", "application/json; charset", ""},
		{"non-json ct", "text/plain", ""},
		{"missing ct", "", ""},
		{"content-encoding gzip", "application/json", "gzip"},
	} {
		resp, _ = postProxy(tc.ct, tc.ce, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"denied"}}`)
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("C1 header bypass (%s): status=%d, want 415", tc.name, resp.StatusCode)
		}
	}

	// C1e: Missing or wrong jsonrpc version, trailing garbage -> rejected 400!
	for _, payload := range []string{
		`{"id":1,"method":"tools/call","params":{"name":"denied"}}`,
		`{"jsonrpc":"1.0","id":1,"method":"tools/call","params":{"name":"denied"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"} trailing`,
	} {
		resp, _ = postProxy("application/json", "", payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("C1 invalid jsonrpc (%s): status=%d, want 400", payload, resp.StatusCode)
		}
	}

	// C2: Case-variant & duplicate keys MUST fail closed on BOTH proxy AND StdioBridge!
	c2Payloads := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"denied","NAME":"allowed"}}`,
		`{"jsonrpc":"2.0","id":1,"Method":"ping","method":"tools/call","params":{"name":"denied"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"allowed","name":"denied"}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","PARAMS":{"name":"allowed"},"params":{"name":"denied"}}`,
	}
	for _, payload := range c2Payloads {
		resp, _ = postProxy("application/json", "", payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("C2 proxy bypass (%s): status=%d, want 400", payload, resp.StatusCode)
		}
	}

	// Tool pattern with '/' and '[...]' character class MUST deny fs/delete_3!
	resp, body = postProxy("application/json", "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fs/delete_3"}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "-32601") {
		t.Fatalf("slash+bracket deny pattern failed: status=%d body=%s", resp.StatusCode, body)
	}

	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("blocked C1/C2 requests leaked to upstream (%d hits)", got)
	}

	// Legitimate call with uppercase keys inside params.arguments MUST succeed!
	resp, _ = postProxy("application/json", "", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fs/delete_a","arguments":{"Name":"Alice","ID":42}}}`)
	if resp.StatusCode != http.StatusOK || upstreamHits.Load() != 1 {
		t.Fatalf("legitimate call with uppercase arguments failed: status=%d hits=%d", resp.StatusCode, upstreamHits.Load())
	}

	// Verify C1+C2 on StdioBridge as well (without waking sleeping child!).
	bridge := newTestStdioBridge(t, nil)
	filter := ToolFilter{DenyTools: []string{"denied*"}}
	for _, payload := range c2Payloads {
		rec := doBridgePOST(t, bridge, filter, payload)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("C2 bridge bypass (%s): status=%d body=%s", payload, rec.Code, rec.Body.String())
		}
	}
	// Malformed Content-Type on filtered StdioBridge -> 415!
	reqBadCT := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"denied"}}`))
	reqBadCT.Header.Set("Content-Type", "application/json; charset")
	recBadCT := httptest.NewRecorder()
	bridge.ServeHTTPWithFilter(recBadCT, reqBadCT, filter)
	if recBadCT.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bridge malformed Content-Type status=%d, want 415", recBadCT.Code)
	}
	if bridge.State() != "sleeping" {
		t.Fatalf("bridge woke up during blocked C1/C2 probes! state=%s", bridge.State())
	}
}

func TestProxy_M1_ToolsListFilterFailClosedAndIgnore1xx(t *testing.T) {
	// The interim status must not commit the final response. Malformed representations fail closed.
	var sendMode atomic.Int32 // 0=bad JSON, 1=bad SSE
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusEarlyHints)
		if sendMode.Load() == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"result\":{\"tools\":[{\"name\":\"secret_leak\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"secret_leak"`)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	tokPath := filepath.Join(dir, "tok")
	_ = os.WriteFile(tokPath, []byte("m1-secret-token-1234"), 0o600)
	ps := NewProxyServer(ProxyConfig{
		Name:       "m1-proxy",
		ListenAddr: "127.0.0.1:0",
		TargetURL:  upstream.URL,
		Tokens:     []RouteTokenConfig{{Name: "ro", TokenFile: tokPath, ReadOnlyOnly: true}},
	}, nil)
	if err := ps.Start(); err != nil {
		t.Fatal(err)
	}
	defer ps.Shutdown(context.Background())

	client := NewDirectClient(5 * time.Second)
	for mode, label := range []string{"malformed JSON", "malformed SSE"} {
		sendMode.Store(int32(mode))
		req, _ := http.NewRequest(http.MethodPost, "http://"+ps.ListenAddr()+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer m1-secret-token-1234")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(body), "tools_list_filter_failed") {
			t.Fatalf("%s: expected 502 tools_list_filter_failed, got %d: %s", label, resp.StatusCode, string(body))
		}
		if strings.Contains(string(body), "secret_leak") {
			t.Fatalf("%s leaked unfiltered tool name in error response!", label)
		}
	}
}

func TestStdioBridge_H1_WedgedChildDoesNotDeadlockStopOrTimeout(t *testing.T) {
	b := NewStdioBridge(StdioBridgeOptions{
		Name:           "wedged-bridge",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestHelperRegressionBridge$"},
		Env:            map[string]string{"GO_WANT_REGRESSION_HELPER": "1", "REGRESSION_MODE": "wedge_stdin"},
		IdleTimeout:    0,
		RequestTimeout: 350 * time.Millisecond,
		InitTimeout:    5 * time.Second,
		StderrWriter:   io.Discard,
	})
	defer b.Shutdown()

	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start wedged helper: %v", err)
	}
	// Give child a moment to read notifications/initialized and enter sleep (stop reading stdin).
	time.Sleep(100 * time.Millisecond)

	// Send a 512 KiB request that exceeds the Windows/Unix pipe buffer so stdin.Write blocks!
	hugeText := strings.Repeat("W", 512*1024)
	payload := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"blob":"%s"}}}`, hugeText)

	start := time.Now()
	rec := doBridgePOST(t, b, ToolFilter{}, payload)
	elapsed := time.Since(start)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("wedged stdin POST status=%d want 504 (body=%s)", rec.Code, rec.Body.String())
	}
	if elapsed > 3*time.Second {
		t.Fatalf("RequestTimeout not honored on wedged stdin write: took %s", elapsed)
	}

	// Stop and Shutdown MUST return promptly without deadlocking on writeMu!
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	if err := b.Stop(stopCtx); err != nil {
		t.Fatalf("Stop hung/failed on wedged child: %v", err)
	}
	shutDone := make(chan struct{})
	go func() {
		b.Shutdown()
		close(shutDone)
	}()
	select {
	case <-shutDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown hung on wedged child!")
	}
}

func TestStdioBridge_M4_CircuitBreakerAndL1L2(t *testing.T) {
	// 1. M4: Crash-loop circuit breaker trips to "degraded", blocks auto-respawn, exposes LastError,
	// and resets cleanly on explicit Restart/Start.
	b := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.MaxConsecutiveFailures = 2
		o.RetryBackoff = []time.Duration{15 * time.Millisecond}
	})

	for i := 1; i <= 2; i++ {
		rec := doBridgePOST(t, b, ToolFilter{}, fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"crash_now"}}`, i))
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("crash %d status=%d want 502", i, rec.Code)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			st := b.State()
			if st == "sleeping" || st == "degraded" {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
	}

	if got := b.State(); got != "degraded" {
		t.Fatalf("expected degraded after 2 consecutive crashes, got %q", got)
	}
	if sum := b.HealthSummary(); sum.LastError == "" || sum.Healthy {
		t.Fatalf("expected unhealthy HealthSummary with LastError, got %+v", sum)
	}

	// Auto-respawn MUST be blocked while degraded!
	blocked := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if blocked.Code != http.StatusServiceUnavailable || !strings.Contains(blocked.Body.String(), "service_degraded") {
		t.Fatalf("expected 503 service_degraded while breaker open, got %d: %s", blocked.Code, blocked.Body.String())
	}

	// Explicit Start clears degraded state and recovers!
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start after degraded: %v", err)
	}
	if b.State() != "running" {
		t.Fatalf("expected running after Start, got %q", b.State())
	}

	// 2. L2: client response (id + result) -> 202; missing method with id -> 400 + JSON-RPC -32600 with matching id!
	respClient := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"resp-1","result":{}}`)
	if respClient.Code != http.StatusAccepted {
		t.Fatalf("client response object status=%d want 202", respClient.Code)
	}
	badNoMethod := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"bad-7"}`)
	if badNoMethod.Code != http.StatusBadRequest || !strings.Contains(badNoMethod.Body.String(), "-32600") || !strings.Contains(badNoMethod.Body.String(), `"bad-7"`) {
		t.Fatalf("missing method with id: status=%d body=%s", badNoMethod.Code, badNoMethod.Body.String())
	}

	// 3. L1: server-initiated ping answered with result:{} (not -32601)!
	bPing := NewStdioBridge(StdioBridgeOptions{
		Name:           "ping-bridge",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestHelperRegressionBridge$"},
		Env:            map[string]string{"GO_WANT_REGRESSION_HELPER": "1", "REGRESSION_MODE": "server_ping"},
		RequestTimeout: 5 * time.Second,
		InitTimeout:    5 * time.Second,
		StderrWriter:   io.Discard,
	})
	defer bPing.Shutdown()

	pingRec := doBridgePOST(t, bPing, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"check_ping"}}`)
	if pingRec.Code != http.StatusOK || !strings.Contains(pingRec.Body.String(), `"pingOK":true`) {
		t.Fatalf("server-initiated ping did not get empty result {}: status=%d body=%s", pingRec.Code, pingRec.Body.String())
	}
}

func TestStdioBridge_M5_IdleReaperWaitsForDyingProcBeforeRespawn(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "exclusive.lock")
	b := NewStdioBridge(StdioBridgeOptions{
		Name:           "lock-bridge",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestHelperRegressionBridge$"},
		Env:            map[string]string{"GO_WANT_REGRESSION_HELPER": "1", "REGRESSION_MODE": "slow_exit_lock", "REGRESSION_LOCK_PATH": lockPath},
		IdleTimeout:    120 * time.Millisecond,
		RequestTimeout: 5 * time.Second,
		InitTimeout:    5 * time.Second,
		StderrWriter:   io.Discard,
	})
	defer b.Shutdown()

	// Wake child 1 (acquires exclusive.lock).
	r1 := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ok"}}`)
	if r1.Code != http.StatusOK {
		t.Fatalf("first call failed: %d %s", r1.Code, r1.Body.String())
	}

	// Poll until idle reaper transitions bridge to sleeping (child 1 is still alive holding lock for 250ms!).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.State() == "sleeping" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if b.State() != "sleeping" {
		t.Fatalf("expected sleeping after idle timeout, got %s", b.State())
	}

	// Immediately send request 2 while child 1 is still winding down:
	// M5 ensures child 2 waits for child 1's waitDone so exclusive.lock NEVER conflicts!
	r2 := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ok"}}`)
	if r2.Code != http.StatusOK {
		t.Fatalf("second call after idle sleep failed (lock overlap?): status=%d body=%s", r2.Code, r2.Body.String())
	}
}

func TestServiceRunner_M2_StoppingStateAndM3_StartTimedOutFastFail(t *testing.T) {
	// 1. M2: State transitions to "stopping" BEFORE stopProcess, and EnsureRunning wakes cleanly instead of returning nil.
	shutdownEntered := make(chan struct{})
	releaseShutdown := make(chan struct{})
	var shutdownCalled atomic.Int32
	ctrlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/shutdown":
			if shutdownCalled.Add(1) == 1 {
				close(shutdownEntered)
				<-releaseShutdown
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer ctrlSrv.Close()

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := NewServiceRunner(ServiceConfig{
		Name:            "m2-svc",
		StartMode:       "eager",
		Command:         exe,
		Args:            []string{"-test.run=^TestHelperProcess$"},
		Env:             map[string]string{"GO_WANT_HELPER_PROCESS": "1", "HELPER_MODE": "sleep"},
		HealthURL:       ctrlSrv.URL + "/health",
		ShutdownURL:     ctrlSrv.URL + "/shutdown",
		StartTimeoutSec: 5,
	}, t.TempDir(), DefaultLogMaxBytes, nil)
	runner.shutdownTimeout = 2 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		cancel()
		runner.Wait()
	}()
	runner.Launch(ctx)

	if err := runner.EnsureRunning(context.Background()); err != nil {
		t.Fatalf("initial EnsureRunning: %v", err)
	}

	// Trigger Sleep -> enters stopProcess -> calls /shutdown which blocks until releaseShutdown.
	runner.Sleep()
	select {
	case <-shutdownEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for /shutdown to begin")
	}

	if st := runner.State(); st != "stopping" {
		t.Fatalf("expected state=stopping during stopProcess, got %q", st)
	}

	// Concurrent EnsureRunning MUST block/queue wake while stopping (NOT return nil immediately!).
	wakeDone := make(chan error, 1)
	go func() {
		wakeDone <- runner.EnsureRunning(context.Background())
	}()
	select {
	case err := <-wakeDone:
		t.Fatalf("EnsureRunning returned prematurely while state=stopping: err=%v", err)
	case <-time.After(120 * time.Millisecond):
	}

	// Unblock shutdown -> runner sleeps -> immediately processes queued wake -> reaches running!
	close(releaseShutdown)
	select {
	case err := <-wakeDone:
		if err != nil {
			t.Fatalf("EnsureRunning after stopping failed: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("timed out waiting for EnsureRunning after stopping")
	}
	if runner.State() != "running" {
		t.Fatalf("expected running after wake, got %q", runner.State())
	}

	// 2. M3: Once startTimeoutC fires on an unhealthy child, subsequent EnsureRunning calls fast-fail immediately.
	unhealthySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer unhealthySrv.Close()

	runner2 := NewServiceRunner(ServiceConfig{
		Name:      "m3-svc",
		StartMode: "eager",
		Command:   exe,
		Args:      []string{"-test.run=^TestHelperProcess$"},
		Env:       map[string]string{"GO_WANT_HELPER_PROCESS": "1", "HELPER_MODE": "sleep"},
		HealthURL: unhealthySrv.URL,
	}, t.TempDir(), DefaultLogMaxBytes, nil)
	runner2.startTimeout = 200 * time.Millisecond

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer func() {
		cancel2()
		runner2.Wait()
	}()
	runner2.Launch(ctx2)

	// First EnsureRunning hits startTimeout (~200ms).
	wCtx1, wCancel1 := context.WithTimeout(context.Background(), 3*time.Second)
	err1 := runner2.EnsureRunning(wCtx1)
	wCancel1()
	if err1 == nil {
		t.Fatal("expected first EnsureRunning to fail on startTimeout")
	}

	// Second EnsureRunning with a 5s deadline MUST fail immediately (<250ms), NOT wait 5s!
	start := time.Now()
	wCtx2, wCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	err2 := runner2.EnsureRunning(wCtx2)
	wCancel2()
	if err2 == nil {
		t.Fatal("expected second EnsureRunning to fast-fail after startTimedOut")
	}
	if dur := time.Since(start); dur > 500*time.Millisecond {
		t.Fatalf("second EnsureRunning did not fast-fail (took %s, err=%v)", dur, err2)
	}
}

type failingWriter struct {
	calls atomic.Int32
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.calls.Add(1)
	return 0, errors.New("disk full")
}

func TestRedact_M3_BestEffortWriterBoundaryOverlapAndPasswordParam(t *testing.T) {
	// 1. bestEffortWriter swallows write errors and invokes onErr exactly once.
	fw := &failingWriter{}
	var errCount atomic.Int32
	bw := newBestEffortWriter(fw, func(error) { errCount.Add(1) })
	for i := 0; i < 3; i++ {
		n, err := bw.Write([]byte("hello"))
		if err != nil || n != 5 {
			t.Fatalf("bestEffortWriter Write %d: n=%d err=%v", i, n, err)
		}
	}
	if errCount.Load() != 1 {
		t.Fatalf("onErr called %d times, want 1", errCount.Load())
	}

	// 2. password / passwd query param scrubbing.
	r := NewRedactor()
	r.SetSecrets([]string{"BOUNDARY_SECRET_TOKEN_987654"})
	got := r.Redact("http://127.0.0.1/?password=mySecretPass1&passwd=mySecretPass2&ok=1")
	if strings.Contains(got, "mySecretPass1") || strings.Contains(got, "mySecretPass2") {
		t.Fatalf("password/passwd query param leaked: %q", got)
	}

	// 3. Oversized lines are discarded without leaking or blocking subsequent lines.
	var dst bytes.Buffer
	rw := r.NewWriter(&dst)
	secret := "BOUNDARY_SECRET_TOKEN_987654"
	for _, offset := range []int{redactorMaxBufferSize - 10, redactorMaxBufferSize + 246} {
		dst.Reset()
		if _, err := io.WriteString(rw, strings.Repeat("\x80", offset)); err != nil {
			t.Fatalf("write long prefix: %v", err)
		}
		if _, err := io.WriteString(rw, secret+"\nnext line\n"); err != nil {
			t.Fatalf("write remainder: %v", err)
		}
		if got := dst.String(); got != overlongLogLine+"next line\n" {
			t.Fatalf("unexpected output at offset %d: %d bytes", offset, len(got))
		}
	}

	// 4. Control API redacts secrets in error strings.
	h := NewControlHandler("ctrl-tok", "127.0.0.1:18470", ControlHooks{
		Redactor: r,
		StartService: func(ctx context.Context, name string) (ServiceStatus, bool, error) {
			return ServiceStatus{Name: name}, true, errors.New("boom BOUNDARY_SECRET_TOKEN_987654 Bearer tok_secret_999")
		},
	})
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18470/start/demo", nil)
	req.Header.Set("Authorization", "Bearer ctrl-tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "BOUNDARY_SECRET_TOKEN_987654") || strings.Contains(rec.Body.String(), "tok_secret_999") {
		t.Fatalf("control API error response leaked unredacted secret: %s", rec.Body.String())
	}
}
