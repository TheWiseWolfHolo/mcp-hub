package hub

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ErrDashboardNotMounted indicates no embedded Web UI handler is mounted on the hub.
var ErrDashboardNotMounted = errors.New("web dashboard is not mounted")

// DashboardHandler is an optional embedded Web UI handler mounted at /ui on ControlServer.
type DashboardHandler interface {
	http.Handler
	MintBootstrapToken() (nonce string, expiresAt time.Time)
}

// HubEvent represents a live lifecycle or configuration event emitted by Supervisor.
type HubEvent struct {
	Seq       uint64 `json:"seq,omitempty"`
	Type      string `json:"type"`
	Service   string `json:"service,omitempty"`
	State     string `json:"state,omitempty"`
	Message   string `json:"message,omitempty"`
	Timestamp string `json:"timestamp"`
}

const (
	controlMaxHeaderBytes   = 64 << 10 // 64 KiB
	controlMaxPostBodyBytes = 64 << 10 // 64 KiB
	defaultLogsLines        = 50
	maxLogsLines            = 500
	logsSSEPingInterval     = 15 * time.Second
)

// HealthzResponse is the unauthenticated /healthz JSON summary.
type HealthzResponse struct {
	OK       bool                            `json:"ok"`
	Services map[string]ServiceHealthSummary `json:"services"`
	Proxies  map[string]bool                 `json:"proxies,omitempty"`
	Gateway  *GatewayHealthSummary           `json:"gateway,omitempty"`
}

// StatusResponse is the detailed admin /status JSON payload.
type StatusResponse struct {
	SchemaVersion         int                      `json:"schemaVersion"`
	OK                    bool                     `json:"ok"`
	Version               string                   `json:"version"`
	PID                   int                      `json:"pid"`
	StartedAt             string                   `json:"startedAt"`
	UptimeSec             int64                    `json:"uptimeSec"`
	ControlAddr           string                   `json:"controlAddr"`
	ConfigPath            string                   `json:"configPath,omitempty"`
	LogDir                string                   `json:"logDir,omitempty"`
	TrayEnabled           bool                     `json:"trayEnabled"`
	TreeTotalPrivateMB    float64                  `json:"treeTotalPrivateMB"`
	TreeTotalWorkingSetMB float64                  `json:"treeTotalWorkingSetMB"`
	HubPrivateMB          float64                  `json:"hubPrivateMB,omitempty"`
	HubWorkingSetMB       float64                  `json:"hubWorkingSetMB,omitempty"`
	Goroutines            int                      `json:"goroutines,omitempty"`
	Services              map[string]ServiceStatus `json:"services"`
	Proxies               map[string]ProxyStatus   `json:"proxies"`
	Gateway               *GatewayStatus           `json:"gateway,omitempty"`
}

// RestartResponse is returned by POST /restart/<name>.
type RestartResponse struct {
	OK      bool          `json:"ok"`
	Name    string        `json:"name"`
	Service ServiceStatus `json:"service"`
	Error   string        `json:"error,omitempty"`
}

// ServiceActionResponse is returned by POST /start/<name>, /stop/<name>, /sleep/<name>, /wake/<name>.
type ServiceActionResponse struct {
	OK      bool          `json:"ok"`
	Name    string        `json:"name"`
	Service ServiceStatus `json:"service"`
	Error   string        `json:"error,omitempty"`
}

// BulkServiceActionResponse is returned by POST /services/start-all and POST /services/stop-all.
type BulkServiceActionResponse struct {
	OK       bool                             `json:"ok"`
	Services map[string]ServiceActionResponse `json:"services"`
}

// BulkRestartResponse is returned by POST /services/restart-all.
type BulkRestartResponse struct {
	OK       bool                       `json:"ok"`
	Services map[string]RestartResponse `json:"services"`
}

// DashboardSessionResponse is returned by POST /ui/session.
type DashboardSessionResponse struct {
	OK        bool   `json:"ok"`
	URL       string `json:"url"`
	Path      string `json:"path,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// ReloadHubResponse is returned by POST /reload.
type ReloadHubResponse struct {
	OK                bool     `json:"ok"`
	AddedServices     []string `json:"addedServices,omitempty"`
	RemovedServices   []string `json:"removedServices,omitempty"`
	RestartedServices []string `json:"restartedServices,omitempty"`
	UnchangedServices []string `json:"unchangedServices,omitempty"`
	ReloadedProxies   []string `json:"reloadedProxies,omitempty"`
	ReloadedGateway   bool     `json:"reloadedGateway,omitempty"`
}

// ReloadServiceResponse is returned by POST /reload/<name>.
type ReloadServiceResponse struct {
	OK     bool            `json:"ok"`
	Name   string          `json:"name"`
	Result json.RawMessage `json:"result,omitempty"`
}

// CatalogAuthInfo describes how clients authenticate against a catalog entry.
type CatalogAuthInfo struct {
	Type      string `json:"type"` // "bearer" or "none"
	TokenFile string `json:"tokenFile,omitempty"`
	Header    string `json:"header,omitempty"`
}

// CatalogCapabilities summarizes supported operational capabilities for a catalog entry.
type CatalogCapabilities struct {
	Reload           bool `json:"reload"`
	GracefulShutdown bool `json:"gracefulShutdown"`
	LazyWake         bool `json:"lazyWake"`
	StdioBridge      bool `json:"stdioBridge"`
}

// CatalogEntry joins a managed service/bridge with its proxy and gateway access URLs.
type CatalogEntry struct {
	Name         string              `json:"name"`
	Mode         string              `json:"mode"`
	ClientURL    string              `json:"clientUrl,omitempty"`
	ClientURLs   []string            `json:"clientUrls,omitempty"`
	ViaProxy     string              `json:"viaProxy,omitempty"`
	ViaGateway   string              `json:"viaGateway,omitempty"`
	Auth         CatalogAuthInfo     `json:"auth"`
	State        string              `json:"state"`
	Healthy      bool                `json:"healthy"`
	Capabilities CatalogCapabilities `json:"capabilities"`
	Description  string              `json:"description,omitempty"`
	Tags         []string            `json:"tags,omitempty"`
}

// CatalogResponse is returned by GET /catalog.
type CatalogResponse struct {
	SchemaVersion int            `json:"schemaVersion"`
	OK            bool           `json:"ok"`
	Entries       []CatalogEntry `json:"entries"`
}

// LogsResponse is returned by GET /logs/<name> (non-follow mode).
type LogsResponse struct {
	OK    bool      `json:"ok"`
	Name  string    `json:"name"`
	Lines []LogLine `json:"lines"`
}

type configValidationError struct {
	err error
}

func (e configValidationError) Error() string { return e.err.Error() }
func (e configValidationError) Unwrap() error { return e.err }

type uiMountEntry struct {
	prefix  string
	handler http.Handler
}

// ControlHooks wires ControlServer routes to Supervisor actions (and test fakes).
type ControlHooks struct {
	Healthz                func() HealthzResponse
	Status                 func() StatusResponse
	Catalog                func() CatalogResponse
	GetRingLog             func(name string) (*RingLog, bool)
	StartService           func(ctx context.Context, name string) (ServiceStatus, bool, error)
	StopService            func(ctx context.Context, name string) (ServiceStatus, bool, error)
	SleepService           func(ctx context.Context, name string) (ServiceStatus, bool, error)
	WakeService            func(ctx context.Context, name string) (ServiceStatus, bool, error)
	StartAll               func(ctx context.Context) map[string]ServiceActionResponse
	StopAll                func(ctx context.Context) map[string]ServiceActionResponse
	RestartAll             func(ctx context.Context, waitHealthy bool) map[string]RestartResponse
	Restart                func(ctx context.Context, name string, waitHealthy bool) (ServiceStatus, bool, error)
	ReloadHub              func(ctx context.Context) (ReloadHubResponse, error)
	ValidateConfig         func() (ReloadHubResponse, error)
	ReloadService          func(ctx context.Context, name string) (json.RawMessage, bool, error)
	SubscribeEvents        func() (<-chan HubEvent, func())
	CreateDashboardSession func() (string, time.Time, error)
	Stop                   func()
	Redactor               *Redactor
	bindLogStream          func(*http.Request) (*http.Request, func(), bool)
	uiMount                func() (string, http.Handler)
}

// ControlServer exposes the localhost HTTP control API with hot-reloadable Bearer auth.
type ControlServer struct {
	addr        string
	tokenFile   string
	allowNoAuth atomic.Bool
	tokenStore  *TokenStore
	hooks       ControlHooks
	logger      *log.Logger
	uiMount     atomic.Pointer[uiMountEntry]

	mu         sync.RWMutex
	ln         net.Listener
	srv        *http.Server
	baseCancel context.CancelFunc
	listenAddr string
	logStreams tokenLifetimes
}

// NewControlServer creates a localhost ControlServer.
func NewControlServer(addr, tokenFile string, allowNoAuth bool, hooks ControlHooks, logger *log.Logger) *ControlServer {
	cs := &ControlServer{
		addr:       addr,
		tokenFile:  tokenFile,
		tokenStore: NewTokenStore(),
		hooks:      hooks,
		logger:     logger,
		listenAddr: addr,
	}
	cs.allowNoAuth.Store(allowNoAuth)
	cs.hooks.bindLogStream = cs.bindLogStream
	cs.hooks.uiMount = cs.getUIHandler
	return cs
}

// SetUIHandler mounts an optional HTTP handler under prefix (e.g. "/ui") without weakening
// existing Control API routes. Passing h == nil unmounts the handler.
func (c *ControlServer) SetUIHandler(prefix string, h http.Handler) {
	if c == nil {
		return
	}
	if h == nil {
		c.uiMount.Store(nil)
		return
	}
	norm := "/" + strings.Trim(strings.TrimSpace(prefix), "/")
	if norm == "/" {
		norm = "/ui"
	}
	c.uiMount.Store(&uiMountEntry{prefix: norm, handler: h})
}

func (c *ControlServer) getUIHandler() (string, http.Handler) {
	if m := c.uiMount.Load(); m != nil {
		return m.prefix, m.handler
	}
	return "", nil
}

// AllowNoAuth reports whether the control server currently permits unauthenticated access.
func (c *ControlServer) AllowNoAuth() bool {
	if c == nil {
		return false
	}
	return c.allowNoAuth.Load() && c.tokenStore.Empty()
}

// VerifyControlToken validates a raw control token string in constant time against live control tokens.
func (c *ControlServer) VerifyControlToken(token string) bool {
	if c == nil {
		return false
	}
	if c.tokenStore == nil || c.tokenStore.Empty() {
		return c.allowNoAuth.Load()
	}
	if token == "" {
		return false
	}
	rp := c.tokenStore.rules.Load()
	if rp == nil || len(*rp) == 0 {
		return false
	}
	gotSum := sha256.Sum256([]byte(token))
	matched := false
	for i := range *rp {
		if subtle.ConstantTimeCompare(gotSum[:], (*rp)[i].Digest[:]) == 1 {
			matched = true
		}
	}
	return matched
}

func (c *ControlServer) bindLogStream(r *http.Request) (*http.Request, func(), bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	rule, ok := c.tokenStore.Match(r)
	if !ok && !(c.allowNoAuth.Load() && c.tokenStore.Empty()) {
		return nil, nil, false
	}
	request, release := c.logStreams.bind(r, rule)
	return request, release, true
}

// NewControlHandler builds the control HTTP handler for production and unit tests.
// If expectedToken is non-empty, all endpoints except /healthz require Authorization: Bearer <token>.
// If expectedToken is empty, unauthenticated mode is permitted (intended for unit tests only).
func NewControlHandler(expectedToken string, boundAddr string, hooks ControlHooks) http.Handler {
	store := NewTokenStore()
	allowNoAuth := strings.TrimSpace(expectedToken) == ""
	if !allowNoAuth {
		store.SetSingle("control", expectedToken, nil, nil, false)
	}
	return NewControlHandlerWithStore(store, func() bool { return allowNoAuth }, boundAddr, hooks)
}

// NewControlHandlerWithStore builds the control HTTP handler backed by an atomically hot-reloadable TokenStore.
func NewControlHandlerWithStore(store *TokenStore, allowNoAuth func() bool, boundAddr string, hooks ControlHooks) http.Handler {
	allowedSet := makeAllowedHostSet(DefaultAllowedHosts(boundAddr))
	var stopOnce sync.Once

	requireAuth := func(w http.ResponseWriter, r *http.Request) bool {
		if store != nil && !store.Empty() {
			if !store.Verify(r) {
				w.Header().Set("WWW-Authenticate", "Bearer")
				writeJSONError(w, http.StatusUnauthorized, "unauthorized")
				return false
			}
			return true
		}
		if allowNoAuth != nil && allowNoAuth() {
			return true
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}

	capPostBody := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return true
		}
		if r.ContentLength > controlMaxPostBodyBytes {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
			return false
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, controlMaxPostBodyBytes)
		}
		return true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// Route mounted embedded Web UI prefix directly (enforcing loopback Host check),
		// while keeping <prefix>/session behind CheckLoopbackGuard + Bearer auth.
		if hooks.uiMount != nil {
			if prefix, uiHandler := hooks.uiMount(); uiHandler != nil && prefix != "" {
				if (path == prefix || strings.HasPrefix(path, prefix+"/")) &&
					!(path == prefix+"/session" && hooks.CreateDashboardSession != nil) {
					if !isHostAllowed(r.Host, allowedSet) {
						writeJSONError(w, http.StatusForbidden, "forbidden_host")
						return
					}
					uiHandler.ServeHTTP(w, r)
					return
				}
			}
		}

		allowDirectNav := (r.Method == http.MethodGet || r.Method == http.MethodHead) && path == "/healthz"
		if !CheckLoopbackGuard(w, r, allowedSet, allowDirectNav) {
			return
		}
		if !capPostBody(w, r) {
			return
		}

		switch {
		case path == "/healthz":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			res := HealthzResponse{OK: true, Services: map[string]ServiceHealthSummary{}}
			if hooks.Healthz != nil {
				res = hooks.Healthz()
				if res.Services == nil {
					res.Services = map[string]ServiceHealthSummary{}
				}
			}
			writeJSON(w, http.StatusOK, res)
			return

		case path == "/status":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			res := StatusResponse{
				SchemaVersion: 1,
				Services:      map[string]ServiceStatus{},
				Proxies:       map[string]ProxyStatus{},
			}
			if hooks.Status != nil {
				res = hooks.Status()
				if res.SchemaVersion == 0 {
					res.SchemaVersion = 1
				}
				if res.Services == nil {
					res.Services = map[string]ServiceStatus{}
				}
				if res.Proxies == nil {
					res.Proxies = map[string]ProxyStatus{}
				}
			}
			writeJSON(w, http.StatusOK, res)
			return

		case path == "/catalog":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			res := CatalogResponse{SchemaVersion: 1, OK: true, Entries: []CatalogEntry{}}
			if hooks.Catalog != nil {
				res = hooks.Catalog()
				if res.SchemaVersion == 0 {
					res.SchemaVersion = 1
				}
				if res.Entries == nil {
					res.Entries = []CatalogEntry{}
				}
			}
			writeJSON(w, http.StatusOK, res)
			return

		case strings.HasPrefix(path, "/logs/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/logs/")
			if name != "hub" && !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_log_name")
				return
			}
			if hooks.GetRingLog == nil {
				writeJSONError(w, http.StatusNotFound, "log_not_found")
				return
			}
			ring, ok := hooks.GetRingLog(name)
			if !ok || ring == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "log_not_found", "name": name})
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

			followVal := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("follow")))
			if followVal == "1" || followVal == "true" {
				if hooks.bindLogStream != nil {
					request, release, authorized := hooks.bindLogStream(r)
					if !authorized {
						w.Header().Set("WWW-Authenticate", "Bearer")
						writeJSONError(w, http.StatusUnauthorized, "unauthorized")
						return
					}
					defer release()
					r = request
				}
				serveLogsSSE(w, r, ring, linesN)
				return
			}

			lines := ring.Tail(linesN)
			if lines == nil {
				lines = []LogLine{}
			}
			writeJSON(w, http.StatusOK, LogsResponse{
				OK:    true,
				Name:  name,
				Lines: lines,
			})
			return

		case strings.HasPrefix(path, "/start/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/start/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.StartService == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			st, found, err := hooks.StartService(opCtx, name)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error()), "service": st})
				return
			}
			writeJSON(w, http.StatusOK, ServiceActionResponse{OK: true, Name: name, Service: st})
			return

		case strings.HasPrefix(path, "/stop/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/stop/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.StopService == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			st, found, err := hooks.StopService(opCtx, name)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error()), "service": st})
				return
			}
			writeJSON(w, http.StatusOK, ServiceActionResponse{OK: true, Name: name, Service: st})
			return

		case strings.HasPrefix(path, "/restart/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/restart/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.Restart == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			waitParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
			waitHealthy := waitParam == "healthy" || waitParam == "1" || waitParam == "true"
			timeout := 15 * time.Second
			if waitHealthy {
				timeout = 30 * time.Second
			}
			restartCtx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			st, found, err := hooks.Restart(restartCtx, name, waitHealthy)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error()), "service": st})
				return
			}
			writeJSON(w, http.StatusOK, RestartResponse{OK: true, Name: name, Service: st})
			return

		case path == "/reload":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.ReloadHub == nil {
				writeJSONError(w, http.StatusNotImplemented, "reload_not_supported")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			res, err := hooks.ReloadHub(opCtx)
			if err != nil {
				status := http.StatusInternalServerError
				var valErr configValidationError
				if errors.As(err, &valErr) {
					status = http.StatusBadRequest
				}
				writeJSON(w, status, map[string]any{"ok": false, "error": hooks.Redactor.Redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, res)
			return

		case strings.HasPrefix(path, "/reload/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/reload/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.ReloadService == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
			defer cancel()
			resRaw, found, err := hooks.ReloadService(opCtx, name)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if errors.Is(err, ErrReloadNotSupported) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reload_not_supported", "name": name})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, ReloadServiceResponse{OK: true, Name: name, Result: resRaw})
			return

		case path == "/events":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodGet {
				w.Header().Set("Allow", "GET")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.SubscribeEvents == nil {
				writeJSONError(w, http.StatusNotImplemented, "events_not_supported")
				return
			}
			if hooks.bindLogStream != nil {
				request, release, authorized := hooks.bindLogStream(r)
				if !authorized {
					w.Header().Set("WWW-Authenticate", "Bearer")
					writeJSONError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				defer release()
				r = request
			}
			serveEventsSSE(w, r, hooks.SubscribeEvents)
			return

		case strings.HasPrefix(path, "/sleep/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/sleep/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.SleepService == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			st, found, err := hooks.SleepService(opCtx, name)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if err != nil {
				status := http.StatusInternalServerError
				if isServiceStateConflictError(err) {
					status = http.StatusConflict
				}
				writeJSON(w, status, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error()), "service": st})
				return
			}
			writeJSON(w, http.StatusOK, ServiceActionResponse{OK: true, Name: name, Service: st})
			return

		case strings.HasPrefix(path, "/wake/"):
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			name := strings.TrimPrefix(path, "/wake/")
			if !validNameRe.MatchString(name) {
				writeJSONError(w, http.StatusBadRequest, "invalid_service_name")
				return
			}
			if hooks.WakeService == nil {
				writeJSONError(w, http.StatusNotFound, "service_not_found")
				return
			}
			opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			st, found, err := hooks.WakeService(opCtx, name)
			if !found {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "service_not_found", "name": name})
				return
			}
			if err != nil {
				status := http.StatusInternalServerError
				if isServiceStateConflictError(err) {
					status = http.StatusConflict
				}
				writeJSON(w, status, map[string]any{"ok": false, "name": name, "error": hooks.Redactor.Redact(err.Error()), "service": st})
				return
			}
			writeJSON(w, http.StatusOK, ServiceActionResponse{OK: true, Name: name, Service: st})
			return

		case path == "/services/start-all":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.StartAll == nil {
				writeJSONError(w, http.StatusNotImplemented, "start_all_not_supported")
				return
			}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(65 * time.Second))
			opCtx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
			defer cancel()
			results := hooks.StartAll(opCtx)
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
			writeJSON(w, http.StatusOK, BulkServiceActionResponse{OK: allOK, Services: results})
			return

		case path == "/services/stop-all":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.StopAll == nil {
				writeJSONError(w, http.StatusNotImplemented, "stop_all_not_supported")
				return
			}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(35 * time.Second))
			opCtx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			results := hooks.StopAll(opCtx)
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
			writeJSON(w, http.StatusOK, BulkServiceActionResponse{OK: allOK, Services: results})
			return

		case path == "/services/restart-all":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.RestartAll == nil {
				writeJSONError(w, http.StatusNotImplemented, "restart_all_not_supported")
				return
			}
			waitParam := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("wait")))
			waitHealthy := waitParam == "healthy" || waitParam == "1" || waitParam == "true"
			timeout := 30 * time.Second
			if waitHealthy {
				timeout = 60 * time.Second
			}
			rc := http.NewResponseController(w)
			_ = rc.SetWriteDeadline(time.Now().Add(timeout + 5*time.Second))
			opCtx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			results := hooks.RestartAll(opCtx, waitHealthy)
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
			writeJSON(w, http.StatusOK, BulkRestartResponse{OK: allOK, Services: results})
			return

		case path == "/config/validate":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.ValidateConfig == nil {
				writeJSONError(w, http.StatusNotImplemented, "validate_not_supported")
				return
			}
			res, err := hooks.ValidateConfig()
			if err != nil {
				status := http.StatusInternalServerError
				var valErr configValidationError
				if errors.As(err, &valErr) {
					status = http.StatusBadRequest
				}
				writeJSON(w, status, map[string]any{"ok": false, "error": hooks.Redactor.Redact(err.Error())})
				return
			}
			writeJSON(w, http.StatusOK, res)
			return

		case path == "/ui/session":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			if hooks.CreateDashboardSession == nil {
				writeJSONError(w, http.StatusNotImplemented, "dashboard_not_mounted")
				return
			}
			launchURL, expiresAt, err := hooks.CreateDashboardSession()
			if errors.Is(err, ErrDashboardNotMounted) {
				writeJSONError(w, http.StatusNotImplemented, "dashboard_not_mounted")
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": hooks.Redactor.Redact(err.Error())})
				return
			}
			resp := DashboardSessionResponse{OK: true, URL: launchURL}
			if !expiresAt.IsZero() {
				resp.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
			}
			writeJSON(w, http.StatusOK, resp)
			return

		case path == "/stop":
			if !requireAuth(w, r) {
				return
			}
			if r.Method != http.MethodPost {
				w.Header().Set("Allow", "POST")
				writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "stopping": true})
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if hooks.Stop != nil {
				stopOnce.Do(func() {
					go hooks.Stop()
				})
			}
			return

		default:
			writeJSONError(w, http.StatusNotFound, "not_found")
		}
	})
}

func isServiceStateConflictError(err error) bool {
	return errors.Is(err, ErrServiceStopped) ||
		errors.Is(err, ErrBridgeStopped) ||
		errors.Is(err, ErrServiceDegraded) ||
		errors.Is(err, ErrBridgeDegraded)
}

func serveLogsSSE(w http.ResponseWriter, r *http.Request, ring *RingLog, initialLines int) {
	// Clear per-request write deadline so http.Server.WriteTimeout (45s) does not terminate SSE streams.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	var lastEventID uint64
	if rawID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); rawID != "" {
		if parsed, err := strconv.ParseUint(rawID, 10, 64); err == nil {
			lastEventID = parsed
		}
	}

	// Subscribe BEFORE tailing so no lines emitted between Tail and Subscribe are lost.
	ch, unsub := ring.Subscribe()
	defer unsub()

	var initial []LogLine
	if lastEventID > 0 {
		for _, ln := range ring.Tail(0) {
			if ln.Seq > lastEventID {
				initial = append(initial, ln)
			}
		}
	} else {
		initial = ring.Tail(initialLines)
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)

	lastSentSeq := lastEventID
	writeLogEvent := func(line LogLine) bool {
		if line.Seq > 0 {
			if line.Seq <= lastSentSeq {
				return true
			}
			lastSentSeq = line.Seq
		}
		payload, err := marshalCompactJSON(line)
		if err != nil {
			return true
		}
		var writeErr error
		if line.Seq > 0 {
			_, writeErr = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", line.Seq, payload)
		} else {
			_, writeErr = fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload)
		}
		if writeErr != nil {
			return false
		}
		_ = rc.Flush()
		return true
	}

	for _, ln := range initial {
		if !writeLogEvent(ln) {
			return
		}
	}
	_ = rc.Flush()

	pingTicker := time.NewTicker(logsSSEPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ln, ok := <-ch:
			if !ok {
				return
			}
			if !writeLogEvent(ln) {
				return
			}
		case <-pingTicker.C:
			if _, err := fmt.Fprint(w, ":ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

func serveEventsSSE(w http.ResponseWriter, r *http.Request, subscribe func() (<-chan HubEvent, func())) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	ch, unsub := subscribe()
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)

	readyEv := HubEvent{
		Type:      "ready",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	if payload, err := marshalCompactJSON(readyEv); err == nil {
		if _, err := fmt.Fprintf(w, "event: ready\ndata: %s\n\n", payload); err != nil {
			return
		}
	}
	_ = rc.Flush()

	pingTicker := time.NewTicker(logsSSEPingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			payload, err := marshalCompactJSON(ev)
			if err != nil {
				continue
			}
			var writeErr error
			if ev.Seq > 0 {
				_, writeErr = fmt.Fprintf(w, "id: %d\nevent: event\ndata: %s\n\n", ev.Seq, payload)
			} else {
				_, writeErr = fmt.Fprintf(w, "event: event\ndata: %s\n\n", payload)
			}
			if writeErr != nil {
				return
			}
			_ = rc.Flush()
		case <-pingTicker.C:
			if _, err := fmt.Fprint(w, ":ping\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

// ReloadTokens reloads controlTokenFile (+ optional .next grace file) into the live TokenStore atomically.
func (c *ControlServer) ReloadTokens(tokenFile string, allowNoAuth bool) ([]string, error) {
	commit, secrets, err := c.prepareTokens(tokenFile, allowNoAuth)
	if err != nil {
		return nil, err
	}
	commit()
	return secrets, nil
}

func (c *ControlServer) prepareTokens(tokenFile string, allowNoAuth bool) (func(), []string, error) {
	trimmed := strings.TrimSpace(tokenFile)
	var rules []TokenRule
	var secrets []string
	if trimmed == "" {
		if !allowNoAuth {
			return nil, nil, errors.New("controlTokenFile is required unless allowInsecureNoControlAuth is true")
		}
	} else {
		store := NewTokenStore()
		var err error
		secrets, err = store.LoadTokenFileWithGrace("control", trimmed, nil, nil, false)
		if err != nil {
			return nil, nil, fmt.Errorf("controlTokenFile: %w", err)
		}
		rules = *store.rules.Load()
	}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.hooks.Redactor.AddSecrets(secrets)
		streamRules := rules
		if trimmed == "" {
			streamRules = []TokenRule{{}} // the unauthenticated stream lifetime
		}
		nextStreams := lifetimesForRules(streamRules, c.logStreams)
		c.logStreams.retire(nextStreams)
		c.logStreams = nextStreams
		c.tokenStore.SetRules(rules)
		c.allowNoAuth.Store(trimmed == "")
		c.tokenFile = trimmed
	}, secrets, nil
}

// Start loads the controlTokenFile (with .next grace), binds addr, and begins serving.
func (c *ControlServer) Start() error {
	if _, err := c.ReloadTokens(c.tokenFile, c.allowNoAuth.Load()); err != nil {
		return err
	}

	ln, err := net.Listen("tcp", c.addr)
	if err != nil {
		return fmt.Errorf("control listen %q: %w", c.addr, err)
	}
	boundAddr := ln.Addr().String()
	handler := NewControlHandlerWithStore(c.tokenStore, c.allowNoAuth.Load, boundAddr, c.hooks)

	baseCtx, baseCancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           handler,
		MaxHeaderBytes:    controlMaxHeaderBytes,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      45 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return baseCtx
		},
	}

	c.mu.Lock()
	c.ln = ln
	c.srv = srv
	c.baseCancel = baseCancel
	c.listenAddr = boundAddr
	c.mu.Unlock()

	if c.logger != nil {
		c.logger.Printf("[control] listening on %s (auth=%v)", boundAddr, !c.tokenStore.Empty())
	}

	go func() {
		defer baseCancel()
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) && c.logger != nil {
			c.logger.Printf("[control] serve stopped with error: %v", err)
		}
	}()
	return nil
}

// CancelBaseContext aborts open /logs SSE streams immediately during shutdown.
func (c *ControlServer) CancelBaseContext() {
	c.mu.Lock()
	cancel := c.baseCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ListenAddr returns the bound control address.
func (c *ControlServer) ListenAddr() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.listenAddr
}

// Shutdown gracefully closes the control HTTP server.
func (c *ControlServer) Shutdown(ctx context.Context) error {
	c.mu.Lock()
	srv := c.srv
	cancel := c.baseCancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
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
