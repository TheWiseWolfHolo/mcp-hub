package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	urlpath "path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxProxyRequestBodyBytes int64 = 32 << 20 // 32 MiB
	maxToolsListBufferBytes        = 8 << 20  // 8 MiB
	defaultHealthCacheTTL          = 1 * time.Second
	noWakeHeader                   = "X-McpHub-No-Wake"
)

type proxyFilterContextKey struct{}

type proxyResponseFilter struct {
	filter    ToolFilter
	toolsList bool
	epoch     uint64
}

var errToolsListTooLarge = errors.New("tools_list_too_large")

var errToolsListFilterFailed = errors.New("tools_list_filter_failed")

// ProxyStatus reports runtime metadata for one configured reverse proxy.
type ProxyStatus struct {
	Name            string              `json:"name"`
	ListenAddr      string              `json:"listenAddr"`
	TargetURL       string              `json:"targetUrl"`
	Running         bool                `json:"running"`
	UpstreamService string              `json:"upstreamService,omitempty"`
	Metrics         *MCPMetricsSnapshot `json:"metrics,omitempty"`
}

// upstreamHealthCache deduplicates concurrent upstream health probes via singleflight
// and caches the probe result for ttl (default 1s).
type upstreamHealthCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	cachedAt time.Time
	cachedOK bool
	inFlight chan struct{}
}

func newUpstreamHealthCache(ttl time.Duration) *upstreamHealthCache {
	return &upstreamHealthCache{ttl: ttl}
}

func (c *upstreamHealthCache) invalidate() {
	c.mu.Lock()
	c.cachedAt = time.Time{}
	c.mu.Unlock()
}

func (c *upstreamHealthCache) check(ctx context.Context, probeFn func(context.Context) bool) bool {
	for {
		c.mu.Lock()
		if c.ttl > 0 && !c.cachedAt.IsZero() && time.Since(c.cachedAt) < c.ttl {
			ok := c.cachedOK
			c.mu.Unlock()
			return ok
		}
		if c.inFlight != nil {
			ch := c.inFlight
			c.mu.Unlock()
			select {
			case <-ch:
				c.mu.Lock()
				ok := c.cachedOK
				c.mu.Unlock()
				return ok
			case <-ctx.Done():
				return false
			}
		}
		ch := make(chan struct{})
		c.inFlight = ch
		c.mu.Unlock()

		ok := probeFn(ctx)

		c.mu.Lock()
		c.cachedOK = ok
		c.cachedAt = time.Now()
		c.inFlight = nil
		close(ch)
		c.mu.Unlock()
		return ok
	}
}

// loadTokenStoreFromRoute loads primary TokenFile (+ optional .next) and multi-token profiles
// (+ optional .next for each) into a single TokenStore.
func loadTokenStoreFromRoute(tokenFile string, tokens []RouteTokenConfig) (*TokenStore, []string, error) {
	var rules []TokenRule
	var secrets []string

	loadOne := func(name, path string, allow, deny []string, readOnly bool) error {
		primary, err := ReadSecretFile(path)
		if err != nil {
			return err
		}
		rules = append(rules, NewTokenRule(name, primary, allow, deny, readOnly))
		secrets = append(secrets, primary)

		nextPath := filepath.Clean(strings.TrimSpace(path)) + ".next"
		if _, statErr := os.Stat(nextPath); statErr == nil {
			nextSecret, readErr := ReadSecretFile(nextPath)
			if readErr != nil {
				return fmt.Errorf("grace token file %q: %w", nextPath, readErr)
			}
			if nextSecret != primary {
				rules = append(rules, NewTokenRule(name, nextSecret, allow, deny, readOnly))
				secrets = append(secrets, nextSecret)
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("stat grace token file %q: %w", nextPath, statErr)
		}
		return nil
	}

	if strings.TrimSpace(tokenFile) != "" {
		if err := loadOne("default", tokenFile, nil, nil, false); err != nil {
			return nil, nil, err
		}
	}
	for i, tc := range tokens {
		name := tc.Name
		if name == "" {
			name = fmt.Sprintf("token-%d", i)
		}
		if err := loadOne(name, tc.TokenFile, tc.AllowTools, tc.DenyTools, tc.ReadOnlyOnly); err != nil {
			return nil, nil, fmt.Errorf("tokens[%d] (%s): %w", i, name, err)
		}
	}
	if len(rules) == 0 {
		return nil, nil, errors.New("at least one token must be configured")
	}

	store := NewTokenStore()
	store.SetRules(rules)
	return store, secrets, nil
}

func stripDangerousProxyHeaders(h http.Header) {
	h.Del("X-Forwarded-Host")
	h.Del("X-Forwarded-Proto")
	h.Del("X-Forwarded-Port")
	h.Del("Forwarded")
	h.Del("X-Real-IP")
	h.Del("Cookie")
	h.Del("Proxy-Authorization")
}

// mcpUpstreamHandler implements the hardened MCP reverse proxy logic shared by ProxyServer and GatewayServer.
type mcpUpstreamHandler struct {
	name            string
	targetURL       *url.URL
	targetDialAddr  string
	healthPath      string
	upstreamToken   string
	upstreamService string
	clientPath      string
	tokenStore      *TokenStore
	transport       *http.Transport
	probeClient     *http.Client
	metrics         *MCPMetrics
	serviceLookup   func(string) *ServiceRunner
	redactor        *Redactor
	logger          *log.Logger
	rp              *httputil.ReverseProxy
	healthCache     *upstreamHealthCache

	hintMu       sync.RWMutex
	hintEpoch    uint64
	toolReadOnly map[string]*bool
}

type mcpUpstreamOptions struct {
	Name            string
	TargetURL       string
	HealthPath      string
	UpstreamToken   string
	UpstreamService string
	ClientPath      string
	TokenStore      *TokenStore
	Transport       *http.Transport
	Metrics         *MCPMetrics
	ServiceLookup   func(string) *ServiceRunner
	Redactor        *Redactor
	Logger          *log.Logger
	HealthCacheTTL  time.Duration
}

func newMCPUpstreamHandler(opts mcpUpstreamOptions) (*mcpUpstreamHandler, error) {
	targetURL, err := url.Parse(strings.TrimSpace(opts.TargetURL))
	if err != nil || (targetURL.Scheme != "http" && targetURL.Scheme != "https") || targetURL.Host == "" {
		return nil, fmt.Errorf("proxy %q: invalid targetUrl", opts.Name)
	}
	if proxyAdministrativePath(targetURL.Path) ||
		proxyAdministrativePath(opts.ClientPath) || proxyAdministrativePath(opts.HealthPath) {
		return nil, fmt.Errorf("proxy %q: administrative paths are reserved for the control API", opts.Name)
	}
	if opts.TokenStore == nil || opts.TokenStore.Empty() {
		return nil, fmt.Errorf("proxy %q: token store must not be empty", opts.Name)
	}
	transport := opts.Transport
	if transport == nil {
		transport = NewDirectTransport()
	}
	metrics := opts.Metrics
	if metrics == nil {
		metrics = NewMCPMetrics()
	}
	cacheTTL := opts.HealthCacheTTL
	if cacheTTL == 0 {
		cacheTTL = defaultHealthCacheTTL
	} else if cacheTTL < 0 {
		cacheTTL = 0
	}

	probeClient := &http.Client{
		Transport: transport,
		Timeout:   0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	h := &mcpUpstreamHandler{
		name:            opts.Name,
		targetURL:       targetURL,
		targetDialAddr:  targetHostPort(targetURL),
		healthPath:      opts.HealthPath,
		upstreamToken:   strings.TrimSpace(opts.UpstreamToken),
		upstreamService: strings.TrimSpace(opts.UpstreamService),
		clientPath:      strings.TrimSpace(opts.ClientPath),
		tokenStore:      opts.TokenStore,
		transport:       transport,
		probeClient:     probeClient,
		metrics:         metrics,
		serviceLookup:   opts.ServiceLookup,
		redactor:        opts.Redactor,
		logger:          opts.Logger,
		healthCache:     newUpstreamHealthCache(cacheTTL),
	}

	h.rp = &httputil.ReverseProxy{
		FlushInterval: -1, // Immediate flush for SSE streaming.
		Transport:     transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Header.Del(noWakeHeader)
			stripDangerousProxyHeaders(pr.Out.Header)
			if h.upstreamToken != "" {
				pr.Out.Header.Set("Authorization", "Bearer "+h.upstreamToken)
			} else {
				// Strip the proxy's own Bearer credential before forwarding to the local upstream.
				pr.Out.Header.Del("Authorization")
			}
			if pr.In.Context().Value(proxyFilterContextKey{}) != nil {
				pr.Out.Header.Set("Accept-Encoding", "identity")
			}
		},
		ModifyResponse: h.filterUpstreamResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
				return
			}
			var maxBytesErr *http.MaxBytesError
			if errors.As(err, &maxBytesErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
				return
			}
			if errors.Is(err, errToolsListTooLarge) || errors.Is(err, errToolsListFilterFailed) {
				writeJSONError(w, http.StatusBadGateway, err.Error())
				return
			}
			if h.logger != nil {
				msg := fmt.Sprintf("[proxy:%s] upstream error (%s %s): %v", h.name, r.Method, r.URL.Path, err)
				if h.redactor != nil {
					msg = h.redactor.Redact(msg)
				}
				h.logger.Print(msg)
			}
			writeJSONError(w, http.StatusBadGateway, "bad_gateway")
		},
	}
	return h, nil
}

func (h *mcpUpstreamHandler) lookupRunner() *ServiceRunner {
	if h.serviceLookup == nil || h.upstreamService == "" {
		return nil
	}
	return h.serviceLookup(h.upstreamService)
}

func (h *mcpUpstreamHandler) cachedToolReadOnlyHint(toolName string, epoch uint64) *bool {
	h.hintMu.RLock()
	defer h.hintMu.RUnlock()
	if h.toolReadOnly == nil || h.hintEpoch != epoch {
		return nil
	}
	return h.toolReadOnly[toolName]
}

func (h *mcpUpstreamHandler) cacheToolsReadOnlyHints(body []byte, isSSE bool, epoch uint64) {
	if len(body) == 0 {
		return
	}
	var payloads [][]byte
	if isSSE {
		lines := splitSSELines(body)
		var dataParts []string
		flush := func() {
			if len(dataParts) > 0 {
				payloads = append(payloads, []byte(strings.Join(dataParts, "\n")))
				dataParts = dataParts[:0]
			}
		}
		for _, ln := range lines {
			if len(ln.raw) == 0 {
				flush()
				continue
			}
			if ln.isData {
				dataParts = append(dataParts, ln.dataVal)
			}
		}
		flush()
	} else {
		payloads = [][]byte{body}
	}

	extract := func(raw []byte) {
		var parsed struct {
			Result *struct {
				Tools []struct {
					Name        string `json:"name"`
					Annotations *struct {
						ReadOnlyHint *bool `json:"readOnlyHint"`
					} `json:"annotations"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Result == nil || len(parsed.Result.Tools) == 0 {
			return
		}
		h.hintMu.Lock()
		if epoch < h.hintEpoch {
			h.hintMu.Unlock()
			return
		}
		if h.toolReadOnly == nil || h.hintEpoch != epoch {
			h.toolReadOnly = make(map[string]*bool, len(parsed.Result.Tools))
			h.hintEpoch = epoch
		}
		for _, t := range parsed.Result.Tools {
			if t.Name == "" {
				continue
			}
			if t.Annotations != nil && t.Annotations.ReadOnlyHint != nil {
				v := *t.Annotations.ReadOnlyHint
				h.toolReadOnly[t.Name] = &v
			} else {
				h.toolReadOnly[t.Name] = nil
			}
		}
		h.hintMu.Unlock()
	}

	for _, p := range payloads {
		trimmed := bytes.TrimSpace(bytes.TrimPrefix(p, utf8BOM))
		if len(trimmed) == 0 {
			continue
		}
		if trimmed[0] == '[' {
			var elems []json.RawMessage
			if err := json.Unmarshal(trimmed, &elems); err == nil {
				for _, e := range elems {
					extract(e)
				}
			}
		} else if trimmed[0] == '{' {
			extract(trimmed)
		}
	}
}

func (h *mcpUpstreamHandler) serveHealthz(w http.ResponseWriter, r *http.Request) {
	if runner := h.lookupRunner(); runner != nil {
		switch runner.State() {
		case "sleeping":
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": "sleeping"})
			return
		case "stopped":
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "state": "stopped", "error": "service_stopped"})
			return
		case "degraded":
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "state": "degraded", "error": "service_degraded"})
			return
		}
	}
	ok := h.healthCache.check(r.Context(), func(ctx context.Context) bool {
		return probeProxyUpstream(ctx, h.targetURL, h.healthPath, h.targetDialAddr, h.probeClient)
	})
	if ok {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	} else {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "upstream_unhealthy"})
	}
}

func (h *mcpUpstreamHandler) serveAuthenticated(w http.ResponseWriter, r *http.Request, rule TokenRule) {
	runner := h.lookupRunner()
	clientPath := h.clientPath
	if clientPath == "" && runner != nil {
		clientPath = runner.cfg.EffectiveClientPath()
	}
	if !allowMCPProxyRequest(w, r, clientPath) {
		return
	}
	noWake := r.Header.Get(noWakeHeader) == "1"
	// Fast-fail oversized requests before waking or proxying.
	if r.ContentLength > maxProxyRequestBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxProxyRequestBodyBytes)
	}

	filter := ToolFilter{
		AllowTools:   rule.AllowTools,
		DenyTools:    rule.DenyTools,
		ReadOnlyOnly: rule.ReadOnlyOnly,
	}

	var peek MCPPeekInfo
	if filter.Active() && r.Method == http.MethodPost {
		if status, code, herr := ValidateStrictMCPHeaders(r); herr != nil {
			writeJSONError(w, status, code)
			return
		}
		if r.Body == nil {
			writeJSONError(w, http.StatusBadRequest, "empty_body")
			return
		}
		fullBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(readErr, &maxBytesErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
				return
			}
			writeJSONError(w, http.StatusBadRequest, "read_body_failed")
			return
		}
		// Forward the exact validated bytes upstream (never re-read from the wire).
		r.Body = io.NopCloser(bytes.NewReader(fullBody))
		r.ContentLength = int64(len(fullBody))
		r.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(fullBody)), nil
		}

		strictReq, parseErr := ParseStrictMCPRequest(fullBody)
		if parseErr != nil {
			writeJSONRPCStatusError(w, http.StatusBadRequest, extractStrictRPCErrorID(parseErr), -32600, parseErr.Error())
			return
		}
		peek = strictReq.Peek
	} else {
		var peekErr error
		peek, peekErr = PeekMCPRequest(r, defaultPeekMaxBytes)
		if peekErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(peekErr, &maxBytesErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
				return
			}
			writeJSONError(w, http.StatusBadRequest, "read_body_failed")
			return
		}
	}

	var currentEpoch uint64
	if runner != nil {
		currentEpoch = runner.Epoch()
	}

	// Pre-flight ToolFilter enforcement on tools/call before waking or forwarding upstream.
	if filter.Active() && peek.HasToolsCall {
		if len(peek.ToolCalls) == 0 {
			WriteToolDeniedJSONRPC(w, peek.ID, peek.ToolName)
			return
		}
		for _, tc := range peek.ToolCalls {
			if strings.TrimSpace(tc.Name) == "" {
				writeJSONRPCStatusError(w, http.StatusBadRequest, tc.ID, -32600, "tools/call requires a non-empty string params.name")
				return
			}
			hint := h.cachedToolReadOnlyHint(tc.Name, currentEpoch)
			if !filter.Allows(tc.Name, hint) {
				WriteToolDeniedJSONRPC(w, tc.ID, tc.Name)
				return
			}
		}
	}

	if runner != nil {
		wakeCtx, wakeCancel := context.WithTimeout(r.Context(), runner.StartTimeout())
		epoch, epochCtx, release, err := runner.BeginProxyRequest(wakeCtx, noWake)
		wakeCancel()
		if err != nil {
			if r.Context().Err() == nil {
				writeProxyAdmissionError(w, err)
			}
			return
		}
		defer release()
		currentEpoch = epoch
		if filter.ReadOnlyOnly {
			for _, tc := range peek.ToolCalls {
				if !filter.Allows(tc.Name, h.cachedToolReadOnlyHint(tc.Name, currentEpoch)) {
					WriteToolDeniedJSONRPC(w, tc.ID, tc.Name)
					return
				}
			}
		}

		if reqSess := strings.TrimSpace(r.Header.Get("Mcp-Session-Id")); reqSess != "" {
			if known, stale := h.metrics.CheckSessionEpoch(reqSess, currentEpoch); known && stale {
				h.metrics.DeleteSession(reqSess)
				runner.UntrackSession(reqSess)
				writeJSONError(w, http.StatusNotFound, "session_expired")
				return
			}
		}

		reqCtx, cancelReq := context.WithCancel(r.Context())
		if epochCtx.Err() != nil {
			cancelReq()
			writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable")
			return
		}
		stopAfter := context.AfterFunc(epochCtx, cancelReq)
		defer func() {
			stopAfter()
			cancelReq()
		}()
		r = r.WithContext(reqCtx)
	}

	var onSession func(id string, closed bool)
	if runner != nil {
		onSession = func(id string, closed bool) {
			if closed {
				runner.UntrackSession(id)
			} else {
				runner.TrackSession(id)
			}
		}
	}

	rec, finish := h.metrics.WrapHTTP(w, r, peek, currentEpoch, onSession)
	defer finish()

	var outW http.ResponseWriter = rec
	var sseTracker *sseActivityWriter
	if runner != nil {
		sseTracker = &sseActivityWriter{w: rec, runner: runner}
		defer sseTracker.finish()
		outW = sseTracker
	}

	if filter.Active() && (peek.HasToolsList || r.Method == http.MethodGet) {
		policy := proxyResponseFilter{filter: filter, toolsList: peek.HasToolsList, epoch: currentEpoch}
		r = r.WithContext(context.WithValue(r.Context(), proxyFilterContextKey{}, policy))
	}

	h.rp.ServeHTTP(outW, r)
}

func allowMCPProxyRequest(w http.ResponseWriter, r *http.Request, clientPath string) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	configuredPath := strings.TrimSuffix(clientPath, "/")
	allow := "GET, POST, DELETE"
	switch {
	case proxyAdministrativePath(path):
		writeJSONError(w, http.StatusNotFound, "not_found")
		return false
	case path == "", path == "/mcp", path == configuredPath:
	case path == "/sse":
		allow = "GET"
	case path == "/messages":
		allow = "POST"
	default:
		writeJSONError(w, http.StatusNotFound, "not_found")
		return false
	}
	allowed := r.Method == http.MethodPost && allow != "GET" ||
		r.Method == http.MethodGet && allow != "POST" ||
		r.Method == http.MethodDelete && allow == "GET, POST, DELETE"
	if !allowed {
		w.Header().Set("Allow", allow)
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return false
	}
	if r.Method != http.MethodPost && r.ContentLength != 0 {
		writeJSONError(w, http.StatusBadRequest, "unexpected_request_body")
		return false
	}
	return true
}

func proxyAdministrativePath(path string) bool {
	path = strings.ToLower(urlpath.Clean(strings.ReplaceAll(path, "\\", "/")))
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

func writeProxyAdmissionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrServiceSleeping):
		writeJSONError(w, http.StatusConflict, "service_sleeping")
	case errors.Is(err, ErrServiceStopped):
		writeJSONError(w, http.StatusServiceUnavailable, "service_stopped")
	case errors.Is(err, ErrServiceDegraded):
		writeJSONError(w, http.StatusServiceUnavailable, "service_degraded")
	default:
		writeJSONError(w, http.StatusServiceUnavailable, "service_unavailable")
	}
}

// sseActivityWriter tracks active text/event-stream responses on a linked ServiceRunner
// while preserving Flusher and Unwrap() for zero-copy streaming.
type sseActivityWriter struct {
	w           http.ResponseWriter
	runner      *ServiceRunner
	mu          sync.Mutex
	wroteHeader bool
	isSSE       bool
}

func (s *sseActivityWriter) Header() http.Header {
	return s.w.Header()
}

func (s *sseActivityWriter) WriteHeader(statusCode int) {
	if statusCode >= 100 && statusCode < 200 {
		s.w.WriteHeader(statusCode)
		return
	}
	s.mu.Lock()
	if !s.wroteHeader {
		s.wroteHeader = true
		mediaType, _, _ := mime.ParseMediaType(s.w.Header().Get("Content-Type"))
		if strings.EqualFold(mediaType, "text/event-stream") {
			s.isSSE = true
			s.runner.SSEAdd(1)
		}
	}
	s.mu.Unlock()
	s.w.WriteHeader(statusCode)
}

func (s *sseActivityWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	wrote := s.wroteHeader
	s.mu.Unlock()
	if !wrote {
		s.WriteHeader(http.StatusOK)
	}
	return s.w.Write(p)
}

func (s *sseActivityWriter) Flush() {
	if fl, ok := s.w.(http.Flusher); ok {
		fl.Flush()
	}
}

func (s *sseActivityWriter) Unwrap() http.ResponseWriter {
	return s.w
}

func (s *sseActivityWriter) finish() {
	s.mu.Lock()
	wasSSE := s.isSSE
	s.isSSE = false
	s.mu.Unlock()
	if wasSSE {
		s.runner.SSEAdd(-1)
	}
}

func (h *mcpUpstreamHandler) filterUpstreamResponse(resp *http.Response) error {
	policy, ok := resp.Request.Context().Value(proxyFilterContextKey{}).(proxyResponseFilter)
	if !ok || resp.StatusCode < 200 || resp.StatusCode >= 300 ||
		resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.EqualFold(mediaType, "text/event-stream") {
		if strings.TrimSpace(resp.Header.Get("Content-Encoding")) != "" {
			return errToolsListFilterFailed
		}
		body := &filteredSSEBody{ReadCloser: resp.Body, in: bufio.NewReader(resp.Body), filter: policy.filter}
		if policy.toolsList {
			body.cache = func(frame []byte) { h.cacheToolsReadOnlyHints(frame, true, policy.epoch) }
			// Check the initial frame before committing response headers. Later invalid
			// frames stop the stream without forwarding their contents.
			if err := body.nextFrame(); err != nil && !errors.Is(err, io.EOF) {
				return err
			}
		}
		resp.Body = body
		resp.ContentLength = -1
		clearRewrittenResponseHeaders(resp.Header)
		return nil
	}
	if !policy.toolsList {
		return nil
	}
	if strings.TrimSpace(resp.Header.Get("Content-Encoding")) != "" {
		return errToolsListFilterFailed
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxToolsListBufferBytes+1))
	if err != nil {
		return errToolsListFilterFailed
	}
	if len(raw) > maxToolsListBufferBytes {
		return errToolsListTooLarge
	}
	filtered, _, err := FilterToolsListJSON(raw, policy.filter)
	if err != nil {
		return errToolsListFilterFailed
	}
	h.cacheToolsReadOnlyHints(raw, false, policy.epoch)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(filtered))
	resp.ContentLength = int64(len(filtered))
	clearRewrittenResponseHeaders(resp.Header)
	resp.Header.Set("Content-Length", strconv.Itoa(len(filtered)))
	return nil
}

func clearRewrittenResponseHeaders(h http.Header) {
	for _, name := range [...]string{"Content-Length", "Etag", "Content-Md5", "Content-Digest", "Repr-Digest"} {
		h.Del(name)
	}
}

type filteredSSEBody struct {
	io.ReadCloser
	in      *bufio.Reader
	filter  ToolFilter
	cache   func([]byte)
	output  []byte
	afterCR bool
}

func (b *filteredSSEBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.output) == 0 {
		if err := b.nextFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.output)
	b.output = b.output[n:]
	return n, nil
}

func (b *filteredSSEBody) nextFrame() error {
	var frame bytes.Buffer
	lineSize := 0
	for {
		c, err := b.in.ReadByte()
		if err != nil {
			if !errors.Is(err, io.EOF) || frame.Len() == 0 {
				return err
			}
			break
		}
		if b.afterCR {
			b.afterCR = false
			if c == '\n' {
				continue
			}
		}
		if frame.Len() == maxToolsListBufferBytes {
			return errToolsListTooLarge
		}
		if c == '\r' || c == '\n' {
			b.afterCR = c == '\r'
			frame.WriteByte('\n')
			if lineSize == 0 {
				break
			}
			lineSize = 0
		} else {
			frame.WriteByte(c)
			lineSize++
		}
	}
	raw := frame.Bytes()
	filtered, _, err := FilterToolsListSSE(raw, b.filter)
	if err != nil {
		return errToolsListFilterFailed
	}
	if b.cache != nil {
		b.cache(raw)
	}
	b.output = filtered
	return nil
}

type proxyRuntime struct {
	cfg        ProxyConfig
	allowedSet map[string]struct{}
	upstream   *mcpUpstreamHandler
	lifetimes  tokenLifetimes
}

func (rt *proxyRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	isHealthz := (r.Method == http.MethodGet || r.Method == http.MethodHead) && r.URL.Path == "/healthz"
	if !CheckLoopbackGuard(w, r, rt.allowedSet, isHealthz) {
		return
	}
	if isHealthz {
		rt.upstream.serveHealthz(w, r)
		return
	}
	rule, ok := rt.upstream.tokenStore.Match(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	r, release := rt.lifetimes.bind(r, rule)
	defer release()
	if r.Context().Err() != nil {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	rt.upstream.serveAuthenticated(w, r, rule)
}

// ProxyServer hosts one localhost authenticated MCP ReverseProxy with atomic hot-reload support.
type ProxyServer struct {
	cfg           ProxyConfig
	logger        *log.Logger
	redactor      *Redactor
	transport     *http.Transport
	metrics       *MCPMetrics
	serviceLookup func(string) *ServiceRunner

	active atomic.Pointer[proxyRuntime]

	mu         sync.RWMutex
	ln         net.Listener
	srv        *http.Server
	baseCancel context.CancelFunc
	listenAddr string
	running    bool
}

// NewProxyServer creates a ProxyServer from config. Call Start to bind and serve.
func NewProxyServer(cfg ProxyConfig, logger *log.Logger) *ProxyServer {
	return &ProxyServer{
		cfg:        cfg,
		logger:     logger,
		transport:  NewDirectTransport(),
		metrics:    NewMCPMetrics(),
		listenAddr: cfg.ListenAddr,
	}
}

// SetServiceLookup wires the dynamic ServiceRunner lookup for lazy wake, epoch tracking, and sleep-aware healthz.
func (p *ProxyServer) SetServiceLookup(fn func(string) *ServiceRunner) {
	p.mu.Lock()
	p.serviceLookup = fn
	p.mu.Unlock()
}

// SetRedactor wires the shared Redactor for scrubbing upstream error logs.
func (p *ProxyServer) SetRedactor(r *Redactor) {
	p.mu.Lock()
	p.redactor = r
	p.mu.Unlock()
}

// MCPMetrics returns this proxy's underlying MCPMetrics collector.
func (p *ProxyServer) MCPMetrics() *MCPMetrics {
	if runtime := p.active.Load(); runtime != nil {
		return runtime.upstream.metrics
	}
	return p.metrics
}

// NewProxyHandler builds the HTTP handler enforcing Host allowlist, Origin/OPTIONS/Sec-Fetch-Site
// rejection, unauthenticated /healthz probing, constant-time Bearer auth, and unbuffered SSE reverse proxying.
// Authorization headers are stripped before forwarding to the local upstream.
func NewProxyHandler(cfg ProxyConfig, expectedToken string, boundAddr string, logger *log.Logger) (http.Handler, error) {
	return NewProxyHandlerWithUpstream(cfg, expectedToken, "", boundAddr, logger)
}

// NewProxyHandlerWithUpstream builds the HTTP handler like NewProxyHandler, optionally injecting
// an upstream Bearer token instead of stripping Authorization when upstreamToken is non-empty.
func NewProxyHandlerWithUpstream(cfg ProxyConfig, expectedToken string, upstreamToken string, boundAddr string, logger *log.Logger) (http.Handler, error) {
	if strings.TrimSpace(expectedToken) == "" {
		return nil, fmt.Errorf("proxy %q: expected bearer token must not be empty", cfg.Name)
	}
	store := NewTokenStore()
	store.SetSingle("default", expectedToken, nil, nil, false)
	return newProxyHandlerWithOptions(cfg, store, upstreamToken, boundAddr, nil, nil, nil, nil, defaultHealthCacheTTL, logger)
}

func newProxyHandlerWithOptions(
	cfg ProxyConfig,
	store *TokenStore,
	upstreamToken string,
	boundAddr string,
	transport *http.Transport,
	metrics *MCPMetrics,
	serviceLookup func(string) *ServiceRunner,
	redactor *Redactor,
	healthCacheTTL time.Duration,
	logger *log.Logger,
) (*proxyRuntime, error) {
	hosts := cfg.AllowedHosts
	if len(hosts) == 0 {
		addr := boundAddr
		if addr == "" {
			addr = cfg.ListenAddr
		}
		hosts = DefaultAllowedHosts(addr)
	}
	allowedSet := makeAllowedHostSet(hosts)

	up, err := newMCPUpstreamHandler(mcpUpstreamOptions{
		Name:            cfg.Name,
		TargetURL:       cfg.TargetURL,
		HealthPath:      cfg.HealthPath,
		UpstreamToken:   upstreamToken,
		UpstreamService: cfg.UpstreamService,
		ClientPath:      cfg.ClientPath,
		TokenStore:      store,
		Transport:       transport,
		Metrics:         metrics,
		ServiceLookup:   serviceLookup,
		Redactor:        redactor,
		Logger:          logger,
		HealthCacheTTL:  healthCacheTTL,
	})
	if err != nil {
		return nil, err
	}
	return &proxyRuntime{
		cfg:        cfg,
		allowedSet: allowedSet,
		upstream:   up,
		lifetimes:  store.lifetimes(nil),
	}, nil
}

func targetHostPort(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(host, port)
}

func probeProxyUpstream(ctx context.Context, targetURL *url.URL, healthPath, dialAddr string, client *http.Client) bool {
	if healthPath != "" {
		probeURL := *targetURL
		probeURL.Path = healthPath
		probeURL.RawQuery = ""
		probeURL.Fragment = ""

		reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, probeURL.String(), nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}

	d := net.Dialer{Timeout: 1500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", dialAddr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (p *ProxyServer) buildRuntime(cfg ProxyConfig, boundAddr string) (*proxyRuntime, error) {
	store, secrets, err := loadTokenStoreFromRoute(cfg.TokenFile, cfg.Tokens)
	if err != nil {
		return nil, fmt.Errorf("proxy %q auth tokens: %w", cfg.Name, err)
	}
	var upstreamToken string
	if strings.TrimSpace(cfg.UpstreamTokenFile) != "" {
		upstreamToken, err = ReadSecretFile(cfg.UpstreamTokenFile)
		if err != nil {
			return nil, fmt.Errorf("proxy %q upstreamTokenFile: %w", cfg.Name, err)
		}
	}
	p.mu.RLock()
	lookup := p.serviceLookup
	redactor := p.redactor
	p.mu.RUnlock()
	redactor.AddSecrets(append(secrets, upstreamToken))

	metrics := p.metrics
	if current := p.active.Load(); current != nil {
		if current.cfg.UpstreamService == cfg.UpstreamService && current.cfg.TargetURL == cfg.TargetURL {
			metrics = current.upstream.metrics
		} else {
			metrics = NewMCPMetrics()
		}
	}
	return newProxyHandlerWithOptions(
		cfg, store, upstreamToken, boundAddr, p.transport, metrics, lookup, redactor, defaultHealthCacheTTL, p.logger,
	)
}

// Start loads proxy tokens, binds ListenAddr, and begins serving in a goroutine.
func (p *ProxyServer) Start() error {
	start, _, err := p.prepareStart(context.Background())
	if err != nil {
		return err
	}
	start()
	return nil
}

func (p *ProxyServer) prepareStart(ctx context.Context) (func(), func(), error) {
	cfg := p.Config()
	var listener net.ListenConfig
	ln, err := listener.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("proxy %q listen %q: %w", cfg.Name, cfg.ListenAddr, err)
	}
	rt, err := p.buildRuntime(cfg, ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	return func() { p.serve(ln, cfg, rt) }, func() { _ = ln.Close() }, nil
}

func (p *ProxyServer) serve(ln net.Listener, cfg ProxyConfig, rt *proxyRuntime) {
	boundAddr := ln.Addr().String()
	p.active.Store(rt)

	dispatcher := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := p.active.Load()
		if cur == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "proxy_not_ready")
			return
		}
		cur.ServeHTTP(w, r)
	})

	baseCtx, baseCancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           dispatcher,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return baseCtx
		},
	}

	p.mu.Lock()
	p.ln = ln
	p.srv = srv
	p.baseCancel = baseCancel
	p.listenAddr = boundAddr
	p.running = true
	p.mu.Unlock()

	if p.logger != nil {
		p.logger.Printf("[proxy:%s] listening on %s -> %s", cfg.Name, boundAddr, diagnosticURL(p.redactor, cfg.TargetURL))
	}

	go func() {
		defer baseCancel()
		err := srv.Serve(ln)
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) && p.logger != nil {
			p.logger.Printf("[proxy:%s] serve stopped with error: %v", cfg.Name, err)
		}
	}()

}

// ReloadConfig swaps the runtime in-place and closes streams whose authorization changed.
func (p *ProxyServer) ReloadConfig(cfg ProxyConfig) error {
	commit, err := p.prepareReload(cfg)
	if err != nil {
		return err
	}
	commit()
	return nil
}

func (p *ProxyServer) prepareReload(cfg ProxyConfig) (func(), error) {
	p.mu.RLock()
	boundAddr := p.listenAddr
	oldListen := p.cfg.ListenAddr
	p.mu.RUnlock()

	if normalizeBindKey(cfg.ListenAddr) != normalizeBindKey(oldListen) || cfg.ListenAddr != oldListen {
		return nil, fmt.Errorf("proxy %q: cannot hot-reload listenAddr change (%q -> %q)", cfg.Name, oldListen, cfg.ListenAddr)
	}

	nextRT, err := p.buildRuntime(cfg, boundAddr)
	if err != nil {
		return nil, err
	}

	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.cfg = cfg
		old := p.active.Load()
		if old != nil {
			if maps.Equal(old.allowedSet, nextRT.allowedSet) && sameUpstream(old.upstream, nextRT.upstream) {
				nextRT.lifetimes.retire(nil)
				nextRT.lifetimes = nextRT.upstream.tokenStore.lifetimes(old.lifetimes)
			}
			old.lifetimes.retire(nextRT.lifetimes)
		}
		p.active.Store(nextRT)
	}, nil
}

func sameUpstream(old, next *mcpUpstreamHandler) bool {
	return old.targetURL.String() == next.targetURL.String() &&
		old.upstreamService == next.upstreamService && old.upstreamToken == next.upstreamToken &&
		old.clientPath == next.clientPath
}

// ResumeFromSuspend closes stale idle TCP connections and clears cached health results after OS wake.
func (p *ProxyServer) ResumeFromSuspend() {
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	if rt := p.active.Load(); rt != nil && rt.upstream != nil && rt.upstream.healthCache != nil {
		rt.upstream.healthCache.invalidate()
	}
}

// CancelBaseContext cancels the server's BaseContext so active long-lived requests
// (such as open SSE streams) abort immediately during shutdown.
func (p *ProxyServer) CancelBaseContext() {
	p.mu.Lock()
	cancel := p.baseCancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ListenAddr returns the actual bound address (resolving :0 in tests).
func (p *ProxyServer) ListenAddr() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.listenAddr
}

// Config returns the proxy's current ProxyConfig.
func (p *ProxyServer) Config() ProxyConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// Status returns a snapshot of proxy status including live MCP metrics.
func (p *ProxyServer) Status() ProxyStatus {
	p.mu.RLock()
	cfg := p.cfg
	listenAddr := p.listenAddr
	running := p.running
	p.mu.RUnlock()

	snap := p.MCPMetrics().Snapshot()
	return ProxyStatus{
		Name:            cfg.Name,
		ListenAddr:      listenAddr,
		TargetURL:       diagnosticURL(p.redactor, cfg.TargetURL),
		Running:         running,
		UpstreamService: cfg.UpstreamService,
		Metrics:         &snap,
	}
}

// Shutdown gracefully stops the proxy listener, active connections, and idle upstream pool.
func (p *ProxyServer) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	srv := p.srv
	cancel := p.baseCancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		_ = srv.Close()
		return err
	}
	return nil
}
