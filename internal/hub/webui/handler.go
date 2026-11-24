package webui

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed assets/*
var assetsFS embed.FS

const (
	defaultLogsLines = 120
	maxLogsLines     = 500
	ssePingInterval  = 15 * time.Second
)

var (
	indexTmpl  = template.Must(template.ParseFS(assetsFS, "assets/index.html"))
	unlockTmpl = template.Must(template.ParseFS(assetsFS, "assets/unlock.html"))
)

var staticAssetContentTypes = map[string]string{
	"app.css":     "text/css; charset=utf-8",
	"app.js":      "application/javascript; charset=utf-8",
	"unlock.js":   "application/javascript; charset=utf-8",
	"favicon.svg": "image/svg+xml; charset=utf-8",
}

// Handler serves the embedded local Web Dashboard and its authenticated /ui/api/* endpoints.
type Handler struct {
	backend   Backend
	sec       *securityState
	closeOnce sync.Once
	closedCh  chan struct{}
}

// NewHandler builds a localhost-only Web Dashboard http.Handler mounted at /ui and /ui/*.
func NewHandler(boundAddr string, backend Backend) *Handler {
	return &Handler{
		backend:  backend,
		sec:      newSecurityState(boundAddr),
		closedCh: make(chan struct{}),
	}
}

// SetBoundAddr updates the loopback Host allowlist after the listener binds a concrete address.
func (h *Handler) SetBoundAddr(addr string) {
	h.sec.setBoundAddr(addr)
}

// SetAllowedHosts explicitly overrides the Host allowlist (intended for custom loopback bindings).
func (h *Handler) SetAllowedHosts(hosts []string) {
	h.sec.setAllowedHosts(hosts)
}

// MintBootstrapToken generates a single-use 256-bit hex bootstrap nonce valid for 60 seconds.
func (h *Handler) MintBootstrapToken() (nonce string, expiresAt time.Time) {
	return h.sec.mintBootstrapToken()
}

// ActiveSessionCount returns the number of currently active non-expired UI sessions.
func (h *Handler) ActiveSessionCount() int {
	return h.sec.activeSessionCount()
}

// Close terminates all active SSE streams and revokes in-memory sessions.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		close(h.closedCh)
		h.sec.closeAllSessions()
	})
}

func (h *Handler) redact(s string) string {
	if h.backend == nil {
		return s
	}
	return h.backend.Redact(s)
}

func (h *Handler) allowNoAuth() bool {
	return h.backend != nil && h.backend.AllowNoAuth()
}

// ServeHTTP handles /ui and /ui/* under strict loopback Host, Fetch Metadata, Cookie, and CSRF guards.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.sec.isHostAllowed(r.Host) {
		writeJSONError(w, http.StatusForbidden, "forbidden_host")
		return
	}
	if r.Method == http.MethodOptions {
		writeJSONError(w, http.StatusForbidden, "forbidden_preflight")
		return
	}

	p := r.URL.Path
	switch {
	case p == "/ui" || p == "/ui/":
		h.handleRoot(w, r)
	case p == "/ui/session":
		h.handleMintSession(w, r)
	case strings.HasPrefix(p, "/ui/assets/"):
		h.handleAsset(w, r)
	case strings.HasPrefix(p, "/ui/api/"):
		h.handleAPI(w, r)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) handleMintSession(w http.ResponseWriter, r *http.Request) {
	if blocked, code := hasAnyBrowserOriginOrFetchSite(r); blocked {
		writeJSONError(w, http.StatusForbidden, code)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if !h.allowNoAuth() {
		tok, ok := extractBearerToken(r)
		if !ok || h.backend == nil || !h.backend.VerifyControlToken(tok) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
	}
	nonce, expiresAt := h.MintBootstrapToken()
	launchPath := "/ui/?bootstrap=" + nonce
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"nonce":     nonce,
		"path":      launchPath,
		"url":       "http://" + strings.TrimSpace(r.Host) + launchPath,
		"expiresAt": expiresAt.Format(time.RFC3339),
	})
}

func (h *Handler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ok, code := checkGetFetchMetadata(r); !ok {
		writeJSONError(w, http.StatusForbidden, code)
		return
	}

	if r.URL.Query().Has("bootstrap") {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		nonce := r.URL.Query().Get("bootstrap")
		if h.sec.consumeBootstrapToken(nonce) {
			sessionID, _ := h.sec.createSession()
			setSecurityHeaders(w, true)
			setSessionCookie(w, sessionID)
			w.Header().Set("Location", "/ui/")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		setSecurityHeaders(w, true)
		w.Header().Set("Location", "/ui/?auth=expired")
		w.WriteHeader(http.StatusSeeOther)
		return
	}

	if r.URL.Path == "/ui" {
		setSecurityHeaders(w, true)
		w.Header().Set("Location", "/ui/")
		w.WriteHeader(http.StatusSeeOther)
		return
	}

	sess, hasSession := h.sec.lookupSession(r)
	if !hasSession && h.allowNoAuth() {
		sessionID, created := h.sec.createSession()
		setSessionCookie(w, sessionID)
		sess = created
		hasSession = true
	}

	if hasSession {
		authMode := "token"
		if h.allowNoAuth() {
			authMode = "noauth"
		}
		h.renderTemplate(w, r, indexTmpl, struct {
			CSRFToken string
			AuthMode  string
		}{
			CSRFToken: sess.csrfToken,
			AuthMode:  authMode,
		})
		return
	}

	expired := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("auth")), "expired")
	h.renderTemplate(w, r, unlockTmpl, struct {
		Expired bool
	}{
		Expired: expired,
	})
}

func (h *Handler) renderTemplate(w http.ResponseWriter, r *http.Request, tmpl *template.Template, data any) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "template_error")
		return
	}
	setSecurityHeaders(w, true)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

func (h *Handler) handleAsset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ok, code := checkGetFetchMetadata(r); !ok {
		writeJSONError(w, http.StatusForbidden, code)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/ui/assets/")
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || path.Clean(name) != name {
		writeJSONError(w, http.StatusNotFound, "asset_not_found")
		return
	}
	ct, ok := staticAssetContentTypes[name]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "asset_not_found")
		return
	}
	data, err := assetsFS.ReadFile("assets/" + name)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "asset_not_found")
		return
	}
	setSecurityHeaders(w, true)
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) handleAPI(w http.ResponseWriter, r *http.Request) {
	sub := strings.TrimPrefix(r.URL.Path, "/ui/api/")

	if sub == "login" {
		h.handleLogin(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		if ok, code := checkGetFetchMetadata(r); !ok {
			writeJSONError(w, http.StatusForbidden, code)
			return
		}
		sess, hasSession := h.sec.lookupSession(r)
		if !hasSession {
			if h.allowNoAuth() {
				sessionID, created := h.sec.createSession()
				setSessionCookie(w, sessionID)
				sess = created
			} else {
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
		}
		h.handleAPIGet(w, r, sub, sess)

	case http.MethodPost:
		if ok, code := checkPostFetchMetadata(r); !ok {
			writeJSONError(w, http.StatusForbidden, code)
			return
		}
		if r.ContentLength > maxPostBodyBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxPostBodyBytes)
		}
		_, hasSession, csrfOK := h.sec.verifyCSRF(r)
		if !hasSession {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if !csrfOK {
			writeJSONError(w, http.StatusForbidden, "forbidden_csrf")
			return
		}
		h.handleAPIPost(w, r, sub)

	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if ok, code := checkPostFetchMetadata(r); !ok {
		writeJSONError(w, http.StatusForbidden, code)
		return
	}
	if r.ContentLength > maxPostBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxPostBodyBytes)
	}
	if allowed, retryAfter := h.sec.checkLoginRateLimit(); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"ok":            false,
			"error":         "rate_limited",
			"retryAfterSec": retryAfter,
		})
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	token := strings.TrimSpace(req.Token)
	if !h.allowNoAuth() {
		if token == "" || h.backend == nil || !h.backend.VerifyControlToken(token) {
			h.sec.recordLoginFailure()
			writeJSONError(w, http.StatusUnauthorized, "invalid_token")
			return
		}
	}

	h.sec.clearLoginFailures()
	sessionID, view := h.sec.createSession()
	setSessionCookie(w, sessionID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"csrfToken": view.csrfToken,
		"expiresAt": view.expiresAt.Format(time.RFC3339),
	})
}

func (h *Handler) handleAPIGet(w http.ResponseWriter, r *http.Request, sub string, sess sessionView) {
	if h.backend == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "backend_unavailable")
		return
	}

	switch {
	case sub == "session":
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"authenticated": true,
			"noAuth":        h.allowNoAuth(),
			"csrfToken":     sess.csrfToken,
			"expiresAt":     sess.expiresAt.Format(time.RFC3339),
		})

	case sub == "status":
		raw, err := h.backend.StatusJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "catalog":
		raw, err := h.backend.CatalogJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "healthz":
		raw, err := h.backend.HealthzJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "events":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.serveEventsSSE(w, r, sess)

	case strings.HasPrefix(sub, "logs/"):
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.handleLogs(w, r, strings.TrimPrefix(sub, "logs/"), sess)

	case sub == "snippet":
		h.handleSnippet(w, r)

	default:
		writeJSONError(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) handleLogs(w http.ResponseWriter, r *http.Request, name string, sess sessionView) {
	if name != "hub" && !isValidName(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid_log_name")
		return
	}
	linesN := defaultLogsLines
	if rawLines := strings.TrimSpace(r.URL.Query().Get("lines")); rawLines != "" {
		parsed, err := strconv.Atoi(rawLines)
		if err != nil || parsed < 1 {
			writeJSONError(w, http.StatusBadRequest, "invalid_lines_param")
			return
		}
		if parsed > maxLogsLines {
			parsed = maxLogsLines
		}
		linesN = parsed
	}

	follow := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("follow")))
	if follow == "1" || follow == "true" {
		h.serveLogsSSE(w, r, name, linesN, sess)
		return
	}

	entries, found, err := h.backend.LogsTail(name, linesN)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "log_not_found", "name": name})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
		return
	}
	out := make([]LogEntry, 0, len(entries))
	for _, e := range entries {
		e.Line = h.redact(e.Line)
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"name":  name,
		"lines": out,
	})
}

func (h *Handler) serveLogsSSE(w http.ResponseWriter, r *http.Request, name string, initialLines int, sess sessionView) {
	// Subscribe BEFORE tailing so no lines emitted in the gap are missed.
	ch, unsub, found := h.backend.SubscribeLogs(name)
	if !found || ch == nil {
		if unsub != nil {
			unsub()
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "log_not_found", "name": name})
		return
	}
	defer func() {
		if unsub != nil {
			unsub()
		}
	}()

	initial, _, err := h.backend.LogsTail(name, initialLines)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
		return
	}

	var lastEventID uint64
	if rawID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); rawID != "" {
		if parsed, parseErr := strconv.ParseUint(rawID, 10, 64); parseErr == nil {
			lastEventID = parsed
		}
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	writeEntry := func(entry LogEntry) bool {
		entry.Line = h.redact(entry.Line)
		payload, mErr := json.Marshal(entry)
		if mErr != nil {
			return true
		}
		if entry.Seq > 0 {
			if _, wErr := fmt.Fprintf(w, "id: %d\n", entry.Seq); wErr != nil {
				return false
			}
		}
		if _, wErr := fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload); wErr != nil {
			return false
		}
		flush()
		return true
	}

	maxSentSeq := lastEventID
	for _, entry := range initial {
		if entry.Seq > 0 && entry.Seq <= maxSentSeq {
			continue
		}
		if !writeEntry(entry) {
			return
		}
		if entry.Seq > maxSentSeq {
			maxSentSeq = entry.Seq
		}
	}
	if _, wErr := io.WriteString(w, ": connected\n\n"); wErr != nil {
		return
	}
	flush()

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closedCh:
			return
		case <-sess.revoked:
			return
		case entry, ok := <-ch:
			if !ok {
				return
			}
			if entry.Seq > 0 && entry.Seq <= maxSentSeq {
				continue
			}
			if !writeEntry(entry) {
				return
			}
			if entry.Seq > maxSentSeq {
				maxSentSeq = entry.Seq
			}
		case <-ticker.C:
			if _, wErr := io.WriteString(w, ": ping\n\n"); wErr != nil {
				return
			}
			flush()
		}
	}
}

func (h *Handler) serveEventsSSE(w http.ResponseWriter, r *http.Request, sess sessionView) {
	ch, unsub := h.backend.SubscribeEvents()
	if unsub != nil {
		defer unsub()
	}

	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	setSecurityHeaders(w, false)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	if _, err := io.WriteString(w, "event: ready\ndata: {\"ok\":true}\n\n"); err != nil {
		return
	}
	flush()

	ticker := time.NewTicker(ssePingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.closedCh:
			return
		case <-sess.revoked:
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			ev.Message = h.redact(ev.Message)
			if ev.Type == "" {
				ev.Type = "state_change"
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", sanitizeSSEEventName(ev.Type), payload); err != nil {
				return
			}
			flush()
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flush()
		}
	}
}

func sanitizeSSEEventName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "message"
	}
	return b.String()
}

func (h *Handler) handleSnippet(w http.ResponseWriter, r *http.Request) {
	service := strings.TrimSpace(r.URL.Query().Get("service"))
	client := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("client")))
	if !isValidName(service) {
		writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
		return
	}
	if client == "" {
		client = "claude"
	}
	if _, ok := validSnippetClients[client]; !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_client")
		return
	}
	snippet, err := h.backend.ClientSnippet(service, client)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": h.redact(err.Error())})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": service,
		"client":  client,
		"snippet": h.redact(snippet),
	})
}

func (h *Handler) handleAPIPost(w http.ResponseWriter, r *http.Request, sub string) {
	if sub == "logout" {
		h.sec.deleteSession(r)
		clearSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	if h.backend == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "backend_unavailable")
		return
	}

	switch {
	case sub == "reload":
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, err := h.backend.ReloadHub(opCtx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "services/start-all":
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, err := h.backend.StartAll(opCtx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "services/stop-all":
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, err := h.backend.StopAll(opCtx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "services/restart-all":
		opCtx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		raw, err := h.backend.RestartAll(opCtx)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": h.redact(err.Error())})
			return
		}
		writeRawJSON(w, http.StatusOK, raw)

	case sub == "probe-tools":
		h.handleProbeTools(w, r)

	case strings.HasPrefix(sub, "services/"):
		h.handleServiceAction(w, r, strings.TrimPrefix(sub, "services/"))

	default:
		writeJSONError(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) handleServiceAction(w http.ResponseWriter, r *http.Request, rest string) {
	name, action, ok := strings.Cut(rest, "/")
	if !ok || !isValidName(name) || strings.Contains(action, "/") {
		writeJSONError(w, http.StatusBadRequest, "invalid_service_action")
		return
	}

	var (
		raw   []byte
		found bool
		err   error
	)
	switch action {
	case "start":
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, found, err = h.backend.StartService(opCtx, name)

	case "stop":
		opCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		raw, found, err = h.backend.StopService(opCtx, name)

	case "restart":
		waitHealthy := false
		waitParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
		if waitParam == "healthy" || waitParam == "1" || waitParam == "true" {
			waitHealthy = true
		} else if r.Body != nil {
			var body struct {
				WaitHealthy bool `json:"waitHealthy"`
			}
			if decErr := json.NewDecoder(r.Body).Decode(&body); decErr == nil && body.WaitHealthy {
				waitHealthy = true
			}
		}
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, found, err = h.backend.RestartService(opCtx, name, waitHealthy)

	case "reload":
		opCtx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
		defer cancel()
		raw, found, err = h.backend.ReloadService(opCtx, name)

	case "sleep":
		opCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		raw, found, err = h.backend.SleepService(opCtx, name)

	case "wake":
		opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		raw, found, err = h.backend.WakeService(opCtx, name)

	default:
		writeJSONError(w, http.StatusNotFound, "unknown_action")
		return
	}

	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "service_not_found", "name": name})
		return
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrReloadNotSupported) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"ok": false, "name": name, "error": h.redact(err.Error())})
		return
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
		return
	}
	writeRawJSON(w, http.StatusOK, raw)
}

func (h *Handler) handleProbeTools(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Service string `json:"service"`
		Wake    bool   `json:"wake"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_json")
		return
	}
	service := strings.TrimSpace(req.Service)
	if !isValidName(service) {
		writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
		return
	}

	opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := h.backend.ProbeTools(opCtx, service, req.Wake)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":      false,
			"service": service,
			"error":   h.redact(err.Error()),
		})
		return
	}
	res.Endpoint = h.redact(res.Endpoint)
	res.Reason = h.redact(res.Reason)
	if res.Tools == nil {
		res.Tools = []ToolInfo{}
	}
	for i := range res.Tools {
		res.Tools[i].Description = h.redact(res.Tools[i].Description)
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		ToolsProbeResult
	}{
		OK:               true,
		ToolsProbeResult: res,
	})
}
