package hub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GatewayRouteStatus reports runtime metadata and MCP metrics for one /s/<slug> gateway route.
type GatewayRouteStatus struct {
	Slug        string              `json:"slug"`
	Mode        string              `json:"mode"` // "http" or "stdio-bridge"
	Service     string              `json:"service,omitempty"`
	TargetURL   string              `json:"targetUrl,omitempty"`
	Description string              `json:"description,omitempty"`
	State       string              `json:"state,omitempty"`
	Metrics     *MCPMetricsSnapshot `json:"metrics,omitempty"`
}

// GatewayStatus reports overall status of the unified single-port gateway.
type GatewayStatus struct {
	Running    bool                 `json:"running"`
	ListenAddr string               `json:"listenAddr,omitempty"`
	Routes     []GatewayRouteStatus `json:"routes,omitempty"`
}

// GatewayHealthSummary is the compact /healthz summary for the gateway.
type GatewayHealthSummary struct {
	Running    bool            `json:"running"`
	ListenAddr string          `json:"listenAddr,omitempty"`
	Routes     map[string]bool `json:"routes,omitempty"`
}

type gatewayRoute struct {
	cfg        GatewayRouteConfig
	mode       string // "http" or "stdio-bridge"
	targetURL  string
	tokenStore *TokenStore
	httpProxy  *mcpUpstreamHandler
	metrics    *MCPMetrics
	lifetimes  tokenLifetimes
}

type gatewayRouteTable struct {
	allowedSet map[string]struct{}
	routes     map[string]*gatewayRoute
	slugOrder  []string
}

// GatewayServer exposes a single loopback HTTP listener routing /s/<slug>/* to either
// managed StdioBridge services or HTTP MCP upstreams with per-route isolated auth & tool ACLs.
type GatewayServer struct {
	cfg           GatewayConfig
	serviceLookup func(string) *ServiceRunner
	bridgeLookup  func(string) *StdioBridge
	svcCfgLookup  func(string) (ServiceConfig, bool)
	redactor      *Redactor
	logger        *log.Logger
	transport     *http.Transport

	table atomic.Pointer[gatewayRouteTable]

	mu         sync.RWMutex
	ln         net.Listener
	srv        *http.Server
	baseCancel context.CancelFunc
	listenAddr string
	running    bool
}

// NewGatewayServer initializes a GatewayServer from config. Call Start to bind and serve.
func NewGatewayServer(
	cfg GatewayConfig,
	serviceLookup func(string) *ServiceRunner,
	bridgeLookup func(string) *StdioBridge,
	svcCfgLookup func(string) (ServiceConfig, bool),
	redactor *Redactor,
	logger *log.Logger,
) *GatewayServer {
	listenAddr := strings.TrimSpace(cfg.ListenAddr)
	if listenAddr == "" {
		listenAddr = DefaultGatewayAddr
	}
	cfg.ListenAddr = listenAddr
	return &GatewayServer{
		cfg:           cfg,
		serviceLookup: serviceLookup,
		bridgeLookup:  bridgeLookup,
		svcCfgLookup:  svcCfgLookup,
		redactor:      redactor,
		logger:        logger,
		transport:     NewDirectTransport(),
		listenAddr:    listenAddr,
	}
}

func (g *GatewayServer) buildRouteTable(cfg GatewayConfig, boundAddr string, svcCfgLookup func(string) (ServiceConfig, bool)) (*gatewayRouteTable, error) {
	hosts := cfg.AllowedHosts
	if len(hosts) == 0 {
		addr := boundAddr
		if addr == "" {
			addr = cfg.ListenAddr
		}
		hosts = DefaultAllowedHosts(addr)
	}
	allowedSet := makeAllowedHostSet(hosts)

	routes := make(map[string]*gatewayRoute, len(cfg.Routes))
	slugOrder := make([]string, 0, len(cfg.Routes))

	previous := g.table.Load()

	for _, rcfg := range cfg.Routes {
		store, secrets, err := loadTokenStoreFromRoute(rcfg.TokenFile, rcfg.Tokens)
		if err != nil {
			return nil, fmt.Errorf("gateway route %q auth tokens: %w", rcfg.Slug, err)
		}

		mode := "http"
		targetURL := strings.TrimSpace(rcfg.TargetURL)
		if strings.TrimSpace(rcfg.Service) != "" && svcCfgLookup != nil {
			if svcCfg, ok := svcCfgLookup(rcfg.Service); ok {
				if svcCfg.EffectiveMode() == "stdio-bridge" {
					mode = "stdio-bridge"
				} else if targetURL == "" {
					targetURL = InferServiceBaseURL(svcCfg)
				}
			}
		}

		if mode == "stdio-bridge" {
			g.redactor.AddSecrets(secrets)
			routes[rcfg.Slug] = &gatewayRoute{
				cfg:        rcfg,
				mode:       "stdio-bridge",
				tokenStore: store,
				lifetimes:  store.lifetimes(nil),
			}
			slugOrder = append(slugOrder, rcfg.Slug)
			continue
		}

		var upstreamToken string
		if strings.TrimSpace(rcfg.UpstreamTokenFile) != "" {
			upstreamToken, err = ReadSecretFile(rcfg.UpstreamTokenFile)
			if err != nil {
				return nil, fmt.Errorf("gateway route %q upstreamTokenFile: %w", rcfg.Slug, err)
			}
		}
		g.redactor.AddSecrets(append(secrets, upstreamToken))

		var m *MCPMetrics
		if previous != nil {
			if old := previous.routes[rcfg.Slug]; old != nil && old.cfg.Service == rcfg.Service && old.targetURL == targetURL {
				m = old.metrics
			}
		}
		if m == nil {
			m = NewMCPMetrics()
		}
		up, err := newMCPUpstreamHandler(mcpUpstreamOptions{
			Name:            "gateway/" + rcfg.Slug,
			TargetURL:       targetURL,
			HealthPath:      rcfg.HealthPath,
			UpstreamToken:   upstreamToken,
			UpstreamService: rcfg.Service,
			ClientPath:      rcfg.ClientPath,
			TokenStore:      store,
			Transport:       g.transport,
			Metrics:         m,
			ServiceLookup:   g.serviceLookup,
			Redactor:        g.redactor,
			Logger:          g.logger,
			HealthCacheTTL:  defaultHealthCacheTTL,
		})
		if err != nil {
			return nil, fmt.Errorf("gateway route %q: %w", rcfg.Slug, err)
		}

		routes[rcfg.Slug] = &gatewayRoute{
			cfg:        rcfg,
			mode:       "http",
			targetURL:  targetURL,
			tokenStore: store,
			httpProxy:  up,
			metrics:    m,
			lifetimes:  store.lifetimes(nil),
		}
		slugOrder = append(slugOrder, rcfg.Slug)
	}

	return &gatewayRouteTable{
		allowedSet: allowedSet,
		routes:     routes,
		slugOrder:  slugOrder,
	}, nil
}

// Start validates route tokens, binds ListenAddr, and begins serving in a goroutine.
func (g *GatewayServer) Start() error {
	start, _, err := g.prepareStart(context.Background())
	if err != nil {
		return err
	}
	start()
	return nil
}

func (g *GatewayServer) prepareStart(ctx context.Context) (func(), func(), error) {
	g.mu.RLock()
	cfg, lookup := g.cfg, g.svcCfgLookup
	g.mu.RUnlock()
	var listener net.ListenConfig
	ln, err := listener.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("gateway listen %q: %w", cfg.ListenAddr, err)
	}
	table, err := g.buildRouteTable(cfg, ln.Addr().String(), lookup)
	if err != nil {
		_ = ln.Close()
		return nil, nil, err
	}
	return func() { g.serve(ln, table) }, func() { _ = ln.Close() }, nil
}

func (g *GatewayServer) serve(ln net.Listener, table *gatewayRouteTable) {
	boundAddr := ln.Addr().String()
	g.table.Store(table)

	baseCtx, baseCancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           http.HandlerFunc(g.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return baseCtx
		},
	}

	g.mu.Lock()
	g.ln = ln
	g.srv = srv
	g.baseCancel = baseCancel
	g.listenAddr = boundAddr
	g.running = true
	g.mu.Unlock()

	if g.logger != nil {
		g.logger.Printf("[gateway] listening on %s (routes=%d)", boundAddr, len(table.slugOrder))
	}

	go func() {
		defer baseCancel()
		err := srv.Serve(ln)
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
		if err != nil && !errors.Is(err, http.ErrServerClosed) && g.logger != nil {
			g.logger.Printf("[gateway] serve stopped with error: %v", err)
		}
	}()

}

// ReloadConfig atomically swaps gateway routes, tokens, and tool filters without dropping the listener.
func (g *GatewayServer) ReloadConfig(cfg GatewayConfig, svcCfgLookup func(string) (ServiceConfig, bool)) error {
	commit, err := g.prepareReload(cfg, svcCfgLookup)
	if err != nil {
		return err
	}
	commit()
	return nil
}

func (g *GatewayServer) prepareReload(cfg GatewayConfig, svcCfgLookup func(string) (ServiceConfig, bool)) (func(), error) {
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		cfg.ListenAddr = DefaultGatewayAddr
	}
	g.mu.RLock()
	boundAddr := g.listenAddr
	oldListen := g.cfg.ListenAddr
	g.mu.RUnlock()

	if normalizeBindKey(cfg.ListenAddr) != normalizeBindKey(oldListen) || cfg.ListenAddr != oldListen {
		return nil, fmt.Errorf("gateway: cannot hot-reload listenAddr change (%q -> %q)", oldListen, cfg.ListenAddr)
	}

	nextTable, err := g.buildRouteTable(cfg, boundAddr, svcCfgLookup)
	if err != nil {
		return nil, err
	}

	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.cfg = cfg
		g.svcCfgLookup = svcCfgLookup
		oldTable := g.table.Load()
		if oldTable != nil {
			sameHosts := maps.Equal(oldTable.allowedSet, nextTable.allowedSet)
			for slug, old := range oldTable.routes {
				next := nextTable.routes[slug]
				if sameHosts && next != nil && sameGatewayRoute(old, next) {
					next.lifetimes.retire(nil)
					next.lifetimes = next.tokenStore.lifetimes(old.lifetimes)
					old.lifetimes.retire(next.lifetimes)
				} else {
					old.lifetimes.retire(nil)
				}
			}
		}
		g.table.Store(nextTable)
	}, nil
}

func sameGatewayRoute(old, next *gatewayRoute) bool {
	return old.mode == next.mode && old.cfg.Service == next.cfg.Service &&
		old.cfg.EffectiveClientPath() == next.cfg.EffectiveClientPath() &&
		(old.mode == "stdio-bridge" || sameUpstream(old.httpProxy, next.httpProxy))
}

func (g *GatewayServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	table := g.table.Load()
	if table == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "gateway_not_ready")
		return
	}

	path := r.URL.Path
	isGetOrHead := r.Method == http.MethodGet || r.Method == http.MethodHead

	// GET /healthz — unauthenticated loopback summary (never wakes sleeping services).
	if path == "/healthz" {
		if !CheckLoopbackGuard(w, r, table.allowedSet, isGetOrHead) {
			return
		}
		if !isGetOrHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		g.serveGatewayHealthz(w, table)
		return
	}

	// /s/<slug> and /s/<slug>/*
	if strings.HasPrefix(path, "/s/") {
		rest := strings.TrimPrefix(path, "/s/")
		slug, tail, hasSlash := strings.Cut(rest, "/")
		if slug == "" {
			if !CheckLoopbackGuard(w, r, table.allowedSet, false) {
				return
			}
			writeJSONError(w, http.StatusNotFound, "route_not_found")
			return
		}

		isRouteHealthz := isGetOrHead && hasSlash && tail == "healthz"
		if !CheckLoopbackGuard(w, r, table.allowedSet, isRouteHealthz) {
			return
		}

		route, ok := table.routes[slug]
		if !ok {
			writeJSONError(w, http.StatusNotFound, "route_not_found")
			return
		}

		if isRouteHealthz {
			g.serveRouteHealthz(w, r, route)
			return
		}

		rule, matched := route.tokenStore.Match(r)
		if !matched {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		r, release := route.lifetimes.bind(r, rule)
		defer release()
		if r.Context().Err() != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// Strip /s/<slug> prefix so upstream sees /mcp (or / when bare /s/<slug> is called).
		r2 := r.Clone(r.Context())
		if hasSlash && tail != "" {
			r2.URL.Path = "/" + tail
		} else {
			r2.URL.Path = "/"
		}
		r2.URL.RawPath = ""

		if route.mode == "stdio-bridge" {
			if !allowMCPProxyRequest(w, r2, route.cfg.ClientPath) {
				return
			}
			var bridge *StdioBridge
			if g.bridgeLookup != nil {
				bridge = g.bridgeLookup(route.cfg.Service)
			}
			if bridge == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "bridge_unavailable")
				return
			}
			filter := ToolFilter{
				AllowTools:   rule.AllowTools,
				DenyTools:    rule.DenyTools,
				ReadOnlyOnly: rule.ReadOnlyOnly,
			}
			bridge.serveHTTPWithScope(w, r2, filter, bridgeSessionScope{route: route.cfg.Slug, token: rule.Digest})
			return
		}

		route.httpProxy.serveAuthenticated(w, r2, rule)
		return
	}

	if !CheckLoopbackGuard(w, r, table.allowedSet, false) {
		return
	}
	writeJSONError(w, http.StatusNotFound, "not_found")
}

func (g *GatewayServer) serveGatewayHealthz(w http.ResponseWriter, table *gatewayRouteTable) {
	allOK := true
	routesOut := make(map[string]map[string]any, len(table.slugOrder))
	for _, slug := range table.slugOrder {
		rt := table.routes[slug]
		ok, state := g.routeHealthState(rt)
		if !ok {
			allOK = false
		}
		routesOut[slug] = map[string]any{
			"ok":    ok,
			"mode":  rt.mode,
			"state": state,
		}
	}
	status := http.StatusOK
	if !allOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"ok":     allOK,
		"routes": routesOut,
	})
}

func (g *GatewayServer) serveRouteHealthz(w http.ResponseWriter, r *http.Request, route *gatewayRoute) {
	if route.mode == "stdio-bridge" {
		var bridge *StdioBridge
		if g.bridgeLookup != nil {
			bridge = g.bridgeLookup(route.cfg.Service)
		}
		if bridge == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "state": "stopped", "error": "bridge_unavailable"})
			return
		}
		state := bridge.State()
		sum := bridge.HealthSummary()
		switch state {
		case "running", "sleeping":
			payload := map[string]any{"ok": true, "mode": "stdio-bridge", "state": state}
			if sum.LastError != "" {
				payload["lastError"] = sum.LastError
			}
			writeJSON(w, http.StatusOK, payload)
		default:
			payload := map[string]any{"ok": false, "mode": "stdio-bridge", "state": state}
			if sum.LastError != "" {
				payload["lastError"] = sum.LastError
			}
			writeJSON(w, http.StatusServiceUnavailable, payload)
		}
		return
	}
	route.httpProxy.serveHealthz(w, r)
}

func (g *GatewayServer) routeHealthState(route *gatewayRoute) (bool, string) {
	if route.mode == "stdio-bridge" {
		if g.bridgeLookup == nil {
			return false, "stopped"
		}
		bridge := g.bridgeLookup(route.cfg.Service)
		if bridge == nil {
			return false, "stopped"
		}
		st := bridge.State()
		return st == "running" || st == "sleeping", st
	}
	if route.cfg.Service != "" {
		if g.serviceLookup == nil {
			return false, "stopped"
		}
		runner := g.serviceLookup(route.cfg.Service)
		if runner == nil {
			return false, "stopped"
		}
		sum := runner.HealthSummary()
		ok := (sum.Running && sum.Healthy) || sum.State == "sleeping"
		return ok, sum.State
	}
	return true, "external"
}

// RouteMetrics returns the MCPMetrics collectors attached to managed HTTP service names.
func (g *GatewayServer) RouteMetricsForService(serviceName string) []*MCPMetrics {
	table := g.table.Load()
	if table == nil || serviceName == "" {
		return nil
	}
	var out []*MCPMetrics
	for _, slug := range table.slugOrder {
		rt := table.routes[slug]
		if rt.mode == "http" && rt.cfg.Service == serviceName && rt.metrics != nil {
			out = append(out, rt.metrics)
		}
	}
	return out
}

// ListenAddr returns the bound gateway address.
func (g *GatewayServer) ListenAddr() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.listenAddr
}

// Config returns the current GatewayConfig.
func (g *GatewayServer) Config() GatewayConfig {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.cfg
}

// HealthSummary returns the compact /healthz summary for the gateway.
func (g *GatewayServer) HealthSummary() GatewayHealthSummary {
	g.mu.RLock()
	running := g.running
	addr := g.listenAddr
	g.mu.RUnlock()

	table := g.table.Load()
	routesMap := make(map[string]bool)
	if table != nil {
		for _, slug := range table.slugOrder {
			ok, _ := g.routeHealthState(table.routes[slug])
			routesMap[slug] = ok
		}
	}
	return GatewayHealthSummary{
		Running:    running,
		ListenAddr: addr,
		Routes:     routesMap,
	}
}

// Status returns the full /status payload for the gateway.
func (g *GatewayServer) Status() GatewayStatus {
	g.mu.RLock()
	running := g.running
	addr := g.listenAddr
	g.mu.RUnlock()

	table := g.table.Load()
	var routes []GatewayRouteStatus
	if table != nil {
		routes = make([]GatewayRouteStatus, 0, len(table.slugOrder))
		for _, slug := range table.slugOrder {
			rt := table.routes[slug]
			_, state := g.routeHealthState(rt)
			rs := GatewayRouteStatus{
				Slug:        slug,
				Mode:        rt.mode,
				Service:     rt.cfg.Service,
				TargetURL:   diagnosticURL(g.redactor, rt.targetURL),
				Description: rt.cfg.Description,
				State:       state,
			}
			if rt.mode == "stdio-bridge" && g.bridgeLookup != nil {
				if bridge := g.bridgeLookup(rt.cfg.Service); bridge != nil {
					snap := bridge.Metrics()
					rs.Metrics = &snap
				}
			} else if rt.metrics != nil {
				snap := rt.metrics.Snapshot()
				rs.Metrics = &snap
			}
			routes = append(routes, rs)
		}
	}
	return GatewayStatus{
		Running:    running,
		ListenAddr: addr,
		Routes:     routes,
	}
}

// ResumeFromSuspend closes idle upstream TCP connections and invalidates health caches after OS wake.
func (g *GatewayServer) ResumeFromSuspend() {
	if g.transport != nil {
		g.transport.CloseIdleConnections()
	}
	if table := g.table.Load(); table != nil {
		for _, rt := range table.routes {
			if rt.httpProxy != nil && rt.httpProxy.healthCache != nil {
				rt.httpProxy.healthCache.invalidate()
			}
		}
	}
}

// CancelBaseContext cancels the server's BaseContext so open SSE streams abort immediately during shutdown.
func (g *GatewayServer) CancelBaseContext() {
	g.mu.Lock()
	cancel := g.baseCancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Shutdown gracefully stops the gateway listener and idle connections.
func (g *GatewayServer) Shutdown(ctx context.Context) error {
	g.mu.Lock()
	srv := g.srv
	cancel := g.baseCancel
	g.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if g.transport != nil {
		g.transport.CloseIdleConnections()
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
