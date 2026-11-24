package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// TestHelperStdioMCP acts as an in-binary STDIO MCP server subprocess when
// GO_WANT_HELPER_PROCESS=1 is set in the environment.
func TestHelperStdioMCP(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	var (
		writeMu      sync.Mutex
		stateMu      sync.Mutex
		initCount    int
		cancelledIDs []string
		inflight     = make(map[string]chan struct{})
		probeReplies = make(map[string]chan json.RawMessage)
	)

	writeLine := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		writeMu.Lock()
		_, _ = os.Stdout.Write(append(b, '\n'))
		writeMu.Unlock()
	}

	// Emit a non-JSON banner and blank line on startup to verify StdioBridge tolerates them.
	writeMu.Lock()
	_, _ = os.Stdout.WriteString("helper-stdio-mcp starting up (non-json banner)\n\n")
	writeMu.Unlock()

	br := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		rawLine, err := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(rawLine)
		if len(trimmed) > 0 {
			var msg struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
				Error   json.RawMessage `json:"error"`
				Result  json.RawMessage `json:"result"`
			}
			if jerr := json.Unmarshal(trimmed, &msg); jerr == nil {
				// Response to a server->client probe request sent by us.
				if msg.Method == "" && len(bytes.TrimSpace(msg.ID)) > 0 {
					key := string(bytes.TrimSpace(msg.ID))
					stateMu.Lock()
					ch := probeReplies[key]
					delete(probeReplies, key)
					stateMu.Unlock()
					if ch != nil {
						ch <- append(json.RawMessage(nil), trimmed...)
					}
					continue
				}

				switch msg.Method {
				case "initialize":
					stateMu.Lock()
					initCount++
					cnt := initCount
					stateMu.Unlock()
					protoVer := os.Getenv("HELPER_PROTO_VERSION")
					if protoVer == "" {
						protoVer = "2025-03-26"
					}
					writeLine(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result": map[string]any{
							"protocolVersion": protoVer,
							"capabilities":    map[string]any{"tools": map[string]any{}},
							"serverInfo": map[string]any{
								"name":      "helper-stdio-mcp",
								"version":   "1.0.0",
								"pid":       os.Getpid(),
								"initCount": cnt,
							},
						},
					})

				case "notifications/initialized":
					// No response.

				case "notifications/cancelled":
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
						Reason    string          `json:"reason"`
					}
					_ = json.Unmarshal(msg.Params, &p)
					key := string(bytes.TrimSpace(p.RequestID))
					stateMu.Lock()
					cancelledIDs = append(cancelledIDs, key)
					ch := inflight[key]
					delete(inflight, key)
					stateMu.Unlock()
					if ch != nil {
						close(ch)
					}

				case "ping":
					writeLine(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result":  map[string]any{},
					})

				case "tools/list":
					writeLine(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result": map[string]any{
							"nextCursor": "cursor-end",
							"tools": []map[string]any{
								{
									"name":        "fast_echo",
									"description": "Echoes message quickly",
									"annotations": map[string]any{"readOnlyHint": true},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "write_data",
									"description": "Mutating tool",
									"annotations": map[string]any{"readOnlyHint": false},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "unannotated_op",
									"description": "Tool without annotations",
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "slow_wait",
									"description": "Waits until cancelled or duration elapses",
									"annotations": map[string]any{"readOnlyHint": true},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "large_payload",
									"description": "Returns >128 KiB payload",
									"annotations": map[string]any{"readOnlyHint": true},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "trigger_probe",
									"description": "Sends server->client JSON-RPC request",
									"annotations": map[string]any{"readOnlyHint": true},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "get_stats",
									"description": "Returns helper server stats",
									"annotations": map[string]any{"readOnlyHint": true},
									"inputSchema": map[string]any{"type": "object"},
								},
								{
									"name":        "crash_now",
									"description": "Exits abruptly",
									"annotations": map[string]any{"readOnlyHint": false},
									"inputSchema": map[string]any{"type": "object"},
								},
							},
						},
					})

				case "tools/call":
					idCopy := append(json.RawMessage(nil), bytes.TrimSpace(msg.ID)...)
					paramsCopy := append(json.RawMessage(nil), msg.Params...)
					go func() {
						var p struct {
							Name      string         `json:"name"`
							Arguments map[string]any `json:"arguments"`
						}
						_ = json.Unmarshal(paramsCopy, &p)

						switch p.Name {
						case "fast_echo":
							if ms, ok := p.Arguments["delayMs"].(float64); ok && ms > 0 {
								time.Sleep(time.Duration(ms) * time.Millisecond)
							}
							text, _ := p.Arguments["text"].(string)
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      idCopy,
								"result": map[string]any{
									"content": []map[string]any{{"type": "text", "text": text}},
									"pid":     os.Getpid(),
								},
							})

						case "write_data":
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      idCopy,
								"result": map[string]any{
									"content": []map[string]any{{"type": "text", "text": "written"}},
								},
							})

						case "slow_wait":
							key := string(idCopy)
							cancelCh := make(chan struct{})
							stateMu.Lock()
							inflight[key] = cancelCh
							stateMu.Unlock()

							waitMs := 5000.0
							if ms, ok := p.Arguments["waitMs"].(float64); ok && ms > 0 {
								waitMs = ms
							}
							select {
							case <-cancelCh:
								return
							case <-time.After(time.Duration(waitMs) * time.Millisecond):
								stateMu.Lock()
								delete(inflight, key)
								stateMu.Unlock()
								writeLine(map[string]any{
									"jsonrpc": "2.0",
									"id":      idCopy,
									"result": map[string]any{
										"content": []map[string]any{{"type": "text", "text": "completed"}},
									},
								})
							}

						case "large_payload":
							size := 196 * 1024 // 196 KiB (>128 KiB)
							if sz, ok := p.Arguments["bytes"].(float64); ok && sz > 0 {
								size = int(sz)
							}
							blob := strings.Repeat("Z", size)
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      idCopy,
								"result": map[string]any{
									"content": []map[string]any{{"type": "text", "text": blob}},
								},
							})

						case "trigger_probe":
							probeID := fmt.Sprintf(`"srv-probe-%s"`, strings.Trim(string(idCopy), `"`))
							replyCh := make(chan json.RawMessage, 1)
							stateMu.Lock()
							probeReplies[probeID] = replyCh
							stateMu.Unlock()

							// Send unexpected server->client JSON-RPC request.
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      json.RawMessage(probeID),
								"method":  "sampling/createMessage",
								"params":  map[string]any{"maxTokens": 10},
							})

							select {
							case rawReply := <-replyCh:
								var rep struct {
									Error *struct {
										Code    int    `json:"code"`
										Message string `json:"message"`
									} `json:"error"`
								}
								_ = json.Unmarshal(rawReply, &rep)
								errCode := 0
								if rep.Error != nil {
									errCode = rep.Error.Code
								}
								writeLine(map[string]any{
									"jsonrpc": "2.0",
									"id":      idCopy,
									"result": map[string]any{
										"probeErrorCode": errCode,
									},
								})
							case <-time.After(5 * time.Second):
								writeLine(map[string]any{
									"jsonrpc": "2.0",
									"id":      idCopy,
									"result": map[string]any{
										"probeErrorCode": 0,
										"isError":        true,
									},
								})
							}

						case "get_stats":
							stateMu.Lock()
							ic := initCount
							cids := append([]string(nil), cancelledIDs...)
							active := len(inflight)
							stateMu.Unlock()
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      idCopy,
								"result": map[string]any{
									"pid":          os.Getpid(),
									"initCount":    ic,
									"cancelledIDs": cids,
									"inFlight":     active,
								},
							})

						case "crash_now":
							if msg := os.Getenv("HELPER_CRASH_STDERR"); msg != "" {
								_, _ = os.Stderr.WriteString(msg)
							}
							os.Exit(42)

						default:
							writeLine(map[string]any{
								"jsonrpc": "2.0",
								"id":      idCopy,
								"error": map[string]any{
									"code":    -32601,
									"message": "unknown tool: " + p.Name,
								},
							})
						}
					}()

				default:
					if len(bytes.TrimSpace(msg.ID)) > 0 {
						writeLine(map[string]any{
							"jsonrpc": "2.0",
							"id":      msg.ID,
							"error":   map[string]any{"code": -32601, "message": "method not found"},
						})
					}
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				if marker := os.Getenv("HELPER_EOF_MARKER"); marker != "" {
					time.Sleep(120 * time.Millisecond)
					_ = os.WriteFile(marker, []byte("clean-eof-exit"), 0o600)
				}
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
}

func newTestStdioBridge(t *testing.T, mutate func(*StdioBridgeOptions)) *StdioBridge {
	t.Helper()
	var logBuf bytes.Buffer
	opts := StdioBridgeOptions{
		Name:           "helper-mcp",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestHelperStdioMCP$"},
		Env:            map[string]string{"GO_WANT_HELPER_PROCESS": "1"},
		IdleTimeout:    0, // disabled unless explicitly configured by test
		RequestTimeout: 15 * time.Second,
		InitTimeout:    10 * time.Second,
		MaxConcurrent:  32,
		MaxLineBytes:   32 << 20,
		StderrWriter:   io.Discard,
		Logger:         log.New(&logBuf, "", 0),
	}
	if mutate != nil {
		mutate(&opts)
	}
	b := NewStdioBridge(opts)
	t.Cleanup(func() {
		b.Shutdown()
	})
	return b
}

func doBridgePOST(t *testing.T, b *StdioBridge, filter ToolFilter, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	b.ServeHTTPWithFilter(rec, req, filter)
	return rec
}

func TestStdioBridge_LazyColdStartAndCachedInit(t *testing.T) {
	b := newTestStdioBridge(t, nil)

	if b.State() != "sleeping" || b.Status().PID != 0 || b.ActiveJob() != nil {
		t.Fatalf("expected initial sleeping state with PID 0, got state=%s status=%+v", b.State(), b.Status())
	}

	// Pure notification must return 202 Accepted WITHOUT waking child.
	notifRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if notifRec.Code != http.StatusAccepted {
		t.Fatalf("notification status = %d, want 202", notifRec.Code)
	}
	if b.State() != "sleeping" || b.Status().PID != 0 {
		t.Fatalf("notification woke sleeping child! state=%s pid=%d", b.State(), b.Status().PID)
	}

	// First initialize wakes child once and returns real capabilities.
	init1 := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"c1","version":"1"}}}`)
	if init1.Code != http.StatusOK {
		t.Fatalf("init1 status=%d body=%s", init1.Code, init1.Body.String())
	}
	if b.State() != "running" || b.Status().PID <= 0 || b.ActiveJob() == nil {
		t.Fatalf("expected running after init1, state=%s status=%+v", b.State(), b.Status())
	}
	var resp1 struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ServerInfo struct {
				Name      string `json:"name"`
				PID       int    `json:"pid"`
				InitCount int    `json:"initCount"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init1.Body.Bytes(), &resp1); err != nil {
		t.Fatalf("unmarshal init1: %v", err)
	}
	if string(resp1.ID) != "1" || resp1.Result.ServerInfo.Name != "helper-stdio-mcp" || resp1.Result.ServerInfo.InitCount != 1 {
		t.Fatalf("unexpected init1 response: %+v", resp1)
	}

	// Second initialize reuses cached result, echoing client's string ID without re-initializing child.
	init2 := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"client-2-init","method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if init2.Code != http.StatusOK {
		t.Fatalf("init2 status=%d body=%s", init2.Code, init2.Body.String())
	}
	var resp2 struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ServerInfo struct {
				PID       int `json:"pid"`
				InitCount int `json:"initCount"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(init2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal init2: %v", err)
	}
	if string(resp2.ID) != `"client-2-init"` || resp2.Result.ServerInfo.PID != resp1.Result.ServerInfo.PID {
		t.Fatalf("unexpected init2 response: %+v", resp2)
	}

	// Confirm child truly saw initialize only ONCE.
	statsRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stats"}}`)
	if statsRec.Code != http.StatusOK {
		t.Fatalf("get_stats status=%d body=%s", statsRec.Code, statsRec.Body.String())
	}
	var statsResp struct {
		Result struct {
			InitCount int `json:"initCount"`
		} `json:"result"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &statsResp); err != nil {
		t.Fatalf("unmarshal stats: %v", err)
	}
	if statsResp.Result.InitCount != 1 {
		t.Fatalf("child received %d initialize calls, want 1", statsResp.Result.InitCount)
	}
}

func TestStdioBridge_ConcurrentMultiplexingAndLimit(t *testing.T) {
	b := newTestStdioBridge(t, nil)

	const n = 10
	var wg sync.WaitGroup
	errCh := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var idJSON string
			if idx%2 == 0 {
				idJSON = fmt.Sprintf(`"str-%d"`, idx)
			} else {
				idJSON = fmt.Sprintf(`%d`, 100+idx)
			}
			wantText := fmt.Sprintf("payload-%d", idx)
			delayMs := (n - idx) * 8 // reverse order completion to test out-of-order muxing
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"fast_echo","arguments":{"text":%q,"delayMs":%d}}}`, idJSON, wantText, delayMs)

			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			b.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("req %d status=%d body=%s", idx, rec.Code, rec.Body.String())
				return
			}
			var resp struct {
				ID     json.RawMessage `json:"id"`
				Result struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				errCh <- fmt.Errorf("req %d unmarshal: %v", idx, err)
				return
			}
			if string(resp.ID) != idJSON {
				errCh <- fmt.Errorf("req %d id=%s want=%s", idx, string(resp.ID), idJSON)
				return
			}
			if len(resp.Result.Content) != 1 || resp.Result.Content[0].Text != wantText {
				errCh <- fmt.Errorf("req %d text mismatch: %+v", idx, resp.Result.Content)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	// Also verify MaxConcurrent semaphore returns HTTP 429 JSON-RPC error when saturated.
	bSmall := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.MaxConcurrent = 1
	})
	if err := bSmall.Start(context.Background()); err != nil {
		t.Fatalf("start bSmall: %v", err)
	}
	slowCtx, cancelSlow := context.WithCancel(context.Background())
	defer cancelSlow()
	started := make(chan struct{})
	doneSlow := make(chan struct{})
	go func() {
		defer close(doneSlow)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow_wait","arguments":{"waitMs":3000}}}`)).WithContext(slowCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		close(started)
		bSmall.ServeHTTP(rec, req)
	}()
	<-started
	// Wait until slow_wait actually occupies the single semaphore slot so ping cannot steal it first.
	slotDeadline := time.Now().Add(2 * time.Second)
	for len(bSmall.sem) == 0 && time.Now().Before(slotDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	deadline := time.Now().Add(2 * time.Second)
	var got429 bool
	for time.Now().Before(deadline) {
		rec := doBridgePOST(t, bSmall, ToolFilter{}, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)
		if rec.Code == http.StatusTooManyRequests {
			if !strings.Contains(rec.Body.String(), `"error"`) || !strings.Contains(rec.Body.String(), `"id":2`) {
				t.Fatalf("expected JSON-RPC error on 429, got: %s", rec.Body.String())
			}
			got429 = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cancelSlow()
	<-doneSlow
	if !got429 {
		t.Fatal("expected HTTP 429 when MaxConcurrent=1 was saturated")
	}
}

func TestStdioBridge_LargePayloadOver128KiB(t *testing.T) {
	b := newTestStdioBridge(t, nil)

	rec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"large_payload","arguments":{"bytes":196608}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("large_payload status=%d body=%s", rec.Code, truncatePreview(rec.Body.String(), 200))
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal large_payload: %v", err)
	}
	if string(resp.ID) != "77" || len(resp.Result.Content) != 1 || len(resp.Result.Content[0].Text) != 196608 {
		t.Fatalf("unexpected large_payload response: id=%s len=%d", string(resp.ID), len(resp.Result.Content[0].Text))
	}
}

func TestStdioBridge_ServerToClientRequestNoDeadlock(t *testing.T) {
	b := newTestStdioBridge(t, nil)

	rec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"probe-call","method":"tools/call","params":{"name":"trigger_probe"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger_probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			ProbeErrorCode int `json:"probeErrorCode"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal trigger_probe: %v", err)
	}
	if string(resp.ID) != `"probe-call"` || resp.Result.ProbeErrorCode != -32601 {
		t.Fatalf("expected bridge to reply -32601 to server->client request, got: %+v", resp)
	}
}

func TestStdioBridge_ClientDisconnectSendsCancelled(t *testing.T) {
	b := newTestStdioBridge(t, nil)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("start bridge: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":"slow-1","method":"tools/call","params":{"name":"slow_wait","arguments":{"waitMs":10000}}}`)).WithContext(reqCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.ServeHTTP(rec, req)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return promptly after client disconnect")
	}

	// Verify helper child received notifications/cancelled.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		statsRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_stats"}}`)
		var statsResp struct {
			Result struct {
				CancelledIDs []string `json:"cancelledIDs"`
			} `json:"result"`
		}
		if statsRec.Code == http.StatusOK && json.Unmarshal(statsRec.Body.Bytes(), &statsResp) == nil {
			if len(statsResp.Result.CancelledIDs) > 0 {
				return
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("child subprocess never received notifications/cancelled after client disconnect")
}

func TestStdioBridge_NoWakeAndScopedCancellation(t *testing.T) {
	b := newTestStdioBridge(t, nil)
	scope := bridgeSessionScope{route: "fixture", token: [32]byte{1}}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`
	post := func(scope bridgeSessionScope, sessionID string, noWake bool, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if sessionID != "" {
			r.Header.Set("Mcp-Session-Id", sessionID)
		}
		if noWake {
			r.Header.Set(noWakeHeader, "1")
		}
		rec := httptest.NewRecorder()
		b.serveHTTPWithScope(rec, r, ToolFilter{}, scope)
		return rec
	}
	if rec := post(scope, "", true, initialize); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "service_sleeping") || b.State() != "sleeping" {
		t.Fatalf("no-wake initialize status=%d state=%s body=%s", rec.Code, b.State(), rec.Body)
	}
	discover := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}`
	if rec := post(scope, "", false, discover); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"code":-32601`) || b.State() != "sleeping" {
		t.Fatalf("discover fallback status=%d state=%s body=%s", rec.Code, b.State(), rec.Body)
	}
	init := post(scope, "", false, initialize)
	sessionID := init.Header().Get("Mcp-Session-Id")
	if init.Code != http.StatusOK || sessionID == "" {
		t.Fatalf("initialize status=%d session assigned=%v body=%s", init.Code, sessionID != "", init.Body)
	}

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- post(scope, sessionID, true, `{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"slow_wait","arguments":{"waitMs":10000}}}`)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		stats := post(scope, sessionID, true, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_stats"}}`)
		if strings.Contains(stats.Body.String(), `"inFlight":1`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start the slow request")
		}
		time.Sleep(10 * time.Millisecond)
	}

	notification := `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow"}}`
	wrongScope := bridgeSessionScope{route: "fixture", token: [32]byte{2}}
	if rec := post(wrongScope, sessionID, true, notification); rec.Code != http.StatusNotFound {
		t.Fatalf("cancellation with different credential status=%d", rec.Code)
	}
	if rec := post(scope, sessionID, true, notification); rec.Code != http.StatusAccepted {
		t.Fatalf("cancellation status=%d body=%s", rec.Code, rec.Body)
	}
	select {
	case rec := <-done:
		if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
			t.Fatalf("cancelled call status=%d body=%s", rec.Code, rec.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("explicit cancellation did not stop the original call")
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		stats := post(scope, sessionID, true, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_stats"}}`)
		if strings.Contains(stats.Body.String(), `"cancelledIDs":["`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not receive the upstream cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStdioBridge_ToolFilterBlockingAndListFiltering(t *testing.T) {
	b := newTestStdioBridge(t, nil)
	filter := ToolFilter{
		AllowTools: []string{"fast_*", "write_*", "get_*"},
		DenyTools:  []string{"write_*"},
	}

	// Denied tools/call must return -32601 WITHOUT waking the sleeping child!
	denied := doBridgePOST(t, b, filter, `{"jsonrpc":"2.0","id":"deny-1","method":"tools/call","params":{"name":"write_data"}}`)
	if denied.Code != http.StatusOK {
		t.Fatalf("denied status=%d want 200", denied.Code)
	}
	if b.State() != "sleeping" || b.Status().PID != 0 {
		t.Fatalf("denied tools/call woke sleeping child! state=%s pid=%d", b.State(), b.Status().PID)
	}
	var errResp struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(denied.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("unmarshal denied: %v", err)
	}
	if string(errResp.ID) != `"deny-1"` || errResp.Error.Code != -32601 {
		t.Fatalf("unexpected denied payload: %+v", errResp)
	}

	// tools/list wakes child and filters tools cleanly.
	listRec := doBridgePOST(t, b, filter, `{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)
	if listRec.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Result struct {
			NextCursor string `json:"nextCursor"`
			Tools      []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal tools/list: %v", err)
	}
	if listResp.Result.NextCursor != "cursor-end" {
		t.Fatalf("lost nextCursor: %q", listResp.Result.NextCursor)
	}
	gotNames := map[string]bool{}
	for _, tl := range listResp.Result.Tools {
		gotNames[tl.Name] = true
	}
	if len(gotNames) != 2 || !gotNames["fast_echo"] || !gotNames["get_stats"] {
		t.Fatalf("expected only {fast_echo, get_stats}, got %+v", gotNames)
	}

	// ReadOnlyOnly filter excludes write_data and unannotated_op from tools/list,
	// allows readOnlyHint=true tools/call, and blocks write_data tools/call.
	roFilter := ToolFilter{ReadOnlyOnly: true}
	roList := doBridgePOST(t, b, roFilter, `{"jsonrpc":"2.0","id":11,"method":"tools/list"}`)
	if roList.Code != http.StatusOK {
		t.Fatalf("ro tools/list status=%d", roList.Code)
	}
	if strings.Contains(roList.Body.String(), `"write_data"`) || strings.Contains(roList.Body.String(), `"unannotated_op"`) {
		t.Fatalf("ReadOnlyOnly leaked non-readonly tools: %s", roList.Body.String())
	}
	if !strings.Contains(roList.Body.String(), `"fast_echo"`) {
		t.Fatalf("ReadOnlyOnly dropped readOnlyHint=true tool: %s", roList.Body.String())
	}

	roDeny := doBridgePOST(t, b, roFilter, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"write_data"}}`)
	if !strings.Contains(roDeny.Body.String(), `-32601`) {
		t.Fatalf("expected -32601 for write_data under ReadOnlyOnly, got %s", roDeny.Body.String())
	}
	roAllow := doBridgePOST(t, b, roFilter, `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"fast_echo","arguments":{"text":"ro-ok"}}}`)
	if !strings.Contains(roAllow.Body.String(), `"ro-ok"`) {
		t.Fatalf("expected fast_echo allowed under ReadOnlyOnly after tools/list, got %s", roAllow.Body.String())
	}
}

func TestStdioBridge_IdleAutoSleepAndRewake(t *testing.T) {
	b := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.IdleTimeout = 250 * time.Millisecond
	})

	getPID := func() int {
		rec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_stats"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("get_stats status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Result struct {
				PID int `json:"pid"`
			} `json:"result"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal get_stats: %v", err)
		}
		return resp.Result.PID
	}

	pid1 := getPID()
	if pid1 <= 0 || b.State() != "running" {
		t.Fatalf("expected running pid1 > 0, got pid=%d state=%s", pid1, b.State())
	}

	// Suspend prevents idle sleep.
	b.Suspend()
	time.Sleep(400 * time.Millisecond)
	if b.State() != "running" || b.Status().PID != pid1 {
		t.Fatalf("bridge slept while suspended! state=%s pid=%d", b.State(), b.Status().PID)
	}
	b.Resume()

	// Wait for idle reaper to put child to sleep.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if b.State() == "sleeping" && b.Status().PID == 0 && b.ActiveJob() == nil {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if b.State() != "sleeping" || b.Status().PID != 0 || b.ActiveJob() != nil {
		t.Fatalf("expected sleeping with PID 0 after idle timeout, got state=%s status=%+v", b.State(), b.Status())
	}

	// Next request wakes a fresh child cleanly: WakeCount increments, Restarts & CrashRestarts stay 0.
	pid2 := getPID()
	if pid2 <= 0 || pid2 == pid1 {
		t.Fatalf("expected new child PID after re-wake, pid1=%d pid2=%d", pid1, pid2)
	}
	st2 := b.Status()
	if st2.State != "running" || st2.WakeCount != 2 || st2.Restarts != 0 || st2.CrashRestarts != 0 {
		t.Fatalf("expected running with WakeCount=2 Restarts=0 CrashRestarts=0 after re-wake, status=%+v", st2)
	}
}

func TestStdioBridge_HTTPMethodsStopRestartCrashAndMetrics(t *testing.T) {
	b := newTestStdioBridge(t, nil)

	// GET -> 405 with Allow: POST, DELETE.
	getReq := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	getRec := httptest.NewRecorder()
	b.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d want 405", getRec.Code)
	}
	if got := getRec.Header().Get("Allow"); got != "POST, DELETE" {
		t.Fatalf("GET Allow=%q want %q", got, "POST, DELETE")
	}

	// DELETE -> 200 {"ok":true}.
	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delRec := httptest.NewRecorder()
	b.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK || !strings.Contains(delRec.Body.String(), `"ok":true`) {
		t.Fatalf("DELETE status=%d body=%s", delRec.Code, delRec.Body.String())
	}

	// Child crash fails in-flight request immediately and transitions to sleeping for clean cold-start recovery.
	crashRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"crash_now"}}`)
	if crashRec.Code != http.StatusBadGateway {
		t.Fatalf("crash_now status=%d want 502 (body=%s)", crashRec.Code, crashRec.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b.State() == "sleeping" && b.Status().PID == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b.State() != "sleeping" || b.Status().PID != 0 {
		t.Fatalf("expected sleeping after crash, got state=%s status=%+v", b.State(), b.Status())
	}

	// Next request cold-starts fresh child and succeeds.
	recoverRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fast_echo","arguments":{"text":"recovered"}}}`)
	if recoverRec.Code != http.StatusOK || !strings.Contains(recoverRec.Body.String(), `"recovered"`) {
		t.Fatalf("recovery failed: status=%d body=%s", recoverRec.Code, recoverRec.Body.String())
	}
	pidBeforeRestart := b.Status().PID
	if pidBeforeRestart <= 0 {
		t.Fatalf("expected running PID after recovery")
	}

	// Restart spawns a new PID.
	if err := b.Restart(context.Background()); err != nil {
		t.Fatalf("Restart error: %v", err)
	}
	pidAfterRestart := b.Status().PID
	if pidAfterRestart <= 0 || pidAfterRestart == pidBeforeRestart {
		t.Fatalf("expected fresh PID after Restart: before=%d after=%d", pidBeforeRestart, pidAfterRestart)
	}

	// Manual Stop -> state=stopped, PID=0, POST -> 503 service_stopped.
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if b.State() != "stopped" || b.Status().PID != 0 || b.HealthSummary().Running {
		t.Fatalf("expected stopped state after Stop, state=%s status=%+v", b.State(), b.Status())
	}
	stoppedRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	if stoppedRec.Code != http.StatusServiceUnavailable || !strings.Contains(stoppedRec.Body.String(), "service_stopped") {
		t.Fatalf("POST while stopped: status=%d body=%s", stoppedRec.Code, stoppedRec.Body.String())
	}

	// Start clears manual stop and resumes normal operation.
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start after Stop error: %v", err)
	}
	if b.State() != "running" || !b.HealthSummary().Healthy {
		t.Fatalf("expected running+healthy after Start, state=%s health=%+v", b.State(), b.HealthSummary())
	}

	// Metrics reflect recorded requests, tools, and errors.
	m := b.Metrics()
	if m.TotalRequests < 2 || m.ErrorRequests < 1 {
		t.Fatalf("unexpected bridge metrics: %+v", m)
	}
	if m.Tools["fast_echo"].Calls < 1 || m.Tools["crash_now"].Errors < 1 {
		t.Fatalf("unexpected bridge tool metrics: %+v", m.Tools)
	}
}

func TestStdioBridge_BatchAndOversizedStdoutGuard(t *testing.T) {
	// 1. Notifications-only batch returns 202 without waking sleeping child.
	b := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.MaxLineBytes = 16 * 1024 // 16 KiB cap for oversized stdout test
	})

	notifBatch := doBridgePOST(t, b, ToolFilter{}, `[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}]`)
	if notifBatch.Code != http.StatusAccepted {
		t.Fatalf("notifications batch status=%d want 202", notifBatch.Code)
	}
	if b.State() != "sleeping" || b.Status().PID != 0 {
		t.Fatalf("notifications batch woke child! state=%s pid=%d", b.State(), b.Status().PID)
	}

	// 2. Mixed batch with denied tool, allowed tool, notification, and filtered tools/list.
	filter := ToolFilter{DenyTools: []string{"write_*"}}
	batchReq := `[
		{"jsonrpc":"2.0","method":"notifications/initialized"},
		{"jsonrpc":"2.0","id":"b-deny","method":"tools/call","params":{"name":"write_data"}},
		{"jsonrpc":"2.0","id":"b-ok","method":"tools/call","params":{"name":"fast_echo","arguments":{"text":"batch-hi"}}},
		{"jsonrpc":"2.0","id":"b-list","method":"tools/list"}
	]`
	batchRec := doBridgePOST(t, b, filter, batchReq)
	if batchRec.Code != http.StatusOK {
		t.Fatalf("mixed batch status=%d body=%s", batchRec.Code, batchRec.Body.String())
	}
	var batchResp []map[string]any
	if err := json.Unmarshal(batchRec.Body.Bytes(), &batchResp); err != nil {
		t.Fatalf("unmarshal batch response: %v (%s)", err, batchRec.Body.String())
	}
	if len(batchResp) != 3 {
		t.Fatalf("expected 3 responses in batch (excluding notification), got %d: %s", len(batchResp), batchRec.Body.String())
	}
	if batchResp[0]["id"] != "b-deny" || batchResp[0]["error"] == nil {
		t.Fatalf("expected denied error for b-deny: %+v", batchResp[0])
	}
	if batchResp[1]["id"] != "b-ok" || !strings.Contains(batchRec.Body.String(), `"batch-hi"`) {
		t.Fatalf("expected fast_echo result for b-ok: %+v", batchResp[1])
	}
	if batchResp[2]["id"] != "b-list" || strings.Contains(batchRec.Body.String(), `"write_data"`) {
		t.Fatalf("expected write_data filtered out of batch tools/list: %s", batchRec.Body.String())
	}

	// 3. Oversized stdout line (> MaxLineBytes) fails that specific call promptly without killing child.
	overRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"too-big","method":"tools/call","params":{"name":"large_payload","arguments":{"bytes":32768}}}`)
	if overRec.Code != http.StatusBadGateway || !strings.Contains(overRec.Body.String(), "maxLineBytes") {
		t.Fatalf("expected 502 maxLineBytes error, got status=%d body=%s", overRec.Code, overRec.Body.String())
	}
	// Subsequent normal call on the same child still succeeds cleanly.
	okRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":"after-big","method":"tools/call","params":{"name":"fast_echo","arguments":{"text":"still-alive"}}}`)
	if okRec.Code != http.StatusOK || !strings.Contains(okRec.Body.String(), `"still-alive"`) {
		t.Fatalf("child failed to stay healthy after oversized line: status=%d body=%s", okRec.Code, okRec.Body.String())
	}
}

func TestStdioBridge_V03ReliabilitySessionsProtocolsAndGracefulShutdown(t *testing.T) {
	// 1. Atomic Status/HealthSummary fields, security headers on writeRawJSON, session cleanup,
	// crash vs wake vs restart counters, OnCrash stderr flush, and OnStateChange hook.
	ring := NewRingLog(32, 1024)
	ringW := ring.Writer("stderr")
	var crashCalls, stateChanges atomic.Int32

	b := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.Env["HELPER_CRASH_STDERR"] = "unterminated-crash-stderr-line"
		o.StderrWriter = ringW
		o.OnCrash = func() {
			flushIfPossible(ringW)
			ring.FreezeCrashTail(10)
			crashCalls.Add(1)
		}
		o.OnStateChange = func() {
			stateChanges.Add(1)
		}
	})

	initRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`)
	if initRec.Code != http.StatusOK {
		t.Fatalf("init status=%d body=%s", initRec.Code, initRec.Body.String())
	}
	if got := initRec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := initRec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	sessID := initRec.Header().Get("Mcp-Session-Id")
	if sessID == "" {
		t.Fatal("expected Mcp-Session-Id on initialize")
	}
	if got := b.MCPMetrics().ActiveSessionCount(); got != 1 {
		t.Fatalf("ActiveSessionCount after initialize = %d, want 1", got)
	}

	st := b.Status()
	hz := b.HealthSummary()
	if st.State != "running" || st.Mode != "stdio-bridge" || st.MetricsSource != "bridge" || st.LastActivityAt == "" {
		t.Fatalf("incomplete Status(): %+v", st)
	}
	if hz.State != "running" || hz.Mode != "stdio-bridge" {
		t.Fatalf("incomplete HealthSummary(): %+v", hz)
	}
	if stateChanges.Load() == 0 {
		t.Fatalf("expected OnStateChange to fire on bridge startup")
	}

	// HTTP DELETE must clean up MCPMetrics.ActiveSessions.
	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delReq.Header.Set("Mcp-Session-Id", sessID)
	delRec := httptest.NewRecorder()
	b.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE session status=%d", delRec.Code)
	}
	if got := b.MCPMetrics().ActiveSessionCount(); got != 0 {
		t.Fatalf("ActiveSessionCount after DELETE = %d, want 0", got)
	}

	// Explicit Sleep + Wake increments WakeCount and keeps Restarts/CrashRestarts == 0.
	if err := b.Sleep(context.Background()); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if b.State() != "sleeping" {
		t.Fatalf("state after Sleep = %q, want sleeping", b.State())
	}
	if err := b.Wake(context.Background()); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	stWake := b.Status()
	if stWake.WakeCount != 2 || stWake.Restarts != 0 || stWake.CrashRestarts != 0 {
		t.Fatalf("after Sleep+Wake: WakeCount=%d Restarts=%d CrashRestarts=%d, want 2/0/0", stWake.WakeCount, stWake.Restarts, stWake.CrashRestarts)
	}

	// Crash freezes flushed CrashTail via OnCrash, clears sessions, and next recovery increments Restarts + CrashRestarts.
	b.MCPMetrics().TrackSessionEpoch("crash-sess", 1)
	crashRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"crash_now"}}`)
	if crashRec.Code != http.StatusBadGateway {
		t.Fatalf("crash status=%d want 502", crashRec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && b.State() != "sleeping" {
		time.Sleep(15 * time.Millisecond)
	}
	if crashCalls.Load() != 1 {
		t.Fatalf("OnCrash calls = %d, want 1", crashCalls.Load())
	}
	if tail := ring.CrashTail(); len(tail) == 0 || !strings.Contains(strings.Join(tail, "\n"), "unterminated-crash-stderr-line") {
		t.Fatalf("expected flushed stderr line in frozen CrashTail, got %v", tail)
	}
	if got := b.MCPMetrics().ActiveSessionCount(); got != 0 {
		t.Fatalf("ActiveSessionCount after crash = %d, want 0", got)
	}

	okRec := doBridgePOST(t, b, ToolFilter{}, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fast_echo","arguments":{"text":"back"}}}`)
	if okRec.Code != http.StatusOK {
		t.Fatalf("post-crash recovery status=%d body=%s", okRec.Code, okRec.Body.String())
	}
	stCrash := b.Status()
	if stCrash.Restarts != 1 || stCrash.CrashRestarts != 1 {
		t.Fatalf("after crash recovery: Restarts=%d CrashRestarts=%d, want 1/1", stCrash.Restarts, stCrash.CrashRestarts)
	}

	// Manual Restart increments Restarts only (not CrashRestarts).
	if err := b.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	stManual := b.Status()
	if stManual.Restarts != 2 || stManual.CrashRestarts != 1 {
		t.Fatalf("after manual Restart: Restarts=%d CrashRestarts=%d, want 2/1", stManual.Restarts, stManual.CrashRestarts)
	}

	// 2. Relaxed protocolVersion negotiation accepts valid YYYY-MM-DD versions and reports invalid version in error.
	for _, ver := range []string{"2024-11-05", "2025-06-18"} {
		bv := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
			o.Env["HELPER_PROTO_VERSION"] = ver
		})
		if err := bv.Start(context.Background()); err != nil {
			t.Fatalf("bridge with protocolVersion %q failed Start: %v", ver, err)
		}
	}
	bBadVer := newTestStdioBridge(t, func(o *StdioBridgeOptions) {
		o.Env["HELPER_PROTO_VERSION"] = "legacy-v1"
	})
	if err := bBadVer.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "legacy-v1") {
		t.Fatalf("expected protocolVersion error containing 'legacy-v1', got %v", err)
	}

	// 3. Shutdown gives child the stdin-EOF graceful window instead of immediately hard-killing.
	marker := filepath.Join(t.TempDir(), "eof-clean.marker")
	bGrace := NewStdioBridge(StdioBridgeOptions{
		Name:           "graceful-shutdown-bridge",
		Command:        os.Args[0],
		Args:           []string{"-test.run=^TestHelperStdioMCP$"},
		Env:            map[string]string{"GO_WANT_HELPER_PROCESS": "1", "HELPER_EOF_MARKER": marker},
		RequestTimeout: 5 * time.Second,
		InitTimeout:    5 * time.Second,
		StderrWriter:   io.Discard,
	})
	if err := bGrace.Start(context.Background()); err != nil {
		t.Fatalf("Start bGrace: %v", err)
	}
	bGrace.Shutdown()
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "clean-eof-exit" {
		t.Fatalf("Shutdown killed child before stdin-EOF graceful exit completed: err=%v data=%q", err, string(data))
	}
}
