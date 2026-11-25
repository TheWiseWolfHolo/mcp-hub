package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

type fakeHub struct {
	ts          *httptest.Server
	addr        string
	dir         string
	cfgPath     string
	tokenPath   string
	store       *hub.TokenStore
	stopped     atomic.Bool
	restartWait atomic.Bool
	reloads     atomic.Int32
	mu          sync.Mutex
	actions     []string
	catalog     []hub.CatalogEntry
}

func (f *fakeHub) record(s string) {
	f.mu.Lock()
	f.actions = append(f.actions, s)
	f.mu.Unlock()
}

func (f *fakeHub) actionList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

var fakeServices = map[string]bool{"alpha": true, "beta": true, "gamma": true}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{dir: t.TempDir(), store: hub.NewTokenStore()}
	f.tokenPath = filepath.Join(f.dir, "secrets", "control.token")
	writeFile(t, f.tokenPath, testToken)
	if _, err := f.store.LoadTokenFileWithGrace("control", f.tokenPath, nil, nil, false); err != nil {
		t.Fatal(err)
	}

	f.ts = httptest.NewUnstartedServer(nil)
	f.addr = f.ts.Listener.Addr().String()

	ring := func(lines ...string) *hub.RingLog {
		r := hub.NewRingLog(0, 0)
		for _, l := range lines {
			r.Append("stderr", l)
		}
		return r
	}
	svcStatus := func(name, state string) hub.ServiceStatus {
		return hub.ServiceStatus{Name: name, State: state, Running: state == "running", Healthy: state == "running", PID: 4242}
	}
	hooks := hub.ControlHooks{
		Healthz: func() hub.HealthzResponse {
			return hub.HealthzResponse{OK: true, Services: map[string]hub.ServiceHealthSummary{
				"alpha": {Running: true, Healthy: true, PID: 4242, Port: 18000, State: "running", Mode: "http"},
				"beta":  {State: "sleeping", Mode: "stdio-bridge"},
			}, Proxies: map[string]bool{"alpha-proxy": true}}
		},
		Status: func() hub.StatusResponse {
			return hub.StatusResponse{
				OK: true, Version: hub.Version, PID: 99, UptimeSec: 3725, ControlAddr: f.addr,
				ConfigPath: f.cfgPath, LogDir: filepath.Join(f.dir, "logs"), TrayEnabled: true,
				TreeTotalPrivateMB: 12.5, TreeTotalWorkingSetMB: 20.25,
				Services: map[string]hub.ServiceStatus{
					"alpha": {Name: "alpha", Mode: "http", State: "running", Running: true, Healthy: true, PID: 4242, ProcessCount: 3, TreePrivateMB: 10.5,
						Metrics: &hub.MCPMetricsSnapshot{TotalRequests: 7, ActiveSSE: 1}},
					"beta":  {Name: "beta", Mode: "stdio-bridge", State: "sleeping"},
					"gamma": {Name: "gamma", Mode: "http", State: "degraded", LastError: "exit status 1", CrashTail: []string{"boom: crash detail"}},
				},
				Proxies: map[string]hub.ProxyStatus{
					"alpha-proxy": {Name: "alpha-proxy", ListenAddr: "127.0.0.1:1", TargetURL: "http://127.0.0.1:18000", Running: false},
				},
			}
		},
		Catalog: func() hub.CatalogResponse {
			return hub.CatalogResponse{OK: true, Entries: f.catalog}
		},
		GetRingLog: func(name string) (*hub.RingLog, bool) {
			switch name {
			case "alpha":
				return ring("alpha line 1", "alpha line 2"), true
			case "hub":
				return ring("hub started"), true
			}
			return nil, false
		},
		StartService: func(ctx context.Context, name string) (hub.ServiceStatus, bool, error) {
			f.record("start:" + name)
			return svcStatus(name, "running"), fakeServices[name], nil
		},
		StopService: func(ctx context.Context, name string) (hub.ServiceStatus, bool, error) {
			f.record("stop:" + name)
			return svcStatus(name, "stopped"), fakeServices[name], nil
		},
		Restart: func(ctx context.Context, name string, waitHealthy bool) (hub.ServiceStatus, bool, error) {
			f.record("restart:" + name)
			f.restartWait.Store(waitHealthy)
			return svcStatus(name, "running"), fakeServices[name], nil
		},
		ReloadHub: func(ctx context.Context) (hub.ReloadHubResponse, error) {
			f.reloads.Add(1)
			if _, err := f.store.LoadTokenFileWithGrace("control", f.tokenPath, nil, nil, false); err != nil {
				return hub.ReloadHubResponse{}, err
			}
			return hub.ReloadHubResponse{OK: true, AddedServices: []string{"delta"}, UnchangedServices: []string{"alpha"}}, nil
		},
		ReloadService: func(ctx context.Context, name string) (json.RawMessage, bool, error) {
			switch name {
			case "alpha":
				return json.RawMessage(`{"reloaded":true}`), true, nil
			case "beta":
				return nil, true, hub.ErrReloadNotSupported
			}
			return nil, false, nil
		},
		Stop: func() { f.stopped.Store(true) },
	}
	f.ts.Config.Handler = hub.NewControlHandlerWithStore(f.store, func() bool { return false }, f.addr, hooks)
	f.ts.Start()
	t.Cleanup(f.ts.Close)

	f.cfgPath = filepath.Join(f.dir, "config.json")
	writeFile(t, f.cfgPath, fmt.Sprintf(`{
  "controlAddr": %q,
  "controlTokenFile": %q,
  "logDir": %q,
  "services": [],
  "proxies": []
}`, f.addr, filepath.ToSlash(f.tokenPath), filepath.ToSlash(filepath.Join(f.dir, "logs"))))
	return f
}

func (f *fakeHub) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLI(t, append([]string{"--config", f.cfgPath}, args...)...)
}

func decodeJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("stdout is not clean JSON: %v\n%s", err, s)
	}
	return m
}

func errorCode(t *testing.T, s string) string {
	t.Helper()
	m := decodeJSON(t, s)
	if ok, _ := m["ok"].(bool); ok {
		t.Fatalf("expected ok=false, got %s", s)
	}
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("missing error object: %s", s)
	}
	if msg, _ := e["message"].(string); msg == "" {
		t.Fatalf("missing error.message: %s", s)
	}
	c, _ := e["code"].(string)
	return c
}

// ---------------------------------------------------------------------------

func TestParseArgsGlobalFlagsAnywhere(t *testing.T) {
	opts, pos, err := parseArgs([]string{"status", "--json", "-c", "x.json", "--timeout", "5s", "--watch", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pos) != 1 || pos[0] != "status" || !opts.asJSON || opts.configPath != "x.json" || !opts.configExplicit ||
		opts.timeout != 5*time.Second || !opts.watch || opts.watchInterval != 3*time.Second {
		t.Fatalf("unexpected parse: %+v %v", opts, pos)
	}

	opts, pos, err = parseArgs([]string{"logs", "alpha", "-n", "10", "-f", "--addr=127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, " ") != "logs alpha" || opts.lines != 10 || !opts.follow || opts.addr != "127.0.0.1:1" || !opts.addrSet {
		t.Fatalf("unexpected parse: %+v %v", opts, pos)
	}

	opts, _, err = parseArgs([]string{"status", "--watch"})
	if err != nil || opts.watchInterval != defaultWatchInterval {
		t.Fatalf("bare --watch: %v %v", err, opts.watchInterval)
	}

	if _, _, err := parseArgs([]string{"status", "--bogus"}); err == nil {
		t.Fatal("expected unknown flag error")
	}
	if code, _, _ := runCLI(t, "status", "--all"); code != exitUsage {
		t.Fatalf("foreign flag should be usage error, got %d", code)
	}
	if code, _, _ := runCLI(t, "--timeout", "nope", "health"); code != exitUsage {
		t.Fatalf("bad timeout should be usage error, got %d", code)
	}
}

func TestAddrNonLoopbackRejected(t *testing.T) {
	for _, addr := range []string{"10.1.2.3:18470", "example.com:18470", "0.0.0.0:18470", "https://127.0.0.1:18470"} {
		code, _, stderr := runCLI(t, "--addr", addr, "--token-file", "nope", "status")
		if code != exitUsage {
			t.Fatalf("--addr %s: exit %d (stderr %s)", addr, code, stderr)
		}
	}
	code, out, _ := runCLI(t, "--json", "--addr", "192.168.1.5:18470", "health")
	if code != exitUsage || errorCode(t, out) != "non_loopback_addr" {
		t.Fatalf("json non-loopback: %d %s", code, out)
	}
	for _, ok := range []string{"127.0.0.1:18470", "localhost:18470", "[::1]:18470", "http://127.0.0.1:18470/"} {
		if _, err := normalizeControlAddr(ok); err != nil {
			t.Fatalf("%s should be accepted: %v", ok, err)
		}
	}
}

func TestNoCrossTargetTokenLeak(t *testing.T) {
	f := newFakeHub(t)
	var gotAuth atomic.Value
	gotAuth.Store("unset")
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer other.Close()
	otherAddr := strings.TrimPrefix(other.URL, "http://")

	code, _, _ := f.run(t, "--addr", otherAddr, "status")
	if code != exitAuth {
		t.Fatalf("expected exit 4 from foreign target, got %d", code)
	}
	if v := gotAuth.Load().(string); v != "" {
		t.Fatalf("config control token leaked to a different --addr: Authorization=%q", v)
	}

	// Explicit --token-file is honoured.
	code, _, _ = f.run(t, "--addr", otherAddr, "--token-file", f.tokenPath, "status")
	if code != exitAuth || gotAuth.Load().(string) == "" {
		t.Fatalf("explicit --token-file should be sent: code=%d", code)
	}

	// An override for the configured endpoint keeps its token.
	code, out, stderr := f.run(t, "--addr", f.addr, "status")
	if code != exitOK || !strings.Contains(out, "SERVICES") {
		t.Fatalf("configured addr should authenticate: %d %s %s", code, out, stderr)
	}
	// A hostname and an IP can reach different listeners on the same port.
	_, port, _ := net.SplitHostPort(f.addr)
	for _, host := range []string{"localhost", "127.0.0.2", "::1"} {
		if sameBind(f.addr, net.JoinHostPort(host, port)) {
			t.Fatalf("distinct address %s inherited the configured endpoint identity", host)
		}
	}
}

func TestBareStopRefused(t *testing.T) {
	f := newFakeHub(t)
	code, out, stderr := f.run(t, "stop")
	if code != exitUsage || !strings.Contains(stderr, "shutdown") || !strings.Contains(stderr, "stop --all") {
		t.Fatalf("bare stop: %d out=%q err=%q", code, out, stderr)
	}
	if f.stopped.Load() {
		t.Fatal("bare stop must not stop the hub")
	}
	code, out, _ = f.run(t, "--json", "stop")
	if code != exitUsage || errorCode(t, out) != "usage" {
		t.Fatalf("json bare stop: %d %s", code, out)
	}
}

func TestJSONErrorEnvelope(t *testing.T) {
	code, out, stderr := runCLI(t, "--json", "restart")
	if code != exitUsage || errorCode(t, out) != "usage" || stderr != "" {
		t.Fatalf("restart without args: %d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "--json", "frobnicate")
	if code != exitUsage || errorCode(t, out) != "usage" || stderr != "" {
		t.Fatalf("unknown command: %d %q %q", code, out, stderr)
	}
	code, out, _ = runCLI(t, "--bogus", "--json")
	if code != exitUsage || errorCode(t, out) != "usage" {
		t.Fatalf("unknown flag: %d %q", code, out)
	}
	code, out, stderr = runCLI(t, "--bogus", "--json=True")
	if code != exitUsage || errorCode(t, out) != "usage" || stderr != "" {
		t.Fatalf("boolean spelling on usage error: %d out=%q err=%q", code, out, stderr)
	}
	for _, flag := range []string{"--help", "--version"} {
		code, out, stderr = runCLI(t, flag, "--json")
		if code != exitOK || decodeJSON(t, out)["ok"] != true || stderr != "" {
			t.Fatalf("%s before JSON: %d out=%q err=%q", flag, code, out, stderr)
		}
	}
	code, out, stderr = runCLI(t, "--json", "--addr", closedAddr(t), "--timeout", "3s", "health")
	if code != exitUnreachable || errorCode(t, out) != "hub_unreachable" || stderr != "" {
		t.Fatalf("unreachable: %d %q %q", code, out, stderr)
	}
	code, out, _ = runCLI(t, "--json", "--config", filepath.Join(t.TempDir(), "missing.json"), "status")
	if code != exitUsage || errorCode(t, out) != "invalid_config" {
		t.Fatalf("missing explicit config: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "--json", "version")
	if m := decodeJSON(t, out); code != 0 || m["ok"] != true || m["version"] != hub.Version {
		t.Fatalf("version json: %d %q", code, out)
	}

	f := newFakeHub(t)
	code, out, _ = f.run(t, "--json", "start", "nope")
	if code != exitNotFound || errorCode(t, out) != "service_not_found" {
		t.Fatalf("404: %d %q", code, out)
	}
	code, out, _ = f.run(t, "--json", "--token-file", writeTemp(t, "wrong-token-value-1234567890abcdef"), "status")
	if code != exitAuth || errorCode(t, out) != "unauthorized" {
		t.Fatalf("401: %d %q", code, out)
	}
	code, out, _ = f.run(t, "--json", "reload", "beta")
	if code != exitUsage || errorCode(t, out) != "reload_not_supported" {
		t.Fatalf("400: %d %q", code, out)
	}
}

func TestWatchJSONTerminalErrorUsesSingleLine(t *testing.T) {
	f := newFakeHub(t)
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer ts.Close()
	code, out, stderr := f.run(t, "--addr", ts.Listener.Addr().String(), "status", "--watch=1ms", "--json")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != exitAuth || stderr != "" || len(lines) != 2 || decodeJSON(t, lines[0])["ok"] != true ||
		errorCode(t, lines[1]) != "unauthorized" {
		t.Fatalf("watch output: %d stdout=%q stderr=%q", code, out, stderr)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tok")
	writeFile(t, p, content)
	return p
}

func TestClassifyExitCodes(t *testing.T) {
	cases := []struct {
		err  error
		exit int
	}{
		{errors.New(`control API GET /status returned HTTP 401: {"error":"unauthorized"}`), exitAuth},
		{errors.New(`control API GET /status returned HTTP 403: {"error":"forbidden_host"}`), exitAuth},
		{errors.New(`control API POST /start/x returned HTTP 404: {"error":"service_not_found","name":"x"}`), exitNotFound},
		{errors.New(`control API POST /reload returned HTTP 500: {"ok":false,"error":"boom"}`), exitRuntime},
		{&net.OpError{Op: "dial", Err: errors.New("refused")}, exitUnreachable},
		{context.DeadlineExceeded, exitUnreachable},
		{errors.New("other"), exitRuntime},
	}
	for _, c := range cases {
		if got := classify(c.err).exit; got != c.exit {
			t.Errorf("%v: exit %d, want %d", c.err, got, c.exit)
		}
	}
}

func TestCommandsAgainstFakeHub(t *testing.T) {
	f := newFakeHub(t)

	code, out, stderr := f.run(t, "status")
	if code != 0 {
		t.Fatalf("status: %d %s", code, stderr)
	}
	for _, want := range []string{"mcp-hub v" + hub.Version, "uptime=1h2m", "tray=on", "SERVICES", "4242(3)", "10.5", "PROXIES", "RECENT ERRORS", "boom: crash detail", "DEGRADED"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	code, out, _ = f.run(t, "status", "--json")
	if m := decodeJSON(t, out); code != 0 || m["version"] != hub.Version {
		t.Fatalf("status json: %d %s", code, out)
	}

	code, out, _ = f.run(t, "health")
	if code != 0 || !strings.Contains(out, "alpha") || !strings.Contains(out, "sleeping") {
		t.Fatalf("health: %d %s", code, out)
	}
	code, out, _ = f.run(t, "healthz", "--json")
	if m := decodeJSON(t, out); code != 0 || m["ok"] != true {
		t.Fatalf("healthz json: %d %s", code, out)
	}

	code, out, _ = f.run(t, "logs", "alpha", "-n", "5")
	if code != 0 || !strings.Contains(out, "alpha line 2") || !strings.Contains(out, "[stderr]") {
		t.Fatalf("logs: %d %s", code, out)
	}
	code, out, _ = f.run(t, "logs", "hub", "--json")
	if m := decodeJSON(t, out); code != 0 || m["name"] != "hub" {
		t.Fatalf("logs hub json: %d %s", code, out)
	}
	if code, _, _ = f.run(t, "logs", "nope"); code != exitNotFound {
		t.Fatalf("logs unknown: %d", code)
	}
	if code, _, _ = f.run(t, "logs", "../etc"); code != exitUsage {
		t.Fatalf("logs traversal: %d", code)
	}

	code, out, _ = f.run(t, "reload")
	if code != 0 || !strings.Contains(out, "added:     delta") || !strings.Contains(out, "unchanged: alpha") {
		t.Fatalf("reload: %d %s", code, out)
	}
	code, out, _ = f.run(t, "reload", "alpha")
	if code != 0 || !strings.Contains(out, `{"reloaded":true}`) {
		t.Fatalf("reload alpha: %d %s", code, out)
	}

	code, out, _ = f.run(t, "start", "alpha")
	if code != 0 || !strings.Contains(out, "started alpha") {
		t.Fatalf("start: %d %s", code, out)
	}
	code, out, _ = f.run(t, "stop", "alpha")
	if code != 0 || !strings.Contains(out, "stopped alpha") {
		t.Fatalf("stop: %d %s", code, out)
	}
	code, out, _ = f.run(t, "restart", "alpha", "--wait")
	if code != 0 || !strings.Contains(out, "restarted alpha") || !f.restartWait.Load() {
		t.Fatalf("restart --wait: %d %s wait=%v", code, out, f.restartWait.Load())
	}
	code, out, _ = f.run(t, "stop", "--all", "--json")
	if m := decodeJSON(t, out); code != 0 || m["ok"] != true {
		t.Fatalf("stop --all: %d %s", code, out)
	}
	acts := strings.Join(f.actionList(), ",")
	for _, want := range []string{"stop:alpha", "stop:beta", "stop:gamma", "restart:alpha", "start:alpha"} {
		if !strings.Contains(acts, want) {
			t.Errorf("missing action %s in %s", want, acts)
		}
	}
	if f.stopped.Load() {
		t.Fatal("service stop must not stop the hub")
	}

	f.catalog = []hub.CatalogEntry{{Name: "alpha", Mode: "http", ClientURL: "http://127.0.0.1:18476/mcp", State: "running",
		Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath}, Capabilities: hub.CatalogCapabilities{Reload: true, LazyWake: true}}}
	code, out, _ = f.run(t, "catalog")
	if code != 0 || !strings.Contains(out, "http://127.0.0.1:18476/mcp") || !strings.Contains(out, "bearer(control.token)") || !strings.Contains(out, "reload,lazy") {
		t.Fatalf("catalog: %d %s", code, out)
	}

	code, out, _ = f.run(t, "shutdown", "--json")
	if m := decodeJSON(t, out); code != 0 || m["stopping"] != true {
		t.Fatalf("shutdown: %d %s", code, out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !f.stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !f.stopped.Load() {
		t.Fatal("shutdown did not reach POST /stop")
	}
}

func TestStopHubAliasAndShutdownWait(t *testing.T) {
	f := newFakeHub(t)
	// Fake Stop hook closes the server, so --wait observes the hub going away.
	f.ts.Config.Handler = wrapStop(f.ts.Config.Handler, f.ts)
	code, out, stderr := f.run(t, "stop", "--hub", "--wait")
	if code != 0 || !strings.Contains(out, "mcp-hub stopped") {
		t.Fatalf("stop --hub --wait: %d %q %q", code, out, stderr)
	}
}

func wrapStop(next http.Handler, ts *httptest.Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		if r.Method == http.MethodPost && r.URL.Path == "/stop" {
			go func() {
				time.Sleep(50 * time.Millisecond)
				ts.Listener.Close()
				ts.CloseClientConnections()
			}()
		}
	})
}

func TestLogsOfflineRedacts(t *testing.T) {
	f := newFakeHub(t)
	logPath := filepath.Join(f.dir, "logs", "alpha.log")
	var b strings.Builder
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	fmt.Fprintf(&b, "leaked token %s here\n", testToken)
	writeFile(t, logPath, b.String())
	code, out, stderr := f.run(t, "logs", "alpha", "--offline", "-n", "3")
	if code != 0 {
		t.Fatalf("offline logs: %d %s", code, stderr)
	}
	if strings.Contains(out, testToken) || !strings.Contains(out, hub.RedactedPlaceholder) {
		t.Fatalf("offline logs not redacted:\n%s", out)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 2 || !strings.Contains(out, "line 19") {
		t.Fatalf("expected last 3 lines:\n%s", out)
	}
	if code, _, _ = f.run(t, "logs", "alpha", "--offline", "-f"); code != exitUsage {
		t.Fatalf("--offline -f should be refused: %d", code)
	}
}

func TestTailFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	var b strings.Builder
	for i := 0; i < 50000; i++ {
		fmt.Fprintf(&b, "row-%05d\r\n", i)
	}
	writeFile(t, p, b.String())
	lines, err := tailFile(p, 4, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, ",") != "row-49996,row-49997,row-49998,row-49999" {
		t.Fatalf("tail: %v", lines)
	}
	writeFile(t, p, "")
	if lines, err := tailFile(p, 4, 1<<20); err != nil || len(lines) != 0 {
		t.Fatalf("empty tail: %v %v", lines, err)
	}
}

func TestInitAndRotateTokenNeverPrintsToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets", "client.token")

	code, out, stderr := runCLI(t, "init-token", path)
	if code != 0 || !strings.Contains(out, "sha256:") {
		t.Fatalf("init-token: %d %q %q", code, out, stderr)
	}
	tok1, err := hub.ReadSecretFile(path)
	if err != nil || len(tok1) != 64 {
		t.Fatalf("token file: %v len=%d", err, len(tok1))
	}
	if strings.Contains(out+stderr, tok1) {
		t.Fatal("init-token printed the raw token")
	}
	if code, _, _ = runCLI(t, "init-token", path); code != exitUsage {
		t.Fatalf("overwrite without --force: %d", code)
	}
	code, out, _ = runCLI(t, "init-token", path, "--force", "--json")
	tok2, _ := hub.ReadSecretFile(path)
	if code != 0 || tok2 == tok1 || strings.Contains(out, tok2) || decodeJSON(t, out)["fingerprint"] == nil {
		t.Fatalf("init-token --force: %d %s", code, out)
	}

	code, out, stderr = runCLI(t, "rotate-token", path, "--stage", "--no-reload")
	if code != 0 {
		t.Fatalf("stage: %d %q %q", code, out, stderr)
	}
	next, err := hub.ReadSecretFile(path + ".next")
	if err != nil || next == tok2 {
		t.Fatalf("staged token: %v", err)
	}
	if strings.Contains(out+stderr, next) || strings.Contains(out+stderr, tok2) {
		t.Fatal("rotate --stage printed a raw token")
	}
	if code, _, _ = runCLI(t, "rotate-token", path, "--stage", "--no-reload"); code != exitUsage {
		t.Fatalf("re-stage without --force: %d", code)
	}

	code, out, stderr = runCLI(t, "rotate-token", path, "--commit", "--no-reload", "--json")
	if code != 0 {
		t.Fatalf("commit: %d %q %q", code, out, stderr)
	}
	cur, _ := hub.ReadSecretFile(path)
	if cur != next {
		t.Fatal("commit did not promote .next")
	}
	if _, err := os.Stat(path + ".next"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(".next not removed after commit")
	}
	if strings.Contains(out+stderr, next) {
		t.Fatal("rotate --commit printed the raw token")
	}
	if code, _, _ = runCLI(t, "rotate-token", path, "--commit", "--no-reload"); code != exitUsage {
		t.Fatalf("commit without .next: %d", code)
	}
	if code, _, _ = runCLI(t, "rotate-token", path); code != exitUsage {
		t.Fatalf("rotate without mode: %d", code)
	}
}

func TestRotateControlTokenWithHubReload(t *testing.T) {
	f := newFakeHub(t)
	code, out, stderr := f.run(t, "rotate-token", f.tokenPath, "--stage")
	if code != 0 || f.reloads.Load() != 1 {
		t.Fatalf("stage with reload: %d %q %q reloads=%d", code, out, stderr, f.reloads.Load())
	}
	next, _ := hub.ReadSecretFile(f.tokenPath + ".next")
	// Grace period: both tokens accepted.
	if code, _, _ = f.run(t, "--token-file", writeTemp(t, next), "status", "--json"); code != 0 {
		t.Fatalf("staged token should be accepted during grace: %d", code)
	}
	if code, _, _ = f.run(t, "status", "--json"); code != 0 {
		t.Fatalf("old token should be accepted during grace: %d", code)
	}

	code, out, stderr = f.run(t, "rotate-token", f.tokenPath, "--commit")
	if code != 0 || !strings.Contains(out, "previous control token now rejected") {
		t.Fatalf("commit: %d %q %q", code, out, stderr)
	}
	if strings.Contains(out+stderr, next) || strings.Contains(out+stderr, testToken) {
		t.Fatal("rotation output contains a raw token")
	}
	if code, _, _ = f.run(t, "--token-file", writeTemp(t, testToken), "status"); code != exitAuth {
		t.Fatalf("old token must be rejected after commit: %d", code)
	}
	if code, _, _ = f.run(t, "status"); code != 0 {
		t.Fatalf("new token must work after commit: %d", code)
	}
}

func TestRotateControlTokenAfterOfflineStage(t *testing.T) {
	f := newFakeHub(t)
	if code, out, stderr := f.run(t, "rotate-token", f.tokenPath, "--stage", "--no-reload"); code != exitOK {
		t.Fatalf("offline stage: %d %q %q", code, out, stderr)
	}
	other := newFakeHub(t)
	if code, out, stderr := f.run(t, "--addr", other.addr, "--token-file", other.tokenPath, "rotate-token", f.tokenPath, "--commit"); code != exitUsage {
		t.Fatalf("wrong control target should be declined: %d %q %q", code, out, stderr)
	}
	if code, out, stderr := f.run(t, "rotate-token", f.tokenPath, "--commit"); code != exitOK || f.reloads.Load() != 2 {
		t.Fatalf("commit should activate an offline stage before promotion: %d %q %q", code, out, stderr)
	}
}

func TestSnippetNeverContainsToken(t *testing.T) {
	f := newFakeHub(t)
	f.catalog = []hub.CatalogEntry{
		{Name: "alpha", Mode: "http", ClientURL: "http://127.0.0.1:18476/mcp", State: "running",
			Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath, Header: "Authorization"}},
		{Name: "open", Mode: "http", ClientURL: "http://127.0.0.1:18486/mcp", State: "running", Auth: hub.CatalogAuthInfo{Type: "none"}},
	}
	code, out, stderr := f.run(t, "snippet", "alpha")
	if code != 0 {
		t.Fatalf("snippet: %d %s", code, stderr)
	}
	if strings.Contains(out, testToken) {
		t.Fatal("snippet printed the raw token")
	}
	for _, want := range []string{"# agy", `"serverUrl"`, "# claude", `"type": "http"`, "# codex", "[mcp_servers.alpha]", "http_headers", "# opencode", `"type": "remote"`, `"oauth": false`, "# devin", "<TOKEN_FROM: " + filepath.ToSlash(f.tokenPath) + ">"} {
		if !strings.Contains(out, want) {
			t.Errorf("snippet missing %q:\n%s", want, out)
		}
	}
	code, out, _ = f.run(t, "snippet", "alpha", "--client", "codex", "--json")
	m := decodeJSON(t, out)
	if code != 0 || strings.Contains(out, testToken) || m["snippets"].(map[string]any)["codex"] == nil {
		t.Fatalf("snippet json: %d %s", code, out)
	}
	code, out, _ = f.run(t, "snippet", "open", "--client", "claude")
	if code != 0 || strings.Contains(out, "headers") {
		t.Fatalf("no-auth snippet should omit headers: %d %s", code, out)
	}
	if code, _, _ = f.run(t, "snippet", "missing"); code != exitNotFound {
		t.Fatalf("snippet missing: %d", code)
	}
	if code, _, _ = f.run(t, "snippet", "alpha", "--client", "vim"); code != exitUsage {
		t.Fatalf("bad client: %d", code)
	}
}

// fakeMCP is a stateful Streamable HTTP MCP server that requires a Bearer token.
type fakeMCP struct {
	ts           *httptest.Server
	deletes      atomic.Int32
	noWake       atomic.Int32
	sleeping     atomic.Bool
	deleteStatus atomic.Int32
}

func newFakeMCP(t *testing.T, token string) *fakeMCP {
	t.Helper()
	m := &fakeMCP{}
	m.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-McpHub-No-Wake") == "1" {
			m.noWake.Add(1)
			if m.sleeping.Load() {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"service_sleeping"}`))
				return
			}
		}
		switch r.Method {
		case http.MethodDelete:
			if r.Header.Get("Mcp-Session-Id") == "sess-1" {
				m.deletes.Add(1)
			}
			if status := m.deleteStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		case http.MethodPost:
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != "sess-1" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"protocolVersion\":\"2025-06-18\",\"serverInfo\":{\"name\":\"fake\"}}}\n\n", req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"zeta"},{"name":"alpha_tool"}]}}`, req.ID)
		default:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"nope"}}`, req.ID)
		}
	}))
	t.Cleanup(m.ts.Close)
	return m
}

func TestCatalogProbeToolsAndProbe(t *testing.T) {
	f := newFakeHub(t)
	mcp := newFakeMCP(t, testToken)
	cfg, err := hub.LoadConfig(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Proxies = []hub.ProxyConfig{{
		Name: "alpha", ListenAddr: mcp.ts.Listener.Addr().String(),
		TargetURL: "http://" + closedAddr(t), TokenFile: f.tokenPath,
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.cfgPath, string(raw))
	f.catalog = []hub.CatalogEntry{
		{Name: "alpha", Mode: "http", ClientURL: mcp.ts.URL + "/mcp", ClientURLs: []string{mcp.ts.URL + "/mcp"}, State: "running",
			Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath, Header: "Authorization"}},
		{Name: "beta", Mode: "stdio-bridge", ClientURL: "http://127.0.0.1:1/s/beta", State: "sleeping",
			Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath}, Capabilities: hub.CatalogCapabilities{StdioBridge: true, LazyWake: true}},
	}

	code, out, stderr := f.run(t, "catalog", "--probe-tools", "--json")
	if code != 0 {
		t.Fatalf("catalog --probe-tools: %d %s %s", code, out, stderr)
	}
	if strings.Contains(out, testToken) {
		t.Fatal("catalog probe printed the raw token")
	}
	m := decodeJSON(t, out)
	entries := m["entries"].([]any)
	alpha := entries[0].(map[string]any)
	if alpha["toolCount"] != float64(2) || fmt.Sprint(alpha["toolNames"]) != "[alpha_tool zeta]" {
		t.Fatalf("tool probe result: %v", alpha)
	}
	if beta := entries[1].(map[string]any); !strings.Contains(fmt.Sprint(beta["probeSkipped"]), "sleeping") {
		t.Fatalf("sleeping entry must be skipped: %v", beta)
	}
	if mcp.deletes.Load() != 1 || mcp.noWake.Load() == 0 {
		t.Fatalf("expected no-wake requests and DELETE, got %d and %d", mcp.noWake.Load(), mcp.deletes.Load())
	}

	code, out, stderr = f.run(t, "probe", "--mcp")
	if code != 0 {
		t.Fatalf("probe --mcp: %d\n%s\n%s", code, out, stderr)
	}
	for _, want := range []string{"hub-healthz", "host-guard", "origin-guard", "control-401", "auth-401", "2 tools", "sleeping (skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("probe output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testToken) {
		t.Fatal("probe printed the raw token")
	}
	if mcp.deletes.Load() != 2 {
		t.Fatalf("probe should DELETE its session too, deletes=%d", mcp.deletes.Load())
	}

	code, out, _ = f.run(t, "doctor", "alpha", "--json")
	if m := decodeJSON(t, out); code != 0 || m["ok"] != true {
		t.Fatalf("doctor json: %d %s", code, out)
	}
	if code, out, _ = f.run(t, "probe", "missing", "--json"); code != exitNotFound || errorCode(t, out) != "service_not_found" {
		t.Fatalf("probe missing: %d %s", code, out)
	}

	mcp.sleeping.Store(true)
	code, out, stderr = f.run(t, "catalog", "--probe-tools", "--json")
	alpha = decodeJSON(t, out)["entries"].([]any)[0].(map[string]any)
	if code != exitOK || !strings.Contains(fmt.Sprint(alpha["probeSkipped"]), "sleeping") {
		t.Fatalf("service sleeping after catalog snapshot: %d %s %s", code, out, stderr)
	}
	mcp.sleeping.Store(false)
	mcp.deleteStatus.Store(http.StatusMethodNotAllowed)
	code, out, stderr = f.run(t, "catalog", "--probe-tools", "--json")
	alpha = decodeJSON(t, out)["entries"].([]any)[0].(map[string]any)
	if code != exitOK || !strings.Contains(fmt.Sprint(alpha["probeError"]), "close probe session: HTTP 405") {
		t.Fatalf("failed session cleanup should be reported: %d %s %s", code, out, stderr)
	}

	// Authentication gets its destination and credential only from local config.
	f.catalog[0].Auth.TokenFile = filepath.Join(f.dir, "not-configured.token")
	prev := mcp.noWake.Load()
	code, out, stderr = f.run(t, "catalog", "--probe-tools", "--json")
	alpha = decodeJSON(t, out)["entries"].([]any)[0].(map[string]any)
	if code != exitOK || alpha["probeSkipped"] == nil || mcp.noWake.Load() != prev {
		t.Fatalf("catalog-provided token path should not be probed: %d %s %s", code, out, stderr)
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "control.token")
	writeFile(t, tok, testToken)
	exe, _ := os.Executable()
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, fmt.Sprintf(`{"controlAddr":"127.0.0.1:18999","controlTokenFile":%q,"logDir":%q,
"services":[{"name":"svc","command":%q,"healthUrl":"tcp://127.0.0.1:18998"}],"proxies":[]}`,
		filepath.ToSlash(tok), filepath.ToSlash(dir), filepath.ToSlash(exe)))
	code, out, stderr := runCLI(t, "--config", good, "validate", "--json")
	if m := decodeJSON(t, out); code != 0 || m["ok"] != true {
		t.Fatalf("validate good: %d %s %s", code, out, stderr)
	}

	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, fmt.Sprintf(`{"controlAddr":"127.0.0.1:18999","controlTokenFile":%q,"logDir":%q,
"services":[{"name":"svc","command":%q,"healthUrl":"tcp://127.0.0.1:18998"}],"proxies":[]}`,
		filepath.ToSlash(tok), filepath.ToSlash(dir), filepath.ToSlash(filepath.Join(dir, "missing.exe"))))
	code, out, _ = runCLI(t, "--config", bad, "validate", "--json")
	if code != exitUsage || errorCode(t, out) != "invalid_config" {
		t.Fatalf("validate bad: %d %s", code, out)
	}
	code, _, stderr = runCLI(t, "--config", bad, "validate")
	if code != exitUsage || !strings.Contains(stderr, "missing.exe") {
		t.Fatalf("validate bad text: %d %s", code, stderr)
	}

	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := hub.ParseConfig(example)
	if err != nil {
		t.Fatalf("config.example.json must parse: %v", err)
	}
	if cfg.Gateway == nil || len(cfg.Gateway.Routes) == 0 || !cfg.ShouldShowTray() {
		t.Fatalf("config.example.json should demonstrate gateway and tray:true")
	}
}

func TestAuditFindsViolations(t *testing.T) {
	dir := t.TempDir()
	control := filepath.Join(dir, "control.token")
	weak := filepath.Join(dir, "weak.token")
	writeFile(t, control, testToken)
	writeFile(t, weak, "short-token-123")
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, fmt.Sprintf(`{"controlAddr":"127.0.0.1:18999","controlTokenFile":%q,"logDir":%q,
"services":[{"name":"svc","command":"node.exe","port":18990,"env":{"OPENAI_API_KEY":"inline-value-should-not-print"}}],
"proxies":[{"name":"p1","listenAddr":"127.0.0.1:18991","targetUrl":"http://127.0.0.1:18990","tokenFile":%q},
           {"name":"p2","listenAddr":"127.0.0.1:18992","targetUrl":"http://127.0.0.1:18993","tokenFile":%q}]}`,
		filepath.ToSlash(control), filepath.ToSlash(filepath.Join(dir, "logs")), filepath.ToSlash(control), filepath.ToSlash(weak)))
	code, out, stderr := runCLI(t, "--config", cfgPath, "audit", "--json")
	if code != exitAudit || errorCode(t, out) != "audit_violation" {
		t.Fatalf("audit: %d %s %s", code, out, stderr)
	}
	for _, want := range []string{"weak-secret", "duplicate-token", "inline-secret", "OPENAI_API_KEY"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit missing %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{testToken, "short-token-123", "inline-value-should-not-print"} {
		if strings.Contains(out, secret) {
			t.Fatalf("audit output leaked a secret value")
		}
	}
}


// ---------------------------------------------------------------------------
// v0.3.0 bug fixes, visual craft, and agent-first subcommand tests
// ---------------------------------------------------------------------------

func TestParseArgsStrictFlagsAndEdgeCases(t *testing.T) {
	badCases := [][]string{
		{"---json", "status"},
		{"-json", "status"},
		{"--c", "x.json", "status"},
		{"--f", "logs", "alpha"},
		{"-c", "-f", "status"},
		{"--config", "--json", "status"},
		{"--watch", "0s", "status"},
		{"--watch=0s", "status"},
		{"--watch", "-2s", "status"},
		{"rotate-token", "tok", "--commit", "--force"},
	}
	for _, args := range badCases {
		code, out, stderr := runCLI(t, append([]string{"--json"}, args...)...)
		if code != exitUsage {
			t.Errorf("args %v: got exit %d, want %d (out=%q err=%q)", args, code, exitUsage, out, stderr)
		}
	}

	// Specifically verify `--watch 0s status` produces the same clear error as `--watch=0s`.
	_, outSpace, _ := runCLI(t, "--json", "--watch", "0s", "status")
	_, outEq, _ := runCLI(t, "--json", "--watch=0s", "status")
	if !strings.Contains(outSpace, "invalid --watch interval") || !strings.Contains(outEq, "invalid --watch interval") {
		t.Fatalf("watch 0s mismatch:\nspace=%s\neq=%s", outSpace, outEq)
	}

	// `--watch status` still uses defaultWatchInterval and preserves positional command.
	opts, pos, err := parseArgs([]string{"--watch", "status"})
	if err != nil || !opts.watch || opts.watchInterval != defaultWatchInterval || strings.Join(pos, ",") != "status" {
		t.Fatalf("--watch status: err=%v opts=%+v pos=%v", err, opts, pos)
	}

	// truncate must never panic for n < 3.
	for _, n := range []int{-5, 0, 1, 2, 3, 4, 10} {
		got := truncate("abcdef", n)
		if n <= 0 && got != "" {
			t.Fatalf("truncate(%d) = %q, want empty", n, got)
		}
		if n > 0 && len([]rune(got)) > n {
			t.Fatalf("truncate(%d) = %q exceeds limit", n, got)
		}
	}
}

func TestTimeoutPropagationShutdownWaitAndPlainStatus(t *testing.T) {
	// Server that ignores /stop so shutdown --wait must time out honoring --timeout (not 10s).
	f := newFakeHub(t)
	start := time.Now()
	code, out, _ := f.run(t, "--json", "--timeout", "180ms", "shutdown", "--wait")
	elapsed := time.Since(start)
	if code != exitRuntime || errorCode(t, out) != "shutdown_timeout" {
		t.Fatalf("shutdown --wait timeout: code=%d out=%s", code, out)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("shutdown --wait did not honor --timeout=180ms (took %v)", elapsed)
	}

	// plainStatus honors canceled context immediately.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := plainStatus(ctx, hub.NewDirectClient(5*time.Second), 5*time.Second, http.MethodGet, "http://"+f.addr+"/healthz", nil); err == nil {
		t.Fatal("plainStatus should fail on canceled context")
	}
}

func TestLocalhostDualStackFallbackAndDirectHTTPBearer(t *testing.T) {
	// Start an IPv4-only listener and confirm localhost:<port> resolves and matches it.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	localhostAddr := net.JoinHostPort("localhost", port)

	dialAddr, err := loopbackDialAddr(localhostAddr)
	if err != nil {
		t.Fatalf("loopbackDialAddr(%s): %v", localhostAddr, err)
	}
	if dialAddr != ln.Addr().String() {
		t.Fatalf("loopbackDialAddr(%s) = %q, want IPv4 listener %q", localhostAddr, dialAddr, ln.Addr().String())
	}
	matchedAddr, ok := probeListenAddr(localhostAddr, ln.Addr().String())
	if !ok || matchedAddr != ln.Addr().String() {
		t.Fatalf("probeListenAddr(%s, %s) = (%q, %v), want (%q, true)", localhostAddr, ln.Addr().String(), matchedAddr, ok, ln.Addr().String())
	}
	// IPv6 advertised address also matches configured localhost on the same port.
	v6Addr := net.JoinHostPort("::1", port)
	matchedV6, ok := probeListenAddr(localhostAddr, v6Addr)
	if !ok || matchedV6 != v6Addr {
		t.Fatalf("probeListenAddr(%s, %s) = (%q, %v), want (%q, true)", localhostAddr, v6Addr, matchedV6, ok, v6Addr)
	}
	// Explicit 127.0.0.1 must NOT match ::1.
	if _, ok := probeListenAddr(ln.Addr().String(), v6Addr); ok {
		t.Fatal("explicit 127.0.0.1 must not match ::1")
	}

	// Direct HTTP Bearer auth inference in probe --mcp and offline snippet.
	f := newFakeHub(t)
	directToken := "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	directTokPath := filepath.Join(f.dir, "secrets", "direct.token")
	writeFile(t, directTokPath, directToken)
	mcp := newFakeMCP(t, directToken)
	_, mcpPortStr, _ := net.SplitHostPort(mcp.ts.Listener.Addr().String())
	mcpPort := 0
	_, _ = fmt.Sscanf(mcpPortStr, "%d", &mcpPort)

	exe, _ := os.Executable()
	cfg, err := hub.LoadConfig(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = []hub.ServiceConfig{{
		Name:      "direct-svc",
		Command:   exe,
		Port:      mcpPort,
		HealthURL: mcp.ts.URL + "/healthz",
		EnvFiles:  map[string]string{"SERVICE_API_TOKEN": directTokPath},
	}}
	raw, _ := json.Marshal(cfg)
	writeFile(t, f.cfgPath, string(raw))

	f.catalog = []hub.CatalogEntry{{
		Name:       "direct-svc",
		Mode:       "http",
		ClientURL:  mcp.ts.URL + "/mcp",
		ClientURLs: []string{mcp.ts.URL + "/mcp"},
		State:      "running",
		Healthy:    true,
		Auth:       hub.CatalogAuthInfo{Type: "bearer", TokenFile: directTokPath, Header: "Authorization"},
	}}

	code, out, stderr := f.run(t, "probe", "direct-svc", "--mcp", "--json")
	if code != exitOK {
		t.Fatalf("direct HTTP probe --mcp failed: code=%d out=%s err=%s", code, out, stderr)
	}
	if strings.Contains(out, directToken) {
		t.Fatal("probe output leaked direct bearer token")
	}
	if !strings.Contains(out, `"2 tools`) {
		t.Fatalf("expected direct HTTP mcp-tools probe to pass with 2 tools:\n%s", out)
	}

	// Offline snippet (pointing at a closed hub address) infers the direct HTTP bearer token file.
	code, out, stderr = runCLI(t, "--config", f.cfgPath, "--addr", closedAddr(t), "--timeout", "1s", "snippet", "direct-svc", "--client", "claude", "--json")
	if code != exitOK {
		t.Fatalf("offline direct snippet failed: code=%d out=%s err=%s", code, out, stderr)
	}
	m := decodeJSON(t, out)
	authObj := m["auth"].(map[string]any)
	if m["source"] != "config" || authObj["type"] != "bearer" || authObj["tokenFile"] != filepath.ToSlash(directTokPath) {
		t.Fatalf("unexpected offline direct snippet auth: %v", m)
	}
	if strings.Contains(out, directToken) {
		t.Fatal("offline snippet leaked raw direct token")
	}
}

func TestProbeAddrWithoutLocalConfigWarns(t *testing.T) {
	f := newFakeHub(t)
	missingCfg := filepath.Join(t.TempDir(), "nonexistent.json")
	t.Setenv("MCP_HUB_CONFIG", missingCfg)
	f.catalog = []hub.CatalogEntry{{
		Name:      "alpha",
		Mode:      "http",
		ClientURL: "http://127.0.0.1:18476/mcp",
		State:     "running",
	}}
	code, out, stderr := runCLI(t, "--addr", f.addr, "--token-file", f.tokenPath, "probe", "--json")
	if code != exitOK {
		t.Fatalf("probe without local config: %d out=%s err=%s", code, out, stderr)
	}
	m := decodeJSON(t, out)
	summary := m["summary"].(map[string]any)
	if summary["warn"].(float64) < 1 || !strings.Contains(out, `"local-config"`) {
		t.Fatalf("expected local-config warning when cfg==nil:\n%s", out)
	}
}

func TestValidateWarningsAlwaysStderrAndJSON(t *testing.T) {
	dir := t.TempDir()
	tok := filepath.Join(dir, "control.token")
	writeFile(t, tok, testToken)
	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, fmt.Sprintf(`{
  "controlAddr": "127.0.0.1:18999",
  "controlTokenFile": %q,
  "logDir": %q,
  "services": [{"name": "svc", "command": "definitely-not-on-path-xyz-99999", "healthUrl": "tcp://127.0.0.1:18998"}],
  "proxies": []
}`, filepath.ToSlash(tok), filepath.ToSlash(dir)))

	code, out, stderr := runCLI(t, "--config", cfgPath, "validate")
	if code != exitOK || !strings.Contains(stderr, "warning:") || strings.Contains(out, "warning:") || !strings.Contains(out, "OK:") {
		t.Fatalf("validate human warnings should go to stderr only: code=%d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = runCLI(t, "--config", cfgPath, "validate", "--json")
	if code != exitOK || stderr != "" {
		t.Fatalf("validate json: code=%d out=%q err=%q", code, out, stderr)
	}
	m := decodeJSON(t, out)
	warns := m["warnings"].([]any)
	if len(warns) == 0 {
		t.Fatalf("validate json missing warnings array: %s", out)
	}
}

func TestAuditSecretsDirDuplicateAllAndInstallationChecks(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets")
	control := filepath.Join(secretsDir, "control.token")
	sharedTok := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	svcATok := filepath.Join(secretsDir, "svc-a.token")
	svcBTok := filepath.Join(secretsDir, "svc-b.token")
	writeFile(t, control, testToken)
	writeFile(t, svcATok, sharedTok)
	writeFile(t, svcBTok, sharedTok)

	cfgPath := filepath.Join(dir, "config.json")
	writeFile(t, cfgPath, fmt.Sprintf(`{
  "controlAddr": "127.0.0.1:18999",
  "controlTokenFile": %q,
  "logDir": %q,
  "services": [
    {"name": "svc-a", "command": "node", "port": 18991, "shutdownTokenFile": %q},
    {"name": "svc-b", "command": "node", "port": 18992, "envFiles": {"API_TOKEN": %q}}
  ],
  "proxies": []
}`, filepath.ToSlash(control), filepath.ToSlash(filepath.Join(dir, "logs")), filepath.ToSlash(svcATok), filepath.ToSlash(svcBTok)))

	code, out, _ := runCLI(t, "--config", cfgPath, "audit", "--json")
	if code != exitAudit {
		t.Fatalf("audit expected exitAudit, got %d: %s", code, out)
	}
	if !strings.Contains(out, "secretsDir") {
		t.Errorf("audit did not inspect secrets/ directory:\n%s", out)
	}
	if !strings.Contains(out, "duplicate-token") {
		t.Errorf("audit missed duplicate secret across non-bearer secret files:\n%s", out)
	}
	if strings.Contains(out, sharedTok) {
		t.Fatal("audit output leaked secret value")
	}

	// Test auditInstallationFrom edge cases: empty binaries, path outside install dir, and valid binary.
	instDir := t.TempDir()
	fakeExe := filepath.Join(instDir, "mcp-hub-ctl.exe")
	writeFile(t, fakeExe, "binary-content-1")
	receiptPath := filepath.Join(instDir, "installation.json")

	// 1. Empty binaries map -> violation.
	writeFile(t, receiptPath, `{"version":"0.3.0","binaries":{}}`)
	findings := auditInstallationFrom(fakeExe)
	if len(findings) != 1 || findings[0].Status != "violation" {
		t.Fatalf("empty binaries map should fail: %+v", findings)
	}

	// 2. Binary path escaping install dir -> violation.
	outside := filepath.Join(filepath.Dir(instDir), "outside.exe")
	writeFile(t, outside, "outside")
	defer os.Remove(outside)
	outsideSum, _ := fileSHA256(outside)
	writeFile(t, receiptPath, fmt.Sprintf(`{"version":"0.3.0","binaries":{"ctl":{"path":%q,"sha256":%q}}}`, outside, outsideSum))
	findings = auditInstallationFrom(fakeExe)
	if len(findings) != 1 || findings[0].Status != "violation" || !strings.Contains(findings[0].Detail, "outside install directory") {
		t.Fatalf("outside binary path should fail: %+v", findings)
	}

	// 3. Valid in-dir binary -> ok.
	validSum, _ := fileSHA256(fakeExe)
	writeFile(t, receiptPath, fmt.Sprintf(`{"version":"0.3.0","binaries":{"ctl":{"path":%q,"sha256":%q}}}`, fakeExe, validSum))
	findings = auditInstallationFrom(fakeExe)
	if len(findings) != 1 || findings[0].Status != "ok" {
		t.Fatalf("valid installation should pass: %+v", findings)
	}
}

func TestVersionWithGitCommit(t *testing.T) {
	orig := hub.GitCommit
	t.Cleanup(func() { hub.GitCommit = orig })

	hub.GitCommit = "deadbee"
	code, out, _ := runCLI(t, "version")
	if code != exitOK || strings.TrimSpace(out) != "mcp-hub-ctl "+hub.Version+" (deadbee)" {
		t.Fatalf("version text with commit: %d %q", code, out)
	}
	code, out, _ = runCLI(t, "version", "--json")
	m := decodeJSON(t, out)
	if code != exitOK || m["gitCommit"] != "deadbee" {
		t.Fatalf("version json with commit: %d %s", code, out)
	}
}

func TestANSIFormatCJKTableAndColorControls(t *testing.T) {
	// CJK & ANSI-aware alignment check.
	st := termStyle{color: true}
	tbl := newColTable(st,
		tableCol{Header: "NAME", Align: alignLeft},
		tableCol{Header: "STATE", Align: alignLeft},
		tableCol{Header: "CALLS", Align: alignRight},
	)
	tbl.addRow("alpha", st.stateBadge("running"), "12")
	tbl.addRow("中文服务", st.stateBadge("sleeping"), "4")

	var buf bytes.Buffer
	tbl.render(&buf)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 table lines, got %d:\n%s", len(lines), buf.String())
	}
	w0, w1, w2 := visibleWidth(lines[0]), visibleWidth(lines[1]), visibleWidth(lines[2])
	if w0 != w1 || w1 != w2 {
		t.Fatalf("misaligned ANSI/CJK table widths: header=%d row1=%d row2=%d\n%s", w0, w1, w2, buf.String())
	}

	// Color detection respects NO_COLOR, CLICOLOR=0, TERM=dumb, and --json even with CLICOLOR_FORCE=1.
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("TERM", "xterm-256color")
	var b bytes.Buffer
	if !newTermStyle(&b, false).color {
		t.Fatal("CLICOLOR_FORCE=1 should enable color for non-json")
	}
	if newTermStyle(&b, true).color {
		t.Fatal("--json must never enable ANSI color")
	}
	t.Setenv("NO_COLOR", "1")
	if newTermStyle(&b, false).color {
		t.Fatal("NO_COLOR=1 must disable color")
	}
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "0")
	if newTermStyle(&b, false).color {
		t.Fatal("CLICOLOR=0 must disable color")
	}
	t.Setenv("CLICOLOR", "")
	t.Setenv("TERM", "dumb")
	if newTermStyle(&b, false).color {
		t.Fatal("TERM=dumb must disable color")
	}

	// formatWatchFrame produces flicker-free home + clear-EOL + clear-EOS without full-screen \x1b[2J.
	frame := string(formatWatchFrame([]byte("line1\nline2\n")))
	if !strings.HasPrefix(frame, "\x1b[H") || strings.Contains(frame, "\x1b[2J") ||
		!strings.Contains(frame, "line1\x1b[K\n") || !strings.HasSuffix(frame, "\x1b[J") {
		t.Fatalf("unexpected watch frame: %q", frame)
	}
}

func TestRicherStatusLogsGrepAndRingHint(t *testing.T) {
	f := newFakeHub(t)
	// Override status with HubPrivateMB, CrashRestarts, WakeCount, LastHealthError, and empty-services case.
	var emptySvcs atomic.Bool
	bigRing := hub.NewRingLog(150, 0)
	for i := 0; i < 140; i++ {
		if i%2 == 0 {
			bigRing.Append("stdout", fmt.Sprintf("INFO entry %d", i))
		} else {
			bigRing.Append("stderr", fmt.Sprintf("ERROR entry %d [REDACTED]", i))
		}
	}
	hooks := hub.ControlHooks{
		Status: func() hub.StatusResponse {
			svcs := map[string]hub.ServiceStatus{}
			if !emptySvcs.Load() {
				svcs["alpha"] = hub.ServiceStatus{
					Name: "alpha", Mode: "http", State: "running", Running: true, Healthy: true,
					PID: 1001, ProcessCount: 2, TreePrivateMB: 18.4, CrashRestarts: 2, WakeCount: 5,
					Metrics: &hub.MCPMetricsSnapshot{TotalRequests: 20, ErrorRequests: 3, ActiveSSE: 1, ActiveSessions: 2},
				}
				svcs["gamma"] = hub.ServiceStatus{
					Name: "gamma", Mode: "http", State: "starting", Running: true, Healthy: false,
					LastHealthError: "connection refused on 127.0.0.1:18099",
				}
			}
			return hub.StatusResponse{
				OK: true, Version: hub.Version, PID: 77, UptimeSec: 90, ControlAddr: f.addr,
				HubPrivateMB: 11.3, TreeTotalPrivateMB: 18.4, TreeTotalWorkingSetMB: 25.0,
				Services: svcs,
			}
		},
		GetRingLog: func(name string) (*hub.RingLog, bool) {
			if name == "alpha" {
				return bigRing, true
			}
			return nil, false
		},
	}
	f.ts.Config.Handler = hub.NewControlHandlerWithStore(f.store, func() bool { return false }, f.addr, hooks)

	code, out, stderr := f.run(t, "status")
	if code != exitOK {
		t.Fatalf("status: %d %s", code, stderr)
	}
	for _, want := range []string{"hub=11.3 MB", "CRASH/WAKE", "ERRS", "SESS", "2/5", "lastHealthError:", "18099"} {
		if !strings.Contains(out, want) {
			t.Errorf("richer status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("piped status output must not contain ANSI escapes:\n%q", out)
	}

	emptySvcs.Store(true)
	code, out, _ = f.run(t, "status")
	if code != exitOK || !strings.Contains(out, "services: (none)") {
		t.Fatalf("empty services status: %d\n%s", code, out)
	}

	// logs --grep filters online logs + prints dim --offline hint when requesting more than ring buffer returned.
	code, out, stderr = f.run(t, "logs", "alpha", "-n", "200", "--grep", "error")
	if code != exitOK {
		t.Fatalf("logs --grep: %d %s", code, stderr)
	}
	if !strings.Contains(out, "ERROR entry") || strings.Contains(out, "INFO entry") {
		t.Fatalf("logs --grep did not filter properly:\n%s", out)
	}
	if !strings.Contains(stderr, "--offline") {
		t.Fatalf("expected --offline hint on stderr when -n > returned ring buffer, got: %q", stderr)
	}

	// Offline logs --grep must not match redacted secret values.
	logPath := filepath.Join(f.dir, "logs", "alpha.log")
	writeFile(t, logPath, "normal line\nsecret "+testToken+" here\nWARN keep me\n")
	code, out, _ = f.run(t, "logs", "alpha", "--offline", "--grep", testToken)
	if code != exitOK || strings.TrimSpace(out) != "" {
		t.Fatalf("offline --grep must not act as oracle on redacted secrets: %d %q", code, out)
	}
	code, out, _ = f.run(t, "logs", "alpha", "--offline", "--grep", "warn")
	if code != exitOK || strings.TrimSpace(out) != "WARN keep me" {
		t.Fatalf("offline --grep warn: %d %q", code, out)
	}
}

func TestStatsAndMetricsAlias(t *testing.T) {
	f := newFakeHub(t)
	hooks := hub.ControlHooks{
		Status: func() hub.StatusResponse {
			return hub.StatusResponse{
				OK: true, Version: hub.Version, ControlAddr: f.addr,
				Services: map[string]hub.ServiceStatus{
					"alpha": {
						Name: "alpha", State: "running", Running: true, Healthy: true,
						Metrics: &hub.MCPMetricsSnapshot{
							Tools: map[string]hub.ToolMetric{
								"search": {Calls: 25, Errors: 1, AvgMs: 42.5, LastCalledAt: "2026-11-24T00:00:00Z"},
								"fetch":  {Calls: 5, Errors: 0, AvgMs: 120.0},
							},
						},
					},
					"beta": {
						Name: "beta", State: "running", Running: true, Healthy: true,
						Metrics: &hub.MCPMetricsSnapshot{
							Tools: map[string]hub.ToolMetric{
								"list": {Calls: 12, Errors: 2, AvgMs: 9.2},
							},
						},
					},
				},
			}
		},
	}
	f.ts.Config.Handler = hub.NewControlHandlerWithStore(f.store, func() bool { return false }, f.addr, hooks)

	code, out, stderr := f.run(t, "metrics")
	if code != exitOK {
		t.Fatalf("metrics: %d %s", code, stderr)
	}
	searchIdx := strings.Index(out, "search")
	listIdx := strings.Index(out, "list")
	fetchIdx := strings.Index(out, "fetch")
	if searchIdx < 0 || listIdx < 0 || fetchIdx < 0 || !(searchIdx < listIdx && listIdx < fetchIdx) {
		t.Fatalf("stats should sort by calls descending (search=25, list=12, fetch=5):\n%s", out)
	}

	code, out, _ = f.run(t, "stats", "alpha", "--json")
	m := decodeJSON(t, out)
	if code != exitOK || m["service"] != "alpha" || m["totalCalls"].(float64) != 30 || m["totalErrors"].(float64) != 1 {
		t.Fatalf("stats alpha json: %d %s", code, out)
	}

	code, out, _ = f.run(t, "stats", "missing", "--json")
	if code != exitNotFound || errorCode(t, out) != "service_not_found" {
		t.Fatalf("stats missing: %d %s", code, out)
	}
}

func TestToolsCommandWithAnnotationsAndSchema(t *testing.T) {
	f := newFakeHub(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s-tools")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","serverInfo":{"name":"demo-srv"}}}`, req.ID)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[
				{"name":"write_doc","description":"Write a doc","annotations":{"readOnlyHint":false},"inputSchema":{"type":"object","properties":{"title":{"type":"string"}}}},
				{"name":"read_doc","description":"Read a doc","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object"}}
			]}}`, req.ID)
		}
	}))
	defer ts.Close()

	cfg, _ := hub.LoadConfig(f.cfgPath)
	cfg.Proxies = []hub.ProxyConfig{{
		Name: "alpha", ListenAddr: ts.Listener.Addr().String(),
		TargetURL: "http://" + closedAddr(t), TokenFile: f.tokenPath,
	}}
	raw, _ := json.Marshal(cfg)
	writeFile(t, f.cfgPath, string(raw))

	f.catalog = []hub.CatalogEntry{
		{Name: "alpha", Mode: "http", ClientURL: ts.URL + "/mcp", State: "running",
			Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath, Header: "Authorization"}},
		{Name: "nap", Mode: "stdio-bridge", ClientURL: ts.URL + "/mcp", State: "sleeping",
			Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath}},
	}

	code, out, stderr := f.run(t, "tools", "alpha", "--schema")
	if code != exitOK {
		t.Fatalf("tools --schema: %d out=%s err=%s", code, out, stderr)
	}
	for _, want := range []string{"read_doc", "read-only", "write_doc", "write", "inputSchema:", `"title"`} {
		if !strings.Contains(out, want) {
			t.Errorf("tools output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, testToken) {
		t.Fatal("tools output leaked raw token")
	}

	code, out, _ = f.run(t, "tools", "alpha", "--json")
	m := decodeJSON(t, out)
	if code != exitOK || m["toolCount"].(float64) != 2 || strings.Contains(out, "inputSchema") {
		t.Fatalf("tools json without --schema should omit inputSchema: %d %s", code, out)
	}
	code, out, _ = f.run(t, "tools", "alpha", "--schema", "--json")
	if code != exitOK || !strings.Contains(out, `"inputSchema"`) {
		t.Fatalf("tools json with --schema should include inputSchema: %d %s", code, out)
	}

	// Sleeping service refuses without --wake.
	code, out, _ = f.run(t, "tools", "nap", "--json")
	if code != exitRuntime || errorCode(t, out) != "service_sleeping" {
		t.Fatalf("sleeping tools without --wake: %d %s", code, out)
	}
}

func TestSleepWakeUIAndBulkActions(t *testing.T) {
	f := newFakeHub(t)
	var slept, woke, startedAll atomic.Bool
	hooks := hub.ControlHooks{
		SleepService: func(ctx context.Context, name string) (hub.ServiceStatus, bool, error) {
			if name != "alpha" {
				return hub.ServiceStatus{}, false, nil
			}
			slept.Store(true)
			return hub.ServiceStatus{Name: name, State: "sleeping"}, true, nil
		},
		WakeService: func(ctx context.Context, name string) (hub.ServiceStatus, bool, error) {
			if name != "alpha" {
				return hub.ServiceStatus{}, false, nil
			}
			woke.Store(true)
			return hub.ServiceStatus{Name: name, State: "running", Running: true, Healthy: true, WakeCount: 1}, true, nil
		},
		StartAll: func(ctx context.Context) map[string]hub.ServiceActionResponse {
			startedAll.Store(true)
			return map[string]hub.ServiceActionResponse{
				"alpha": {OK: true, Name: "alpha", Service: hub.ServiceStatus{Name: "alpha", State: "running", Running: true, Healthy: true}},
			}
		},
		CreateDashboardSession: func() (string, time.Time, error) {
			return "http://" + f.addr + "/ui?bootstrap=testnonce123", time.Date(2026, 11, 24, 1, 0, 0, 0, time.UTC), nil
		},
	}
	f.ts.Config.Handler = hub.NewControlHandlerWithStore(f.store, func() bool { return false }, f.addr, hooks)

	code, out, stderr := f.run(t, "sleep", "alpha")
	if code != exitOK || !slept.Load() || !strings.Contains(out, "slept alpha") {
		t.Fatalf("sleep: %d out=%q err=%q", code, out, stderr)
	}
	code, out, stderr = f.run(t, "wake", "alpha", "--json")
	if code != exitOK || !woke.Load() || decodeJSON(t, out)["ok"] != true {
		t.Fatalf("wake json: %d out=%q err=%q", code, out, stderr)
	}
	if code, out, _ = f.run(t, "sleep", "nope", "--json"); code != exitNotFound || errorCode(t, out) != "service_not_found" {
		t.Fatalf("sleep 404: %d %s", code, out)
	}

	// Bulk start --all hits POST /services/start-all.
	code, out, _ = f.run(t, "start", "--all", "--json")
	if code != exitOK || !startedAll.Load() || decodeJSON(t, out)["ok"] != true {
		t.Fatalf("start --all bulk: %d %s", code, out)
	}

	// ui [--no-open] & --json
	origOpen := openExternalFn
	t.Cleanup(func() { openExternalFn = origOpen })
	var opened []string
	openExternalFn = func(target string) error {
		opened = append(opened, target)
		return nil
	}

	code, out, stderr = f.run(t, "ui", "--json")
	m := decodeJSON(t, out)
	if code != exitOK || m["ok"] != true || !strings.Contains(fmt.Sprint(m["url"]), "/ui?bootstrap=testnonce123") || len(opened) != 0 {
		t.Fatalf("ui --json: %d out=%s err=%s opened=%v", code, out, stderr, opened)
	}
	code, out, _ = f.run(t, "ui", "--no-open")
	if code != exitOK || !strings.Contains(out, "/ui?bootstrap=testnonce123") || len(opened) != 0 {
		t.Fatalf("ui --no-open: %d out=%s opened=%v", code, out, opened)
	}
	code, out, _ = f.run(t, "ui")
	if code != exitOK || len(opened) != 1 || !strings.Contains(opened[0], "/ui?bootstrap=testnonce123") {
		t.Fatalf("ui auto-open: %d out=%s opened=%v", code, out, opened)
	}
}

func TestStartHubOpenLogsOpenConfigAndInit(t *testing.T) {
	origFind, origLaunch, origOpen := findHubBinaryFn, launchHubFn, openExternalFn
	t.Cleanup(func() {
		findHubBinaryFn = origFind
		launchHubFn = origLaunch
		openExternalFn = origOpen
	})

	// 1. init portable bootstrap + immediate audit passes cleanly with 0 violations and zero leaked token!
	dataDir := filepath.Join(t.TempDir(), "portable-hub")
	code, out, stderr := runCLI(t, "init", "--dir", dataDir, "--json")
	if code != exitOK {
		t.Fatalf("init --json: %d out=%s err=%s", code, out, stderr)
	}
	m := decodeJSON(t, out)
	cfgPath := fmt.Sprint(m["configPath"])
	tokPath := fmt.Sprint(m["controlTokenFile"])
	tok, err := hub.ReadSecretFile(tokPath)
	if err != nil || len(tok) != 64 {
		t.Fatalf("init token file invalid: %v len=%d", err, len(tok))
	}
	if strings.Contains(out+stderr, tok) {
		t.Fatal("init leaked raw control token")
	}
	if !strings.HasPrefix(fmt.Sprint(m["tokenFingerprint"]), "sha256:") {
		t.Fatalf("init missing sha256 fingerprint: %s", out)
	}
	// Refuses overwrite without --force.
	if code, out, _ = runCLI(t, "init", "--dir", dataDir, "--json"); code != exitUsage || errorCode(t, out) != "file_exists" {
		t.Fatalf("init overwrite without --force: %d %s", code, out)
	}
	// Audit of newly initialized directory must have 0 violations!
	code, out, stderr = runCLI(t, "--config", cfgPath, "audit", "--json")
	if code != exitOK {
		t.Fatalf("audit after init failed: %d out=%s err=%s", code, out, stderr)
	}

	// 2. open-config & open-logs (--json returns paths without opening; human invokes openExternalFn).
	var opened []string
	openExternalFn = func(target string) error {
		opened = append(opened, target)
		return nil
	}
	logFile := filepath.Join(dataDir, "logs", "mcp-hub.log")
	writeFile(t, logFile, "boot ok\n")

	code, out, _ = runCLI(t, "--config", cfgPath, "open-config", "--json")
	if code != exitOK || decodeJSON(t, out)["path"] != cfgPath || len(opened) != 0 {
		t.Fatalf("open-config --json: %d %s opened=%v", code, out, opened)
	}
	code, out, _ = runCLI(t, "--config", cfgPath, "open-logs", "hub", "--json")
	if code != exitOK || decodeJSON(t, out)["path"] != logFile || len(opened) != 0 {
		t.Fatalf("open-logs hub --json: %d %s opened=%v", code, out, opened)
	}
	code, out, _ = runCLI(t, "--config", cfgPath, "open-logs")
	if code != exitOK || len(opened) != 1 || opened[0] != filepath.Join(dataDir, "logs") {
		t.Fatalf("open-logs dir: %d out=%q opened=%v", code, out, opened)
	}
	if code, out, _ = runCLI(t, "--config", cfgPath, "open-logs", "missing", "--json"); code != exitNotFound || errorCode(t, out) != "log_not_found" {
		t.Fatalf("open-logs missing: %d %s", code, out)
	}

	// 3. start --hub --wait launches detached via hook and waits for /healthz.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hubAddr := ln.Addr().String()
	_ = ln.Close()

	cfg, _ := hub.LoadConfig(cfgPath)
	cfg.ControlAddr = hubAddr
	rawCfg, _ := json.Marshal(cfg)
	writeFile(t, cfgPath, string(rawCfg))

	var launchedBin, launchedCfg string
	var srv *httptest.Server
	t.Cleanup(func() {
		if srv != nil {
			srv.Close()
		}
	})
	findHubBinaryFn = func() (string, error) { return "/fake/mcp-hub", nil }
	launchHubFn = func(bin, cfgP string) (int, error) {
		launchedBin, launchedCfg = bin, cfgP
		l, lerr := net.Listen("tcp4", hubAddr)
		if lerr != nil {
			return 0, lerr
		}
		srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"services":{}}`))
		}))
		srv.Listener = l
		srv.Start()
		return 43210, nil
	}

	code, out, stderr = runCLI(t, "--config", cfgPath, "start", "--hub", "--wait", "--timeout", "5s", "--json")
	if code != exitOK {
		t.Fatalf("start --hub --wait: %d out=%s err=%s", code, out, stderr)
	}
	sm := decodeJSON(t, out)
	if sm["started"] != true || sm["ready"] != true || sm["pid"].(float64) != 43210 || launchedBin != "/fake/mcp-hub" || launchedCfg != cfgPath {
		t.Fatalf("unexpected start --hub result: %v bin=%s cfg=%s", sm, launchedBin, launchedCfg)
	}

	// Idempotent second call sees hub already running without launching again.
	code, out, _ = runCLI(t, "--config", cfgPath, "start", "--hub", "--json")
	if code != exitOK || decodeJSON(t, out)["alreadyRunning"] != true {
		t.Fatalf("idempotent start --hub: %d %s", code, out)
	}
}

func TestExpandedSnippetPresets(t *testing.T) {
	f := newFakeHub(t)
	f.catalog = []hub.CatalogEntry{{
		Name: "docs", Mode: "http", ClientURL: "http://127.0.0.1:18471/s/docs/mcp", State: "running",
		Auth: hub.CatalogAuthInfo{Type: "bearer", TokenFile: f.tokenPath, Header: "Authorization"},
	}}
	code, out, stderr := f.run(t, "snippet", "docs", "--client", "all", "--json")
	if code != exitOK {
		t.Fatalf("snippet all: %d %s %s", code, out, stderr)
	}
	if strings.Contains(out, testToken) {
		t.Fatal("snippet leaked raw token")
	}
	m := decodeJSON(t, out)
	snips := m["snippets"].(map[string]any)
	wantKeys := map[string]string{
		"agy":      `"serverUrl"`,
		"claude":   `"type": "http"`,
		"codex":    `[mcp_servers.docs]`,
		"cursor":   `"mcpServers"`,
		"vscode":   `"servers"`,
		"windsurf": `"serverUrl"`,
		"gemini":   `"httpUrl"`,
		"opencode": `"type": "remote"`,
		"devin":    `"mcpServers"`,
	}
	for client, substr := range wantKeys {
		body, ok := snips[client].(string)
		if !ok || !strings.Contains(body, substr) || !strings.Contains(body, "<TOKEN_FROM: ") {
			t.Errorf("snippet %q missing %q or placeholder:\n%v", client, substr, snips[client])
		}
	}
}

func TestAuditAllowsMultilineJSONTokensPolicyFileWhileCheckingACL(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "policy-audit")
	code, out, stderr := runCLI(t, "init", "--dir", dataDir, "--json")
	if code != exitOK {
		t.Fatalf("init failed: %d out=%s err=%s", code, out, stderr)
	}
	m := decodeJSON(t, out)
	cfgPath := fmt.Sprint(m["configPath"])

	policyPath := filepath.Join(dataDir, "secrets", "tokens.json")
	policyJSON := "{\n  \"tokens\": [\n    {\"name\": \"agent\", \"allow\": [\"search\"]}\n  ]\n}\n"
	if err := hub.WritePrivateFileAtomic(policyPath, []byte(policyJSON)); err != nil {
		t.Fatalf("write private policy file: %v", err)
	}

	cfg, err := hub.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = []hub.ServiceConfig{{
		Name:    "app",
		Command: "node",
		Port:    19123,
		Env:     map[string]string{"APP_TOKENS_FILE": filepath.ToSlash(policyPath)},
	}}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.WritePrivateFileAtomic(cfgPath, raw); err != nil {
		t.Fatalf("write config: %v", err)
	}

	code, out, stderr = runCLI(t, "--config", cfgPath, "audit", "--json")
	if code != exitOK {
		t.Fatalf("audit should pass with 0 violations on valid multi-line JSON tokens policy file: code=%d out=%s err=%s", code, out, stderr)
	}
	res := decodeJSON(t, out)
	if res["ok"] != true || res["violations"].(float64) != 0 {
		t.Fatalf("unexpected audit result: %s", out)
	}
	findings := res["findings"].([]any)
	foundACL := false
	for _, item := range findings {
		fm := item.(map[string]any)
		if fm["check"] == "acl" && strings.Contains(fmt.Sprint(fm["target"]), "APP_TOKENS_FILE") && fm["status"] == "ok" {
			foundACL = true
		}
		if fm["check"] == "secret-read" {
			t.Fatalf("unexpected secret-read finding on JSON policy file: %v", fm)
		}
	}
	if !foundACL {
		t.Fatalf("expected ACL check finding for APP_TOKENS_FILE:\n%s", out)
	}

	// Removing the policy file must still trigger an ACL violation.
	if err := os.Remove(policyPath); err != nil {
		t.Fatal(err)
	}
	code, out, _ = runCLI(t, "--config", cfgPath, "audit", "--json")
	if code != exitAudit || !strings.Contains(out, "APP_TOKENS_FILE") {
		t.Fatalf("missing policy file must still fail ACL audit: code=%d out=%s", code, out)
	}
}

func TestSnippetAndProbeSkipStructuredTokensFile(t *testing.T) {
	dataDir := t.TempDir()
	cfgPath := filepath.Join(dataDir, "config.json")
	code, out, stderr := runCLI(t, "--config", cfgPath, "init")
	if code != exitOK {
		t.Fatalf("init failed: code=%d out=%s err=%s", code, out, stderr)
	}

	policyPath := filepath.Join(dataDir, "secrets", "tokens.json")
	policyJSON := "[\n  {\"hash\": \"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\", \"profile\": \"full\"}\n]\n"
	if err := hub.WritePrivateFileAtomic(policyPath, []byte(policyJSON)); err != nil {
		t.Fatalf("write private policy file: %v", err)
	}
	clientTokenPath := filepath.Join(dataDir, "secrets", "search-toolkit-full.token")
	if err := hub.WritePrivateFileAtomic(clientTokenPath, []byte("client-bearer-token-value-1234567890\n")); err != nil {
		t.Fatalf("write private client token file: %v", err)
	}

	cfg, err := hub.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Services = []hub.ServiceConfig{{
		Name:              "search-toolkit",
		Command:           "node",
		Port:              18473,
		Env:               map[string]string{"SEARCH_TOOLKIT_HTTP_TOKENS_FILE": filepath.ToSlash(policyPath)},
		ShutdownURL:       "http://127.0.0.1:18473/admin/shutdown",
		ShutdownTokenFile: filepath.ToSlash(clientTokenPath),
		ReloadURL:         "http://127.0.0.1:18473/admin/reload",
		ReloadTokenFile:   filepath.ToSlash(clientTokenPath),
	}}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.WritePrivateFileAtomic(cfgPath, raw); err != nil {
		t.Fatalf("write config: %v", err)
	}

	endpoints := configuredProbeEndpoints(cfg, "search-toolkit")
	if len(endpoints) != 1 {
		t.Fatalf("expected 1 configured endpoint, got %+v", endpoints)
	}
	if !samePath(endpoints[0].tokenFile, clientTokenPath) {
		t.Fatalf("configuredProbeEndpoints picked %q, want %q", endpoints[0].tokenFile, clientTokenPath)
	}

	// Offline snippet fallback (hub unreachable on ephemeral unused port) must also use ShutdownTokenFile, not tokens.json.
	code, out, stderr = runCLI(t, "--config", cfgPath, "--addr", "127.0.0.1:1", "--timeout", "1s", "snippet", "search-toolkit", "--client", "claude", "--json")
	if code != exitOK {
		t.Fatalf("snippet failed: code=%d out=%s err=%s", code, out, stderr)
	}
	res := decodeJSON(t, out)
	auth, _ := res["auth"].(map[string]any)
	gotTokFile, _ := auth["tokenFile"].(string)
	if !samePath(gotTokFile, clientTokenPath) {
		t.Fatalf("snippet auth.tokenFile = %q, want %q (full output: %s)", gotTokFile, clientTokenPath, out)
	}
	if strings.Contains(out, "tokens.json") {
		t.Fatalf("snippet must not reference tokens.json policy file: %s", out)
	}
}
