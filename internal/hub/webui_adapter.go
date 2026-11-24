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
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub/webui"
)

const (
	webUIProbeProtocolVersion = "2025-03-26"
	webUIProbeDefaultTimeout  = 25 * time.Second
	webUIProbeMaxResponseSize = 16 << 20 // 16 MiB
	webUIProbeMaxPages        = 50
)

var (
	_                 webui.Backend = (*supervisorWebUIBackend)(nil)
	snippetTOMLBareRe               = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	errWebUISleeping                = errors.New("service is sleeping")
)

type supervisorWebUIBackend struct {
	sup *Supervisor
}

func newSupervisorWebUIBackend(sup *Supervisor) *supervisorWebUIBackend {
	return &supervisorWebUIBackend{sup: sup}
}

func (a *supervisorWebUIBackend) VerifyControlToken(token string) bool {
	return a.sup.VerifyControlToken(token)
}

func (a *supervisorWebUIBackend) AllowNoAuth() bool {
	return a.sup.AllowNoAuth()
}

func (a *supervisorWebUIBackend) StatusJSON() ([]byte, error) {
	return json.Marshal(a.sup.Status())
}

func (a *supervisorWebUIBackend) CatalogJSON() ([]byte, error) {
	return json.Marshal(a.sup.Catalog())
}

func (a *supervisorWebUIBackend) HealthzJSON() ([]byte, error) {
	return json.Marshal(a.sup.Healthz())
}

func (a *supervisorWebUIBackend) Redact(s string) string {
	if a.sup != nil && a.sup.Redactor() != nil {
		return a.sup.Redactor().Redact(s)
	}
	return s
}

func (a *supervisorWebUIBackend) LogsTail(name string, lines int) ([]webui.LogEntry, bool, error) {
	ring, ok := a.sup.GetRingLog(name)
	if !ok || ring == nil {
		return nil, false, nil
	}
	raw := ring.Tail(lines)
	out := make([]webui.LogEntry, 0, len(raw))
	for _, ln := range raw {
		out = append(out, webui.LogEntry{
			Seq:       ln.Seq,
			Timestamp: ln.Timestamp,
			Stream:    ln.Stream,
			Line:      a.Redact(ln.Text),
		})
	}
	return out, true, nil
}

func (a *supervisorWebUIBackend) SubscribeLogs(name string) (<-chan webui.LogEntry, func(), bool) {
	ring, ok := a.sup.GetRingLog(name)
	if !ok || ring == nil {
		return nil, nil, false
	}
	src, srcUnsub := ring.Subscribe()
	dst := make(chan webui.LogEntry, 64)
	stopCh := make(chan struct{})
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			close(stopCh)
			if srcUnsub != nil {
				srcUnsub()
			}
		})
	}
	go func() {
		defer close(dst)
		for {
			select {
			case <-stopCh:
				return
			case ln, open := <-src:
				if !open {
					return
				}
				entry := webui.LogEntry{
					Seq:       ln.Seq,
					Timestamp: ln.Timestamp,
					Stream:    ln.Stream,
					Line:      a.Redact(ln.Text),
				}
				select {
				case dst <- entry:
				case <-stopCh:
					return
				default:
					// Drop oldest queued entry so slow SSE clients never block RingLog.
					select {
					case <-dst:
					default:
					}
					select {
					case dst <- entry:
					case <-stopCh:
						return
					default:
					}
				}
			}
		}
	}()
	return dst, unsub, true
}

func (a *supervisorWebUIBackend) SubscribeEvents() (<-chan webui.Event, func()) {
	src, srcUnsub := a.sup.SubscribeEvents()
	dst := make(chan webui.Event, 64)
	stopCh := make(chan struct{})
	var once sync.Once
	unsub := func() {
		once.Do(func() {
			close(stopCh)
			if srcUnsub != nil {
				srcUnsub()
			}
		})
	}
	go func() {
		defer close(dst)
		for {
			select {
			case <-stopCh:
				return
			case ev, open := <-src:
				if !open {
					return
				}
				mapped := a.mapHubEvent(ev)
				select {
				case dst <- mapped:
				case <-stopCh:
					return
				default:
					select {
					case <-dst:
					default:
					}
					select {
					case dst <- mapped:
					case <-stopCh:
						return
					default:
					}
				}
			}
		}
	}()
	return dst, unsub
}

func (a *supervisorWebUIBackend) mapHubEvent(ev HubEvent) webui.Event {
	evType := "state_change"
	switch ev.Type {
	case "hub.reload":
		evType = "reload"
	case "service.start":
		evType = "service_started"
	case "service.stop":
		evType = "service_stopped"
	case "service.sleep":
		evType = "service_sleeping"
	case "service.wake":
		evType = "service_woken"
	case "service.state":
		switch ev.State {
		case "degraded":
			evType = "service_degraded"
		case "sleeping":
			evType = "service_sleeping"
		case "stopped":
			evType = "service_stopped"
		default:
			evType = "service_change"
		}
	case "service.restart", "service.reload":
		evType = "service_change"
	}
	msg := ev.Message
	if msg == "" && ev.State != "" {
		msg = ev.State
	}
	ts := ev.Timestamp
	if ts == "" {
		ts = time.Now().UTC().Format(time.RFC3339)
	}
	return webui.Event{
		Type:      evType,
		Service:   ev.Service,
		Message:   a.Redact(msg),
		Timestamp: ts,
	}
}

func (a *supervisorWebUIBackend) StartService(ctx context.Context, name string) ([]byte, bool, error) {
	st, found, err := a.sup.StartService(ctx, name)
	if !found {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(ServiceActionResponse{OK: true, Name: name, Service: st})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) StopService(ctx context.Context, name string) ([]byte, bool, error) {
	st, found, err := a.sup.StopService(ctx, name)
	if !found {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(ServiceActionResponse{OK: true, Name: name, Service: st})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) RestartService(ctx context.Context, name string, waitHealthy bool) ([]byte, bool, error) {
	st, found, err := a.sup.RestartService(ctx, name, waitHealthy)
	if !found {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(RestartResponse{OK: true, Name: name, Service: st})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) ReloadService(ctx context.Context, name string) ([]byte, bool, error) {
	resp, found, err := a.sup.ReloadService(ctx, name)
	if !found {
		return nil, false, nil
	}
	if errors.Is(err, ErrReloadNotSupported) {
		return nil, true, webui.ErrReloadNotSupported
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(ReloadServiceResponse{OK: true, Name: name, Result: resp})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) SleepService(ctx context.Context, name string) ([]byte, bool, error) {
	st, found, err := a.sup.SleepService(ctx, name)
	if !found {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(ServiceActionResponse{OK: true, Name: name, Service: st})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) WakeService(ctx context.Context, name string) ([]byte, bool, error) {
	st, found, err := a.sup.WakeService(ctx, name)
	if !found {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	raw, mErr := json.Marshal(ServiceActionResponse{OK: true, Name: name, Service: st})
	return raw, true, mErr
}

func (a *supervisorWebUIBackend) StartAll(ctx context.Context) ([]byte, error) {
	results := a.sup.StartAllServices(ctx)
	if results == nil {
		results = map[string]ServiceActionResponse{}
	}
	allOK := true
	for _, item := range results {
		if !item.OK {
			allOK = false
			break
		}
	}
	return json.Marshal(BulkServiceActionResponse{OK: allOK, Services: results})
}

func (a *supervisorWebUIBackend) StopAll(ctx context.Context) ([]byte, error) {
	results := a.sup.StopAllServices(ctx)
	if results == nil {
		results = map[string]ServiceActionResponse{}
	}
	allOK := true
	for _, item := range results {
		if !item.OK {
			allOK = false
			break
		}
	}
	return json.Marshal(BulkServiceActionResponse{OK: allOK, Services: results})
}

func (a *supervisorWebUIBackend) RestartAll(ctx context.Context) ([]byte, error) {
	results := a.sup.RestartAllServices(ctx, true)
	if results == nil {
		results = map[string]RestartResponse{}
	}
	allOK := true
	for _, item := range results {
		if !item.OK {
			allOK = false
			break
		}
	}
	return json.Marshal(BulkRestartResponse{OK: allOK, Services: results})
}

func (a *supervisorWebUIBackend) ReloadHub(ctx context.Context) ([]byte, error) {
	res, err := a.sup.ReloadConfig(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

func (a *supervisorWebUIBackend) findCatalogEntry(service string) (CatalogEntry, bool) {
	cat := a.sup.Catalog()
	for _, entry := range cat.Entries {
		if entry.Name == service {
			return entry, true
		}
	}
	return CatalogEntry{}, false
}

func (a *supervisorWebUIBackend) ClientSnippet(service, client string) (string, error) {
	entry, ok := a.findCatalogEntry(service)
	if !ok {
		return "", fmt.Errorf("service %q not found", service)
	}
	if strings.TrimSpace(entry.ClientURL) == "" {
		return "", fmt.Errorf("service %q has no client URL", service)
	}
	return buildClientSnippet(strings.ToLower(strings.TrimSpace(client)), entry)
}

func buildClientSnippet(client string, entry CatalogEntry) (string, error) {
	hasAuth := entry.Auth.Type == "bearer" && strings.TrimSpace(entry.Auth.TokenFile) != ""
	tokenHeader := ""
	if hasAuth {
		tokenHeader = "Bearer <TOKEN_FROM: " + filepath.ToSlash(strings.TrimSpace(entry.Auth.TokenFile)) + ">"
	}
	headers := map[string]string{}
	if hasAuth {
		headers["Authorization"] = tokenHeader
	}

	if client == "codex" {
		key := entry.Name
		if !snippetTOMLBareRe.MatchString(key) {
			key = strconv.Quote(key)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", key)
		fmt.Fprintf(&b, "url = %s\n", strconv.Quote(entry.ClientURL))
		if hasAuth {
			fmt.Fprintf(&b, "http_headers = { Authorization = %s }\n", strconv.Quote(tokenHeader))
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}

	server := map[string]any{}
	var root map[string]any
	switch client {
	case "claude":
		server["type"] = "http"
		server["url"] = entry.ClientURL
		root = map[string]any{"mcpServers": map[string]any{entry.Name: server}}
	case "cursor", "devin":
		server["url"] = entry.ClientURL
		root = map[string]any{"mcpServers": map[string]any{entry.Name: server}}
	case "vscode":
		server["type"] = "http"
		server["url"] = entry.ClientURL
		root = map[string]any{"servers": map[string]any{entry.Name: server}}
	case "agy", "windsurf":
		server["serverUrl"] = entry.ClientURL
		root = map[string]any{"mcpServers": map[string]any{entry.Name: server}}
	case "opencode":
		server["type"] = "remote"
		server["url"] = entry.ClientURL
		server["oauth"] = false
		root = map[string]any{"mcp": map[string]any{entry.Name: server}}
	case "gemini":
		server["httpUrl"] = entry.ClientURL
		root = map[string]any{"mcpServers": map[string]any{entry.Name: server}}
	default:
		return "", fmt.Errorf("unsupported client %q", client)
	}

	if hasAuth {
		server["headers"] = headers
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return "", err
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func (a *supervisorWebUIBackend) scrubProbeError(msg, token string) string {
	if token != "" {
		msg = strings.NewReplacer(
			url.QueryEscape(token), RedactedPlaceholder,
			url.PathEscape(token), RedactedPlaceholder,
			token, RedactedPlaceholder,
		).Replace(msg)
	}
	return a.Redact(msg)
}

func (a *supervisorWebUIBackend) ProbeTools(ctx context.Context, service string, wake bool) (webui.ToolsProbeResult, error) {
	entry, ok := a.findCatalogEntry(service)
	if !ok {
		return webui.ToolsProbeResult{Service: service}, fmt.Errorf("service %q not found", service)
	}
	endpoint := strings.TrimSpace(entry.ClientURL)
	if endpoint == "" {
		return webui.ToolsProbeResult{Service: service}, fmt.Errorf("service %q has no MCP endpoint URL", service)
	}

	if !wake {
		if entry.State == "sleeping" {
			return webui.ToolsProbeResult{
				Service:  service,
				Endpoint: a.Redact(endpoint),
				Tools:    []webui.ToolInfo{},
				Skipped:  true,
				Reason:   "service is sleeping",
			}, nil
		}
		if entry.State == "stopped" {
			return webui.ToolsProbeResult{
				Service:  service,
				Endpoint: a.Redact(endpoint),
				Tools:    []webui.ToolInfo{},
				Skipped:  true,
				Reason:   "service is stopped",
			}, nil
		}
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, webUIProbeDefaultTimeout)
		defer cancel()
	}

	if wake && entry.State == "sleeping" {
		// Direct HTTP services need explicit WakeService when not routed through a wake-capable gateway/proxy.
		if entry.ViaGateway == "" && entry.ViaProxy == "" {
			if _, found, err := a.sup.WakeService(ctx, service); found && err != nil {
				return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("wake service: %s", a.Redact(err.Error()))
			}
		}
	}

	var token string
	if entry.Auth.Type == "bearer" && strings.TrimSpace(entry.Auth.TokenFile) != "" {
		tok, err := ReadSecretFile(entry.Auth.TokenFile)
		if err != nil {
			return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("read service token file: %s", a.Redact(err.Error()))
		}
		token = tok
	}

	start := time.Now()
	client := &webUIMCPProbeClient{
		httpc:  NewDirectClient(0),
		url:    endpoint,
		token:  token,
		noWake: !wake,
	}
	defer client.close()

	initEnv, err := client.post(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": webUIProbeProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "mcp-hub-ui", "version": Version},
		},
	}, "1")
	if err != nil {
		if errors.Is(err, errWebUISleeping) {
			return webui.ToolsProbeResult{
				Service:  service,
				Endpoint: a.Redact(endpoint),
				Tools:    []webui.ToolInfo{},
				Skipped:  true,
				Reason:   "service is sleeping",
			}, nil
		}
		return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("initialize: %s", a.scrubProbeError(err.Error(), token))
	}
	if initEnv.Error != nil {
		return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("initialize: JSON-RPC %d: %s", initEnv.Error.Code, a.scrubProbeError(initEnv.Error.Message, token))
	}

	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(initEnv.Result, &initRes)
	client.proto = strings.TrimSpace(initRes.ProtocolVersion)
	if client.proto == "" {
		client.proto = webUIProbeProtocolVersion
	}

	if _, err := client.post(ctx, map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}, ""); err != nil {
		return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("notifications/initialized: %s", a.scrubProbeError(err.Error(), token))
	}

	tools := make([]webui.ToolInfo, 0)
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; page < webUIProbeMaxPages; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		id := page + 2
		env, err := client.post(ctx, map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/list",
			"params":  params,
		}, strconv.Itoa(id))
		if err != nil {
			return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("tools/list: %s", a.scrubProbeError(err.Error(), token))
		}
		if env.Error != nil {
			return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("tools/list: JSON-RPC %d: %s", env.Error.Code, a.scrubProbeError(env.Error.Message, token))
		}
		var lr struct {
			Tools []struct {
				Name         string          `json:"name"`
				Description  string          `json:"description"`
				ReadOnlyHint *bool           `json:"readOnlyHint"`
				InputSchema  json.RawMessage `json:"inputSchema"`
				Annotations  *struct {
					ReadOnlyHint *bool `json:"readOnlyHint"`
				} `json:"annotations"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(env.Result, &lr); err != nil {
			return webui.ToolsProbeResult{Service: service, Endpoint: a.Redact(endpoint)}, fmt.Errorf("decode tools/list: %s", a.scrubProbeError(err.Error(), token))
		}
		for _, wt := range lr.Tools {
			var roHint *bool
			if wt.Annotations != nil && wt.Annotations.ReadOnlyHint != nil {
				v := *wt.Annotations.ReadOnlyHint
				roHint = &v
			} else if wt.ReadOnlyHint != nil {
				v := *wt.ReadOnlyHint
				roHint = &v
			}
			var schema json.RawMessage
			if trimmed := bytes.TrimSpace(wt.InputSchema); len(trimmed) > 0 && json.Valid(trimmed) {
				if a.sup != nil && a.sup.Redactor() != nil {
					if redacted, rErr := a.sup.Redactor().RedactJSON(trimmed); rErr == nil {
						schema = redacted
					} else {
						schema = append(json.RawMessage(nil), trimmed...)
					}
				} else {
					schema = append(json.RawMessage(nil), trimmed...)
				}
			}
			tools = append(tools, webui.ToolInfo{
				Name:         a.scrubProbeError(wt.Name, token),
				Description:  a.scrubProbeError(wt.Description, token),
				ReadOnlyHint: roHint,
				InputSchema:  schema,
			})
		}
		next := strings.TrimSpace(lr.NextCursor)
		if next == "" || seenCursors[next] {
			break
		}
		seenCursors[next] = true
		cursor = next
	}

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return webui.ToolsProbeResult{
		Service:   service,
		Endpoint:  a.Redact(endpoint),
		Tools:     tools,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

type webUIRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type webUIRPCEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *webUIRPCError  `json:"error"`
}

type webUIMCPProbeClient struct {
	httpc   *http.Client
	url     string
	token   string
	session string
	proto   string
	noWake  bool
}

func (c *webUIMCPProbeClient) newRequest(ctx context.Context, method string, body []byte) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.noWake {
		req.Header.Set("X-McpHub-No-Wake", "1")
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.proto != "" {
		req.Header.Set("MCP-Protocol-Version", c.proto)
	}
	return req, nil
}

func (c *webUIMCPProbeClient) post(ctx context.Context, payload map[string]any, wantID string) (*webUIRPCEnvelope, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodPost, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if c.noWake && resp.StatusCode == http.StatusConflict {
			var errObj struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(snippet, &errObj) == nil && errObj.Error == "service_sleeping" {
				return nil, errWebUISleeping
			}
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if payload["method"] == "initialize" {
		c.session = strings.TrimSpace(resp.Header.Get("Mcp-Session-Id"))
	}

	limited := io.LimitReader(resp.Body, webUIProbeMaxResponseSize)
	if wantID == "" {
		_, _ = io.Copy(io.Discard, limited)
		return nil, nil
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		return readWebUISSEResponse(limited, wantID)
	}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	env, ok := findWebUIRPCResponse(raw, wantID)
	if !ok {
		return nil, fmt.Errorf("no JSON-RPC response with id %s", wantID)
	}
	return env, nil
}

func (c *webUIMCPProbeClient) close() {
	if c.session == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodDelete, nil)
	if err != nil {
		return
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	c.session = ""
}

func findWebUIRPCResponse(raw []byte, wantID string) (*webUIRPCEnvelope, bool) {
	raw = bytes.TrimSpace(bytes.TrimPrefix(raw, utf8BOM))
	if len(raw) == 0 {
		return nil, false
	}
	var list []webUIRPCEnvelope
	if raw[0] == '[' {
		if json.Unmarshal(raw, &list) != nil {
			return nil, false
		}
	} else {
		var one webUIRPCEnvelope
		if json.Unmarshal(raw, &one) != nil {
			return nil, false
		}
		list = []webUIRPCEnvelope{one}
	}
	for i := range list {
		if strings.Trim(strings.TrimSpace(string(list[i].ID)), `"`) == wantID {
			return &list[i], true
		}
	}
	return nil, false
}

func readWebUISSEResponse(r io.Reader, wantID string) (*webUIRPCEnvelope, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	var data []string
	flush := func() *webUIRPCEnvelope {
		if len(data) == 0 {
			return nil
		}
		joined := strings.Join(data, "\n")
		data = data[:0]
		if env, ok := findWebUIRPCResponse([]byte(joined), wantID); ok {
			return env
		}
		return nil
	}
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if env := flush(); env != nil {
					return env, nil
				}
			} else if strings.HasPrefix(trimmed, "data:") {
				val := strings.TrimPrefix(trimmed, "data:")
				val = strings.TrimPrefix(val, " ")
				data = append(data, val)
			}
		}
		if err != nil {
			if env := flush(); env != nil {
				return env, nil
			}
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("SSE stream ended without a response for id %s", wantID)
			}
			return nil, err
		}
	}
}
