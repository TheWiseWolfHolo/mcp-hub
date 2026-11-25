package hub

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub/webui"
)

type managedBridge struct {
	cfg       ServiceConfig
	logDir    string
	bridge    *StdioBridge
	ringLog   *RingLog
	rotLog    *RotatingFileWriter
	stderrW   io.Writer
	ringW     io.Writer
	closeOnce sync.Once

	memMu        sync.Mutex
	memCachedAt  time.Time
	memCachedJob *ProcessJob
	memCached    JobMemoryStats
}

func newManagedBridge(cfg ServiceConfig, logDir string, logMaxBytes int64, redactor *Redactor, logger *log.Logger, onStateChange func()) *managedBridge {
	ring := NewRingLog(256, DefaultRingLogMaxLineBytes)
	ringW := ring.Writer("stderr")

	var rotLog *RotatingFileWriter
	var stderrDst io.Writer = ringW
	logPath := filepath.Join(logDir, cfg.Name+".log")
	if lw, err := NewRotatingFileWriter(logPath, logMaxBytes); err == nil {
		rotLog = lw
		safeFileW := newBestEffortWriter(lw, func(writeErr error) {
			if logger != nil {
				logger.Printf("[bridge:%s] log write error (continuing with ring log): %v", cfg.Name, writeErr)
			}
		})
		stderrDst = io.MultiWriter(safeFileW, ringW)
	} else if logger != nil {
		logger.Printf("[bridge:%s] failed to open log %q (falling back to ring log): %v", cfg.Name, logPath, err)
	}
	stderrW := redactor.NewWriter(stderrDst)

	opts := StdioBridgeOptions{
		Name:                   cfg.Name,
		Command:                cfg.Command,
		Args:                   cfg.Args,
		Cwd:                    cfg.Cwd,
		Env:                    cfg.Env,
		EnvFiles:               cfg.EnvFiles,
		StripProxyEnv:          cfg.StripProxyEnv,
		IdleTimeout:            cfg.EffectiveIdleSleep(),
		MaxConsecutiveFailures: cfg.EffectiveMaxConsecutiveFailures(),
		StderrWriter:           stderrW,
		Redactor:               redactor,
		Logger:                 logger,
		OnCrash: func() {
			flushIfPossible(stderrW)
			flushIfPossible(ringW)
			ring.FreezeCrashTail(10)
		},
		OnStateChange: onStateChange,
	}
	if cfg.RequestTimeoutSec > 0 {
		opts.RequestTimeout = time.Duration(cfg.RequestTimeoutSec) * time.Second
	}
	if cfg.InitTimeoutSec > 0 {
		opts.InitTimeout = time.Duration(cfg.InitTimeoutSec) * time.Second
	}
	if cfg.MaxConcurrent > 0 {
		opts.MaxConcurrent = cfg.MaxConcurrent
	}
	if cfg.MaxLineBytes > 0 {
		opts.MaxLineBytes = cfg.MaxLineBytes
	}

	return &managedBridge{
		cfg:     cfg,
		logDir:  logDir,
		bridge:  NewStdioBridge(opts),
		ringLog: ring,
		rotLog:  rotLog,
		stderrW: stderrW,
		ringW:   ringW,
	}
}

func (mb *managedBridge) cachedMemoryStats(job *ProcessJob) JobMemoryStats {
	if job == nil {
		return JobMemoryStats{}
	}
	mb.memMu.Lock()
	if mb.memCachedJob == job && time.Since(mb.memCachedAt) < memoryStatsCacheTTL {
		cached := mb.memCached
		mb.memMu.Unlock()
		return cached
	}
	mb.memMu.Unlock()

	stats, err := job.MemoryStats()
	if err != nil {
		return JobMemoryStats{}
	}

	mb.memMu.Lock()
	mb.memCachedJob = job
	mb.memCachedAt = time.Now()
	mb.memCached = stats
	mb.memMu.Unlock()
	return stats
}

func (mb *managedBridge) Snapshot() ServiceStatus {
	st := mb.bridge.Status()
	st.Mode = "stdio-bridge"
	st.StartMode = mb.cfg.EffectiveStartMode()
	st.LogFile = filepath.Join(mb.logDir, mb.cfg.Name+".log")
	st.HasReload = false
	st.Description = mb.cfg.Description
	if len(mb.cfg.Tags) > 0 {
		st.Tags = append([]string(nil), mb.cfg.Tags...)
	}
	m := mb.bridge.Metrics()
	st.Metrics = &m
	st.MetricsSource = "bridge"
	if st.LastActivityAt == "" {
		st.LastActivityAt = m.LastRequestAt
	}
	if mb.ringLog != nil {
		st.LastStderr = mb.ringLog.TailText(5)
		if ct := mb.ringLog.CrashTail(); len(ct) > 0 {
			st.CrashTail = ct
		} else if st.LastError != "" && !st.Running {
			st.CrashTail = mb.ringLog.TailText(10)
		}
	}
	if job := mb.bridge.ActiveJob(); job != nil {
		stats := mb.cachedMemoryStats(job)
		st.ProcessCount = stats.ProcessCount
		st.TreeWorkingSetMB = bytesToMB(stats.WorkingSetBytes)
		st.TreePrivateMB = bytesToMB(stats.PrivateBytes)
		st.TreePeakMB = bytesToMB(stats.PeakBytes)
	}
	return st
}

func (mb *managedBridge) HealthSummary() ServiceHealthSummary {
	return mb.bridge.HealthSummary()
}

func (mb *managedBridge) Close() {
	mb.closeOnce.Do(func() {
		mb.bridge.Shutdown()
		flushIfPossible(mb.stderrW)
		flushIfPossible(mb.ringW)
		if mb.rotLog != nil {
			_ = mb.rotLog.Close()
		}
	})
}

// Supervisor coordinates managed HTTP services, STDIO bridges, authenticated reverse proxies,
// the unified single-port gateway, and the localhost control API.
type Supervisor struct {
	mu         sync.RWMutex
	reloadMu   sync.Mutex
	cfg        *Config
	configPath string
	startedAt  time.Time

	redactor   *Redactor
	hubRingLog *RingLog
	logWriter  *RotatingFileWriter
	hubLogW    io.Writer
	logger     *log.Logger

	services     map[string]*ServiceRunner
	svcCancel    map[string]context.CancelFunc
	bridges      map[string]*managedBridge
	fingerprints map[string]string
	proxies      map[string]*ProxyServer
	gateway      *GatewayServer
	control      *ControlServer
	dashboard    DashboardHandler

	baseCtx         context.Context
	runCancel       context.CancelFunc
	closed          bool
	stopCh          chan struct{}
	stopOnce        sync.Once
	shutOnce        sync.Once
	sessionDeadline time.Time
	sessionEnd      chan struct{}

	stateChangeMu sync.RWMutex
	onStateChange func()

	eventMu        sync.Mutex
	eventSeq       uint64
	nextEventSubID uint64
	eventSubs      map[uint64]chan HubEvent
}

// NewSupervisor creates a Supervisor for the validated Config.
func NewSupervisor(cfg *Config, configPath string) (*Supervisor, error) {
	ApplyConfigDefaults(cfg)
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	redactor := NewRedactor()
	redactor.SetSecrets(CollectAllSecrets(cfg))
	return &Supervisor{
		cfg:          cfg,
		configPath:   configPath,
		redactor:     redactor,
		hubRingLog:   NewRingLog(256, DefaultRingLogMaxLineBytes),
		services:     make(map[string]*ServiceRunner, len(cfg.Services)),
		svcCancel:    make(map[string]context.CancelFunc, len(cfg.Services)),
		bridges:      make(map[string]*managedBridge),
		fingerprints: make(map[string]string, len(cfg.Services)),
		proxies:      make(map[string]*ProxyServer, len(cfg.Proxies)),
		stopCh:       make(chan struct{}),
		sessionEnd:   make(chan struct{}),
		eventSubs:    make(map[uint64]chan HubEvent),
	}, nil
}

// SetOnStateChange registers an optional callback notified on config reloads or service actions (e.g. tray refresh).
func (s *Supervisor) SetOnStateChange(fn func()) {
	s.stateChangeMu.Lock()
	s.onStateChange = fn
	s.stateChangeMu.Unlock()
}

// SetDashboardHandler registers an optional embedded Web UI handler and mounts it at /ui on ControlServer.
func (s *Supervisor) SetDashboardHandler(h DashboardHandler) {
	s.mu.Lock()
	s.dashboard = h
	ctrl := s.control
	s.mu.Unlock()
	if ctrl != nil {
		if h != nil {
			if addr := ctrl.ListenAddr(); addr != "" {
				if sb, ok := h.(interface{ SetBoundAddr(string) }); ok {
					sb.SetBoundAddr(addr)
				}
			}
		}
		ctrl.SetUIHandler("/ui", h)
	}
}

// CreateDashboardLaunchURL mints a single-use bootstrap URL for the mounted Web Dashboard.
// Returns ErrDashboardNotMounted when no DashboardHandler has been registered via SetDashboardHandler.
func (s *Supervisor) CreateDashboardLaunchURL() (string, error) {
	u, _, err := s.createDashboardSession()
	return u, err
}

func (s *Supervisor) createDashboardSession() (string, time.Time, error) {
	s.mu.RLock()
	h := s.dashboard
	s.mu.RUnlock()
	if h == nil {
		return "", time.Time{}, ErrDashboardNotMounted
	}
	nonce, expiresAt := h.MintBootstrapToken()
	addr := s.ControlAddr()
	return fmt.Sprintf("http://%s/ui?bootstrap=%s", addr, url.QueryEscape(nonce)), expiresAt, nil
}

// VerifyControlToken validates a raw control token string against the active ControlServer tokens.
func (s *Supervisor) VerifyControlToken(token string) bool {
	s.mu.RLock()
	ctrl := s.control
	s.mu.RUnlock()
	if ctrl == nil {
		return false
	}
	return ctrl.VerifyControlToken(token)
}

// AllowNoAuth reports whether the ControlServer currently allows unauthenticated access.
func (s *Supervisor) AllowNoAuth() bool {
	s.mu.RLock()
	ctrl := s.control
	s.mu.RUnlock()
	if ctrl == nil {
		return false
	}
	return ctrl.AllowNoAuth()
}

// SubscribeEvents returns a buffered channel receiving live HubEvent updates and an idempotent unsubscribe function.
func (s *Supervisor) SubscribeEvents() (<-chan HubEvent, func()) {
	ch := make(chan HubEvent, 64)
	s.eventMu.Lock()
	if s.eventSubs == nil {
		s.eventSubs = make(map[uint64]chan HubEvent)
	}
	s.nextEventSubID++
	id := s.nextEventSubID
	s.eventSubs[id] = ch
	s.eventMu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			s.eventMu.Lock()
			if existing, ok := s.eventSubs[id]; ok {
				delete(s.eventSubs, id)
				close(existing)
			}
			s.eventMu.Unlock()
		})
	}
	return ch, unsub
}

func (s *Supervisor) emitEvent(eventType, service, state, message string) {
	if s.redactor != nil && message != "" {
		message = s.redactor.Redact(message)
	}
	s.eventMu.Lock()
	s.eventSeq++
	ev := HubEvent{
		Seq:       s.eventSeq,
		Type:      eventType,
		Service:   service,
		State:     state,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	for _, ch := range s.eventSubs {
		select {
		case ch <- ev:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
	s.eventMu.Unlock()
}

func (s *Supervisor) notifyStateChange() {
	s.stateChangeMu.RLock()
	fn := s.onStateChange
	s.stateChangeMu.RUnlock()
	if fn != nil {
		fn()
	}
}

// Logger returns the supervisor's redacted logger.
func (s *Supervisor) Logger() *log.Logger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logger
}

// Redactor returns the supervisor's shared secret Redactor.
func (s *Supervisor) Redactor() *Redactor {
	return s.redactor
}

// StopCh returns the channel closed when RequestStop() is invoked.
func (s *Supervisor) StopCh() <-chan struct{} {
	return s.stopCh
}

func (s *Supervisor) lookupRunner(name string) *ServiceRunner {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.services[name]
}

func (s *Supervisor) lookupBridge(name string) *StdioBridge {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if mb, ok := s.bridges[name]; ok {
		return mb.bridge
	}
	return nil
}

func (s *Supervisor) lookupServiceCfg(name string) (ServiceConfig, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.cfg == nil {
		return ServiceConfig{}, false
	}
	for _, svc := range s.cfg.Services {
		if svc.Name == name {
			return svc, true
		}
	}
	return ServiceConfig{}, false
}

// Start opens logs, starts all reverse proxies, the optional gateway, the control API,
// and launches all managed services and bridges.
func (s *Supervisor) Start(ctx context.Context) error {
	for _, scfg := range s.cfg.Services {
		fp, err := serviceFingerprint(scfg)
		if err != nil {
			return err
		}
		s.fingerprints[scfg.Name] = fp
	}
	if err := os.MkdirAll(s.cfg.LogDir, 0o700); err != nil {
		return fmt.Errorf("create logDir %q: %w", s.cfg.LogDir, err)
	}
	lw, err := NewRotatingFileWriter(filepath.Join(s.cfg.LogDir, "mcp-hub.log"), s.cfg.LogMaxBytes)
	if err != nil {
		return err
	}
	hubRingW := s.hubRingLog.Writer("hub")
	redactedLogW := s.redactor.NewWriter(io.MultiWriter(newBestEffortWriter(lw, nil), hubRingW))
	logger := log.New(redactedLogW, "", log.LstdFlags|log.LUTC)

	s.mu.Lock()
	s.logWriter = lw
	s.hubLogW = redactedLogW
	s.logger = logger
	s.startedAt = time.Now().UTC()
	s.mu.Unlock()

	logger.Printf("[hub] starting mcp-hub v%s pid=%d config=%q", Version, os.Getpid(), s.configPath)

	var startedProxies []*ProxyServer
	var startedGateway *GatewayServer
	var startedBridges []*managedBridge
	cleanup := func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for _, p := range startedProxies {
			_ = p.Shutdown(shutCtx)
		}
		if startedGateway != nil {
			_ = startedGateway.Shutdown(shutCtx)
		}
		if s.control != nil {
			_ = s.control.Shutdown(shutCtx)
		}
		for _, b := range startedBridges {
			b.Close()
		}
		s.mu.Lock()
		dash := s.dashboard
		s.dashboard = nil
		clear(s.services)
		clear(s.svcCancel)
		clear(s.bridges)
		clear(s.proxies)
		s.gateway = nil
		s.control = nil
		s.mu.Unlock()
		if dash != nil {
			if cl, ok := dash.(interface{ Close() }); ok {
				cl.Close()
			}
		}
		flushIfPossible(redactedLogW)
		flushIfPossible(hubRingW)
		if lw != nil {
			_ = lw.Close()
		}
	}

	// Build service runners & stdio bridges before starting listeners so lookups and /healthz see them.
	s.mu.Lock()
	for _, scfg := range s.cfg.Services {
		svcName := scfg.Name
		if scfg.EffectiveMode() == "stdio-bridge" {
			mb := newManagedBridge(scfg, s.cfg.LogDir, s.cfg.LogMaxBytes, s.redactor, logger, nil)
			mb.bridge.SetOnStateChange(func() {
				s.emitEvent("service.state", svcName, mb.bridge.State(), "")
				s.notifyStateChange()
			})
			s.bridges[scfg.Name] = mb
			startedBridges = append(startedBridges, mb)
		} else {
			runner := NewServiceRunner(scfg, s.cfg.LogDir, s.cfg.LogMaxBytes, logger)
			runner.SetRedactor(s.redactor)
			runner.SetOnStateChange(func() {
				s.emitEvent("service.state", svcName, runner.State(), "")
				s.notifyStateChange()
			})
			s.services[scfg.Name] = runner
		}
	}
	s.mu.Unlock()

	// Start reverse proxies.
	for _, pcfg := range s.cfg.Proxies {
		ps := NewProxyServer(pcfg, logger)
		ps.SetServiceLookup(s.lookupRunner)
		ps.SetRedactor(s.redactor)
		if err := ps.Start(); err != nil {
			logger.Printf("[hub] proxy %q failed to start: %v", pcfg.Name, err)
			cleanup()
			return err
		}
		s.mu.Lock()
		s.proxies[pcfg.Name] = ps
		if pcfg.UpstreamService != "" {
			if runner, ok := s.services[pcfg.UpstreamService]; ok {
				runner.AttachMetricsWithSource(ps.MCPMetrics(), "proxy")
			}
		}
		s.mu.Unlock()
		startedProxies = append(startedProxies, ps)
	}

	// Start unified gateway if configured with routes (or explicit listenAddr).
	if s.cfg.Gateway != nil && (len(s.cfg.Gateway.Routes) > 0 || strings.TrimSpace(s.cfg.Gateway.ListenAddr) != "") {
		gw := NewGatewayServer(*s.cfg.Gateway, s.lookupRunner, s.lookupBridge, s.lookupServiceCfg, s.redactor, logger)
		if err := gw.Start(); err != nil {
			logger.Printf("[hub] gateway failed to start: %v", err)
			cleanup()
			return err
		}
		s.mu.Lock()
		s.gateway = gw
		s.attachGatewayMetricsLocked(gw)
		s.mu.Unlock()
		startedGateway = gw
	}

	ctrl := NewControlServer(s.cfg.ControlAddr, s.cfg.ControlTokenFile, s.cfg.AllowInsecureNoControlAuth, ControlHooks{
		Healthz:                s.Healthz,
		Status:                 s.Status,
		Catalog:                s.Catalog,
		GetRingLog:             s.GetRingLog,
		StartService:           s.StartService,
		StopService:            s.StopService,
		SleepService:           s.SleepService,
		WakeService:            s.WakeService,
		StartAll:               s.StartAllServices,
		StopAll:                s.StopAllServices,
		RestartAll:             s.RestartAllServices,
		Restart:                s.RestartService,
		ReloadHub:              s.ReloadConfig,
		ValidateConfig:         s.ValidateReloadConfig,
		ReloadService:          s.ReloadService,
		SubscribeEvents:        s.SubscribeEvents,
		CreateDashboardSession: s.createDashboardSession,
		Stop:                   s.RequestStop,
		Redactor:               s.redactor,
	}, logger)
	s.mu.Lock()
	s.control = ctrl
	if s.dashboard == nil {
		s.dashboard = webui.NewHandler(s.cfg.ControlAddr, newSupervisorWebUIBackend(s))
	}
	dash := s.dashboard
	ctrl.SetUIHandler("/ui", dash)
	s.mu.Unlock()
	if err := ctrl.Start(); err != nil {
		logger.Printf("[hub] control server failed to start: %v", err)
		cleanup()
		return err
	}
	if dash != nil {
		if sb, ok := dash.(interface{ SetBoundAddr(string) }); ok {
			sb.SetBoundAddr(ctrl.ListenAddr())
		}
	}

	baseCtx, runCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.baseCtx = baseCtx
	s.runCancel = runCancel
	for name, runner := range s.services {
		svcCtx, svcCancel := context.WithCancel(baseCtx)
		s.svcCancel[name] = svcCancel
		runner.Launch(svcCtx)
	}
	s.mu.Unlock()

	// Start any stdio-bridge services explicitly configured with startMode: "eager".
	for _, mb := range startedBridges {
		if mb.cfg.EffectiveStartMode() == "eager" && baseCtx.Err() == nil {
			go func(b *managedBridge) {
				if baseCtx.Err() != nil {
					return
				}
				startCtx, cancel := context.WithTimeout(baseCtx, b.cfg.EffectiveStartTimeout())
				defer cancel()
				if err := b.bridge.Start(startCtx); err != nil {
					logger.Printf("[bridge:%s] eager start failed: %v", b.cfg.Name, err)
				}
			}(mb)
		}
	}

	return nil
}

func (s *Supervisor) attachGatewayMetricsLocked(gw *GatewayServer) {
	if gw == nil {
		return
	}
	for name, runner := range s.services {
		for _, m := range gw.RouteMetricsForService(name) {
			runner.AttachMetricsWithSource(m, "gateway")
		}
	}
}

func (s *Supervisor) refreshMetricsLocked() {
	proxyMetrics := make(map[string][]*MCPMetrics)
	for _, proxy := range s.proxies {
		if name := proxy.Config().UpstreamService; name != "" {
			proxyMetrics[name] = append(proxyMetrics[name], proxy.MCPMetrics())
		}
	}
	for name, runner := range s.services {
		var combined []*MCPMetrics
		source := ""
		if pm := proxyMetrics[name]; len(pm) > 0 {
			combined = append(combined, pm...)
			source = "proxy"
		}
		if s.gateway != nil {
			if gm := s.gateway.RouteMetricsForService(name); len(gm) > 0 {
				combined = append(combined, gm...)
				source = "gateway"
			}
		}
		runner.setMetricsWithSource(combined, source)
	}
}

// Run starts the supervisor and blocks until ctx is canceled or /stop is invoked,
// then performs a full graceful shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	if err := s.Start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
	case <-s.stopCh:
	}
	s.Shutdown()
	return nil
}

// RequestStop triggers graceful supervisor shutdown (called by POST /stop or Windows shutdown).
func (s *Supervisor) RequestStop() {
	s.stopOnce.Do(func() {
		if l := s.Logger(); l != nil {
			l.Printf("[hub] stop requested")
		}
		close(s.stopCh)
	})
}

func (s *Supervisor) RequestSessionEnd(deadline time.Time) {
	s.mu.Lock()
	if s.sessionDeadline.IsZero() {
		s.sessionDeadline = deadline
		close(s.sessionEnd)
	}
	s.mu.Unlock()
	s.RequestStop()
}

// Shutdown stops the control server, reverse proxies, gateway, and all managed services and bridges.
func (s *Supervisor) Shutdown() {
	s.shutOnce.Do(func() {
		// Closing and publishing a reload are mutually exclusive under mu. Preparation
		// may finish independently and will release its uncommitted listeners.
		s.mu.Lock()
		s.closed = true
		logger := s.logger
		ctrl := s.control
		dash := s.dashboard
		gw := s.gateway
		runCancel := s.runCancel
		proxies := make([]*ProxyServer, 0, len(s.proxies))
		for _, p := range s.proxies {
			proxies = append(proxies, p)
		}
		runners := make([]*ServiceRunner, 0, len(s.services))
		for _, r := range s.services {
			runners = append(runners, r)
		}
		bridges := make([]*managedBridge, 0, len(s.bridges))
		for _, b := range s.bridges {
			bridges = append(bridges, b)
		}
		logWriter := s.logWriter
		hubLogW := s.hubLogW
		s.mu.Unlock()

		if logger != nil {
			logger.Printf("[hub] shutting down...")
		}

		// Cancel all BaseContexts immediately so open SSE streams abort without stalling Shutdown.
		if dash != nil {
			if cl, ok := dash.(interface{ Close() }); ok {
				cl.Close()
			}
		}
		if ctrl != nil {
			ctrl.CancelBaseContext()
		}
		if gw != nil {
			gw.CancelBaseContext()
		}
		for _, p := range proxies {
			p.CancelBaseContext()
		}

		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		var wg sync.WaitGroup
		if ctrl != nil {
			wg.Add(1)
			go func(c *ControlServer) {
				defer wg.Done()
				_ = c.Shutdown(shutCtx)
			}(ctrl)
		}
		if gw != nil {
			wg.Add(1)
			go func(g *GatewayServer) {
				defer wg.Done()
				_ = g.Shutdown(shutCtx)
			}(gw)
		}
		for _, p := range proxies {
			wg.Add(1)
			go func(ps *ProxyServer) {
				defer wg.Done()
				_ = ps.Shutdown(shutCtx)
			}(p)
		}
		if runCancel != nil {
			runCancel()
		}
		for _, r := range runners {
			wg.Add(1)
			go func(runner *ServiceRunner) {
				defer wg.Done()
				runner.Wait()
			}(r)
		}
		for _, b := range bridges {
			wg.Add(1)
			go func(mb *managedBridge) {
				defer wg.Done()
				mb.Close()
			}(b)
		}
		finished := make(chan struct{})
		go func() {
			wg.Wait()
			close(finished)
		}()
		select {
		case <-finished:
		case <-s.sessionEnd:
			s.mu.RLock()
			deadline := s.sessionDeadline
			s.mu.RUnlock()
			force := time.NewTimer(time.Until(deadline.Add(-1500 * time.Millisecond)))
			defer force.Stop()
			select {
			case <-finished:
			case <-force.C:
				for _, runner := range runners {
					runner.forceStop()
				}
				remaining := time.NewTimer(time.Until(deadline.Add(-250 * time.Millisecond)))
				defer remaining.Stop()
				select {
				case <-finished:
				case <-remaining.C:
					if logger != nil {
						logger.Print("[hub] system shutdown deadline reached")
					}
				}
			}
		}

		if logger != nil {
			logger.Printf("[hub] shutdown complete")
		}
		flushIfPossible(hubLogW)
		if logWriter != nil {
			_ = logWriter.Close()
		}
	})
}

// ReloadConfig prepares credentials, routes and listeners before changing any live service.
func (s *Supervisor) ReloadConfig(ctx context.Context) (ReloadHubResponse, error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.mu.RLock()
	if s.closed || s.baseCtx == nil {
		s.mu.RUnlock()
		return ReloadHubResponse{}, errors.New("supervisor is not running")
	}
	oldCfg, baseCtx, logger, ctrl := s.cfg, s.baseCtx, s.logger, s.control
	oldProxies, oldGW := maps.Clone(s.proxies), s.gateway
	s.mu.RUnlock()
	ctx, cancelPreparation := context.WithCancel(ctx)
	defer cancelPreparation()
	stopOnShutdown := context.AfterFunc(baseCtx, cancelPreparation)
	defer stopOnShutdown()

	newCfg, err := LoadConfig(s.configPath)
	if err != nil {
		return ReloadHubResponse{}, configValidationError{err}
	}
	if err := ValidateSecretFiles(newCfg); err != nil {
		return ReloadHubResponse{}, configValidationError{err}
	}
	if strings.TrimSpace(newCfg.ControlAddr) != strings.TrimSpace(oldCfg.ControlAddr) {
		return ReloadHubResponse{}, configValidationError{errors.New("controlAddr change requires a hub restart")}
	}
	if newCfg.LogDir != oldCfg.LogDir || newCfg.LogMaxBytes != oldCfg.LogMaxBytes ||
		newCfg.ShouldShowTray() != oldCfg.ShouldShowTray() {
		return ReloadHubResponse{}, configValidationError{errors.New("logDir, logMaxBytes or tray change requires a hub restart")}
	}

	newSvcMap := make(map[string]ServiceConfig, len(newCfg.Services))
	newFingerprints := make(map[string]string, len(newCfg.Services))
	for _, scfg := range newCfg.Services {
		fp, err := serviceFingerprint(scfg)
		if err != nil {
			return ReloadHubResponse{}, configValidationError{err}
		}
		newSvcMap[scfg.Name] = scfg
		newFingerprints[scfg.Name] = fp
	}

	var applyControl func()
	if ctrl != nil {
		applyControl, _, err = ctrl.prepareTokens(newCfg.ControlTokenFile, newCfg.AllowInsecureNoControlAuth)
		if err != nil {
			return ReloadHubResponse{}, configValidationError{err}
		}
	}
	// Keep older credentials in the redactor while old requests and children drain.
	s.redactor.AddSecrets(CollectAllSecrets(newCfg))

	var (
		applyListeners []func()
		closeStaged    []func()
		res            ReloadHubResponse
	)
	defer func() {
		for _, abort := range closeStaged {
			abort()
		}
	}()

	byAddr := make(map[string]*ProxyServer, len(oldProxies))
	for _, p := range oldProxies {
		if key := normalizeBindKey(p.Config().ListenAddr); key != "" && p.Status().Running {
			byAddr[key] = p
		}
	}
	nextProxies := make(map[string]*ProxyServer, len(newCfg.Proxies))
	reused := make(map[*ProxyServer]bool)
	for _, pcfg := range newCfg.Proxies {
		if err := ctx.Err(); err != nil {
			return ReloadHubResponse{}, err
		}
		key := normalizeBindKey(pcfg.ListenAddr)
		if key != "" && oldGW != nil &&
			key == normalizeBindKey(oldGW.Config().ListenAddr) {
			return ReloadHubResponse{}, configValidationError{errors.New("changing a gateway listener into a proxy requires a hub restart")}
		}
		var p *ProxyServer
		if key != "" {
			p = byAddr[key]
		}
		if p == nil {
			if old := oldProxies[pcfg.Name]; old != nil && old.Config().ListenAddr == pcfg.ListenAddr && old.Status().Running {
				p = old
			}
		}
		if p != nil {
			apply, err := p.prepareReload(pcfg)
			if err != nil {
				return ReloadHubResponse{}, err
			}
			applyListeners = append(applyListeners, apply)
			reused[p] = true
		} else {
			p = NewProxyServer(pcfg, logger)
			p.SetServiceLookup(s.lookupRunner)
			p.SetRedactor(s.redactor)
			apply, abort, err := p.prepareStart(ctx)
			if err != nil {
				return ReloadHubResponse{}, err
			}
			applyListeners = append(applyListeners, apply)
			closeStaged = append(closeStaged, abort)
		}
		nextProxies[pcfg.Name] = p
		res.ReloadedProxies = append(res.ReloadedProxies, pcfg.Name)
	}

	var nextGW *GatewayServer
	if newCfg.Gateway != nil && (len(newCfg.Gateway.Routes) != 0 || strings.TrimSpace(newCfg.Gateway.ListenAddr) != "") {
		lookup := func(name string) (ServiceConfig, bool) {
			svc, ok := newSvcMap[name]
			return svc, ok
		}
		if oldGW != nil && oldGW.Config().ListenAddr == newCfg.Gateway.ListenAddr && oldGW.HealthSummary().Running {
			nextGW = oldGW
			apply, err := oldGW.prepareReload(*newCfg.Gateway, lookup)
			if err != nil {
				return ReloadHubResponse{}, err
			}
			applyListeners = append(applyListeners, apply)
		} else {
			if addr := normalizeBindKey(newCfg.Gateway.ListenAddr); addr != "" {
				for _, old := range oldProxies {
					if addr == normalizeBindKey(old.Config().ListenAddr) {
						return ReloadHubResponse{}, configValidationError{errors.New("changing a proxy listener into a gateway requires a hub restart")}
					}
				}
			}
			nextGW = NewGatewayServer(*newCfg.Gateway, s.lookupRunner, s.lookupBridge, lookup, s.redactor, logger)
			apply, abort, err := nextGW.prepareStart(ctx)
			if err != nil {
				return ReloadHubResponse{}, err
			}
			applyListeners = append(applyListeners, apply)
			closeStaged = append(closeStaged, abort)
		}
	}
	res.ReloadedGateway = oldGW != nil || nextGW != nil

	// A canceled client can abort preparation. Once services begin stopping, finish
	// the commit unless the supervisor itself is shutting down.
	if err := ctx.Err(); err != nil {
		return ReloadHubResponse{}, err
	}
	s.mu.Lock()
	if s.closed || baseCtx.Err() != nil {
		s.mu.Unlock()
		return ReloadHubResponse{}, errors.New("supervisor is shutting down")
	}
	var retiring sync.WaitGroup
	for name, runner := range s.services {
		if newFingerprints[name] != s.fingerprints[name] {
			s.svcCancel[name]()
			retiring.Go(runner.Wait)
		}
	}
	for name, bridge := range s.bridges {
		if newFingerprints[name] != s.fingerprints[name] {
			retiring.Go(bridge.Close)
		}
	}
	s.mu.Unlock()
	retiring.Wait()

	s.mu.Lock()
	if s.closed || baseCtx.Err() != nil {
		s.mu.Unlock()
		return ReloadHubResponse{}, errors.New("supervisor is shutting down")
	}
	for name := range s.fingerprints {
		if newFingerprints[name] == "" {
			res.RemovedServices = append(res.RemovedServices, name)
		}
		if newFingerprints[name] != s.fingerprints[name] {
			delete(s.services, name)
			delete(s.svcCancel, name)
			delete(s.bridges, name)
		}
	}
	for _, scfg := range newCfg.Services {
		name := scfg.Name
		oldFP, existed := s.fingerprints[name]
		if oldFP == newFingerprints[name] {
			res.UnchangedServices = append(res.UnchangedServices, name)
			continue
		}
		svcName := name
		if scfg.EffectiveMode() == "stdio-bridge" {
			mb := newManagedBridge(scfg, newCfg.LogDir, newCfg.LogMaxBytes, s.redactor, logger, nil)
			mb.bridge.SetOnStateChange(func() {
				s.emitEvent("service.state", svcName, mb.bridge.State(), "")
				s.notifyStateChange()
			})
			s.bridges[name] = mb
			if scfg.EffectiveStartMode() == "eager" {
				go func() {
					startCtx, cancel := context.WithTimeout(baseCtx, scfg.EffectiveStartTimeout())
					defer cancel()
					if err := mb.bridge.Start(startCtx); err != nil && baseCtx.Err() == nil && logger != nil {
						logger.Printf("[bridge:%s] eager start failed: %v", name, err)
					}
				}()
			}
		} else {
			runner := NewServiceRunner(scfg, newCfg.LogDir, newCfg.LogMaxBytes, logger)
			runner.SetRedactor(s.redactor)
			runner.SetOnStateChange(func() {
				s.emitEvent("service.state", svcName, runner.State(), "")
				s.notifyStateChange()
			})
			svcCtx, cancel := context.WithCancel(baseCtx)
			s.services[name], s.svcCancel[name] = runner, cancel
			runner.Launch(svcCtx)
		}
		if existed {
			res.RestartedServices = append(res.RestartedServices, name)
		} else {
			res.AddedServices = append(res.AddedServices, name)
		}
	}

	if applyControl != nil {
		applyControl()
	}
	for _, apply := range applyListeners {
		apply()
	}
	closeStaged = nil
	for _, p := range oldProxies {
		if !reused[p] {
			p.CancelBaseContext()
		}
	}
	if oldGW != nil && oldGW != nextGW {
		oldGW.CancelBaseContext()
	}
	s.proxies, s.gateway = nextProxies, nextGW
	s.fingerprints, s.cfg = newFingerprints, newCfg
	s.refreshMetricsLocked()
	s.mu.Unlock()

	shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, p := range oldProxies {
		if !reused[p] {
			retiring.Go(func() { _ = p.Shutdown(shutCtx) })
		}
	}
	if oldGW != nil && oldGW != nextGW {
		retiring.Go(func() { _ = oldGW.Shutdown(shutCtx) })
	}
	retiring.Wait()

	res.OK = true
	sort.Strings(res.AddedServices)
	sort.Strings(res.RemovedServices)
	sort.Strings(res.RestartedServices)
	sort.Strings(res.UnchangedServices)
	sort.Strings(res.ReloadedProxies)
	if logger != nil {
		logger.Printf("[hub] config hot-reloaded (added=%v removed=%v restarted=%v unchanged=%v proxies=%v gateway=%v)",
			res.AddedServices, res.RemovedServices, res.RestartedServices, res.UnchangedServices, res.ReloadedProxies, res.ReloadedGateway)
	}
	s.emitEvent("hub.reload", "", "", "config hot-reloaded")
	s.notifyStateChange()
	return res, nil
}

// ValidateReloadConfig performs a read-only dry-run of config.json hot-reload validation
// and computes the exact service/proxy/gateway diff without mutating any live state.
func (s *Supervisor) ValidateReloadConfig() (ReloadHubResponse, error) {
	s.mu.RLock()
	oldCfg := s.cfg
	configPath := s.configPath
	oldFingerprints := maps.Clone(s.fingerprints)
	oldProxies := maps.Clone(s.proxies)
	oldGW := s.gateway
	s.mu.RUnlock()

	if oldCfg == nil || strings.TrimSpace(configPath) == "" {
		return ReloadHubResponse{}, configValidationError{errors.New("supervisor has no configPath")}
	}

	newCfg, err := LoadConfig(configPath)
	if err != nil {
		return ReloadHubResponse{}, configValidationError{err}
	}
	if err := ValidateSecretFiles(newCfg); err != nil {
		return ReloadHubResponse{}, configValidationError{err}
	}
	if strings.TrimSpace(newCfg.ControlAddr) != strings.TrimSpace(oldCfg.ControlAddr) {
		return ReloadHubResponse{}, configValidationError{errors.New("controlAddr change requires a hub restart")}
	}
	if newCfg.LogDir != oldCfg.LogDir || newCfg.LogMaxBytes != oldCfg.LogMaxBytes ||
		newCfg.ShouldShowTray() != oldCfg.ShouldShowTray() {
		return ReloadHubResponse{}, configValidationError{errors.New("logDir, logMaxBytes or tray change requires a hub restart")}
	}

	newFingerprints := make(map[string]string, len(newCfg.Services))
	for _, scfg := range newCfg.Services {
		fp, err := serviceFingerprint(scfg)
		if err != nil {
			return ReloadHubResponse{}, configValidationError{err}
		}
		newFingerprints[scfg.Name] = fp
	}

	var res ReloadHubResponse
	for _, pcfg := range newCfg.Proxies {
		if key := normalizeBindKey(pcfg.ListenAddr); key != "" && oldGW != nil &&
			key == normalizeBindKey(oldGW.Config().ListenAddr) {
			return ReloadHubResponse{}, configValidationError{errors.New("changing a gateway listener into a proxy requires a hub restart")}
		}
		res.ReloadedProxies = append(res.ReloadedProxies, pcfg.Name)
	}
	hasNextGW := newCfg.Gateway != nil && (len(newCfg.Gateway.Routes) != 0 || strings.TrimSpace(newCfg.Gateway.ListenAddr) != "")
	if hasNextGW && (oldGW == nil || oldGW.Config().ListenAddr != newCfg.Gateway.ListenAddr) {
		if key := normalizeBindKey(newCfg.Gateway.ListenAddr); key != "" {
			for _, old := range oldProxies {
				if key == normalizeBindKey(old.Config().ListenAddr) {
					return ReloadHubResponse{}, configValidationError{errors.New("changing a proxy listener into a gateway requires a hub restart")}
				}
			}
		}
	}
	res.ReloadedGateway = oldGW != nil || hasNextGW

	for name := range oldFingerprints {
		if _, ok := newFingerprints[name]; !ok {
			res.RemovedServices = append(res.RemovedServices, name)
		}
	}
	for _, scfg := range newCfg.Services {
		name := scfg.Name
		oldFP, existed := oldFingerprints[name]
		switch {
		case !existed:
			res.AddedServices = append(res.AddedServices, name)
		case oldFP == newFingerprints[name]:
			res.UnchangedServices = append(res.UnchangedServices, name)
		default:
			res.RestartedServices = append(res.RestartedServices, name)
		}
	}

	res.OK = true
	sort.Strings(res.AddedServices)
	sort.Strings(res.RemovedServices)
	sort.Strings(res.RestartedServices)
	sort.Strings(res.UnchangedServices)
	sort.Strings(res.ReloadedProxies)
	return res, nil
}

func serviceFingerprint(svc ServiceConfig) (string, error) {
	h := sha256.New()
	raw, err := json.Marshal(svc)
	if err != nil {
		return "", err
	}
	h.Write(raw)
	h.Write([]byte{'\n'})

	hashFile := func(label, path string) error {
		p := strings.TrimSpace(path)
		if p == "" {
			return nil
		}
		data, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return fmt.Errorf("service %q hash %s (%q): %w", svc.Name, label, p, err)
		}
		sum := sha256.Sum256(data)
		h.Write([]byte(label))
		h.Write([]byte{'='})
		h.Write(sum[:])
		h.Write([]byte{'\n'})
		return nil
	}

	if len(svc.EnvFiles) > 0 {
		keys := make([]string, 0, len(svc.EnvFiles))
		for k := range svc.EnvFiles {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := hashFile("envFile:"+k, svc.EnvFiles[k]); err != nil {
				return "", err
			}
		}
	}
	if err := hashFile("shutdownTokenFile", svc.ShutdownTokenFile); err != nil {
		return "", err
	}
	if err := hashFile("reloadTokenFile", svc.ReloadTokenFile); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// OnSuspend fans out OS power-suspend notifications to all managed services and bridges.
func (s *Supervisor) OnSuspend() {
	s.mu.RLock()
	logger := s.logger
	runners := make([]*ServiceRunner, 0, len(s.services))
	for _, r := range s.services {
		runners = append(runners, r)
	}
	bridges := make([]*managedBridge, 0, len(s.bridges))
	for _, b := range s.bridges {
		bridges = append(bridges, b)
	}
	s.mu.RUnlock()

	if logger != nil {
		logger.Printf("[hub] OS suspend detected; pausing health probes and idle timers")
	}
	for _, r := range runners {
		r.Suspend()
	}
	for _, b := range bridges {
		b.bridge.Suspend()
	}
}

// OnResume fans out OS wake notifications to all managed services, bridges, proxies, and gateway.
func (s *Supervisor) OnResume() {
	s.mu.RLock()
	logger := s.logger
	runners := make([]*ServiceRunner, 0, len(s.services))
	for _, r := range s.services {
		runners = append(runners, r)
	}
	bridges := make([]*managedBridge, 0, len(s.bridges))
	for _, b := range s.bridges {
		bridges = append(bridges, b)
	}
	proxies := make([]*ProxyServer, 0, len(s.proxies))
	for _, p := range s.proxies {
		proxies = append(proxies, p)
	}
	gw := s.gateway
	s.mu.RUnlock()

	if logger != nil {
		logger.Printf("[hub] OS resume detected; refreshing idle timers and closing stale HTTP connections")
	}
	for _, r := range runners {
		r.Resume()
	}
	for _, b := range bridges {
		b.bridge.Resume()
	}
	for _, p := range proxies {
		p.ResumeFromSuspend()
	}
	if gw != nil {
		gw.ResumeFromSuspend()
	}
	s.emitEvent("hub.resume", "", "", "system resumed from suspend")
	s.notifyStateChange()
}

// ControlAddr returns the active control listener address.
func (s *Supervisor) ControlAddr() string {
	s.mu.RLock()
	ctrl := s.control
	addr := s.cfg.ControlAddr
	s.mu.RUnlock()
	if ctrl != nil {
		return ctrl.ListenAddr()
	}
	return addr
}

// ProxyAddr returns the active bound listen address for a named proxy.
func (s *Supervisor) ProxyAddr(name string) (string, bool) {
	s.mu.RLock()
	p, ok := s.proxies[name]
	s.mu.RUnlock()
	if !ok {
		return "", false
	}
	return p.ListenAddr(), true
}

// GatewayAddr returns the active bound gateway listen address, if running.
func (s *Supervisor) GatewayAddr() (string, bool) {
	s.mu.RLock()
	gw := s.gateway
	s.mu.RUnlock()
	if gw == nil {
		return "", false
	}
	return gw.ListenAddr(), true
}

// GetRingLog returns the RingLog for "hub" or any managed service or bridge.
func (s *Supervisor) GetRingLog(name string) (*RingLog, bool) {
	if name == "hub" {
		return s.hubRingLog, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.services[name]; ok {
		return r.RingLog(), true
	}
	if mb, ok := s.bridges[name]; ok {
		return mb.ringLog, true
	}
	return nil, false
}

// Healthz returns the compact /healthz summary (sleeping services and bridges count as OK).
func (s *Supervisor) Healthz() HealthzResponse {
	s.mu.RLock()
	runners := make(map[string]*ServiceRunner, len(s.services))
	for k, v := range s.services {
		runners[k] = v
	}
	bridges := make(map[string]*managedBridge, len(s.bridges))
	for k, v := range s.bridges {
		bridges[k] = v
	}
	proxies := make(map[string]*ProxyServer, len(s.proxies))
	for k, v := range s.proxies {
		proxies[k] = v
	}
	gw := s.gateway
	s.mu.RUnlock()

	services := make(map[string]ServiceHealthSummary, len(runners)+len(bridges))
	allOK := true
	for name, r := range runners {
		sum := r.HealthSummary()
		services[name] = sum
		if sum.State != "sleeping" && (!sum.Running || !sum.Healthy) {
			allOK = false
		}
	}
	for name, mb := range bridges {
		sum := mb.HealthSummary()
		services[name] = sum
		if sum.State != "sleeping" && (!sum.Running || !sum.Healthy) {
			allOK = false
		}
	}

	var proxyHealth map[string]bool
	if len(proxies) > 0 {
		proxyHealth = make(map[string]bool, len(proxies))
		for name, p := range proxies {
			ok := p.Status().Running
			proxyHealth[name] = ok
			if !ok {
				allOK = false
			}
		}
	}

	var gwSummary *GatewayHealthSummary
	if gw != nil {
		gs := gw.HealthSummary()
		gwSummary = &gs
		if !gs.Running {
			allOK = false
		}
	}

	return HealthzResponse{
		OK:       allOK,
		Services: services,
		Proxies:  proxyHealth,
		Gateway:  gwSummary,
	}
}

// Status returns the full /status report with schemaVersion: 1, memory totals, services, proxies, and gateway.
func (s *Supervisor) Status() StatusResponse {
	s.mu.RLock()
	cfg := s.cfg
	startedAt := s.startedAt
	runners := make(map[string]*ServiceRunner, len(s.services))
	for k, v := range s.services {
		runners[k] = v
	}
	bridges := make(map[string]*managedBridge, len(s.bridges))
	for k, v := range s.bridges {
		bridges[k] = v
	}
	proxies := make(map[string]*ProxyServer, len(s.proxies))
	for k, v := range s.proxies {
		proxies[k] = v
	}
	gw := s.gateway
	s.mu.RUnlock()

	services := make(map[string]ServiceStatus, len(runners)+len(bridges))
	allOK := true
	var totalPrivateMB, totalWorkingSetMB float64

	for name, r := range runners {
		st := r.Snapshot()
		services[name] = st
		totalPrivateMB += st.TreePrivateMB
		totalWorkingSetMB += st.TreeWorkingSetMB
		if st.State != "sleeping" && (!st.Running || !st.Healthy) {
			allOK = false
		}
	}
	for name, mb := range bridges {
		st := mb.Snapshot()
		services[name] = st
		totalPrivateMB += st.TreePrivateMB
		totalWorkingSetMB += st.TreeWorkingSetMB
		if st.State != "sleeping" && (!st.Running || !st.Healthy) {
			allOK = false
		}
	}

	proxiesOut := make(map[string]ProxyStatus, len(proxies))
	for name, p := range proxies {
		st := p.Status()
		proxiesOut[name] = st
		if !st.Running {
			allOK = false
		}
	}

	var gwStatus *GatewayStatus
	if gw != nil {
		gs := gw.Status()
		gwStatus = &gs
		if !gs.Running {
			allOK = false
		}
	}

	hubWS, hubPriv := currentProcessMemoryStats()
	return StatusResponse{
		SchemaVersion:         1,
		OK:                    allOK,
		Version:               Version,
		PID:                   os.Getpid(),
		StartedAt:             startedAt.Format(time.RFC3339),
		UptimeSec:             int64(time.Since(startedAt).Seconds()),
		ControlAddr:           s.ControlAddr(),
		ConfigPath:            s.configPath,
		LogDir:                cfg.LogDir,
		TrayEnabled:           cfg.ShouldShowTray(),
		TreeTotalPrivateMB:    math.Round(totalPrivateMB*100) / 100,
		TreeTotalWorkingSetMB: math.Round(totalWorkingSetMB*100) / 100,
		HubPrivateMB:          bytesToMB(hubPriv),
		HubWorkingSetMB:       bytesToMB(hubWS),
		Goroutines:            runtime.NumGoroutine(),
		Services:              services,
		Proxies:               proxiesOut,
		Gateway:               gwStatus,
	}
}

// Catalog joins services, bridges, proxies, and gateway routes into canonical client endpoints.
func (s *Supervisor) Catalog() CatalogResponse {
	s.mu.RLock()
	cfg := s.cfg
	runners := make(map[string]*ServiceRunner, len(s.services))
	for k, v := range s.services {
		runners[k] = v
	}
	bridges := make(map[string]*managedBridge, len(s.bridges))
	for k, v := range s.bridges {
		bridges[k] = v
	}
	proxies := make(map[string]*ProxyServer, len(s.proxies))
	for k, v := range s.proxies {
		proxies[k] = v
	}
	gw := s.gateway
	s.mu.RUnlock()

	gwAddr := ""
	var gwRoutes []GatewayRouteConfig
	if gw != nil {
		gwAddr = gw.ListenAddr()
		gwRoutes = gw.Config().Routes
	} else if cfg.Gateway != nil {
		gwAddr = cfg.Gateway.ListenAddr
		gwRoutes = cfg.Gateway.Routes
	}

	proxiesBySvc := make(map[string][]ProxyConfig)
	var standaloneProxies []ProxyConfig
	for _, pcfg := range cfg.Proxies {
		if pcfg.UpstreamService != "" {
			proxiesBySvc[pcfg.UpstreamService] = append(proxiesBySvc[pcfg.UpstreamService], pcfg)
		} else {
			standaloneProxies = append(standaloneProxies, pcfg)
		}
	}

	gwBySvc := make(map[string][]GatewayRouteConfig)
	var standaloneGW []GatewayRouteConfig
	for _, rcfg := range gwRoutes {
		if rcfg.Service != "" {
			gwBySvc[rcfg.Service] = append(gwBySvc[rcfg.Service], rcfg)
		} else {
			standaloneGW = append(standaloneGW, rcfg)
		}
	}

	addUniqueURL := func(list []string, u string) []string {
		if u == "" {
			return list
		}
		for _, existing := range list {
			if existing == u {
				return list
			}
		}
		return append(list, u)
	}

	entries := make([]CatalogEntry, 0, len(cfg.Services)+len(standaloneProxies)+len(standaloneGW))
	svcsSorted := append([]ServiceConfig(nil), cfg.Services...)
	sort.Slice(svcsSorted, func(i, j int) bool { return svcsSorted[i].Name < svcsSorted[j].Name })

	for _, scfg := range svcsSorted {
		mode := scfg.EffectiveMode()
		state := "stopped"
		healthy := false
		if r, ok := runners[scfg.Name]; ok {
			sum := r.HealthSummary()
			state = sum.State
			healthy = sum.Healthy || sum.State == "sleeping"
		} else if mb, ok := bridges[scfg.Name]; ok {
			sum := mb.HealthSummary()
			state = sum.State
			healthy = sum.Healthy || sum.State == "sleeping"
		}

		var urls []string
		viaGW := ""
		viaProxy := ""
		auth := CatalogAuthInfo{Type: "none"}

		for _, rcfg := range gwBySvc[scfg.Name] {
			if gwAddr != "" {
				var u string
				if mode == "stdio-bridge" {
					u = fmt.Sprintf("http://%s/s/%s", gwAddr, rcfg.Slug)
				} else {
					cp := rcfg.ClientPath
					if strings.TrimSpace(cp) == "" {
						cp = scfg.EffectiveClientPath()
					}
					u = fmt.Sprintf("http://%s/s/%s%s", gwAddr, rcfg.Slug, cp)
				}
				urls = addUniqueURL(urls, u)
			}
			if viaGW == "" {
				viaGW = rcfg.Slug
				if tok := firstTokenFile(rcfg.TokenFile, rcfg.Tokens); tok != "" {
					auth = CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
				}
			}
		}

		for _, pcfg := range proxiesBySvc[scfg.Name] {
			addr := pcfg.ListenAddr
			if ps, ok := proxies[pcfg.Name]; ok {
				addr = ps.ListenAddr()
			}
			cp := pcfg.ClientPath
			if strings.TrimSpace(cp) == "" {
				cp = scfg.EffectiveClientPath()
			}
			urls = addUniqueURL(urls, fmt.Sprintf("http://%s%s", addr, cp))
			if viaProxy == "" {
				viaProxy = pcfg.Name
				if auth.Type == "none" {
					if tok := firstTokenFile(pcfg.TokenFile, pcfg.Tokens); tok != "" {
						auth = CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
					}
				}
			}
		}

		if mode == "http" {
			if base := InferServiceBaseURL(scfg); base != "" {
				urls = addUniqueURL(urls, strings.TrimRight(base, "/")+scfg.EffectiveClientPath())
			}
			if auth.Type == "none" {
				if tok := InferServiceTokenFile(scfg); tok != "" {
					auth = CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
				}
			}
		}

		clientURL := ""
		if len(urls) > 0 {
			clientURL = urls[0]
		}

		entries = append(entries, CatalogEntry{
			Name:       scfg.Name,
			Mode:       mode,
			ClientURL:  clientURL,
			ClientURLs: urls,
			ViaProxy:   viaProxy,
			ViaGateway: viaGW,
			Auth:       auth,
			State:      state,
			Healthy:    healthy,
			Capabilities: CatalogCapabilities{
				Reload:           strings.TrimSpace(scfg.ReloadURL) != "",
				GracefulShutdown: strings.TrimSpace(scfg.ShutdownURL) != "" || mode == "stdio-bridge",
				LazyWake:         scfg.EffectiveStartMode() == "lazy",
				StdioBridge:      mode == "stdio-bridge",
			},
			Description: scfg.Description,
			Tags:        append([]string(nil), scfg.Tags...),
		})
	}

	// Include standalone proxies/gateway routes not linked to a managed service.
	for _, pcfg := range standaloneProxies {
		addr := pcfg.ListenAddr
		running := false
		if ps, ok := proxies[pcfg.Name]; ok {
			st := ps.Status()
			addr = st.ListenAddr
			running = st.Running
		}
		u := fmt.Sprintf("http://%s%s", addr, pcfg.EffectiveClientPath())
		auth := CatalogAuthInfo{Type: "none"}
		if tok := firstTokenFile(pcfg.TokenFile, pcfg.Tokens); tok != "" {
			auth = CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
		}
		state := "stopped"
		if running {
			state = "running"
		}
		entries = append(entries, CatalogEntry{
			Name:       pcfg.Name,
			Mode:       "http",
			ClientURL:  u,
			ClientURLs: []string{u},
			ViaProxy:   pcfg.Name,
			Auth:       auth,
			State:      state,
			Healthy:    running,
		})
	}

	for _, rcfg := range standaloneGW {
		u := fmt.Sprintf("http://%s/s/%s%s", gwAddr, rcfg.Slug, rcfg.EffectiveClientPath())
		auth := CatalogAuthInfo{Type: "none"}
		if tok := firstTokenFile(rcfg.TokenFile, rcfg.Tokens); tok != "" {
			auth = CatalogAuthInfo{Type: "bearer", TokenFile: tok, Header: "Authorization"}
		}
		running := gw != nil && gw.Status().Running
		state := "stopped"
		if running {
			state = "running"
		}
		entries = append(entries, CatalogEntry{
			Name:        rcfg.Slug,
			Mode:        "http",
			ClientURL:   u,
			ClientURLs:  []string{u},
			ViaGateway:  rcfg.Slug,
			Auth:        auth,
			State:       state,
			Healthy:     running,
			Description: rcfg.Description,
		})
	}

	return CatalogResponse{
		SchemaVersion: 1,
		OK:            true,
		Entries:       entries,
	}
}

func firstTokenFile(primary string, tokens []RouteTokenConfig) string {
	if strings.TrimSpace(primary) != "" {
		return strings.TrimSpace(primary)
	}
	for _, tc := range tokens {
		if strings.TrimSpace(tc.TokenFile) != "" {
			return strings.TrimSpace(tc.TokenFile)
		}
	}
	return ""
}

func isStructuredTokenPolicyFile(envKey, path string) bool {
	ku := strings.ToUpper(envKey)
	if strings.HasSuffix(ku, "_TOKENS_FILE") || strings.HasSuffix(ku, "_TOKENS_FILES") {
		return true
	}
	return strings.EqualFold(filepath.Ext(strings.TrimSpace(path)), ".json")
}

// InferServiceTokenFile resolves a direct HTTP service's client-usable single-line Bearer token file.
// Multi-token JSON policy files (*_TOKENS_FILE / *.json) are skipped so Catalog(), mcp-hub-ctl
// probe/tools/snippet, and the Web UI never attempt to send a structured policy file as a Bearer token.
func InferServiceTokenFile(svc ServiceConfig) string {
	if tok := strings.TrimSpace(svc.ShutdownTokenFile); tok != "" {
		return tok
	}
	if tok := strings.TrimSpace(svc.ReloadTokenFile); tok != "" {
		return tok
	}
	envKeys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		envKeys = append(envKeys, k)
	}
	sort.Strings(envKeys)
	for _, k := range envKeys {
		v := strings.TrimSpace(svc.Env[k])
		ku := strings.ToUpper(k)
		if v == "" || !strings.HasSuffix(ku, "_TOKEN_FILE") || isStructuredTokenPolicyFile(k, v) {
			continue
		}
		return v
	}
	envFileKeys := make([]string, 0, len(svc.EnvFiles))
	for k := range svc.EnvFiles {
		envFileKeys = append(envFileKeys, k)
	}
	sort.Strings(envFileKeys)
	for _, k := range envFileKeys {
		v := strings.TrimSpace(svc.EnvFiles[k])
		ku := strings.ToUpper(k)
		if v == "" || !strings.Contains(ku, "TOKEN") || isStructuredTokenPolicyFile(k, v) {
			continue
		}
		return v
	}
	return ""
}

// StartService starts a managed service or stdio bridge by name.
func (s *Supervisor) StartService(ctx context.Context, name string) (ServiceStatus, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	mb, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		err := r.Start(ctx)
		st := r.Snapshot()
		s.emitEvent("service.start", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	if okB {
		mb.ringLog.ClearCrashTail()
		err := mb.bridge.Start(ctx)
		st := mb.Snapshot()
		s.emitEvent("service.start", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	return ServiceStatus{}, false, nil
}

// StopService stops a managed service or stdio bridge by name.
func (s *Supervisor) StopService(ctx context.Context, name string) (ServiceStatus, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	mb, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		err := r.Stop(ctx)
		st := r.Snapshot()
		s.emitEvent("service.stop", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	if okB {
		err := mb.bridge.Stop(ctx)
		st := mb.Snapshot()
		s.emitEvent("service.stop", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	return ServiceStatus{}, false, nil
}

// SleepService puts a running managed service or stdio bridge to sleep.
func (s *Supervisor) SleepService(ctx context.Context, name string) (ServiceStatus, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	mb, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		err := r.SleepContext(ctx)
		st := r.Snapshot()
		s.emitEvent("service.sleep", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	if okB {
		err := mb.bridge.Sleep(ctx)
		st := mb.Snapshot()
		s.emitEvent("service.sleep", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	return ServiceStatus{}, false, nil
}

// WakeService lazily wakes a sleeping managed service or stdio bridge.
func (s *Supervisor) WakeService(ctx context.Context, name string) (ServiceStatus, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	mb, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		err := r.Wake(ctx)
		st := r.Snapshot()
		s.emitEvent("service.wake", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	if okB {
		err := mb.bridge.Wake(ctx)
		st := mb.Snapshot()
		s.emitEvent("service.wake", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	return ServiceStatus{}, false, nil
}

// RestartService restarts one named managed service or stdio bridge.
func (s *Supervisor) RestartService(ctx context.Context, name string, waitHealthy bool) (ServiceStatus, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	mb, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		err := r.Restart(ctx, waitHealthy)
		st := r.Snapshot()
		s.emitEvent("service.restart", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	if okB {
		mb.ringLog.ClearCrashTail()
		err := mb.bridge.Restart(ctx)
		st := mb.Snapshot()
		s.emitEvent("service.restart", name, st.State, "")
		s.notifyStateChange()
		return st, true, err
	}
	return ServiceStatus{}, false, nil
}

func (s *Supervisor) allServiceNames() []string {
	s.mu.RLock()
	names := make([]string, 0, len(s.services)+len(s.bridges))
	for name := range s.services {
		names = append(names, name)
	}
	for name := range s.bridges {
		names = append(names, name)
	}
	s.mu.RUnlock()
	sort.Strings(names)
	return names
}

// StartAllServices starts all managed services and stdio bridges concurrently.
func (s *Supervisor) StartAllServices(ctx context.Context) map[string]ServiceActionResponse {
	names := s.allServiceNames()
	out := make(map[string]ServiceActionResponse, len(names))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range names {
		wg.Add(1)
		go func(svcName string) {
			defer wg.Done()
			st, _, err := s.StartService(ctx, svcName)
			item := ServiceActionResponse{OK: err == nil, Name: svcName, Service: st}
			if err != nil {
				item.Error = s.redactor.Redact(err.Error())
			}
			mu.Lock()
			out[svcName] = item
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

// StopAllServices stops all managed services and stdio bridges concurrently.
func (s *Supervisor) StopAllServices(ctx context.Context) map[string]ServiceActionResponse {
	names := s.allServiceNames()
	out := make(map[string]ServiceActionResponse, len(names))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range names {
		wg.Add(1)
		go func(svcName string) {
			defer wg.Done()
			st, _, err := s.StopService(ctx, svcName)
			item := ServiceActionResponse{OK: err == nil, Name: svcName, Service: st}
			if err != nil {
				item.Error = s.redactor.Redact(err.Error())
			}
			mu.Lock()
			out[svcName] = item
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

// RestartAllServices restarts all managed services and stdio bridges concurrently.
func (s *Supervisor) RestartAllServices(ctx context.Context, waitHealthy bool) map[string]RestartResponse {
	names := s.allServiceNames()
	out := make(map[string]RestartResponse, len(names))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, name := range names {
		wg.Add(1)
		go func(svcName string) {
			defer wg.Done()
			st, _, err := s.RestartService(ctx, svcName, waitHealthy)
			item := RestartResponse{OK: err == nil, Name: svcName, Service: st}
			if err != nil {
				item.Error = s.redactor.Redact(err.Error())
			}
			mu.Lock()
			out[svcName] = item
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

// ReloadService calls the configured reloadUrl on one named HTTP service.
func (s *Supervisor) ReloadService(ctx context.Context, name string) (json.RawMessage, bool, error) {
	s.mu.RLock()
	r, okR := s.services[name]
	_, okB := s.bridges[name]
	s.mu.RUnlock()

	if okR {
		raw, err := r.Reload(ctx)
		if err == nil {
			s.emitEvent("service.reload", name, r.State(), "")
		}
		return raw, true, err
	}
	if okB {
		return nil, true, ErrReloadNotSupported
	}
	return nil, false, nil
}

// TraySnapshot produces the concise status snapshot rendered by WindowsLifecycle's System Tray.
func (s *Supervisor) TraySnapshot() TraySnapshot {
	st := s.Status()
	cat := s.Catalog()

	clientURLByName := make(map[string]string, len(cat.Entries))
	for _, entry := range cat.Entries {
		if entry.ClientURL != "" {
			clientURLByName[entry.Name] = entry.ClientURL
		}
	}

	names := make([]string, 0, len(st.Services))
	for k := range st.Services {
		names = append(names, k)
	}
	sort.Strings(names)

	items := make([]TrayServiceItem, 0, len(names))
	runningCount := 0
	sleepingCount := 0
	stoppedCount := 0
	startingCount := 0
	errorCount := 0
	hasUnhealthyRunning := false

	for _, name := range names {
		svc := st.Services[name]
		activeReqs := svc.ActiveInFlight
		activeSSE := svc.ActiveSSE
		var totalReqs int64
		if svc.Metrics != nil {
			totalReqs = svc.Metrics.TotalRequests
			if svc.Metrics.ActiveRequests > activeReqs {
				activeReqs = svc.Metrics.ActiveRequests
			}
			if svc.Metrics.ActiveSSE > activeSSE {
				activeSSE = svc.Metrics.ActiveSSE
			}
		}
		lastErr := svc.LastError
		if lastErr == "" && !svc.Healthy && svc.LastHealthError != "" {
			lastErr = svc.LastHealthError
		}
		canSleep := svc.IdleSleepSec > 0 || svc.Mode == "stdio-bridge" || svc.StartMode == "lazy"

		items = append(items, TrayServiceItem{
			Name:          name,
			State:         svc.State,
			Healthy:       svc.Healthy,
			Running:       svc.Running,
			HasReload:     svc.HasReload,
			CanSleep:      canSleep,
			Mode:          svc.Mode,
			StartMode:     svc.StartMode,
			PID:           svc.PID,
			Port:          svc.Port,
			PrivateMB:     svc.TreePrivateMB,
			Restarts:      svc.Restarts,
			CrashRestarts: svc.CrashRestarts,
			WakeCount:     svc.WakeCount,
			ActiveReqs:    activeReqs,
			ActiveSSE:     activeSSE,
			TotalReqs:     totalReqs,
			LogFile:       svc.LogFile,
			ClientURL:     clientURLByName[name],
			LastError:     lastErr,
		})
		switch svc.State {
		case "running":
			runningCount++
			if !svc.Healthy {
				hasUnhealthyRunning = true
			}
		case "sleeping":
			sleepingCount++
		case "starting", "stopping":
			startingCount++
		case "stopped":
			stoppedCount++
		case "degraded":
			errorCount++
		default:
			hasUnhealthyRunning = true
		}
	}

	infraDown := false
	for _, p := range st.Proxies {
		if !p.Running {
			infraDown = true
			break
		}
	}
	if st.Gateway != nil && !st.Gateway.Running {
		infraDown = true
	}

	overall := "ok"
	switch {
	case errorCount > 0:
		overall = "error"
	case hasUnhealthyRunning || stoppedCount > 0 || infraDown:
		overall = "degraded"
	case startingCount > 0:
		overall = "starting"
	case runningCount == 0 && sleepingCount > 0:
		overall = "sleeping"
	default:
		overall = "ok"
	}

	var parts []string
	if runningCount > 0 || (sleepingCount == 0 && startingCount == 0 && stoppedCount == 0 && errorCount == 0) {
		parts = append(parts, fmt.Sprintf("%d up", runningCount))
	}
	if sleepingCount > 0 {
		parts = append(parts, fmt.Sprintf("%d sleep", sleepingCount))
	}
	if startingCount > 0 {
		parts = append(parts, fmt.Sprintf("%d starting", startingCount))
	}
	if stoppedCount > 0 {
		parts = append(parts, fmt.Sprintf("%d stopped", stoppedCount))
	}
	if errorCount > 0 {
		parts = append(parts, fmt.Sprintf("%d err", errorCount))
	}
	if hasUnhealthyRunning || infraDown {
		parts = append(parts, "degraded")
	}
	summary := fmt.Sprintf("McpHub v%s | %s | %.0f MB", Version, strings.Join(parts, ", "), st.TreeTotalPrivateMB)

	return TraySnapshot{
		State:          overall,
		Tooltip:        summary,
		Header:         summary,
		Version:        Version,
		UptimeSec:      st.UptimeSec,
		TotalPrivateMB: st.TreeTotalPrivateMB,
		ControlAddr:    st.ControlAddr,
		ConfigPath:     st.ConfigPath,
		LogDir:         st.LogDir,
		RunningCount:   runningCount,
		SleepingCount:  sleepingCount,
		StoppedCount:   stoppedCount,
		StartingCount:  startingCount,
		ErrorCount:     errorCount,
		Services:       items,
	}
}

// Client talks to a running mcp-hub ControlServer over localhost HTTP without proxy interference.
type Client struct {
	baseURL    string
	token      string
	http       *http.Client
	streamHTTP *http.Client
}

// NewClient creates a control API client for addr (host:port) and optional Bearer token.
// Per-request timeouts follow caller context deadlines (defaulting to 40s when ctx has no deadline).
func NewClient(addr, token string) *Client {
	return NewClientWithTimeout(addr, token, 0)
}

// NewClientWithTimeout creates a control API client with an explicit http.Client timeout.
// Pass timeout <= 0 to rely on caller context deadlines.
func NewClientWithTimeout(addr, token string, timeout time.Duration) *Client {
	trimmed := strings.TrimSpace(addr)
	if !strings.HasPrefix(trimmed, "http://") && !strings.HasPrefix(trimmed, "https://") {
		trimmed = "http://" + trimmed
	}
	return &Client{
		baseURL:    strings.TrimRight(trimmed, "/"),
		token:      token,
		http:       NewDirectClient(timeout),
		streamHTTP: NewDirectClient(0),
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, withAuth bool, dst any) (int, []byte, error) {
	if c.http.Timeout == 0 {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 40*time.Second)
			defer cancel()
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if withAuth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, body, fmt.Errorf("control API %s %s returned HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if dst != nil && len(body) > 0 {
		if err := json.Unmarshal(body, dst); err != nil {
			return resp.StatusCode, body, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return resp.StatusCode, body, nil
}

// Healthz queries GET /healthz (unauthenticated).
func (c *Client) Healthz(ctx context.Context) (*HealthzResponse, []byte, error) {
	var out HealthzResponse
	_, raw, err := c.doJSON(ctx, http.MethodGet, "/healthz", false, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// Status queries GET /status.
func (c *Client) Status(ctx context.Context) (*StatusResponse, []byte, error) {
	var out StatusResponse
	_, raw, err := c.doJSON(ctx, http.MethodGet, "/status", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// Catalog queries GET /catalog.
func (c *Client) Catalog(ctx context.Context) (*CatalogResponse, []byte, error) {
	var out CatalogResponse
	_, raw, err := c.doJSON(ctx, http.MethodGet, "/catalog", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// Logs queries GET /logs/<name>?lines=<n>.
func (c *Client) Logs(ctx context.Context, name string, lines int) (*LogsResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("log name is required")
	}
	q := ""
	if lines > 0 {
		q = "?lines=" + strconv.Itoa(lines)
	}
	var out LogsResponse
	_, raw, err := c.doJSON(ctx, http.MethodGet, "/logs/"+url.PathEscape(name)+q, true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// FollowLogs streams live SSE log lines from GET /logs/<name>?lines=<n>&follow=1 until ctx cancels or onLine returns error.
func (c *Client) FollowLogs(ctx context.Context, name string, lines int, onLine func(LogLine) error) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("log name is required")
	}
	params := url.Values{}
	params.Set("follow", "1")
	if lines > 0 {
		params.Set("lines", strconv.Itoa(lines))
	}
	reqURL := c.baseURL + "/logs/" + url.PathEscape(name) + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control API GET %s returned HTTP %d: %s", reqURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	br := bufio.NewReader(resp.Body)
	var dataParts []string
	flushFrame := func() error {
		if len(dataParts) == 0 {
			return nil
		}
		joined := strings.Join(dataParts, "\n")
		dataParts = dataParts[:0]
		var ln LogLine
		if err := json.Unmarshal([]byte(joined), &ln); err != nil {
			return nil
		}
		if onLine != nil {
			return onLine(ln)
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if cbErr := flushFrame(); cbErr != nil {
					return cbErr
				}
			} else if strings.HasPrefix(trimmed, "data:") {
				val := strings.TrimPrefix(trimmed, "data:")
				val = strings.TrimPrefix(val, " ")
				dataParts = append(dataParts, val)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flushFrame()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// StartService triggers POST /start/<name>.
func (c *Client) StartService(ctx context.Context, name string) (*ServiceActionResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	var out ServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/start/"+url.PathEscape(name), true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// StopService triggers POST /stop/<name>.
func (c *Client) StopService(ctx context.Context, name string) (*ServiceActionResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	var out ServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/stop/"+url.PathEscape(name), true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// Restart triggers POST /restart/<name>.
func (c *Client) Restart(ctx context.Context, name string) (*RestartResponse, []byte, error) {
	return c.RestartWithWait(ctx, name, false)
}

// RestartWithWait triggers POST /restart/<name>, optionally waiting until healthy (?wait=healthy).
func (c *Client) RestartWithWait(ctx context.Context, name string, waitHealthy bool) (*RestartResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	path := "/restart/" + url.PathEscape(name)
	if waitHealthy {
		path += "?wait=healthy"
	}
	var out RestartResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, path, true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// ReloadHub triggers POST /reload (hot-reload config.json & tokens).
func (c *Client) ReloadHub(ctx context.Context) (*ReloadHubResponse, []byte, error) {
	var out ReloadHubResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/reload", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// ReloadService triggers POST /reload/<name> (calls the service's reloadUrl).
func (c *Client) ReloadService(ctx context.Context, name string) (*ReloadServiceResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	var out ReloadServiceResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/reload/"+url.PathEscape(name), true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// SleepService triggers POST /sleep/<name>.
func (c *Client) SleepService(ctx context.Context, name string) (*ServiceActionResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	var out ServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/sleep/"+url.PathEscape(name), true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// WakeService triggers POST /wake/<name>.
func (c *Client) WakeService(ctx context.Context, name string) (*ServiceActionResponse, []byte, error) {
	if strings.TrimSpace(name) == "" {
		return nil, nil, errors.New("service name is required")
	}
	var out ServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/wake/"+url.PathEscape(name), true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// StartAll triggers POST /services/start-all.
func (c *Client) StartAll(ctx context.Context) (*BulkServiceActionResponse, []byte, error) {
	var out BulkServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/services/start-all", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// StopAll triggers POST /services/stop-all.
func (c *Client) StopAll(ctx context.Context) (*BulkServiceActionResponse, []byte, error) {
	var out BulkServiceActionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/services/stop-all", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// RestartAll triggers POST /services/restart-all, optionally waiting until healthy (?wait=healthy).
func (c *Client) RestartAll(ctx context.Context, waitHealthy bool) (*BulkRestartResponse, []byte, error) {
	path := "/services/restart-all"
	if waitHealthy {
		path += "?wait=healthy"
	}
	var out BulkRestartResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, path, true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// ValidateConfig triggers POST /config/validate (dry-run config reload preview).
func (c *Client) ValidateConfig(ctx context.Context) (*ReloadHubResponse, []byte, error) {
	var out ReloadHubResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/config/validate", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// CreateDashboardSession triggers POST /ui/session to mint a single-use Web Dashboard bootstrap URL.
func (c *Client) CreateDashboardSession(ctx context.Context) (*DashboardSessionResponse, []byte, error) {
	var out DashboardSessionResponse
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/ui/session", true, &out)
	if err != nil {
		return nil, raw, err
	}
	return &out, raw, nil
}

// FollowEvents streams live SSE HubEvent records from GET /events until ctx cancels or onEvent returns error.
func (c *Client) FollowEvents(ctx context.Context, onEvent func(HubEvent) error) error {
	reqURL := c.baseURL + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("control API GET %s returned HTTP %d: %s", reqURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	br := bufio.NewReader(resp.Body)
	var dataParts []string
	flushFrame := func() error {
		if len(dataParts) == 0 {
			return nil
		}
		joined := strings.Join(dataParts, "\n")
		dataParts = dataParts[:0]
		var ev HubEvent
		if err := json.Unmarshal([]byte(joined), &ev); err != nil {
			return nil
		}
		if onEvent != nil {
			return onEvent(ev)
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if cbErr := flushFrame(); cbErr != nil {
					return cbErr
				}
			} else if strings.HasPrefix(trimmed, "data:") {
				val := strings.TrimPrefix(trimmed, "data:")
				val = strings.TrimPrefix(val, " ")
				dataParts = append(dataParts, val)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flushFrame()
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// Stop triggers POST /stop.
func (c *Client) Stop(ctx context.Context) ([]byte, error) {
	_, raw, err := c.doJSON(ctx, http.MethodPost, "/stop", true, nil)
	return raw, err
}
