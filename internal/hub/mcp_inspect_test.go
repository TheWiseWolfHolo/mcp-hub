package hub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPeekMCPRequest_SingleAndRestoresBody(t *testing.T) {
	t.Run("tools/call preserves exact body", func(t *testing.T) {
		raw := `{"jsonrpc":"2.0","id":"req-42","method":"tools/call","params":{"name":"read_doc","arguments":{"secret":"do-not-log"}}}`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")

		info, err := PeekMCPRequest(req, 0)
		if err != nil {
			t.Fatalf("PeekMCPRequest error: %v", err)
		}
		if !info.IsJSONRPC || info.IsBatch {
			t.Fatalf("unexpected peek flags: %+v", info)
		}
		if info.Method != "tools/call" || info.ToolName != "read_doc" || !info.HasToolsCall || info.HasToolsList {
			t.Fatalf("unexpected peek content: %+v", info)
		}
		if string(info.ID) != `"req-42"` {
			t.Fatalf("expected id \"req-42\", got %s", string(info.ID))
		}
		if len(info.ToolCalls) != 1 || info.ToolCalls[0].Name != "read_doc" || string(info.ToolCalls[0].ID) != `"req-42"` {
			t.Fatalf("unexpected ToolCalls: %+v", info.ToolCalls)
		}

		restored, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(restored) != raw {
			t.Fatalf("restored body mismatch:\ngot:  %q\nwant: %q", string(restored), raw)
		}
	})

	t.Run("tools/list and UTF-8 BOM preserved byte-for-byte", func(t *testing.T) {
		rawBytes := append([]byte{0xEF, 0xBB, 0xBF}, []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)...)
		req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(rawBytes))
		req.Header.Set("Content-Type", "application/json")

		info, err := PeekMCPRequest(req, 0)
		if err != nil {
			t.Fatalf("PeekMCPRequest error: %v", err)
		}
		if !info.IsJSONRPC || info.Method != "tools/list" || !info.HasToolsList || info.HasToolsCall || string(info.ID) != "7" {
			t.Fatalf("unexpected peek: %+v", info)
		}
		restored, _ := io.ReadAll(req.Body)
		if !bytes.Equal(restored, rawBytes) {
			t.Fatalf("BOM body was altered")
		}
	})

	t.Run("non-POST or non-JSON does not touch body", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			method string
			ct     string
		}{
			{"GET", http.MethodGet, "application/json"},
			{"text/plain", http.MethodPost, "text/plain"},
			{"missing ct", http.MethodPost, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				touched := false
				body := &trackReader{r: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), onRead: func() { touched = true }}
				req := httptest.NewRequest(tc.method, "/mcp", body)
				if tc.ct != "" {
					req.Header.Set("Content-Type", tc.ct)
				}
				info, err := PeekMCPRequest(req, 0)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if info.IsJSONRPC || touched {
					t.Fatalf("expected untouched empty peek, got info=%+v touched=%v", info, touched)
				}
			})
		}
	})
}

type trackReader struct {
	r      io.Reader
	onRead func()
}

func (tr *trackReader) Read(p []byte) (int, error) {
	if tr.onRead != nil {
		tr.onRead()
	}
	return tr.r.Read(p)
}

func (tr *trackReader) Close() error { return nil }

func TestPeekMCPRequest_BatchAndTruncated(t *testing.T) {
	t.Run("batch request extracts all tool calls and list flag", func(t *testing.T) {
		raw := `[
			{"jsonrpc":"2.0","method":"notifications/initialized"},
			{"jsonrpc":"2.0","id":1,"method":"tools/list"},
			{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"alpha","arguments":{}}},
			{"jsonrpc":"2.0","id":"three","method":"tools/call","params":{"name":"beta","arguments":{"k":1}}}
		]`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")

		info, err := PeekMCPRequest(req, 0)
		if err != nil {
			t.Fatalf("PeekMCPRequest error: %v", err)
		}
		if !info.IsJSONRPC || !info.IsBatch || !info.HasToolsList || !info.HasToolsCall {
			t.Fatalf("unexpected batch info: %+v", info)
		}
		if len(info.ToolCalls) != 2 {
			t.Fatalf("expected 2 tool calls, got %d: %+v", len(info.ToolCalls), info.ToolCalls)
		}
		if info.ToolCalls[0].Name != "alpha" || string(info.ToolCalls[0].ID) != "2" {
			t.Fatalf("unexpected ToolCalls[0]: %+v", info.ToolCalls[0])
		}
		if info.ToolCalls[1].Name != "beta" || string(info.ToolCalls[1].ID) != `"three"` {
			t.Fatalf("unexpected ToolCalls[1]: %+v", info.ToolCalls[1])
		}
		restored, _ := io.ReadAll(req.Body)
		if string(restored) != raw {
			t.Fatalf("restored batch body mismatch")
		}
	})

	t.Run("truncated oversized tools/call still parses fields and restores full payload", func(t *testing.T) {
		huge := strings.Repeat(`x\"method\":\"tools/list\"`, 3000) // ~72 KiB decoy inside arguments
		raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":99,"method":"tools/call","params":{"name":"write_big","arguments":{"blob":"%s"}}}`, huge)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")

		info, err := PeekMCPRequest(req, 512)
		if err != nil {
			t.Fatalf("PeekMCPRequest error: %v", err)
		}
		if !info.IsJSONRPC || info.Method != "tools/call" || info.ToolName != "write_big" || !info.HasToolsCall || info.HasToolsList {
			t.Fatalf("unexpected truncated peek: %+v", info)
		}
		if string(info.ID) != "99" {
			t.Fatalf("expected id 99, got %s", string(info.ID))
		}
		restored, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("read restored body: %v", err)
		}
		if string(restored) != raw {
			t.Fatalf("restored oversized body length mismatch: got %d want %d", len(restored), len(raw))
		}
	})

	t.Run("truncated batch still parses complete prefix and partial tail", func(t *testing.T) {
		huge := strings.Repeat("A", 4096)
		raw := fmt.Sprintf(`[{"jsonrpc":"2.0","id":1,"method":"tools/list"},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"tail_tool","arguments":{"data":"%s"}}}]`, huge)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")

		info, err := PeekMCPRequest(req, 256)
		if err != nil {
			t.Fatalf("PeekMCPRequest error: %v", err)
		}
		if !info.IsJSONRPC || !info.IsBatch || !info.HasToolsList || !info.HasToolsCall {
			t.Fatalf("unexpected truncated batch peek: %+v", info)
		}
		if len(info.ToolCalls) != 1 || info.ToolCalls[0].Name != "tail_tool" || string(info.ToolCalls[0].ID) != "2" {
			t.Fatalf("unexpected ToolCalls from truncated batch: %+v", info.ToolCalls)
		}
		restored, _ := io.ReadAll(req.Body)
		if string(restored) != raw {
			t.Fatalf("restored truncated batch mismatch")
		}
	})

	t.Run("malformed JSON never fails request and restores body", func(t *testing.T) {
		raw := `not valid json at all`
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		info, err := PeekMCPRequest(req, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if info.IsJSONRPC {
			t.Fatalf("expected IsJSONRPC=false for malformed JSON")
		}
		restored, _ := io.ReadAll(req.Body)
		if string(restored) != raw {
			t.Fatalf("restored malformed body mismatch")
		}
	})
}

func TestParseStrictMCPRequest_RejectsFoldedEnvelopeKey(t *testing.T) {
	_, err := ParseStrictMCPRequest([]byte(`{"jsonrpc":"2.0","method":"ping","param\u017f":{}}`))
	if err == nil || !strings.Contains(err.Error(), "non-canonical JSON-RPC key") {
		t.Fatalf("expected a non-canonical envelope key error, got %v", err)
	}
}

func TestToolFilter_Allows(t *testing.T) {
	tr := true
	fl := false

	if (ToolFilter{}).Active() {
		t.Fatal("empty ToolFilter should not be Active")
	}
	if !(ToolFilter{}).Allows("anything", nil) {
		t.Fatal("empty ToolFilter should allow everything")
	}

	f := ToolFilter{
		AllowTools: []string{"read_*", "item_?", "fs/*", "bad[bracket*"},
		DenyTools:  []string{"read_secret", "fs/delete*"},
	}
	if !f.Active() {
		t.Fatal("filter should be active")
	}

	cases := []struct {
		name string
		hint *bool
		want bool
	}{
		{"read_file", nil, true},
		{"read_secret", nil, false}, // deny overrides allow
		{"READ_FILE", nil, false},   // case-sensitive
		{"item_1", nil, true},
		{"item_12", nil, false},
		{"fs/read/a", nil, true},
		{"fs/delete_all", nil, false},
		{"bad[bracket_ok", nil, true}, // safe fallback for invalid path.Match bracket
		{"other_tool", nil, false},
	}
	for _, tc := range cases {
		if got := f.Allows(tc.name, tc.hint); got != tc.want {
			t.Errorf("Allows(%q, %v) = %v, want %v", tc.name, tc.hint, got, tc.want)
		}
	}

	ro := ToolFilter{ReadOnlyOnly: true}
	if !ro.Active() {
		t.Fatal("ReadOnlyOnly filter should be active")
	}
	if ro.Allows("read_doc", nil) {
		t.Fatal("ReadOnlyOnly must fail-closed when readOnlyHint is nil")
	}
	if ro.Allows("read_doc", &fl) {
		t.Fatal("ReadOnlyOnly must deny readOnlyHint=false")
	}
	if !ro.Allows("read_doc", &tr) {
		t.Fatal("ReadOnlyOnly must allow readOnlyHint=true")
	}
}

func TestToolFilter_WriteToolDeniedJSONRPC(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteToolDeniedJSONRPC(rec, json.RawMessage(`"call-7"`), "delete_db")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("unexpected Content-Type: %q", rec.Header().Get("Content-Type"))
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	var resp struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, rec.Body.String())
	}
	if resp.JSONRPC != "2.0" || string(resp.ID) != `"call-7"` || resp.Error.Code != -32601 {
		t.Fatalf("unexpected error payload: %+v", resp)
	}
	wantMsg := `tool "delete_db" is not permitted for this token/profile`
	if resp.Error.Message != wantMsg {
		t.Fatalf("message = %q, want %q", resp.Error.Message, wantMsg)
	}

	// Empty/invalid ID falls back to null.
	rec2 := httptest.NewRecorder()
	WriteToolDeniedJSONRPC(rec2, json.RawMessage(`{broken`), "rm_all")
	var resp2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("unmarshal fallback id response: %v", err)
	}
	if resp2["id"] != nil {
		t.Fatalf("expected null id fallback, got %#v", resp2["id"])
	}
}

func TestToolFilter_FilterToolsListJSON(t *testing.T) {
	input := `{
		"jsonrpc":"2.0",
		"id":1,
		"extraEnvelope":"keep-me",
		"result":{
			"nextCursor":"page-2",
			"customMeta":{"v":1},
			"tools":[
				{"name":"read_note","description":"<read>","annotations":{"readOnlyHint":true},"inputSchema":{"type":"object"}},
				{"name":"write_note","description":"write","annotations":{"readOnlyHint":false}},
				{"name":"mystery_op","description":"no annotations"}
			]
		}
	}`

	// ReadOnlyOnly keeps only read_note and preserves all extra fields.
	out, mod, err := FilterToolsListJSON([]byte(input), ToolFilter{ReadOnlyOnly: true})
	if err != nil || !mod {
		t.Fatalf("expected modified=true, err=nil; got mod=%v err=%v", mod, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal filtered json: %v", err)
	}
	if parsed["extraEnvelope"] != "keep-me" {
		t.Fatalf("lost extraEnvelope: %v", parsed)
	}
	res := parsed["result"].(map[string]any)
	if res["nextCursor"] != "page-2" {
		t.Fatalf("lost nextCursor: %v", res)
	}
	tools := res["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "read_note" {
		t.Fatalf("unexpected tools after ReadOnlyOnly filter: %v", tools)
	}

	// When all tools match, modified must be false.
	unchanged, mod, err := FilterToolsListJSON([]byte(input), ToolFilter{AllowTools: []string{"*"}})
	if err != nil || mod || string(unchanged) != input {
		t.Fatalf("expected unchanged passthrough, got mod=%v err=%v", mod, err)
	}

	// When all tools are filtered out, tools must be [] (not null).
	emptyOut, mod, err := FilterToolsListJSON([]byte(input), ToolFilter{DenyTools: []string{"*"}})
	if err != nil || !mod {
		t.Fatalf("expected modified=true, got mod=%v err=%v", mod, err)
	}
	if !strings.Contains(string(emptyOut), `"tools":[]`) {
		t.Fatalf("expected empty JSON array for tools, got: %s", string(emptyOut))
	}

	// Batch JSON-RPC response.
	batchIn := `[
		{"jsonrpc":"2.0","id":1,"result":{"pong":true}},
		{"jsonrpc":"2.0","id":2,"result":{"nextCursor":"c3","tools":[{"name":"ok_tool"},{"name":"bad_tool"}]}}
	]`
	batchOut, mod, err := FilterToolsListJSON([]byte(batchIn), ToolFilter{DenyTools: []string{"bad_*"}})
	if err != nil || !mod {
		t.Fatalf("batch filter failed: mod=%v err=%v", mod, err)
	}
	var batchParsed []map[string]any
	if err := json.Unmarshal(batchOut, &batchParsed); err != nil || len(batchParsed) != 2 {
		t.Fatalf("unmarshal filtered batch: %v (%s)", err, string(batchOut))
	}
	if batchParsed[0]["result"].(map[string]any)["pong"] != true {
		t.Fatalf("non-tools/list batch item was mutated")
	}
	bTools := batchParsed[1]["result"].(map[string]any)["tools"].([]any)
	if len(bTools) != 1 || bTools[0].(map[string]any)["name"] != "ok_tool" {
		t.Fatalf("unexpected batch filtered tools: %v", bTools)
	}

	// Fail closed on unparseable/non-object entries when AllowTools OR ReadOnlyOnly is active.
	dirtyList := `{"jsonrpc":"2.0","id":3,"result":{"tools":[{"name":"good_tool","annotations":{"readOnlyHint":true}},null,123,"bad",{"name":999},{"description":"missing name"}]}}`
	for _, tc := range []struct {
		name   string
		filter ToolFilter
	}{
		{"AllowTools wildcard", ToolFilter{AllowTools: []string{"*"}}},
		{"ReadOnlyOnly", ToolFilter{ReadOnlyOnly: true}},
	} {
		outDirty, modDirty, errDirty := FilterToolsListJSON([]byte(dirtyList), tc.filter)
		if errDirty != nil || !modDirty {
			t.Fatalf("%s: expected modified=true err=nil, got mod=%v err=%v", tc.name, modDirty, errDirty)
		}
		var parsedDirty map[string]any
		if err := json.Unmarshal(outDirty, &parsedDirty); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		dt := parsedDirty["result"].(map[string]any)["tools"].([]any)
		if len(dt) != 1 || dt[0].(map[string]any)["name"] != "good_tool" {
			t.Fatalf("%s: expected only good_tool, got %v", tc.name, dt)
		}
	}
}

func TestToolFilter_FilterToolsListSSE(t *testing.T) {
	t.Run("LF multi-line data preserves comments and metadata", func(t *testing.T) {
		sse := ": keepalive comment\n" +
			"event: message\n" +
			"id: evt-1\n" +
			"retry: 3000\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\n" +
			"data: \"nextCursor\":\"n1\",\"tools\":[{\"name\":\"keep_a\",\"annotations\":{\"readOnlyHint\":true}},{\"name\":\"drop_b\"}]}}\n" +
			"\n" +
			"event: ping\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n" +
			"\n"

		out, mod, err := FilterToolsListSSE([]byte(sse), ToolFilter{DenyTools: []string{"drop_*"}})
		if err != nil || !mod {
			t.Fatalf("expected mod=true err=nil, got mod=%v err=%v", mod, err)
		}
		got := string(out)
		for _, want := range []string{": keepalive comment\n", "event: message\n", "id: evt-1\n", "retry: 3000\n", "event: ping\n", `"nextCursor":"n1"`, `"keep_a"`} {
			if !strings.Contains(got, want) {
				t.Fatalf("filtered SSE missing %q:\n%s", want, got)
			}
		}
		if strings.Contains(got, "drop_b") {
			t.Fatalf("filtered SSE still contains drop_b:\n%s", got)
		}
		if strings.Count(got, "data: ") != 2 {
			t.Fatalf("expected multi-line data collapsed to 1 data line in event 1 + 1 in event 2, got:\n%s", got)
		}
	})

	t.Run("CRLF delimiter preserved", func(t *testing.T) {
		sse := "event: message\r\n" +
			"id: 9\r\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"safe\",\"annotations\":{\"readOnlyHint\":true}},{\"name\":\"unsafe\",\"annotations\":{\"readOnlyHint\":false}}]}}\r\n" +
			"\r\n"
		out, mod, err := FilterToolsListSSE([]byte(sse), ToolFilter{ReadOnlyOnly: true})
		if err != nil || !mod {
			t.Fatalf("expected mod=true err=nil, got mod=%v err=%v", mod, err)
		}
		got := string(out)
		if !strings.Contains(got, "\r\n\r\n") {
			t.Fatalf("expected CRLF event delimiter preserved, got %q", got)
		}
		if !strings.Contains(got, `"safe"`) || strings.Contains(got, `"unsafe"`) {
			t.Fatalf("unexpected CRLF SSE output: %q", got)
		}
	})

	t.Run("untouched SSE returns unmodified bytes", func(t *testing.T) {
		sse := ": comment\ndata: hello\n\n"
		out, mod, err := FilterToolsListSSE([]byte(sse), ToolFilter{DenyTools: []string{"foo"}})
		if err != nil || mod || string(out) != sse {
			t.Fatalf("expected unmodified SSE, got mod=%v err=%v out=%q", mod, err, string(out))
		}
	})

	t.Run("filters the first data field after a BOM", func(t *testing.T) {
		sse := "\ufeffdata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"tools\":[{\"name\":\"keep\"},{\"name\":\"drop\"}]}}\n\n"
		out, mod, err := FilterToolsListSSE([]byte(sse), ToolFilter{AllowTools: []string{"keep"}})
		if err != nil || !mod || !bytes.Contains(out, []byte(`"keep"`)) || bytes.Contains(out, []byte(`"drop"`)) {
			t.Fatalf("unexpected filtered SSE: mod=%v err=%v out=%q", mod, err, out)
		}
	})
}

func TestMCPMetrics_RecorderAndReverseProxySSE(t *testing.T) {
	metrics := NewMCPMetrics()

	// 1. Live ReverseProxy with FlushInterval: -1 proving non-buffering SSE streaming + ActiveSSE gauge.
	chunk1Sent := make(chan struct{})
	allowChunk2 := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sess-live-1")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"step\":1}}\n\n")
		fl.Flush()
		close(chunk1Sent)
		<-allowChunk2
		_, _ = io.WriteString(w, "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"step\":2}}\n\n")
		fl.Flush()
	}))
	defer upstream.Close()

	targetURL, _ := url.Parse(upstream.URL)
	rp := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
		},
	}

	var (
		sessMu     sync.Mutex
		sessEvents []string
	)
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peek, _ := PeekMCPRequest(r, 0)
		rec, finish := metrics.WrapHTTP(w, r, peek, 1, func(id string, closed bool) {
			sessMu.Lock()
			sessEvents = append(sessEvents, fmt.Sprintf("%s:%v", id, closed))
			sessMu.Unlock()
		})
		defer finish()
		if rec.Unwrap() != w {
			t.Errorf("Unwrap() did not return underlying ResponseWriter")
		}
		rp.ServeHTTP(rec, r)
	}))
	defer proxySrv.Close()

	req, _ := http.NewRequest(http.MethodPost, proxySrv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stream_tool"}}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE request failed: %v", err)
	}
	<-chunk1Sent

	// Chunk 1 must arrive IMMEDIATELY before chunk 2 is unblocked (proving unbuffered flush!).
	buf := make([]byte, 256)
	readDone := make(chan string, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		readDone <- string(buf[:n])
	}()
	select {
	case first := <-readDone:
		if !strings.Contains(first, `"step":1`) {
			t.Fatalf("unexpected first SSE chunk: %q", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SSE chunk 1 was buffered instead of flushed immediately")
	}

	// While SSE is open, ActiveSSE and ActiveRequests must be 1.
	snapDuring := metrics.Snapshot()
	if snapDuring.ActiveSSE != 1 || snapDuring.ActiveRequests != 1 {
		t.Fatalf("expected ActiveSSE=1 ActiveRequests=1 while streaming, got %+v", snapDuring)
	}
	if known, stale := metrics.CheckSessionEpoch("sess-live-1", 1); !known || stale {
		t.Fatalf("expected sess-live-1 known=true stale=false at epoch 1, got known=%v stale=%v", known, stale)
	}
	if known, stale := metrics.CheckSessionEpoch("sess-live-1", 2); !known || !stale {
		t.Fatalf("expected sess-live-1 known=true stale=true at epoch 2, got known=%v stale=%v", known, stale)
	}

	close(allowChunk2)
	rest, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(rest), `"step":2`) {
		t.Fatalf("missing second SSE chunk: %q", string(rest))
	}

	// Wait briefly for server handler defer finish() to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := metrics.Snapshot()
		if s.ActiveRequests == 0 && s.ActiveSSE == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapAfter := metrics.Snapshot()
	if snapAfter.ActiveSSE != 0 || snapAfter.ActiveRequests != 0 || snapAfter.TotalRequests != 1 || snapAfter.ErrorRequests != 0 {
		t.Fatalf("unexpected post-SSE metrics: %+v", snapAfter)
	}
	if snapAfter.LastTool != "stream_tool" || snapAfter.Tools["stream_tool"].Calls != 1 || snapAfter.Tools["stream_tool"].Errors != 0 {
		t.Fatalf("unexpected tool metrics: %+v", snapAfter.Tools)
	}

	// 2. Error detection: JSON-RPC error, isError:true, and non-error containing literal word "error".
	runSynthetic := func(method, sessHdr, toolName string, status int, body string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, "/mcp", nil)
		if sessHdr != "" {
			r.Header.Set("Mcp-Session-Id", sessHdr)
		}
		peek := MCPPeekInfo{
			IsJSONRPC:    true,
			Method:       "tools/call",
			ToolName:     toolName,
			HasToolsCall: toolName != "",
		}
		if toolName != "" {
			peek.ToolCalls = []MCPToolCallRef{{ID: json.RawMessage("1"), Name: toolName}}
		}
		rec, finish := metrics.WrapHTTP(w, r, peek, 1, func(id string, closed bool) {
			sessMu.Lock()
			sessEvents = append(sessEvents, fmt.Sprintf("%s:%v", id, closed))
			sessMu.Unlock()
		})
		rec.Header().Set("Content-Type", "application/json")
		rec.WriteHeader(status)
		_, _ = rec.Write([]byte(body))
		finish()
	}

	runSynthetic(http.MethodPost, "", "err_rpc", 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`)
	runSynthetic(http.MethodPost, "", "err_tool", 200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"boom"}],"isError":true}}`)
	runSynthetic(http.MethodPost, "", "ok_text_error", 200, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"0 error lines found"}],"isError":false}}`)

	snapErr := metrics.Snapshot()
	if snapErr.Tools["err_rpc"].Errors != 1 {
		t.Fatalf("expected err_rpc error count 1, got %+v", snapErr.Tools["err_rpc"])
	}
	if snapErr.Tools["err_tool"].Errors != 1 {
		t.Fatalf("expected err_tool error count 1, got %+v", snapErr.Tools["err_tool"])
	}
	if snapErr.Tools["ok_text_error"].Errors != 0 {
		t.Fatalf("false positive error on ok_text_error: %+v", snapErr.Tools["ok_text_error"])
	}

	// 3. DELETE and upstream 404 session cleanup.
	runSynthetic(http.MethodDelete, "sess-live-1", "", 200, `{"ok":true}`)
	if metrics.ActiveSessionCount() != 0 {
		t.Fatalf("expected sess-live-1 deleted on DELETE, count=%d", metrics.ActiveSessionCount())
	}

	metrics.TrackSessionEpoch("sess-404", 1)
	runSynthetic(http.MethodPost, "sess-404", "", 404, `{"error":"session_not_found"}`)
	if metrics.ActiveSessionCount() != 0 {
		t.Fatalf("expected sess-404 deleted on 404, count=%d", metrics.ActiveSessionCount())
	}

	// 4. Bounded session (256) and tool (512) maps.
	for i := 0; i < 300; i++ {
		metrics.TrackSessionEpoch(fmt.Sprintf("s-%d", i), 1)
	}
	if got := metrics.ActiveSessionCount(); got != maxTrackedSessions {
		t.Fatalf("session map exceeded cap: got %d want %d", got, maxTrackedSessions)
	}
	metrics.ClearSessions()
	if got := metrics.ActiveSessionCount(); got != 0 {
		t.Fatalf("ClearSessions left %d sessions", got)
	}

	for i := 0; i < 550; i++ {
		runSynthetic(http.MethodPost, "", fmt.Sprintf("tool_%d", i), 200, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}
	if got := len(metrics.Snapshot().Tools); got != maxTrackedTools {
		t.Fatalf("tool metrics map exceeded cap: got %d want %d", got, maxTrackedTools)
	}
}
