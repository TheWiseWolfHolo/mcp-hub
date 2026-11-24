package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var defaultBackoffSchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
}

var serviceEpochSequence atomic.Uint64

const (
	defaultHealthInterval    = 10 * time.Second
	defaultHealthyResetAfter = 60 * time.Second
	defaultShutdownTimeout   = 5 * time.Second
	defaultStartingPollTick  = 75 * time.Millisecond
	memoryStatsCacheTTL      = 1500 * time.Millisecond
	reloadRequestTimeout     = 30 * time.Second
	maxReloadResponseBytes   = 256 << 10
	logOpenRetryCooldown     = 5 * time.Second
)

var (
	// ErrReloadNotSupported indicates the service has no reloadUrl configured.
	ErrReloadNotSupported = errors.New("service does not configure reloadUrl")
	// ErrServiceStopped indicates the service was manually stopped and must not auto-wake.
	ErrServiceStopped = errors.New("service is stopped")
	// ErrServiceSleeping indicates a no-wake request found no running process.
	ErrServiceSleeping = errors.New("service is not running and wake is disabled")
	// ErrServiceDegraded indicates the crash-loop circuit breaker is open.
	ErrServiceDegraded = errors.New("service is degraded (crash loop circuit breaker open)")
)

// ServiceStatus captures the runtime status of a managed child service or stdio bridge.
type ServiceStatus struct {
	Name                   string              `json:"name"`
	Running                bool                `json:"running"`
	Healthy                bool                `json:"healthy"`
	Restarts               int                 `json:"restarts"`
	CrashRestarts          int                 `json:"crashRestarts,omitempty"`
	WakeCount              int                 `json:"wakeCount,omitempty"`
	PID                    int                 `json:"pid"`
	Port                   int                 `json:"port"`
	StartedAt              string              `json:"startedAt,omitempty"`
	HealthySince           string              `json:"healthySince,omitempty"`
	LastExitCode           int                 `json:"lastExitCode"`
	LastError              string              `json:"lastError,omitempty"`
	LastHealthError        string              `json:"lastHealthError,omitempty"`
	LastHealthCheckAt      string              `json:"lastHealthCheckAt,omitempty"`
	ConsecutiveFailures    int                 `json:"consecutiveFailures,omitempty"`
	MaxConsecutiveFailures int                 `json:"maxConsecutiveFailures,omitempty"`
	IdleSleepSec           int                 `json:"idleSleepSec,omitempty"`
	ActiveInFlight         int64               `json:"activeInFlight,omitempty"`
	ActiveSSE              int64               `json:"activeSSE,omitempty"`
	MetricsSource          string              `json:"metricsSource,omitempty"` // "proxy", "gateway", "bridge", "none"
	LogFile                string              `json:"logFile,omitempty"`
	Mode                   string              `json:"mode,omitempty"`
	StartMode              string              `json:"startMode,omitempty"`
	State                  string              `json:"state,omitempty"`
	ProcessCount           int                 `json:"processCount,omitempty"`
	TreeWorkingSetMB       float64             `json:"treeWorkingSetMB,omitempty"`
	TreePrivateMB          float64             `json:"treePrivateMB,omitempty"`
	TreePeakMB             float64             `json:"treePeakMB,omitempty"`
	LastActivityAt         string              `json:"lastActivityAt,omitempty"`
	DegradedAt             string              `json:"degradedAt,omitempty"`
	HasReload              bool                `json:"hasReload,omitempty"`
	LastStderr             []string            `json:"lastStderr,omitempty"`
	CrashTail              []string            `json:"crashTail,omitempty"`
	Description            string              `json:"description,omitempty"`
	Tags                   []string            `json:"tags,omitempty"`
	Metrics                *MCPMetricsSnapshot `json:"metrics,omitempty"`
}

// ServiceHealthSummary matches the compact /healthz service schema.
type ServiceHealthSummary struct {
	Running   bool   `json:"running"`
	Healthy   bool   `json:"healthy"`
	Restarts  int    `json:"restarts"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	State     string `json:"state,omitempty"`
	Mode      string `json:"mode,omitempty"`
	LastError string `json:"lastError,omitempty"`
}

type serviceControlOp int

const (
	svcOpStart serviceControlOp = iota
	svcOpStop
	svcOpRestart
	svcOpWake
	svcOpSleep
)

type serviceControlReq struct {
	op          serviceControlOp
	waitHealthy bool
	reply       chan error
}

type serviceLaunchKind int

const (
	launchInitial serviceLaunchKind = iota
	launchWake
	launchManualRestart
	launchCrashRecovery
	launchManualStart
)

// ServiceRunner supervises one child daemon with Job Object isolation, redacted rotating logs,
// ring-buffer log capture, lazy start / idle sleep, crash-loop circuit breaker, and graceful stop.
type ServiceRunner struct {
	cfg               ServiceConfig
	logDir            string
	logMaxBytes       int64
	hubLogger         *log.Logger
	redactor          *Redactor
	ringLog           *RingLog
	httpClient        *http.Client
	backoff           []time.Duration
	healthyResetAfter time.Duration
	shutdownTimeout   time.Duration
	startTimeout      time.Duration
	idleSleep         time.Duration
	abandonedSleep    time.Duration
	maxFailures       int
	healthInterval    time.Duration
	startingPollTick  time.Duration

	ctrlCh     chan serviceControlReq
	doneCh     chan struct{}
	runStarted atomic.Bool
	forceCtx   context.Context
	forceStop  context.CancelFunc

	inFlight         atomic.Int64
	activeSSE        atomic.Int64
	lastActivity     atomic.Int64
	suspended        atomic.Bool
	resumeDelayUntil atomic.Int64

	// OnStateChange is invoked outside internal locks when service state or health changes.
	OnStateChange func()

	mu              sync.RWMutex
	status          ServiceStatus
	healthySince    time.Time
	maxHealthyRun   time.Duration
	launchedOnce    bool
	activeJob       *ProcessJob
	epoch           uint64
	epochCtx        context.Context
	epochCancel     context.CancelFunc
	sessions        map[string]struct{}
	attachedMetrics []*MCPMetrics
	metricsSource   string

	memMu        sync.Mutex
	memCachedAt  time.Time
	memCachedJob *ProcessJob
	memCached    JobMemoryStats
}

// NewServiceRunner initializes a runner for one ServiceConfig.
func NewServiceRunner(cfg ServiceConfig, logDir string, logMaxBytes int64, hubLogger *log.Logger) *ServiceRunner {
	interval := defaultHealthInterval
	if cfg.HealthIntervalSec > 0 {
		interval = time.Duration(cfg.HealthIntervalSec) * time.Second
	}
	initState := "stopped"
	if cfg.EffectiveStartMode() == "lazy" {
		initState = "sleeping"
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	forceCtx, forceStop := context.WithCancel(context.Background())

	r := &ServiceRunner{
		cfg:               cfg,
		logDir:            logDir,
		logMaxBytes:       logMaxBytes,
		hubLogger:         hubLogger,
		redactor:          NewRedactor(),
		ringLog:           NewRingLog(256, DefaultRingLogMaxLineBytes),
		httpClient:        NewDirectClient(0),
		backoff:           append([]time.Duration(nil), defaultBackoffSchedule...),
		healthyResetAfter: defaultHealthyResetAfter,
		shutdownTimeout:   defaultShutdownTimeout,
		startTimeout:      cfg.EffectiveStartTimeout(),
		idleSleep:         cfg.EffectiveIdleSleep(),
		abandonedSleep:    cfg.EffectiveAbandonedSessionSleep(),
		maxFailures:       cfg.EffectiveMaxConsecutiveFailures(),
		healthInterval:    interval,
		startingPollTick:  defaultStartingPollTick,
		ctrlCh:            make(chan serviceControlReq, 32),
		doneCh:            make(chan struct{}),
		forceCtx:          forceCtx,
		forceStop:         forceStop,
		epochCtx:          canceledCtx,
		sessions:          make(map[string]struct{}),
		status: ServiceStatus{
			Name:        cfg.Name,
			Port:        InferServicePort(cfg),
			LogFile:     filepath.Join(logDir, cfg.Name+".log"),
			Mode:        cfg.EffectiveMode(),
			StartMode:   cfg.EffectiveStartMode(),
			State:       initState,
			HasReload:   strings.TrimSpace(cfg.ReloadURL) != "",
			Description: cfg.Description,
			Tags:        append([]string(nil), cfg.Tags...),
		},
	}
	r.lastActivity.Store(time.Now().UnixNano())
	return r
}

// SetRedactor installs a shared Redactor before Run starts.
func (r *ServiceRunner) SetRedactor(redactor *Redactor) {
	if redactor != nil {
		r.redactor = redactor
	}
}

// SetOnStateChange registers a callback invoked on state or health transitions.
func (r *ServiceRunner) SetOnStateChange(fn func()) {
	r.mu.Lock()
	r.OnStateChange = fn
	r.mu.Unlock()
}

func (r *ServiceRunner) notifyStateChange() {
	r.mu.RLock()
	fn := r.OnStateChange
	r.mu.RUnlock()
	if fn != nil {
		fn()
	}
}

// RingLog returns the runner's bounded in-memory log ring buffer.
func (r *ServiceRunner) RingLog() *RingLog {
	return r.ringLog
}

// Config returns the ServiceConfig associated with this runner.
func (r *ServiceRunner) Config() ServiceConfig {
	return r.cfg
}

// StartTimeout returns the configured readiness timeout for this service.
func (r *ServiceRunner) StartTimeout() time.Duration {
	if r.startTimeout > 0 {
		return r.startTimeout
	}
	return r.cfg.EffectiveStartTimeout()
}

// AttachMetrics associates an upstream proxy or gateway MCPMetrics collector with this service.
func (r *ServiceRunner) AttachMetrics(m *MCPMetrics) {
	r.AttachMetricsWithSource(m, "proxy")
}

// AttachMetricsWithSource associates an upstream MCPMetrics collector and records its source ("proxy" or "gateway").
func (r *ServiceRunner) AttachMetricsWithSource(m *MCPMetrics, source string) {
	if m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if source != "" {
		if r.metricsSource == "" || source == "gateway" {
			r.metricsSource = source
		}
	}
	for _, existing := range r.attachedMetrics {
		if existing == m {
			return
		}
	}
	r.attachedMetrics = append(r.attachedMetrics, m)
}

// DetachMetrics removes a previously attached MCPMetrics collector.
func (r *ServiceRunner) DetachMetrics(m *MCPMetrics) {
	if m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.attachedMetrics {
		if existing == m {
			r.attachedMetrics = append(r.attachedMetrics[:i], r.attachedMetrics[i+1:]...)
			if len(r.attachedMetrics) == 0 {
				r.metricsSource = ""
			}
			return
		}
	}
}

func (r *ServiceRunner) setMetrics(metrics []*MCPMetrics) {
	source := ""
	if len(metrics) > 0 {
		source = "proxy"
	}
	r.setMetricsWithSource(metrics, source)
}

func (r *ServiceRunner) setMetricsWithSource(metrics []*MCPMetrics, source string) {
	r.mu.Lock()
	r.attachedMetrics = metrics
	if len(metrics) == 0 {
		r.metricsSource = ""
	} else if source != "" {
		r.metricsSource = source
	} else {
		r.metricsSource = "proxy"
	}
	r.mu.Unlock()
}

// State returns the current lifecycle state (stopped|sleeping|starting|running|degraded).
func (r *ServiceRunner) State() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.status.State
}

// Epoch returns the monotonically increasing launch epoch of the current process.
func (r *ServiceRunner) Epoch() uint64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.epoch
}

// EpochContext returns a context that is canceled when the current process stops, restarts, or sleeps.
func (r *ServiceRunner) EpochContext() context.Context {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.epochCtx
}

// TouchActivity records request/session activity for idle-sleep tracking.
func (r *ServiceRunner) TouchActivity() {
	r.lastActivity.Store(time.Now().UnixNano())
}

// InFlightAdd adjusts the active in-flight HTTP request count and refreshes lastActivity.
func (r *ServiceRunner) InFlightAdd(delta int64) {
	n := r.inFlight.Add(delta)
	if n < 0 {
		r.inFlight.Store(0)
	}
	r.TouchActivity()
}

// SSEAdd adjusts the active long-lived SSE stream count and refreshes lastActivity.
func (r *ServiceRunner) SSEAdd(delta int64) {
	n := r.activeSSE.Add(delta)
	if n < 0 {
		r.activeSSE.Store(0)
	}
	r.TouchActivity()
}

// TrackSession registers an active stateful MCP session ID.
func (r *ServiceRunner) TrackSession(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.mu.Lock()
	r.sessions[id] = struct{}{}
	r.mu.Unlock()
	r.TouchActivity()
}

// UntrackSession removes a closed/expired MCP session ID.
func (r *ServiceRunner) UntrackSession(id string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	r.mu.Lock()
	delete(r.sessions, id)
	r.mu.Unlock()
	r.TouchActivity()
}

func (r *ServiceRunner) clearSessionsLocked() {
	clear(r.sessions)
	for _, m := range r.attachedMetrics {
		if m != nil {
			m.ExpireSessions()
		}
	}
}

// Suspend freezes health polling and idle-sleep timers during OS power suspend.
func (r *ServiceRunner) Suspend() {
	r.suspended.Store(true)
}

// Resume unfreezes timers after OS wake, resets lastActivity, closes idle conns, and delays health probes 2s.
func (r *ServiceRunner) Resume() {
	now := time.Now()
	r.lastActivity.Store(now.UnixNano())
	r.resumeDelayUntil.Store(now.Add(2 * time.Second).UnixNano())
	r.suspended.Store(false)
	if tr, ok := r.httpClient.Transport.(*http.Transport); ok && tr != nil {
		tr.CloseIdleConnections()
	}
}

// Snapshot returns a detailed status snapshot including lazy-cached Job Object RAM stats and logs.
func (r *ServiceRunner) Snapshot() ServiceStatus {
	r.mu.RLock()
	st := r.status
	job := r.activeJob
	metricsList := append([]*MCPMetrics(nil), r.attachedMetrics...)
	metricsSource := r.metricsSource
	r.mu.RUnlock()

	if len(st.Tags) > 0 {
		st.Tags = append([]string(nil), st.Tags...)
	}
	st.MaxConsecutiveFailures = r.maxFailures
	st.IdleSleepSec = int(r.idleSleep / time.Second)
	st.ActiveInFlight = r.inFlight.Load()
	st.ActiveSSE = r.activeSSE.Load()
	if ts := r.lastActivity.Load(); ts > 0 {
		st.LastActivityAt = time.Unix(0, ts).UTC().Format(time.RFC3339)
	}
	if r.ringLog != nil {
		st.LastStderr = r.ringLog.TailText(5)
		st.CrashTail = r.ringLog.CrashTail()
	}
	if job != nil {
		stats := r.cachedJobMemory(job)
		st.ProcessCount = stats.ProcessCount
		st.TreeWorkingSetMB = bytesToMB(stats.WorkingSetBytes)
		st.TreePrivateMB = bytesToMB(stats.PrivateBytes)
		st.TreePeakMB = bytesToMB(stats.PeakBytes)
	}
	if len(metricsList) > 0 {
		agg := aggregateMCPMetrics(metricsList)
		st.Metrics = &agg
		if metricsSource != "" {
			st.MetricsSource = metricsSource
		} else {
			st.MetricsSource = "proxy"
		}
	} else {
		st.MetricsSource = "none"
	}
	return st
}

func (r *ServiceRunner) cachedJobMemory(job *ProcessJob) JobMemoryStats {
	r.memMu.Lock()
	if r.memCachedJob == job && time.Since(r.memCachedAt) < memoryStatsCacheTTL {
		cached := r.memCached
		r.memMu.Unlock()
		return cached
	}
	r.memMu.Unlock()

	stats, err := job.MemoryStats()
	if err != nil {
		return JobMemoryStats{}
	}

	r.memMu.Lock()
	r.memCachedJob = job
	r.memCachedAt = time.Now()
	r.memCached = stats
	r.memMu.Unlock()
	return stats
}

func bytesToMB(b uint64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(b)/(1024*1024)*100) / 100
}

func aggregateMCPMetrics(list []*MCPMetrics) MCPMetricsSnapshot {
	if len(list) == 1 {
		return list[0].Snapshot()
	}
	var out MCPMetricsSnapshot
	var lastReqTime time.Time
	var lastToolTime time.Time
	tools := make(map[string]ToolMetric)
	for _, m := range list {
		s := m.Snapshot()
		out.TotalRequests += s.TotalRequests
		out.ErrorRequests += s.ErrorRequests
		out.ActiveRequests += s.ActiveRequests
		out.ActiveSSE += s.ActiveSSE
		out.ActiveSessions += s.ActiveSessions
		if s.LastRequestAt != "" {
			if t, err := time.Parse(time.RFC3339, s.LastRequestAt); err == nil && (lastReqTime.IsZero() || t.After(lastReqTime)) {
				lastReqTime = t
				out.LastRequestAt = s.LastRequestAt
			}
		}
		if s.LastTool != "" {
			var toolTime time.Time
			if tm, ok := s.Tools[s.LastTool]; ok && tm.LastCalledAt != "" {
				toolTime, _ = time.Parse(time.RFC3339, tm.LastCalledAt)
			}
			if toolTime.IsZero() && s.LastRequestAt != "" {
				toolTime, _ = time.Parse(time.RFC3339, s.LastRequestAt)
			}
			if out.LastTool == "" ||
				(!toolTime.IsZero() && (lastToolTime.IsZero() || toolTime.After(lastToolTime))) ||
				(toolTime.Equal(lastToolTime) && s.LastTool > out.LastTool) {
				out.LastTool = s.LastTool
				lastToolTime = toolTime
			}
		}
		for name, tm := range s.Tools {
			cur, ok := tools[name]
			if !ok {
				tools[name] = tm
				continue
			}
			totalCalls := cur.Calls + tm.Calls
			var avg float64
			if totalCalls > 0 {
				avg = (cur.AvgMs*float64(cur.Calls) + tm.AvgMs*float64(tm.Calls)) / float64(totalCalls)
			}
			lastCalled := cur.LastCalledAt
			if tm.LastCalledAt > lastCalled {
				lastCalled = tm.LastCalledAt
			}
			tools[name] = ToolMetric{
				Calls:        totalCalls,
				Errors:       cur.Errors + tm.Errors,
				AvgMs:        avg,
				LastCalledAt: lastCalled,
			}
		}
	}
	if len(tools) > 0 {
		out.Tools = tools
	}
	return out
}

// HealthSummary returns the compact /healthz summary for this service.
func (r *ServiceRunner) HealthSummary() ServiceHealthSummary {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return ServiceHealthSummary{
		Running:   r.status.Running,
		Healthy:   r.status.Healthy,
		Restarts:  r.status.Restarts,
		PID:       r.status.PID,
		Port:      r.status.Port,
		State:     r.status.State,
		Mode:      r.status.Mode,
		LastError: r.status.LastError,
	}
}

func (r *ServiceRunner) sendControl(ctx context.Context, op serviceControlOp, waitHealthy bool) error {
	if !r.runStarted.Load() {
		return errors.New("service runner is not running")
	}
	reply := make(chan error, 1)
	req := serviceControlReq{op: op, waitHealthy: waitHealthy, reply: reply}
	select {
	case r.ctrlCh <- req:
	case <-r.doneCh:
		return errors.New("service runner is stopped")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		return err
	case <-r.doneCh:
		return errors.New("service runner stopped during control operation")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Start clears manual stop / degraded state and launches the service if not running.
func (r *ServiceRunner) Start(ctx context.Context) error {
	return r.sendControl(ctx, svcOpStart, false)
}

// Stop cleanly terminates any active process and enters manual "stopped" state.
func (r *ServiceRunner) Stop(ctx context.Context) error {
	return r.sendControl(ctx, svcOpStop, false)
}

// Restart gracefully stops any running process (or cancels backoff/degraded state) and launches a fresh instance.
// When waitHealthy is true, Restart waits until health probes succeed (or StartTimeout expires).
func (r *ServiceRunner) Restart(ctx context.Context, waitHealthy bool) error {
	return r.sendControl(ctx, svcOpRestart, waitHealthy)
}

// EnsureRunning lazily wakes a sleeping service (or joins an in-flight start via singleflight)
// and waits until it reaches "running" (healthy). Manually stopped and degraded services fail closed.
func (r *ServiceRunner) EnsureRunning(ctx context.Context) error {
	r.mu.RLock()
	state := r.status.State
	r.mu.RUnlock()
	switch state {
	case "running":
		r.TouchActivity()
		return nil
	case "stopped":
		return ErrServiceStopped
	case "degraded":
		return ErrServiceDegraded
	case "stopping", "sleeping", "starting":
		return r.sendControl(ctx, svcOpWake, true)
	}
	return r.sendControl(ctx, svcOpWake, true)
}

// BeginProxyRequest admits a request and captures its process epoch under the same lock
// used to transition to idle sleep.
func (r *ServiceRunner) BeginProxyRequest(ctx context.Context, noWake bool) (uint64, context.Context, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return 0, nil, nil, err
		}
		r.mu.Lock()
		state := r.status.State
		if state == "running" {
			epoch, epochCtx := r.epoch, r.epochCtx
			r.InFlightAdd(1)
			r.mu.Unlock()
			return epoch, epochCtx, func() { r.InFlightAdd(-1) }, nil
		}
		r.mu.Unlock()
		switch state {
		case "stopped":
			return 0, nil, nil, ErrServiceStopped
		case "degraded":
			return 0, nil, nil, ErrServiceDegraded
		}
		if noWake {
			return 0, nil, nil, ErrServiceSleeping
		}
		if err := r.EnsureRunning(ctx); err != nil {
			return 0, nil, nil, err
		}
	}
}

// Wake is an alias for EnsureRunning.
func (r *ServiceRunner) Wake(ctx context.Context) error {
	return r.EnsureRunning(ctx)
}

// Sleep requests a best-effort non-blocking transition to "sleeping" when running.
func (r *ServiceRunner) Sleep() {
	if !r.runStarted.Load() {
		return
	}
	select {
	case r.ctrlCh <- serviceControlReq{op: svcOpSleep}:
	default:
	}
}

// SleepContext transitions a running service to "sleeping" and waits for the process to exit.
func (r *ServiceRunner) SleepContext(ctx context.Context) error {
	return r.sendControl(ctx, svcOpSleep, false)
}

func (r *ServiceRunner) sleepBusyError() error {
	r.mu.RLock()
	state := r.status.State
	r.mu.RUnlock()
	if state == "starting" {
		return errors.New("service is still starting")
	}
	if r.inFlight.Load() > 0 || r.activeSSE.Load() > 0 {
		return errors.New("service has active requests or SSE streams")
	}
	if state == "stopped" {
		return ErrServiceStopped
	}
	if state == "degraded" {
		return ErrServiceDegraded
	}
	return fmt.Errorf("service cannot sleep in state %s", state)
}

// Reload invokes the service's configured reloadUrl with optional reloadTokenFile.
func (r *ServiceRunner) Reload(ctx context.Context) (json.RawMessage, error) {
	reloadURL := strings.TrimSpace(r.cfg.ReloadURL)
	if reloadURL == "" {
		return nil, ErrReloadNotSupported
	}
	r.mu.RLock()
	state := r.status.State
	r.mu.RUnlock()
	if state == "sleeping" {
		return json.RawMessage(`{"ok":true,"skipped":true,"reason":"service_sleeping"}`), nil
	}
	if state != "running" {
		return nil, fmt.Errorf("service %q is not running (state=%s)", r.cfg.Name, state)
	}

	reqCtx, cancel := context.WithTimeout(ctx, reloadRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, reloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build reloadUrl request: %w", err)
	}
	req.Close = true
	if strings.TrimSpace(r.cfg.ReloadTokenFile) != "" {
		token, err := ReadSecretFile(r.cfg.ReloadTokenFile)
		if err != nil {
			return nil, fmt.Errorf("read reloadTokenFile: %w", err)
		}
		r.redactor.AddSecrets([]string{token})
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reloadUrl POST failed: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxReloadResponseBytes))
	_ = resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read reloadUrl response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview := r.redactor.Redact(strings.TrimSpace(string(body)))
		if len(preview) > 256 {
			preview = preview[:256] + "..."
		}
		return nil, fmt.Errorf("reloadUrl returned HTTP %d: %s", resp.StatusCode, preview)
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return r.redactor.RedactJSON([]byte(trimmed))
	}
	return json.RawMessage(`{"ok":true}`), nil
}

// Launch marks the runner as started synchronously and launches Run(ctx) in a goroutine.
// Using Launch prevents a race between Wait() and goroutine scheduling on early shutdown.
func (r *ServiceRunner) Launch(ctx context.Context) {
	shouldNotify := false
	if r.cfg.EffectiveStartMode() != "lazy" {
		r.mu.Lock()
		shouldNotify = r.status.State != "starting" || r.status.Running || r.status.Healthy
		r.status.Running = false
		r.status.Healthy = false
		r.status.PID = 0
		r.status.State = "starting"
		r.mu.Unlock()
	}
	r.runStarted.Store(true)
	go func() {
		if shouldNotify {
			r.notifyStateChange()
		}
		r.Run(ctx)
	}()
}

// Wait blocks until Run has exited and all child resources are cleaned up.
// If Run was never started, Wait returns immediately.
func (r *ServiceRunner) Wait() {
	if !r.runStarted.Load() {
		return
	}
	<-r.doneCh
}

type flushable interface {
	Flush() error
}

func flushIfPossible(w io.Writer) {
	if f, ok := w.(flushable); ok {
		_ = f.Flush()
	}
}

func notifyWaiters(waiters *[]chan error, err error) {
	for _, ch := range *waiters {
		if ch != nil {
			select {
			case ch <- err:
			default:
			}
		}
	}
	*waiters = (*waiters)[:0]
}

// Run executes the service supervision loop until ctx is canceled.
func (r *ServiceRunner) Run(ctx context.Context) {
	r.runStarted.Store(true)
	defer close(r.doneCh)
	defer r.forceStop()

	svcLogPath := filepath.Join(r.logDir, r.cfg.Name+".log")
	var (
		svcLog          *RotatingFileWriter
		lastLogOpenTry  time.Time
		backoffIdx      int
		consecFailures  int
		startWaiters    []chan error
		healthyWaiters  []chan error
		restartInFlight bool
	)

	ensureSvcLog := func() *RotatingFileWriter {
		if svcLog != nil {
			return svcLog
		}
		if !lastLogOpenTry.IsZero() && time.Since(lastLogOpenTry) < logOpenRetryCooldown {
			return nil
		}
		lastLogOpenTry = time.Now()
		lw, err := NewRotatingFileWriter(svcLogPath, r.logMaxBytes)
		if err != nil {
			r.logHub("failed to open service log %q (falling back to ring log; will retry): %v", svcLogPath, err)
			return nil
		}
		svcLog = lw
		return svcLog
	}

	defer func() {
		notifyWaiters(&startWaiters, context.Canceled)
		notifyWaiters(&healthyWaiters, context.Canceled)
		if svcLog != nil {
			_ = svcLog.Close()
		}
	}()

	// Initial parking for lazy-start services (remain in "sleeping" until Wake/Start/Restart).
	nextLaunch := launchInitial
	if r.cfg.EffectiveStartMode() == "lazy" {
		r.setState("sleeping", "")
		kind, ok := r.parkLoop(ctx, "sleeping", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
		if !ok {
			return
		}
		nextLaunch = kind
	}

	for {
		if ctx.Err() != nil {
			return
		}

		lw := ensureSvcLog()
		cmd, job, flushers, err := r.startChild(ctx, lw, nextLaunch)
		if err != nil {
			r.logHub("launch failed: %v", err)
			r.writeServiceMarker(lw, fmt.Sprintf("launch failed: %v", err))
			consecFailures++
			r.setConsecFailures(consecFailures)
			restartInFlight = false

			notifyWaiters(&startWaiters, err)
			notifyWaiters(&healthyWaiters, err)

			if consecFailures >= r.maxFailures {
				r.enterDegraded(err.Error())
				r.logHub("circuit breaker tripped after %d consecutive failures; entering degraded state", consecFailures)
				kind, ok := r.parkLoop(ctx, "degraded", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
				if !ok {
					return
				}
				nextLaunch = kind
				continue
			}

			r.setError(err.Error(), "starting")
			delay := r.nextBackoff(&backoffIdx)
			r.logHub("retrying launch in %s (failure %d/%d)", delay, consecFailures, r.maxFailures)
			action, kind := r.sleepBackoff(ctx, delay, &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
			if action == backoffExit {
				return
			}
			if action == backoffParkStopped {
				parkKind, ok := r.parkLoop(ctx, "stopped", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
				if !ok {
					return
				}
				nextLaunch = parkKind
				continue
			}
			nextLaunch = kind
			continue
		}

		// Launch succeeded (PID active).
		notifyWaiters(&startWaiters, nil)
		if r.cfg.HealthURL == "" {
			restartInFlight = false
			notifyWaiters(&healthyWaiters, nil)
		}

		waitCh := make(chan error, 1)
		go func(c *exec.Cmd) {
			waitCh <- c.Wait()
		}(cmd)

		procCtx, cancelProc := context.WithCancel(ctx)
		readyCh := make(chan struct{}, 1)
		var healthWG sync.WaitGroup
		if r.cfg.HealthURL != "" {
			healthWG.Add(1)
			go func() {
				defer healthWG.Done()
				r.pollHealth(procCtx, readyCh)
			}()
		}

		var startTimer *time.Timer
		var startTimeoutC <-chan time.Time
		if r.cfg.HealthURL != "" {
			startTimer = time.NewTimer(r.StartTimeout())
			startTimeoutC = startTimer.C
		}

		var idleTicker *time.Ticker
		var idleTickC <-chan time.Time
		if r.idleSleep > 0 {
			tick := r.idleSleep / 4
			if tick > 5*time.Second {
				tick = 5 * time.Second
			}
			if tick < 25*time.Millisecond {
				tick = 25 * time.Millisecond
			}
			idleTicker = time.NewTicker(tick)
			idleTickC = idleTicker.C
		}

		stopTimers := func() {
			if startTimer != nil {
				startTimer.Stop()
			}
			if idleTicker != nil {
				idleTicker.Stop()
			}
		}

		type stopReason int
		const (
			reasonCrash stopReason = iota
			reasonManualStop
			reasonManualRestart
			reasonSleep
			reasonShutdown
		)

		var (
			reason          stopReason
			waitErr         error
			stopDone        chan error
			sleepDone       chan error
			startTimedOut   bool
			startTimeoutErr error
		)

	runningLoop:
		for {
			select {
			case <-readyCh:
				readyCh = nil
				if startTimer != nil {
					startTimer.Stop()
					startTimeoutC = nil
				}
				startTimedOut = false
				startTimeoutErr = nil
				restartInFlight = false
				notifyWaiters(&healthyWaiters, nil)

			case <-startTimeoutC:
				startTimeoutC = nil
				restartInFlight = false
				startTimedOut = true
				startTimeoutErr = fmt.Errorf("service %q did not become healthy within %s", r.cfg.Name, r.StartTimeout())
				notifyWaiters(&healthyWaiters, startTimeoutErr)

			case <-idleTickC:
				if r.trySleep(true) {
					r.logHub("idle timeout (%s) reached; putting pid=%d to sleep", r.idleSleep, cmd.Process.Pid)
					reason = reasonSleep
					break runningLoop
				}

			case waitErr = <-waitCh:
				reason = reasonCrash
				break runningLoop

			case req := <-r.ctrlCh:
				switch req.op {
				case svcOpStart:
					if r.State() == "running" {
						sendErr(req.reply, nil)
					} else {
						startWaiters = append(startWaiters, req.reply)
						// Already started (PID live), reply immediately for Start.
						notifyWaiters(&startWaiters, nil)
					}

				case svcOpWake:
					if r.State() == "running" {
						r.TouchActivity()
						sendErr(req.reply, nil)
					} else if startTimedOut {
						// M3: fail immediately after startTimeoutC fires instead of piling up waiters.
						sendErr(req.reply, startTimeoutErr)
					} else {
						healthyWaiters = append(healthyWaiters, req.reply)
					}

				case svcOpStop:
					reason = reasonManualStop
					stopDone = req.reply
					break runningLoop

				case svcOpSleep:
					if r.trySleep(false) {
						reason = reasonSleep
						sleepDone = req.reply
						break runningLoop
					}
					if req.reply != nil {
						sendErr(req.reply, r.sleepBusyError())
					}

				case svcOpRestart:
					if restartInFlight && r.State() == "starting" {
						// Coalesce concurrent restart into the in-flight launch!
						if req.waitHealthy {
							healthyWaiters = append(healthyWaiters, req.reply)
						} else {
							startWaiters = append(startWaiters, req.reply)
							notifyWaiters(&startWaiters, nil)
						}
						continue
					}
					reason = reasonManualRestart
					restartInFlight = true
					if req.waitHealthy {
						healthyWaiters = append(healthyWaiters, req.reply)
					} else {
						startWaiters = append(startWaiters, req.reply)
					}
					break runningLoop
				}

			case <-ctx.Done():
				reason = reasonShutdown
				break runningLoop
			}
		}

		expected := reason != reasonCrash
		// Invalidate admission before canceling the process epoch or waiting for health polling.
		r.setStopping()
		stopTimers()
		cancelProc()
		r.cancelEpoch()
		healthWG.Wait()

		if expected {
			r.clearActiveJob()
			r.stopProcess(cmd, job, waitCh, lw)
		} else {
			r.clearActiveJob()
			_ = job.Close()
		}

		for _, fw := range flushers {
			flushIfPossible(fw)
		}

		nextState := "stopped"
		switch reason {
		case reasonSleep:
			nextState = "sleeping"
		case reasonManualRestart:
			nextState = "starting"
		case reasonCrash:
			nextState = "starting"
		}

		healthyDur := r.markStopped(waitErr, expected, nextState)

		if reason == reasonShutdown || ctx.Err() != nil {
			r.logHub("stopped cleanly")
			sendErr(stopDone, nil)
			sendErr(sleepDone, nil)
			return
		}

		if reason == reasonManualStop {
			r.logHub("stopped manually")
			r.writeServiceMarker(lw, "stopped manually")
			restartInFlight = false
			notifyWaiters(&startWaiters, ErrServiceStopped)
			notifyWaiters(&healthyWaiters, ErrServiceStopped)
			sendErr(stopDone, nil)
			kind, ok := r.parkLoop(ctx, "stopped", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
			if !ok {
				return
			}
			nextLaunch = kind
			continue
		}

		if reason == reasonSleep {
			r.logHub("sleeping cleanly")
			r.writeServiceMarker(lw, "entered idle sleep")
			restartInFlight = false
			notifyWaiters(&startWaiters, nil)
			notifyWaiters(&healthyWaiters, nil)
			sendErr(sleepDone, nil)
			kind, ok := r.parkLoop(ctx, "sleeping", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
			if !ok {
				return
			}
			nextLaunch = kind
			continue
		}

		if reason == reasonManualRestart {
			// Drain any duplicate restarts queued during stopProcess so we coalesce instead of double-restarting,
			// while never reordering a queued Stop/Sleep behind a fresh launch.
			stopReq, sleepReq := r.coalesceQueuedRestarts(&startWaiters, &healthyWaiters)
			if stopReq != nil {
				r.setState("stopped", "")
				r.logHub("manual stop received during restart; staying stopped")
				r.writeServiceMarker(lw, "stopped manually")
				restartInFlight = false
				notifyWaiters(&startWaiters, ErrServiceStopped)
				notifyWaiters(&healthyWaiters, ErrServiceStopped)
				sendErr(stopReq.reply, nil)
				kind, ok := r.parkLoop(ctx, "stopped", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
				if !ok {
					return
				}
				nextLaunch = kind
				continue
			}
			if sleepReq != nil {
				r.setState("sleeping", "")
				r.logHub("sleep received during restart; entering sleeping state")
				r.writeServiceMarker(lw, "entered idle sleep")
				restartInFlight = false
				notifyWaiters(&startWaiters, ErrServiceSleeping)
				notifyWaiters(&healthyWaiters, ErrServiceSleeping)
				sendErr(sleepReq.reply, nil)
				kind, ok := r.parkLoop(ctx, "sleeping", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
				if !ok {
					return
				}
				nextLaunch = kind
				continue
			}
			backoffIdx = 0
			consecFailures = 0
			r.setConsecFailures(0)
			r.clearDegraded()
			r.ringLog.ClearCrashTail()
			r.logHub("manual restart requested; relaunching")
			nextLaunch = launchManualRestart
			continue
		}

		// Unexpected crash path.
		exitMsg := "unexpected exit 0"
		if waitErr != nil {
			exitMsg = waitErr.Error()
		}
		if healthyDur >= r.healthyResetAfter {
			backoffIdx = 0
			consecFailures = 0
		}
		consecFailures++
		r.setConsecFailures(consecFailures)
		restartInFlight = false

		// Fail any callers waiting for this launch to become healthy.
		notifyWaiters(&healthyWaiters, fmt.Errorf("service %q exited unexpectedly (%s)", r.cfg.Name, exitMsg))

		if consecFailures >= r.maxFailures {
			r.enterDegraded(exitMsg)
			r.logHub("process exited (%s); circuit breaker tripped (%d/%d failures), entering degraded state", exitMsg, consecFailures, r.maxFailures)
			r.writeServiceMarker(lw, fmt.Sprintf("process exited (%s); circuit breaker tripped -> degraded", exitMsg))
			kind, ok := r.parkLoop(ctx, "degraded", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
			if !ok {
				return
			}
			nextLaunch = kind
			continue
		}

		delay := r.nextBackoff(&backoffIdx)
		r.logHub("process exited (%s); restarting in %s (failure %d/%d)", exitMsg, delay, consecFailures, r.maxFailures)
		r.writeServiceMarker(lw, fmt.Sprintf("process exited (%s); restarting in %s", exitMsg, delay))

		action, kind := r.sleepBackoff(ctx, delay, &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
		if action == backoffExit {
			return
		}
		if action == backoffParkStopped {
			parkKind, ok := r.parkLoop(ctx, "stopped", &startWaiters, &healthyWaiters, &backoffIdx, &consecFailures, &restartInFlight)
			if !ok {
				return
			}
			nextLaunch = parkKind
			continue
		}
		nextLaunch = kind
	}
}

func (r *ServiceRunner) coalesceQueuedRestarts(startWaiters, healthyWaiters *[]chan error) (stopReq *serviceControlReq, sleepReq *serviceControlReq) {
	for {
		select {
		case req := <-r.ctrlCh:
			switch req.op {
			case svcOpRestart:
				if req.waitHealthy {
					*healthyWaiters = append(*healthyWaiters, req.reply)
				} else {
					*startWaiters = append(*startWaiters, req.reply)
				}
			case svcOpWake:
				*healthyWaiters = append(*healthyWaiters, req.reply)
			case svcOpStart:
				*startWaiters = append(*startWaiters, req.reply)
			case svcOpStop:
				cp := req
				return &cp, nil
			case svcOpSleep:
				cp := req
				return nil, &cp
			}
		default:
			return nil, nil
		}
	}
}

// parkLoop blocks in "stopped", "sleeping", or "degraded" until a control command requests launch or ctx ends.
// Returns the launch kind and true when the run loop should proceed to launch the child, or false on shutdown.
func (r *ServiceRunner) parkLoop(
	ctx context.Context,
	state string,
	startWaiters, healthyWaiters *[]chan error,
	backoffIdx, consecFailures *int,
	restartInFlight *bool,
) (serviceLaunchKind, bool) {
	for {
		select {
		case <-ctx.Done():
			return launchInitial, false
		case req := <-r.ctrlCh:
			switch req.op {
			case svcOpStart:
				*backoffIdx = 0
				*consecFailures = 0
				*restartInFlight = false
				r.setConsecFailures(0)
				r.clearDegraded()
				r.ringLog.ClearCrashTail()
				*startWaiters = append(*startWaiters, req.reply)
				if state == "sleeping" {
					return launchWake, true
				}
				return launchManualStart, true

			case svcOpRestart:
				*backoffIdx = 0
				*consecFailures = 0
				*restartInFlight = true
				r.setConsecFailures(0)
				r.clearDegraded()
				r.ringLog.ClearCrashTail()
				if req.waitHealthy {
					*healthyWaiters = append(*healthyWaiters, req.reply)
				} else {
					*startWaiters = append(*startWaiters, req.reply)
				}
				return launchManualRestart, true

			case svcOpWake:
				switch state {
				case "sleeping":
					*backoffIdx = 0
					*consecFailures = 0
					*restartInFlight = false
					r.setConsecFailures(0)
					*healthyWaiters = append(*healthyWaiters, req.reply)
					return launchWake, true
				case "stopped":
					sendErr(req.reply, ErrServiceStopped)
				case "degraded":
					sendErr(req.reply, ErrServiceDegraded)
				}

			case svcOpStop:
				state = "stopped"
				r.setState("stopped", "")
				sendErr(req.reply, nil)

			case svcOpSleep:
				switch state {
				case "sleeping":
					sendErr(req.reply, nil)
				case "stopped":
					sendErr(req.reply, ErrServiceStopped)
				case "degraded":
					sendErr(req.reply, ErrServiceDegraded)
				default:
					sendErr(req.reply, nil)
				}
			}
		}
	}
}

type backoffResult int

const (
	backoffContinue backoffResult = iota
	backoffParkStopped
	backoffExit
)

func (r *ServiceRunner) sleepBackoff(
	ctx context.Context,
	delay time.Duration,
	startWaiters, healthyWaiters *[]chan error,
	backoffIdx, consecFailures *int,
	restartInFlight *bool,
) (backoffResult, serviceLaunchKind) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return backoffExit, launchInitial
		case <-timer.C:
			return backoffContinue, launchCrashRecovery
		case req := <-r.ctrlCh:
			switch req.op {
			case svcOpStop:
				r.setState("stopped", "")
				notifyWaiters(startWaiters, ErrServiceStopped)
				notifyWaiters(healthyWaiters, ErrServiceStopped)
				sendErr(req.reply, nil)
				return backoffParkStopped, launchInitial

			case svcOpRestart:
				*backoffIdx = 0
				*consecFailures = 0
				*restartInFlight = true
				r.setConsecFailures(0)
				r.clearDegraded()
				r.ringLog.ClearCrashTail()
				if req.waitHealthy {
					*healthyWaiters = append(*healthyWaiters, req.reply)
				} else {
					*startWaiters = append(*startWaiters, req.reply)
				}
				return backoffContinue, launchManualRestart

			case svcOpStart:
				*backoffIdx = 0
				*consecFailures = 0
				r.setConsecFailures(0)
				r.clearDegraded()
				*startWaiters = append(*startWaiters, req.reply)
				return backoffContinue, launchManualStart

			case svcOpWake:
				*healthyWaiters = append(*healthyWaiters, req.reply)
				return backoffContinue, launchCrashRecovery

			case svcOpSleep:
				sendErr(req.reply, errors.New("service is restarting after failure"))
			}
		}
	}
}

func (r *ServiceRunner) trySleep(requireIdle bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status.State != "running" || r.inFlight.Load() > 0 || r.activeSSE.Load() > 0 {
		return false
	}
	if requireIdle {
		if r.idleSleep <= 0 || r.suspended.Load() {
			return false
		}
		timeout := r.idleSleep
		if len(r.sessions) != 0 {
			timeout = r.abandonedSleep
		}
		if timeout <= 0 || time.Since(time.Unix(0, r.lastActivity.Load())) < timeout {
			return false
		}
	}
	r.setStoppingLocked()
	return true
}

func (r *ServiceRunner) startChild(parentCtx context.Context, svcLog io.Writer, kind serviceLaunchKind) (*exec.Cmd, *ProcessJob, []io.Writer, error) {
	env, envSecrets, err := buildServiceEnvWithSecrets(os.Environ(), r.cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	r.redactor.AddSecrets(envSecrets)

	job, err := NewKillOnCloseJob()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create job object: %w", err)
	}

	ringStdoutW := r.ringLog.Writer("stdout")
	ringStderrW := r.ringLog.Writer("stderr")
	var stdoutDst, stderrDst io.Writer
	if svcLog != nil {
		safeFileW := newBestEffortWriter(svcLog, func(writeErr error) {
			r.logHub("service log write error (continuing with ring log): %v", writeErr)
		})
		stdoutDst = io.MultiWriter(safeFileW, ringStdoutW)
		stderrDst = io.MultiWriter(safeFileW, ringStderrW)
	} else {
		stdoutDst = ringStdoutW
		stderrDst = ringStderrW
	}
	stdoutW := r.redactor.NewWriter(stdoutDst)
	stderrW := r.redactor.NewWriter(stderrDst)

	cmd := exec.Command(r.cfg.Command, r.cfg.Args...)
	cmd.Env = env
	if r.cfg.Cwd != "" {
		cmd.Dir = r.cfg.Cwd
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	cmd.WaitDelay = 2 * time.Second
	configureCmd(cmd)

	if err := startCmdInJob(cmd, job); err != nil {
		_ = job.TerminateAndClose()
		return nil, nil, nil, err
	}

	now := time.Now().UTC()
	epochCtx, epochCancel := context.WithCancel(parentCtx)

	r.mu.Lock()
	switch kind {
	case launchWake:
		r.status.WakeCount++
	case launchManualRestart:
		if r.launchedOnce {
			r.status.Restarts++
		}
	case launchCrashRecovery:
		r.status.Restarts++
		r.status.CrashRestarts++
	case launchInitial, launchManualStart:
	}
	r.launchedOnce = true
	r.epoch = serviceEpochSequence.Add(1)
	r.epochCtx = epochCtx
	r.epochCancel = epochCancel
	r.activeJob = job
	r.clearSessionsLocked()
	r.status.Running = true
	r.status.PID = cmd.Process.Pid
	r.status.StartedAt = now.Format(time.RFC3339)
	r.status.LastError = ""
	r.status.LastHealthError = ""
	r.status.DegradedAt = ""
	r.maxHealthyRun = 0
	if r.cfg.HealthURL == "" {
		r.status.Healthy = true
		r.status.State = "running"
		r.healthySince = now
		r.status.HealthySince = now.Format(time.RFC3339)
	} else {
		r.status.Healthy = false
		r.status.State = "starting"
		r.healthySince = time.Time{}
		r.status.HealthySince = ""
	}
	restarts := r.status.Restarts
	epoch := r.epoch
	r.mu.Unlock()

	r.TouchActivity()
	r.notifyStateChange()
	r.logHub("started pid=%d epoch=%d restarts=%d", cmd.Process.Pid, epoch, restarts)
	r.writeServiceMarker(svcLog, fmt.Sprintf("started pid=%d epoch=%d restarts=%d", cmd.Process.Pid, epoch, restarts))
	return cmd, job, []io.Writer{stdoutW, stderrW, ringStdoutW, ringStderrW}, nil
}

func (r *ServiceRunner) clearActiveJob() {
	r.mu.Lock()
	r.activeJob = nil
	r.mu.Unlock()
	r.memMu.Lock()
	r.memCachedJob = nil
	r.memCached = JobMemoryStats{}
	r.memMu.Unlock()
}

func (r *ServiceRunner) cancelEpoch() {
	r.mu.Lock()
	cancel := r.epochCancel
	r.epochCancel = nil
	r.clearSessionsLocked()
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *ServiceRunner) stopProcess(cmd *exec.Cmd, job *ProcessJob, waitCh <-chan error, svcLog io.Writer) {
	if r.cfg.ShutdownURL != "" {
		timeout := r.shutdownTimeout
		if timeout <= 0 {
			timeout = defaultShutdownTimeout
		}
		deadline := time.Now().Add(timeout)
		if r.callShutdownURL(deadline, svcLog) {
			remaining := time.Until(deadline)
			if remaining > 0 {
				timer := time.NewTimer(remaining)
				defer timer.Stop()
				select {
				case <-waitCh:
					_ = job.Close()
					return
				case <-timer.C:
					r.logHub("graceful shutdown timed out after %s; terminating process tree", timeout)
				case <-r.forceCtx.Done():
				}
			} else {
				select {
				case <-waitCh:
					_ = job.Close()
					return
				default:
					r.logHub("graceful shutdown timed out after %s; terminating process tree", timeout)
				}
			}
		} else {
			r.logHub("graceful shutdown endpoint unsuccessful; terminating process tree immediately")
		}
	}

	_ = job.TerminateAndClose()
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}

	killTimer := time.NewTimer(3 * time.Second)
	defer killTimer.Stop()
	select {
	case <-waitCh:
	case <-killTimer.C:
		if cmd.Process != nil {
			r.logHub("warning: timed out waiting for pid=%d exit after termination", cmd.Process.Pid)
		}
	}
}

func (r *ServiceRunner) callShutdownURL(deadline time.Time, svcLog io.Writer) bool {
	reqCtx, cancel := context.WithDeadline(r.forceCtx, deadline)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.cfg.ShutdownURL, nil)
	if err != nil {
		r.logHub("invalid shutdownUrl request: %v", err)
		return false
	}
	req.Close = true
	if r.cfg.ShutdownTokenFile != "" {
		token, err := ReadSecretFile(r.cfg.ShutdownTokenFile)
		if err != nil {
			r.logHub("read shutdownTokenFile failed: %v", err)
			return false
		}
		r.redactor.AddSecrets([]string{token})
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		r.logHub("shutdownUrl POST failed: %v", err)
		r.writeServiceMarker(svcLog, fmt.Sprintf("shutdownUrl POST failed: %v", err))
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	r.logHub("shutdownUrl POST returned status=%d", resp.StatusCode)
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (r *ServiceRunner) pollHealth(ctx context.Context, readyCh chan<- struct{}) {
	pollFast := r.startingPollTick
	if pollFast <= 0 {
		pollFast = defaultStartingPollTick
	}
	fastUntil := time.Now().Add(r.StartTimeout())
	reachedReady := false
	delay := 25 * time.Millisecond
	if delay > pollFast {
		delay = pollFast
	}

	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if r.suspended.Load() {
			delay = 250 * time.Millisecond
			continue
		}
		if until := r.resumeDelayUntil.Load(); until > 0 {
			if rem := time.Until(time.Unix(0, until)); rem > 0 {
				delay = rem
				continue
			}
			r.resumeDelayUntil.Store(0)
		}

		probeErr := r.probeHealthDetail(ctx)
		if ctx.Err() != nil {
			return
		}
		ok := probeErr == nil
		r.setHealthy(ok, probeErr)

		if ok {
			if !reachedReady {
				reachedReady = true
				r.TouchActivity()
				r.ringLog.ClearCrashTail()
				select {
				case readyCh <- struct{}{}:
				default:
				}
			}
			delay = r.healthInterval
		} else if !reachedReady && time.Now().Before(fastUntil) {
			delay = pollFast
		} else {
			delay = r.healthInterval
		}
	}
}

func (r *ServiceRunner) probeHealthOnce(ctx context.Context) bool {
	return r.probeHealthDetail(ctx) == nil
}

func (r *ServiceRunner) probeHealthDetail(ctx context.Context) error {
	if strings.HasPrefix(strings.ToLower(r.cfg.HealthURL), "tcp://") {
		u, err := url.Parse(r.cfg.HealthURL)
		if err != nil || u.Host == "" {
			if err != nil {
				return err
			}
			return errors.New("tcp healthUrl missing host")
		}
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", u.Host)
		if err != nil {
			return err
		}
		_ = conn.Close()
		return nil
	}

	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, r.cfg.HealthURL, nil)
	if err != nil {
		return err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (r *ServiceRunner) setHealthy(ok bool, probeErr error) {
	now := time.Now().UTC()
	changed := false
	r.mu.Lock()
	if !r.status.Running {
		r.mu.Unlock()
		return
	}
	r.status.LastHealthCheckAt = now.Format(time.RFC3339)
	if ok {
		r.status.LastHealthError = ""
		if !r.status.Healthy || r.status.State != "running" {
			r.status.Healthy = true
			r.status.State = "running"
			r.healthySince = now
			r.status.HealthySince = now.Format(time.RFC3339)
			changed = true
		}
	} else {
		if probeErr != nil {
			r.status.LastHealthError = r.redactor.Redact(probeErr.Error())
		} else {
			r.status.LastHealthError = "health check failed"
		}
		if r.status.Healthy {
			if !r.healthySince.IsZero() {
				if dur := now.Sub(r.healthySince); dur > r.maxHealthyRun {
					r.maxHealthyRun = dur
				}
			}
			r.status.Healthy = false
			r.healthySince = time.Time{}
			r.status.HealthySince = ""
			changed = true
		}
	}
	r.mu.Unlock()
	if changed {
		r.notifyStateChange()
	}
}

func (r *ServiceRunner) markStopped(waitErr error, expected bool, nextState string) time.Duration {
	now := time.Now().UTC()
	if !expected && r.ringLog != nil {
		r.ringLog.FreezeCrashTail(10)
	}
	r.mu.Lock()
	healthyDur := r.maxHealthyRun
	if r.status.Healthy && !r.healthySince.IsZero() {
		if dur := now.Sub(r.healthySince); dur > healthyDur {
			healthyDur = dur
		}
	}

	r.status.Running = false
	r.status.Healthy = false
	r.status.PID = 0
	r.status.State = nextState
	r.healthySince = time.Time{}
	r.status.HealthySince = ""
	r.maxHealthyRun = 0

	if !expected {
		if waitErr != nil {
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				r.status.LastExitCode = exitErr.ExitCode()
			} else {
				r.status.LastExitCode = -1
			}
			r.status.LastError = r.redactor.Redact(waitErr.Error())
		} else {
			r.status.LastExitCode = 0
			r.status.LastError = "unexpected exit 0"
		}
	}
	r.mu.Unlock()
	r.notifyStateChange()
	return healthyDur
}

func (r *ServiceRunner) setStopping() {
	r.mu.Lock()
	r.setStoppingLocked()
	r.mu.Unlock()
	r.notifyStateChange()
}

func (r *ServiceRunner) setStoppingLocked() {
	now := time.Now().UTC()
	if r.status.Healthy && !r.healthySince.IsZero() {
		if dur := now.Sub(r.healthySince); dur > r.maxHealthyRun {
			r.maxHealthyRun = dur
		}
	}
	r.status.Running = false
	r.status.Healthy = false
	r.status.State = "stopping"
	r.healthySince = time.Time{}
	r.status.HealthySince = ""
}

func (r *ServiceRunner) setState(state, lastErr string) {
	r.mu.Lock()
	changed := r.status.State != state || r.status.Running || r.status.Healthy
	r.status.Running = false
	r.status.Healthy = false
	r.status.PID = 0
	r.status.State = state
	if lastErr != "" {
		r.status.LastError = r.redactor.Redact(lastErr)
	}
	r.mu.Unlock()
	if changed {
		r.notifyStateChange()
	}
}

func (r *ServiceRunner) setError(msg, state string) {
	r.mu.Lock()
	r.status.Running = false
	r.status.Healthy = false
	r.status.PID = 0
	r.status.State = state
	r.status.LastError = r.redactor.Redact(msg)
	r.mu.Unlock()
	r.notifyStateChange()
}

func (r *ServiceRunner) setConsecFailures(n int) {
	r.mu.Lock()
	r.status.ConsecutiveFailures = n
	r.mu.Unlock()
}

func (r *ServiceRunner) enterDegraded(msg string) {
	now := time.Now().UTC().Format(time.RFC3339)
	r.mu.Lock()
	r.status.Running = false
	r.status.Healthy = false
	r.status.PID = 0
	r.status.State = "degraded"
	r.status.DegradedAt = now
	if msg != "" {
		r.status.LastError = r.redactor.Redact(msg)
	}
	r.mu.Unlock()
	r.notifyStateChange()
}

func (r *ServiceRunner) clearDegraded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.DegradedAt = ""
	r.status.LastError = ""
}

func (r *ServiceRunner) nextBackoff(idx *int) time.Duration {
	if len(r.backoff) == 0 {
		return time.Second
	}
	i := *idx
	if i >= len(r.backoff) {
		i = len(r.backoff) - 1
	} else {
		*idx = i + 1
	}
	return r.backoff[i]
}

func (r *ServiceRunner) logHub(format string, args ...any) {
	if r.hubLogger != nil {
		msg := fmt.Sprintf("[svc:%s] "+format, append([]any{r.cfg.Name}, args...)...)
		r.hubLogger.Print(r.redactor.Redact(msg))
	}
}

func (r *ServiceRunner) writeServiceMarker(w io.Writer, msg string) {
	redacted := r.redactor.Redact(msg)
	if w != nil {
		_, _ = fmt.Fprintf(w, "[mcp-hub %s] %s\n", time.Now().UTC().Format(time.RFC3339), redacted)
	}
}

func sendErr(ch chan error, err error) {
	if ch == nil {
		return
	}
	select {
	case ch <- err:
	default:
	}
}
