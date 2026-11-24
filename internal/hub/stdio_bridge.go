package hub

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultBridgeIdleTimeout is the recommended idle auto-sleep timeout (10 minutes).
	// Passing IdleTimeout == 0 to NewStdioBridge disables idle auto-sleep.
	DefaultBridgeIdleTimeout    = 600 * time.Second
	DefaultBridgeRequestTimeout = 120 * time.Second
	DefaultBridgeInitTimeout    = 20 * time.Second
	DefaultBridgeMaxConcurrent  = 32
	DefaultBridgeMaxLineBytes   = 32 << 20 // 32 MiB

	bridgeProtocolVersion     = "2025-03-26"
	bridgeGracefulStopWait    = 5 * time.Second
	bridgeMaxIdleCheckTick    = 15 * time.Second
	bridgeMinIdleCheckTick    = 25 * time.Millisecond
	bridgeOversizedPrefixSz   = 512
	defaultBridgeMaxFailures  = 5
	defaultBridgeHealthyReset = 60 * time.Second
	bridgeCancelledWriteWait  = time.Second
)

var defaultBridgeBackoffSchedule = []time.Duration{
	200 * time.Millisecond,
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
	5 * time.Second,
}

var (
	// ErrBridgeStopped indicates the bridge was manually stopped or shut down.
	ErrBridgeStopped = errors.New("stdio bridge is stopped")
	// ErrBridgeDegraded indicates the bridge crash-loop circuit breaker is open.
	ErrBridgeDegraded        = errors.New("stdio bridge is degraded (crash loop circuit breaker open)")
	ErrBridgeSleeping        = errors.New("stdio bridge is sleeping")
	errBridgeClientCancelled = errors.New("client cancelled request")
)

// StdioBridgeOptions configures a lazy-started, idle-sleeping STDIO-to-Streamable-HTTP MCP bridge.
type StdioBridgeOptions struct {
	Name                   string
	Command                string
	Args                   []string
	Cwd                    string
	Env                    map[string]string
	EnvFiles               map[string]string
	StripProxyEnv          *bool
	IdleTimeout            time.Duration   // 0 disables idle auto-sleep; use DefaultBridgeIdleTimeout (600s) for default
	RequestTimeout         time.Duration   // default 120s
	InitTimeout            time.Duration   // default 20s
	MaxConcurrent          int             // default 32
	MaxLineBytes           int             // default 32 MiB
	MaxConsecutiveFailures int             // default 5
	RetryBackoff           []time.Duration // optional override for crash/init retry backoff
	HealthyResetAfter      time.Duration   // default 60s
	StderrWriter           io.Writer       // optional
	Redactor               *Redactor       // optional
	Logger                 *log.Logger     // optional
	OnCrash                func()          // optional: invoked on abnormal child exit after stderr flush
	OnStateChange          func()          // optional: invoked outside internal locks on state transitions
}

type pendingResult struct {
	raw []byte
	err error
}

type bridgeSessionScope struct {
	route string
	token [32]byte
}

type bridgeSessionContextKey struct{}

type bridgeSession struct {
	scope    bridgeSessionScope
	lastSeen time.Time // guarded by StdioBridge.mu
	mu       sync.Mutex
	closed   bool
	pending  map[string]context.CancelCauseFunc
}

func (s *bridgeSession) register(id string, cancel context.CancelCauseFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.pending[id] != nil {
		return false
	}
	s.pending[id] = cancel
	return true
}

func (s *bridgeSession) finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

func (s *bridgeSession) cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel := s.pending[id]; cancel != nil {
		cancel(errBridgeClientCancelled)
	}
}

func (s *bridgeSession) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for _, cancel := range s.pending {
		cancel(errBridgeClientCancelled)
	}
}

type bridgeProcess struct {
	bridge *StdioBridge
	cmd    *exec.Cmd
	job    *ProcessJob
	stdin  io.WriteCloser
	stdout io.ReadCloser
	pid    int
	epoch  uint64

	nextID atomic.Uint64

	writeMu     sync.Mutex
	stdinClosed atomic.Bool

	pendingMu sync.Mutex
	pending   map[uint64]chan pendingResult
	closed    bool

	expectedExit atomic.Bool
	startedAt    time.Time

	procCtx    context.Context
	procCancel context.CancelFunc
	stdoutDone chan struct{}
	waitDone   chan struct{}
	waitErr    error
	exited     bool // guarded by bridge.mu
}

type bridgeStopKind int

const (
	bridgeStopInitial bridgeStopKind = iota
	bridgeStopIdleSleep
	bridgeStopManualStop
	bridgeStopManualRestart
	bridgeStopCrash
)

// StdioBridge exposes one shared STDIO MCP process through Streamable HTTP.
// A client session scopes its request IDs and cancellation without keeping the child awake.
type StdioBridge struct {
	opts    StdioBridgeOptions
	metrics *MCPMetrics
	sem     chan struct{}

	// OnStateChange is invoked outside internal locks when bridge lifecycle state changes.
	OnStateChange func()

	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	dyingWG        sync.WaitGroup

	mu               sync.Mutex
	state            string // stopped|sleeping|starting|running|degraded
	manualStopped    bool
	shutdown         bool
	suspended        bool
	launchedOnce     bool
	lastStop         bridgeStopKind
	epoch            uint64
	activeRequests   int
	lastActivity     time.Time
	proc             *bridgeProcess
	dyingProcs       []*bridgeProcess
	startingCh       chan struct{}
	startCancel      context.CancelFunc
	lastStartErr     error
	consecFailures   int
	nextRetryAt      time.Time
	cachedInitResult json.RawMessage
	toolReadOnly     map[string]*bool
	catalogRevision  uint64
	sessions         map[string]*bridgeSession
	status           ServiceStatus
}

// NewStdioBridge initializes a StdioBridge in the "sleeping" state (lazy cold start on first request).
func NewStdioBridge(opts StdioBridgeOptions) *StdioBridge {
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultBridgeRequestTimeout
	}
	if opts.InitTimeout <= 0 {
		opts.InitTimeout = DefaultBridgeInitTimeout
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = DefaultBridgeMaxConcurrent
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultBridgeMaxLineBytes
	}
	if opts.IdleTimeout < 0 {
		opts.IdleTimeout = 0
	}
	if opts.MaxConsecutiveFailures <= 0 {
		opts.MaxConsecutiveFailures = defaultBridgeMaxFailures
	}
	if opts.HealthyResetAfter <= 0 {
		opts.HealthyResetAfter = defaultBridgeHealthyReset
	}
	if len(opts.RetryBackoff) == 0 {
		opts.RetryBackoff = append([]time.Duration(nil), defaultBridgeBackoffSchedule...)
	} else {
		opts.RetryBackoff = append([]time.Duration(nil), opts.RetryBackoff...)
	}

	// Defensive copies of slices/maps.
	opts.Args = append([]string(nil), opts.Args...)
	if opts.Env != nil {
		envCopy := make(map[string]string, len(opts.Env))
		for k, v := range opts.Env {
			envCopy[k] = v
		}
		opts.Env = envCopy
	}
	if opts.EnvFiles != nil {
		efCopy := make(map[string]string, len(opts.EnvFiles))
		for k, v := range opts.EnvFiles {
			efCopy[k] = v
		}
		opts.EnvFiles = efCopy
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &StdioBridge{
		opts:           opts,
		metrics:        NewMCPMetrics(),
		sem:            make(chan struct{}, opts.MaxConcurrent),
		OnStateChange:  opts.OnStateChange,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		state:          "sleeping",
		sessions:       make(map[string]*bridgeSession),
		status: ServiceStatus{
			Name: opts.Name,
		},
	}
}

// SetOnStateChange registers a callback invoked on bridge lifecycle state transitions.
func (b *StdioBridge) SetOnStateChange(fn func()) {
	b.mu.Lock()
	b.OnStateChange = fn
	b.mu.Unlock()
}

func (b *StdioBridge) notifyStateChange() {
	b.mu.Lock()
	fn := b.OnStateChange
	b.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (b *StdioBridge) serviceConfig() ServiceConfig {
	return ServiceConfig{
		Name:          b.opts.Name,
		Command:       b.opts.Command,
		Args:          b.opts.Args,
		Cwd:           b.opts.Cwd,
		Env:           b.opts.Env,
		EnvFiles:      b.opts.EnvFiles,
		StripProxyEnv: b.opts.StripProxyEnv,
	}
}

func (b *StdioBridge) logf(format string, args ...any) {
	if b.opts.Logger != nil {
		msg := fmt.Sprintf("[bridge:%s] "+format, append([]any{b.opts.Name}, args...)...)
		b.opts.Logger.Print(b.opts.Redactor.Redact(msg))
	}
}

func (b *StdioBridge) redactError(err error) string {
	if err == nil {
		return ""
	}
	return b.opts.Redactor.Redact(err.Error())
}

func (b *StdioBridge) backoffDuration(failures int) time.Duration {
	if len(b.opts.RetryBackoff) == 0 {
		return 0
	}
	idx := failures - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(b.opts.RetryBackoff) {
		idx = len(b.opts.RetryBackoff) - 1
	}
	return b.opts.RetryBackoff[idx]
}

// State returns the current bridge lifecycle state: stopped, sleeping, starting, running, or degraded.
func (b *StdioBridge) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// ActiveJob returns the active child's Windows Job Object, or nil if no child is running.
func (b *StdioBridge) ActiveJob() *ProcessJob {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc == nil {
		return nil
	}
	return b.proc.job
}

// Status returns a consistent point-in-time snapshot of the bridge's ServiceStatus.
func (b *StdioBridge) Status() ServiceStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.status
	st.Mode = "stdio-bridge"
	st.State = b.state
	st.ConsecutiveFailures = b.consecFailures
	st.MaxConsecutiveFailures = b.opts.MaxConsecutiveFailures
	st.IdleSleepSec = int(b.opts.IdleTimeout / time.Second)
	st.ActiveInFlight = int64(b.activeRequests)
	st.MetricsSource = "bridge"
	if !b.lastActivity.IsZero() {
		st.LastActivityAt = b.lastActivity.UTC().Format(time.RFC3339)
	}
	return st
}

// HealthSummary returns the compact /healthz summary for this bridge under a single lock.
func (b *StdioBridge) HealthSummary() ServiceHealthSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	return ServiceHealthSummary{
		Running:   b.status.Running,
		Healthy:   b.status.Healthy,
		Restarts:  b.status.Restarts,
		PID:       b.status.PID,
		Port:      b.status.Port,
		State:     b.state,
		Mode:      "stdio-bridge",
		LastError: b.status.LastError,
	}
}

// Metrics returns an immutable snapshot of this bridge's MCP telemetry.
func (b *StdioBridge) Metrics() MCPMetricsSnapshot {
	return b.metrics.Snapshot()
}

// MCPMetrics returns the underlying *MCPMetrics collector.
func (b *StdioBridge) MCPMetrics() *MCPMetrics {
	return b.metrics
}

// Suspend pauses the idle auto-sleep timer (e.g. during OS power suspend).
func (b *StdioBridge) Suspend() {
	b.mu.Lock()
	b.suspended = true
	b.mu.Unlock()
}

// Resume re-enables the idle auto-sleep timer and refreshes lastActivity.
func (b *StdioBridge) Resume() {
	b.mu.Lock()
	b.suspended = false
	b.lastActivity = time.Now()
	b.mu.Unlock()
}

// Start clears any manual stop or degraded state and eagerly starts and initializes the child process.
func (b *StdioBridge) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.shutdown {
		b.mu.Unlock()
		return ErrBridgeStopped
	}
	b.manualStopped = false
	b.consecFailures = 0
	b.nextRetryAt = time.Time{}
	b.status.DegradedAt = ""
	b.status.LastError = ""
	if b.state == "stopped" || b.state == "degraded" {
		b.state = "sleeping"
	}
	b.mu.Unlock()
	_, err := b.ensureRunningProc(ctx, false, false)
	return err
}

// Wake lazily wakes a sleeping bridge (failing closed if stopped or degraded).
func (b *StdioBridge) Wake(ctx context.Context) error {
	return b.EnsureRunning(ctx)
}

// Sleep gracefully puts a running bridge to sleep (idle-capable) if no requests are active.
func (b *StdioBridge) Sleep(ctx context.Context) error {
	b.mu.Lock()
	if b.shutdown || b.manualStopped {
		b.mu.Unlock()
		return ErrBridgeStopped
	}
	if b.state == "degraded" {
		b.mu.Unlock()
		return ErrBridgeDegraded
	}
	if b.state == "sleeping" {
		b.mu.Unlock()
		return nil
	}
	if b.activeRequests > 0 {
		b.mu.Unlock()
		return errors.New("stdio bridge has active requests")
	}
	startCancel := b.startCancel
	startCh := b.startingCh
	proc := b.detachProcLocked("sleeping", true)
	b.lastStop = bridgeStopIdleSleep
	b.consecFailures = 0
	b.nextRetryAt = time.Time{}
	b.retireProcLocked(proc, false)
	b.mu.Unlock()

	b.notifyStateChange()
	if startCancel != nil {
		startCancel()
	}
	if startCh != nil {
		select {
		case <-startCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return waitForBridgeExit(ctx, proc)
}

func waitForBridgeExit(ctx context.Context, proc *bridgeProcess) error {
	if proc == nil {
		return nil
	}
	select {
	case <-proc.waitDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop terminates any running child and marks the bridge manually stopped (returning 503 to POST requests).
func (b *StdioBridge) Stop(ctx context.Context) error {
	b.mu.Lock()
	b.manualStopped = true
	b.lastStop = bridgeStopManualStop
	startCancel := b.startCancel
	startCh := b.startingCh
	proc := b.detachProcLocked("stopped", true)
	b.retireProcLocked(proc, false)
	b.mu.Unlock()

	b.notifyStateChange()
	if startCancel != nil {
		startCancel()
	}
	if startCh != nil {
		select {
		case <-startCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return waitForBridgeExit(ctx, proc)
}

// Restart stops any running child, clears cached initialization/degraded state, and starts a fresh child.
func (b *StdioBridge) Restart(ctx context.Context) error {
	b.mu.Lock()
	if b.shutdown {
		b.mu.Unlock()
		return ErrBridgeStopped
	}
	startCancel := b.startCancel
	startCh := b.startingCh
	proc := b.detachProcLocked("sleeping", true)
	if b.launchedOnce {
		b.lastStop = bridgeStopManualRestart
	}
	b.retireProcLocked(proc, false)
	b.manualStopped = false
	b.consecFailures = 0
	b.nextRetryAt = time.Time{}
	b.status.DegradedAt = ""
	b.status.LastError = ""
	b.mu.Unlock()

	b.notifyStateChange()
	if startCancel != nil {
		startCancel()
	}
	if startCh != nil {
		select {
		case <-startCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := waitForBridgeExit(ctx, proc); err != nil {
		return err
	}
	_, err := b.ensureRunningProc(ctx, false, false)
	return err
}

// Shutdown permanently stops the bridge and terminates any running or draining child process trees.
func (b *StdioBridge) Shutdown() {
	b.mu.Lock()
	if b.shutdown {
		b.mu.Unlock()
		b.dyingWG.Wait()
		return
	}
	b.shutdown = true
	b.manualStopped = true
	b.lastStop = bridgeStopManualStop
	startCancel := b.startCancel
	startCh := b.startingCh
	proc := b.detachProcLocked("stopped", true)
	b.retireProcLocked(proc, false)
	b.mu.Unlock()

	b.notifyStateChange()
	b.shutdownCancel()
	if startCancel != nil {
		startCancel()
	}
	if startCh != nil {
		<-startCh
	}
	b.dyingWG.Wait()
}

// detachProcLocked removes b.proc under b.mu, updates status/state, and marks expectedExit if requested.
func (b *StdioBridge) detachProcLocked(nextState string, expected bool) *bridgeProcess {
	proc := b.proc
	b.proc = nil
	b.state = nextState
	b.status.Running = false
	b.status.Healthy = false
	b.status.PID = 0
	b.status.HealthySince = ""
	b.cachedInitResult = nil
	b.invalidateToolCatalogLocked()
	b.metrics.ClearSessions()
	if proc != nil {
		if expected {
			proc.expectedExit.Store(true)
		}
		proc.procCancel()
	}
	return proc
}

func (b *StdioBridge) invalidateToolCatalogLocked() {
	b.toolReadOnly = nil
	b.catalogRevision++
}

// retireProcLocked records the old process before another caller can start a replacement.
// waitProcessLoop removes it only when its process tree and output have finished.
func (b *StdioBridge) retireProcLocked(proc *bridgeProcess, force bool) {
	if proc == nil {
		return
	}
	b.dyingProcs = append(b.dyingProcs, proc)
	b.dyingWG.Add(1)
	go func() {
		defer b.dyingWG.Done()
		// Use a bounded graceful context independent of b.shutdownCtx so Shutdown() gives
		// children the full stdin-EOF graceful window before hard termination.
		var (
			ctx    context.Context
			cancel context.CancelFunc
		)
		if force {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		} else {
			ctx, cancel = context.WithTimeout(context.Background(), bridgeGracefulStopWait)
			defer cancel()
		}
		b.gracefulStopProc(ctx, proc)
	}()
}

// EnsureRunning lazily cold-starts and initializes the child process using singleflight coordination.
func (b *StdioBridge) EnsureRunning(ctx context.Context) error {
	_, err := b.ensureRunningProc(ctx, false, true)
	return err
}

func (b *StdioBridge) ensureRunningProc(ctx context.Context, noWake bool, wakeTrigger bool) (*bridgeProcess, error) {
	for {
		b.mu.Lock()
		if err := ctx.Err(); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		if b.shutdown || b.manualStopped {
			b.mu.Unlock()
			return nil, ErrBridgeStopped
		}
		if b.state == "degraded" {
			b.mu.Unlock()
			return nil, ErrBridgeDegraded
		}
		if b.state == "running" && b.proc != nil && !b.proc.exited {
			proc := b.proc
			b.mu.Unlock()
			return proc, nil
		}
		if noWake {
			b.mu.Unlock()
			return nil, ErrBridgeSleeping
		}
		if b.startingCh != nil {
			ch := b.startingCh
			b.mu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			b.mu.Lock()
			if b.shutdown || b.manualStopped {
				b.mu.Unlock()
				return nil, ErrBridgeStopped
			}
			if b.state == "running" && b.proc != nil && !b.proc.exited {
				proc := b.proc
				b.mu.Unlock()
				return proc, nil
			}
			if b.state == "degraded" {
				b.mu.Unlock()
				return nil, ErrBridgeDegraded
			}
			err := b.lastStartErr
			b.mu.Unlock()
			if err != nil {
				return nil, err
			}
			// Another caller restarted or transitioned; loop once to re-check.
			continue
		}

		// Become the singleflight starter.
		startCh := make(chan struct{})
		startCtx, startCancel := context.WithCancel(b.shutdownCtx)
		retryWait := time.Until(b.nextRetryAt)
		b.startingCh = startCh
		b.startCancel = startCancel
		b.lastStartErr = nil
		b.state = "starting"
		b.mu.Unlock()
		b.notifyStateChange()

		// Run cold start asynchronously from caller ctx so one caller disconnecting
		// does not abort startup for other concurrent callers waiting on startCh.
		go func() {
			defer startCancel()

			// Wait for any idle-sleeping or wedged dying child to exit before spawning
			// a replacement so exclusive DB/session locks never overlap (M5).
			if err := b.waitForDyingProcs(startCtx); err != nil {
				b.finishFailedStart(startCh, err, false)
				return
			}

			// Honor crash/init backoff schedule before relaunching (M4).
			if retryWait > 0 {
				t := time.NewTimer(retryWait)
				select {
				case <-t.C:
				case <-startCtx.Done():
					t.Stop()
					b.finishFailedStart(startCh, startCtx.Err(), false)
					return
				}
			}

			proc, initRes, err := b.launchAndInit(startCtx)
			if err != nil {
				b.finishFailedStart(startCh, err, true)
				return
			}

			b.mu.Lock()
			if b.shutdown || b.manualStopped || startCtx.Err() != nil {
				// Stopped while starting; discard the freshly launched child cleanly.
				proc.expectedExit.Store(true)
				proc.procCancel()
				if !proc.exited {
					b.retireProcLocked(proc, false)
				}
				b.mu.Unlock()
				b.finishFailedStart(startCh, context.Canceled, false)
				return
			}
			if proc.exited {
				b.mu.Unlock()
				b.finishFailedStart(startCh, errors.New("child exited while starting"), true)
				return
			}

			now := time.Now().UTC()
			switch b.lastStop {
			case bridgeStopCrash:
				b.status.Restarts++
				b.status.CrashRestarts++
			case bridgeStopManualRestart:
				b.status.Restarts++
			case bridgeStopIdleSleep:
				b.status.WakeCount++
			case bridgeStopInitial:
				if wakeTrigger {
					b.status.WakeCount++
				}
			case bridgeStopManualStop:
			}
			b.launchedOnce = true
			b.lastStop = bridgeStopInitial
			restarts := b.status.Restarts
			b.proc = proc
			b.cachedInitResult = initRes
			b.state = "running"
			b.lastActivity = time.Now()
			b.nextRetryAt = time.Time{}
			b.status.Running = true
			b.status.Healthy = true
			b.status.PID = proc.pid
			b.status.StartedAt = now.Format(time.RFC3339)
			b.status.HealthySince = now.Format(time.RFC3339)
			b.status.LastError = ""
			b.status.DegradedAt = ""
			b.startingCh = nil
			b.startCancel = nil
			close(startCh)
			b.mu.Unlock()

			b.notifyStateChange()
			b.logf("started pid=%d epoch=%d restarts=%d", proc.pid, proc.epoch, restarts)
			if b.opts.IdleTimeout > 0 {
				go b.idleReaperLoop(proc)
			}
		}()

		// Wait for our singleflight start to finish or caller ctx to cancel.
		select {
		case <-startCh:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		b.mu.Lock()
		if b.shutdown || b.manualStopped {
			b.mu.Unlock()
			return nil, ErrBridgeStopped
		}
		if b.state == "running" && b.proc != nil && !b.proc.exited {
			proc := b.proc
			b.mu.Unlock()
			return proc, nil
		}
		if b.state == "degraded" {
			b.mu.Unlock()
			return nil, ErrBridgeDegraded
		}
		err := b.lastStartErr
		b.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return nil, errors.New("stdio bridge failed to reach running state")
	}
}

func (b *StdioBridge) finishFailedStart(startCh chan struct{}, err error, countFailure bool) {
	b.mu.Lock()
	b.status.Running = false
	b.status.Healthy = false
	b.status.PID = 0
	b.status.HealthySince = ""
	b.status.LastError = b.redactError(err)
	if b.shutdown || b.manualStopped {
		b.lastStartErr = ErrBridgeStopped
		b.state = "stopped"
	} else if countFailure {
		b.lastStop = bridgeStopCrash
		b.consecFailures++
		if b.consecFailures >= b.opts.MaxConsecutiveFailures {
			b.state = "degraded"
			b.status.DegradedAt = time.Now().UTC().Format(time.RFC3339)
			b.lastStartErr = ErrBridgeDegraded
			b.logf("launch/init failed (%d/%d consecutive failures); circuit breaker tripped -> degraded: %v", b.consecFailures, b.opts.MaxConsecutiveFailures, err)
		} else {
			delay := b.backoffDuration(b.consecFailures)
			b.nextRetryAt = time.Now().Add(delay)
			b.state = "sleeping"
			b.lastStartErr = err
		}
	} else {
		b.lastStartErr = err
		if b.state == "starting" {
			b.state = "sleeping"
		}
	}
	b.startingCh = nil
	b.startCancel = nil
	close(startCh)
	b.mu.Unlock()
	b.notifyStateChange()
}

func (b *StdioBridge) removeDyingProcLocked(proc *bridgeProcess) {
	for i, p := range b.dyingProcs {
		if p == proc {
			b.dyingProcs = append(b.dyingProcs[:i], b.dyingProcs[i+1:]...)
			break
		}
	}
}

func (b *StdioBridge) waitForDyingProcs(ctx context.Context) error {
	b.mu.Lock()
	dying := append([]*bridgeProcess(nil), b.dyingProcs...)
	b.mu.Unlock()
	if len(dying) == 0 {
		return nil
	}
	timer := time.NewTimer(bridgeGracefulStopWait)
	defer timer.Stop()
	for _, p := range dying {
		select {
		case <-p.waitDone:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("previous child pid=%d has not exited", p.pid)
		}
	}
	return nil
}

func (b *StdioBridge) launchAndInit(startCtx context.Context) (*bridgeProcess, json.RawMessage, error) {
	if strings.TrimSpace(b.opts.Command) == "" {
		return nil, nil, errors.New("bridge command is empty")
	}
	env, envSecrets, err := buildServiceEnvWithSecrets(os.Environ(), b.serviceConfig())
	if err != nil {
		return nil, nil, err
	}
	b.opts.Redactor.AddSecrets(envSecrets)

	job, err := NewKillOnCloseJob()
	if err != nil {
		return nil, nil, fmt.Errorf("create job object: %w", err)
	}

	cmd := exec.Command(b.opts.Command, b.opts.Args...)
	cmd.Env = env
	if b.opts.Cwd != "" {
		cmd.Dir = b.opts.Cwd
	}
	cmd.WaitDelay = 2 * time.Second
	if b.opts.StderrWriter != nil {
		cmd.Stderr = b.opts.StderrWriter
	} else {
		cmd.Stderr = io.Discard
	}
	configureCmd(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = job.Close()
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, childStdout, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		_ = job.Close()
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stdout = childStdout

	if err := startCmdInJob(cmd, job); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = childStdout.Close()
		_ = job.TerminateAndClose()
		return nil, nil, err
	}
	_ = childStdout.Close()

	b.mu.Lock()
	b.epoch++
	epoch := b.epoch
	b.mu.Unlock()

	procCtx, procCancel := context.WithCancel(b.shutdownCtx)
	proc := &bridgeProcess{
		bridge:     b,
		cmd:        cmd,
		job:        job,
		stdin:      stdin,
		stdout:     stdout,
		pid:        cmd.Process.Pid,
		epoch:      epoch,
		pending:    make(map[uint64]chan pendingResult),
		startedAt:  time.Now(),
		procCtx:    procCtx,
		procCancel: procCancel,
		stdoutDone: make(chan struct{}),
		waitDone:   make(chan struct{}),
	}

	go b.readStdoutLoop(proc)
	go b.waitProcessLoop(proc)

	// Perform single upstream MCP initialize handshake + notifications/initialized.
	initCtx, initCancel := context.WithTimeout(startCtx, b.opts.InitTimeout)
	defer initCancel()

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      0, // rewritten by proc.call
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": bridgeProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "mcp-hub-bridge",
				"version": Version,
			},
		},
	}
	initReqBytes, err := marshalCompactJSON(initReq)
	if err != nil {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("marshal initialize request: %w", err)
	}

	initRespBytes, err := proc.call(initCtx, initReqBytes, json.RawMessage("1"), "init timeout")
	if err != nil {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("mcp initialize failed: %w", err)
	}

	var initResp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(initRespBytes, &initResp); err != nil {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("decode initialize response: %w", err)
	}
	errTrim := bytes.TrimSpace(initResp.Error)
	if len(errTrim) > 0 && !bytes.Equal(errTrim, []byte("null")) {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("mcp initialize returned error: %s", string(errTrim))
	}
	resTrim := bytes.TrimSpace(initResp.Result)
	if len(resTrim) == 0 || bytes.Equal(resTrim, []byte("null")) {
		b.abortInitProc(proc)
		return nil, nil, errors.New("mcp initialize returned empty result")
	}
	var negotiated struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(resTrim, &negotiated); err != nil || !isValidMCPProtocolVersion(negotiated.ProtocolVersion) {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("mcp child negotiated unsupported protocolVersion %q (expected YYYY-MM-DD)", negotiated.ProtocolVersion)
	}

	initialized := make(chan error, 1)
	go func() {
		initialized <- proc.writeLine([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	}()
	select {
	case err = <-initialized:
	case <-initCtx.Done():
		err = initCtx.Err()
	}
	if err != nil {
		b.abortInitProc(proc)
		return nil, nil, fmt.Errorf("send notifications/initialized: %w", err)
	}

	return proc, cloneRawMessage(resTrim), nil
}

func isValidMCPProtocolVersion(v string) bool {
	if len(v) != 10 {
		return false
	}
	_, err := time.Parse("2006-01-02", v)
	return err == nil
}

func (b *StdioBridge) abortInitProc(proc *bridgeProcess) {
	proc.expectedExit.Store(true)
	proc.procCancel()
	proc.failAllPending(errors.New("initialization aborted"))
	b.mu.Lock()
	if !proc.exited {
		b.retireProcLocked(proc, true)
	}
	b.mu.Unlock()
	stopCtx, cancel := context.WithTimeout(context.Background(), bridgeGracefulStopWait)
	defer cancel()
	_ = waitForBridgeExit(stopCtx, proc)
}

func (b *StdioBridge) readStdoutLoop(proc *bridgeProcess) {
	defer close(proc.stdoutDone)
	defer proc.stdout.Close()
	br := bufio.NewReaderSize(proc.stdout, 64*1024)

	for {
		line, exceeded, prefix, err := readBoundedLine(br, b.opts.MaxLineBytes)
		if exceeded {
			b.logf("stdout line exceeded maxLineBytes (%d bytes); dropping frame", b.opts.MaxLineBytes)
			if id, ok := extractBridgeIDFromPrefix(prefix); ok {
				proc.deliverError(id, fmt.Errorf("upstream response exceeded maxLineBytes (%d)", b.opts.MaxLineBytes))
			}
			if err != nil {
				proc.failAllPending(fmt.Errorf("child stdout closed: %w", err))
				return
			}
			continue
		}

		if len(line) > 0 {
			b.handleStdoutLine(proc, line)
		}

		if err != nil {
			if errors.Is(err, io.EOF) {
				proc.failAllPending(errors.New("child process closed stdout (EOF)"))
			} else {
				proc.failAllPending(fmt.Errorf("child stdout read error: %w", err))
			}
			return
		}
	}
}

// readBoundedLine reads one '\n'-delimited line from br using ReadBytes/ReadSlice semantics
// while enforcing a hard memory ceiling of maxBytes (never using bufio.Scanner).
// If a line exceeds maxBytes, remaining bytes up to '\n' are drained and discarded while returning
// up to bridgeOversizedPrefixSz leading bytes so callers can identify the affected request ID.
func readBoundedLine(br *bufio.Reader, maxBytes int) (line []byte, exceeded bool, prefix []byte, err error) {
	// Fast path when bufio already buffers a small complete line: ReadBytes('\n') only if <= buffer.
	// Using ReadSlice loop guarantees memory never grows past maxBytes even for unbounded lines.
	var buf []byte
	for {
		frag, rerr := br.ReadSlice('\n')
		if !exceeded {
			if len(buf)+len(frag) > maxBytes {
				exceeded = true
				prefix = make([]byte, 0, bridgeOversizedPrefixSz)
				if len(buf) > 0 {
					take := len(buf)
					if take > bridgeOversizedPrefixSz {
						take = bridgeOversizedPrefixSz
					}
					prefix = append(prefix, buf[:take]...)
				}
				if rem := bridgeOversizedPrefixSz - len(prefix); rem > 0 && len(frag) > 0 {
					take := len(frag)
					if take > rem {
						take = rem
					}
					prefix = append(prefix, frag[:take]...)
				}
				buf = nil
			} else if len(buf) == 0 && rerr != bufio.ErrBufferFull {
				out := make([]byte, len(frag))
				copy(out, frag)
				return out, false, nil, rerr
			} else {
				buf = append(buf, frag...)
			}
		}

		if errors.Is(rerr, bufio.ErrBufferFull) {
			continue
		}
		if exceeded {
			return nil, true, prefix, rerr
		}
		return buf, false, nil, rerr
	}
}

func extractBridgeIDFromPrefix(prefix []byte) (uint64, bool) {
	s := jsonScanner{data: prefix}
	s.skipWS()
	if !s.consume('{') {
		return 0, false
	}
	for {
		s.skipWS()
		if s.eof() || s.peek() == '}' {
			return 0, false
		}
		key, ok := s.readString()
		if !ok {
			return 0, false
		}
		s.skipWS()
		if !s.consume(':') {
			return 0, false
		}
		s.skipWS()
		if key == "id" {
			start := s.pos
			if s.skipValue() {
				return parseBridgeID(s.data[start:s.pos])
			}
			return 0, false
		}
		if !s.skipValue() {
			return 0, false
		}
		s.skipWS()
		if !s.consume(',') {
			return 0, false
		}
	}
}

func (b *StdioBridge) handleStdoutLine(proc *bridgeProcess, line []byte) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(line, utf8BOM))
	if len(trimmed) == 0 {
		return
	}
	if trimmed[0] != '{' {
		b.logf("skipping non-JSON stdout line: %q", truncatePreview(string(trimmed), 120))
		return
	}

	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		b.logf("skipping malformed JSON stdout line: %q (%v)", truncatePreview(string(trimmed), 120), err)
		return
	}

	idTrim := bytes.TrimSpace(env.ID)
	hasID := len(idTrim) > 0 && !bytes.Equal(idTrim, []byte("null"))

	// Server->client JSON-RPC request: answer ping with empty result {}; reply -32601 to unsupported requests.
	if hasID && env.Method != "" {
		replyID := cloneRawMessage(idTrim)
		method := env.Method
		go func() {
			var (
				raw []byte
				err error
			)
			if method == "ping" {
				raw, err = buildJSONRPCResult(replyID, json.RawMessage(`{}`))
			} else {
				reply := jsonRPCErrorEnvelope{
					JSONRPC: "2.0",
					ID:      replyID,
					Error: jsonRPCErrorBody{
						Code:    -32601,
						Message: "method not supported by bridge",
					},
				}
				raw, err = marshalCompactJSON(reply)
			}
			if err != nil {
				return
			}
			_ = proc.writeLine(raw)
		}()
		return
	}

	// Server->client notification.
	if env.Method != "" {
		if env.Method == "notifications/tools/list_changed" {
			b.mu.Lock()
			if b.proc == proc {
				b.invalidateToolCatalogLocked()
			}
			b.mu.Unlock()
		}
		return
	}

	// JSON-RPC response to one of our pending requests.
	if hasID {
		bridgeID, ok := parseBridgeID(idTrim)
		if !ok {
			b.logf("ignoring response with unexpected id %s", string(idTrim))
			return
		}
		cp := make([]byte, len(trimmed))
		copy(cp, trimmed)
		proc.deliverResponse(bridgeID, cp)
	}
}

func parseBridgeID(raw []byte) (uint64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0, false
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, false
		}
		v, err := strconv.ParseUint(s, 10, 64)
		return v, err == nil
	}
	v, err := strconv.ParseUint(string(raw), 10, 64)
	return v, err == nil
}

func truncatePreview(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func (b *StdioBridge) waitProcessLoop(proc *bridgeProcess) {
	waitErr := proc.cmd.Wait()
	proc.waitErr = waitErr
	_ = proc.job.TerminateAndClose()
	<-proc.stdoutDone

	exitErr := errors.New("child process exited")
	if waitErr != nil {
		exitErr = fmt.Errorf("child process exited: %w", waitErr)
	}
	proc.failAllPending(exitErr)
	proc.procCancel()

	expected := proc.expectedExit.Load()
	flushIfPossible(b.opts.StderrWriter)
	if !expected && b.opts.OnCrash != nil {
		b.opts.OnCrash()
	}

	var logMsg string
	stateChanged := false
	b.mu.Lock()
	proc.exited = true
	b.removeDyingProcLocked(proc)
	if b.proc == proc {
		stateChanged = true
		b.proc = nil
		b.status.Running = false
		b.status.Healthy = false
		b.status.PID = 0
		b.status.HealthySince = ""
		b.metrics.ClearSessions()
		if !expected {
			b.lastStop = bridgeStopCrash
			b.cachedInitResult = nil
			b.invalidateToolCatalogLocked()
			if waitErr != nil {
				var ee *exec.ExitError
				if errors.As(waitErr, &ee) {
					b.status.LastExitCode = ee.ExitCode()
				} else {
					b.status.LastExitCode = -1
				}
				b.status.LastError = b.redactError(waitErr)
			} else {
				b.status.LastExitCode = 0
				b.status.LastError = "child exited unexpectedly"
			}
			if !proc.startedAt.IsZero() && time.Since(proc.startedAt) >= b.opts.HealthyResetAfter {
				b.consecFailures = 0
			}
			b.consecFailures++
			if b.shutdown || b.manualStopped {
				b.state = "stopped"
			} else if b.consecFailures >= b.opts.MaxConsecutiveFailures {
				b.state = "degraded"
				b.status.DegradedAt = time.Now().UTC().Format(time.RFC3339)
				logMsg = fmt.Sprintf("child pid=%d exited unexpectedly (%v); circuit breaker tripped (%d/%d) -> degraded",
					proc.pid, waitErr, b.consecFailures, b.opts.MaxConsecutiveFailures)
			} else {
				delay := b.backoffDuration(b.consecFailures)
				b.nextRetryAt = time.Now().Add(delay)
				b.state = "sleeping"
				logMsg = fmt.Sprintf("child pid=%d exited unexpectedly (%v); failure %d/%d (backoff %s), transitioned to sleeping",
					proc.pid, waitErr, b.consecFailures, b.opts.MaxConsecutiveFailures, delay)
			}
		} else {
			b.consecFailures = 0
			b.nextRetryAt = time.Time{}
			if b.shutdown || b.manualStopped {
				b.state = "stopped"
			} else {
				b.state = "sleeping"
			}
		}
	}
	close(proc.waitDone)
	b.mu.Unlock()

	if stateChanged {
		b.notifyStateChange()
	}
	if logMsg != "" {
		b.logf("%s", logMsg)
	}
}

func (b *StdioBridge) idleReaperLoop(proc *bridgeProcess) {
	interval := b.opts.IdleTimeout / 4
	if interval > bridgeMaxIdleCheckTick {
		interval = bridgeMaxIdleCheckTick
	}
	if interval < bridgeMinIdleCheckTick {
		interval = bridgeMinIdleCheckTick
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-proc.procCtx.Done():
			return
		case <-proc.waitDone:
			return
		case <-ticker.C:
			b.mu.Lock()
			shouldSleep := b.proc == proc &&
				b.state == "running" &&
				!b.suspended &&
				b.activeRequests == 0 &&
				time.Since(b.lastActivity) >= b.opts.IdleTimeout
			if !shouldSleep {
				b.mu.Unlock()
				continue
			}
			detached := b.detachProcLocked("sleeping", true)
			b.lastStop = bridgeStopIdleSleep
			b.consecFailures = 0
			b.nextRetryAt = time.Time{}
			b.retireProcLocked(detached, false)
			b.mu.Unlock()

			b.notifyStateChange()
			if detached != nil {
				b.logf("idle timeout (%s) reached; putting pid=%d to sleep", b.opts.IdleTimeout, detached.pid)
			}
			return
		}
	}
}

func (b *StdioBridge) terminateWedgedProc(proc *bridgeProcess) {
	if proc == nil {
		return
	}
	changed := false
	b.mu.Lock()
	if b.proc == proc {
		changed = true
		b.lastStop = bridgeStopCrash
		if time.Since(proc.startedAt) >= b.opts.HealthyResetAfter {
			b.consecFailures = 0
		}
		b.consecFailures++
		state := "sleeping"
		if b.consecFailures >= b.opts.MaxConsecutiveFailures {
			state = "degraded"
			b.status.DegradedAt = time.Now().UTC().Format(time.RFC3339)
		} else {
			b.nextRetryAt = time.Now().Add(b.backoffDuration(b.consecFailures))
		}
		b.status.LastError = "write to child stdin did not complete"
		b.retireProcLocked(b.detachProcLocked(state, true), true)
	}
	b.mu.Unlock()
	if changed {
		b.notifyStateChange()
	}
}

func (b *StdioBridge) gracefulStopProc(ctx context.Context, proc *bridgeProcess) {
	if proc == nil {
		return
	}
	proc.expectedExit.Store(true)
	// H1: Close stdin asynchronously without taking writeMu, and start the stop timer immediately
	// so a wedged child that stops reading stdin can never hang Stop/Restart/IdleReaper/Shutdown.
	go proc.closeStdin()

	timer := time.NewTimer(bridgeGracefulStopWait)
	defer timer.Stop()
	select {
	case <-proc.waitDone:
		_ = proc.job.TerminateAndClose()
		return
	case <-timer.C:
	case <-ctx.Done():
	}

	_ = proc.job.TerminateAndClose()
	if proc.cmd.Process != nil {
		_ = proc.cmd.Process.Kill()
	}

	killTimer := time.NewTimer(3 * time.Second)
	defer killTimer.Stop()
	select {
	case <-proc.waitDone:
	case <-killTimer.C:
		_ = proc.stdout.Close()
		b.logf("warning: timed out waiting for pid=%d exit after termination", proc.pid)
	}
}

func (p *bridgeProcess) closeStdin() {
	if p.stdinClosed.CompareAndSwap(false, true) {
		_ = p.stdin.Close()
	}
}

func (p *bridgeProcess) writeLine(payload []byte) error {
	return p.writeLineIf(payload, nil)
}

func (p *bridgeProcess) writeLineIf(payload []byte, start func() bool) error {
	if p.stdinClosed.Load() {
		return errors.New("child stdin is closed")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.stdinClosed.Load() {
		return errors.New("child stdin is closed")
	}
	if start != nil && !start() {
		return context.Canceled
	}
	buf := make([]byte, len(payload)+1)
	copy(buf, payload)
	buf[len(payload)] = '\n'
	_, err := p.stdin.Write(buf)
	return err
}

func (p *bridgeProcess) registerPending(id uint64) (chan pendingResult, error) {
	p.pendingMu.Lock()
	defer p.pendingMu.Unlock()
	if p.closed {
		return nil, errors.New("child process is not running")
	}
	ch := make(chan pendingResult, 1)
	p.pending[id] = ch
	return ch, nil
}

func (p *bridgeProcess) deletePending(id uint64) {
	p.pendingMu.Lock()
	delete(p.pending, id)
	p.pendingMu.Unlock()
}

func (p *bridgeProcess) deliverResponse(id uint64, raw []byte) {
	p.pendingMu.Lock()
	ch, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
	if ok {
		ch <- pendingResult{raw: raw}
	}
}

func (p *bridgeProcess) deliverError(id uint64, err error) {
	p.pendingMu.Lock()
	ch, ok := p.pending[id]
	if ok {
		delete(p.pending, id)
	}
	p.pendingMu.Unlock()
	if ok {
		ch <- pendingResult{err: err}
	}
}

func (p *bridgeProcess) failAllPending(err error) {
	p.pendingMu.Lock()
	if p.closed {
		p.pendingMu.Unlock()
		return
	}
	p.closed = true
	pending := p.pending
	p.pending = make(map[uint64]chan pendingResult)
	p.pendingMu.Unlock()

	for _, ch := range pending {
		ch <- pendingResult{err: err}
	}
}

func (p *bridgeProcess) call(ctx context.Context, reqBytes []byte, origID json.RawMessage, timeoutReason string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bridgeID := p.nextID.Add(1)
	rewritten, err := rewriteRequestID(reqBytes, bridgeID)
	if err != nil {
		return nil, err
	}

	ch, err := p.registerPending(bridgeID)
	if err != nil {
		return nil, err
	}

	const (
		writeQueued uint32 = iota
		writeStarted
		writeCancelled
	)
	var writeState atomic.Uint32
	writeErrCh := make(chan error, 1)
	go func() {
		writeErrCh <- p.writeLineIf(rewritten, func() bool {
			return writeState.CompareAndSwap(writeQueued, writeStarted)
		})
	}()

	select {
	case err := <-writeErrCh:
		if err != nil {
			p.deletePending(bridgeID)
			return nil, fmt.Errorf("write child stdin: %w", err)
		}
	case <-ctx.Done():
		p.deletePending(bridgeID)
		if !writeState.CompareAndSwap(writeQueued, writeCancelled) {
			p.finishCancelledWrite(ctx, writeErrCh, bridgeID, timeoutReason)
		}
		return nil, ctx.Err()
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return rewriteResponseID(res.raw, origID)
	case <-ctx.Done():
		p.deletePending(bridgeID)
		p.sendCancellation(ctx, bridgeID, timeoutReason)
		return nil, ctx.Err()
	}
}

func (p *bridgeProcess) finishCancelledWrite(ctx context.Context, writeErr <-chan error, id uint64, timeoutReason string) {
	select {
	case err := <-writeErr:
		if err == nil {
			p.sendCancellation(ctx, id, timeoutReason)
		}
		return
	default:
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		p.terminateBlockedWrite()
		return
	}
	// A client can cancel while a normal write is completing. Give it time to
	// finish before treating the shared child as wedged and interrupting other calls.
	go func() {
		timer := time.NewTimer(bridgeCancelledWriteWait)
		defer timer.Stop()
		select {
		case err := <-writeErr:
			if err == nil {
				p.sendCancellation(ctx, id, timeoutReason)
			}
		case <-p.procCtx.Done():
		case <-timer.C:
			p.terminateBlockedWrite()
		}
	}()
}

func (p *bridgeProcess) terminateBlockedWrite() {
	if p.bridge != nil {
		p.bridge.terminateWedgedProc(p)
	} else {
		go p.closeStdin()
	}
}

func (p *bridgeProcess) sendCancellation(ctx context.Context, id uint64, timeoutReason string) {
	reason := "client disconnected"
	if errors.Is(context.Cause(ctx), errBridgeClientCancelled) {
		reason = errBridgeClientCancelled.Error()
	} else if errors.Is(ctx.Err(), context.DeadlineExceeded) && timeoutReason != "" {
		reason = timeoutReason
	}
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d,"reason":%s}}`,
		id, mustJSONString(reason))
	go func() { _ = p.writeLine([]byte(msg)) }()
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"cancelled"`
	}
	return string(b)
}

func rewriteRequestID(raw []byte, bridgeID uint64) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("invalid JSON-RPC request object: %w", err)
	}
	obj["id"] = json.RawMessage(strconv.FormatUint(bridgeID, 10))
	return marshalCompactJSON(obj)
}

func rewriteResponseID(raw []byte, origID json.RawMessage) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, nil
	}
	obj["id"] = normalizeJSONRPCID(origID)
	return marshalCompactJSON(obj)
}

// ServeHTTP handles virtual Streamable HTTP MCP requests without tool filtering.
func (b *StdioBridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.ServeHTTPWithFilter(w, r, ToolFilter{})
}

// ServeHTTPWithFilter handles virtual stateless Streamable HTTP MCP requests, enforcing filter
// on tools/call and tools/list.
func (b *StdioBridge) ServeHTTPWithFilter(w http.ResponseWriter, r *http.Request, filter ToolFilter) {
	b.serveHTTPWithScope(w, r, filter, bridgeSessionScope{})
}

func (b *StdioBridge) serveHTTPWithScope(w http.ResponseWriter, r *http.Request, filter ToolFilter, scope bridgeSessionScope) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Allow", "POST, DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	case http.MethodDelete:
		// A known session is closed below, once its credential and route are checked.
	case http.MethodPost:
		// Handled below.
	default:
		w.Header().Set("Allow", "POST, DELETE")
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}

	sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if sessionID != "" {
		b.mu.Lock()
		session := b.sessions[sessionID]
		if session == nil || session.scope != scope {
			b.mu.Unlock()
			writeJSONError(w, http.StatusNotFound, "session_expired")
			return
		}
		session.lastSeen = time.Now()
		if r.Method == http.MethodDelete {
			delete(b.sessions, sessionID)
		}
		b.mu.Unlock()
		if r.Method == http.MethodDelete {
			session.close()
			b.metrics.DeleteSession(sessionID)
		} else {
			r = r.WithContext(context.WithValue(r.Context(), bridgeSessionContextKey{}, session))
		}
	}
	if r.Method == http.MethodDelete {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	if b.isStopped() {
		writeJSONError(w, http.StatusServiceUnavailable, "service_stopped")
		return
	}
	if b.State() == "degraded" {
		writeJSONError(w, http.StatusServiceUnavailable, "service_degraded")
		return
	}

	if filter.Active() {
		if status, code, herr := ValidateStrictMCPHeaders(r); herr != nil {
			writeJSONError(w, status, code)
			return
		}
	} else if strings.TrimSpace(r.Header.Get("Content-Encoding")) != "" {
		writeJSONError(w, http.StatusUnsupportedMediaType, "unsupported_content_encoding")
		return
	}

	if r.Body == nil {
		writeJSONError(w, http.StatusBadRequest, "empty_body")
		return
	}
	limit := int64(b.opts.MaxLineBytes)
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "read_body_failed")
		return
	}
	if int64(len(body)) > limit {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request_body_too_large")
		return
	}

	trimmed := bytes.TrimSpace(bytes.TrimPrefix(body, utf8BOM))
	if len(trimmed) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty_body")
		return
	}

	strictReq, parseErr := ParseStrictMCPRequest(trimmed)
	if parseErr != nil {
		code := -32600
		var se *strictRPCError
		if errors.As(parseErr, &se) && se.code != 0 {
			code = se.code
		}
		writeJSONRPCStatusError(w, http.StatusBadRequest, extractStrictRPCErrorID(parseErr), code, b.redactError(parseErr))
		return
	}

	rec, finish := b.metrics.WrapHTTP(w, r, strictReq.Peek, b.currentEpoch(), nil)
	defer finish()

	if !strictReq.IsBatch {
		b.handleSinglePOST(rec, r, strictReq.Items[0], filter, scope)
		return
	}
	b.handleBatchPOST(rec, r, strictReq.Items, filter)
}

func (b *StdioBridge) startSession(scope bridgeSessionScope) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.sessions) >= maxTrackedSessions {
		var oldestID string
		var oldestSeen time.Time
		for id, session := range b.sessions {
			session.mu.Lock()
			idle := len(session.pending) == 0
			session.mu.Unlock()
			if idle && (oldestID == "" || session.lastSeen.Before(oldestSeen)) {
				oldestID, oldestSeen = id, session.lastSeen
			}
		}
		if oldestID == "" {
			return "", false
		}
		b.sessions[oldestID].close()
		delete(b.sessions, oldestID)
		b.metrics.DeleteSession(oldestID)
	}
	id := rand.Text()
	b.sessions[id] = &bridgeSession{scope: scope, lastSeen: time.Now(), pending: make(map[string]context.CancelCauseFunc)}
	return id, true
}

func cancelBridgeRequest(ctx context.Context, req StrictMCPItem) {
	session, _ := ctx.Value(bridgeSessionContextKey{}).(*bridgeSession)
	if req.Method != "notifications/cancelled" || session == nil {
		return
	}
	var notification struct {
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(req.Raw, &notification); err != nil || len(notification.Params.RequestID) == 0 {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(notification.Params.RequestID))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil {
		return
	}
	id, err := encodeStrictIDToken(token)
	if err == nil {
		session.cancel(string(id))
	}
}

func (b *StdioBridge) isStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.shutdown || b.manualStopped
}

func (b *StdioBridge) currentEpoch() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.epoch
}

func (b *StdioBridge) beginActiveRequest() {
	b.mu.Lock()
	b.activeRequests++
	b.lastActivity = time.Now()
	b.mu.Unlock()
}

func (b *StdioBridge) endActiveRequest() {
	b.mu.Lock()
	b.activeRequests--
	if b.activeRequests < 0 {
		b.activeRequests = 0
	}
	b.lastActivity = time.Now()
	b.mu.Unlock()
}

func (b *StdioBridge) cachedToolReadOnlyHint(toolName string, proc *bridgeProcess) *bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if proc != nil && b.proc != proc {
		return nil
	}
	return b.toolReadOnly[toolName]
}

func (b *StdioBridge) cacheToolsReadOnlyHints(respBytes []byte, proc *bridgeProcess, revision uint64) {
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
	if err := json.Unmarshal(respBytes, &parsed); err != nil || parsed.Result == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.proc != proc || b.catalogRevision != revision {
		return
	}
	if b.toolReadOnly == nil {
		b.toolReadOnly = make(map[string]*bool, len(parsed.Result.Tools))
	}
	for _, t := range parsed.Result.Tools {
		if t.Name == "" {
			continue
		}
		if t.Annotations != nil && t.Annotations.ReadOnlyHint != nil {
			v := *t.Annotations.ReadOnlyHint
			b.toolReadOnly[t.Name] = &v
		} else {
			b.toolReadOnly[t.Name] = nil
		}
	}
}

func (b *StdioBridge) handleSinglePOST(w http.ResponseWriter, r *http.Request, req StrictMCPItem, filter ToolFilter, scope bridgeSessionScope) {
	// Pure JSON-RPC notification (no "id") or client-to-server JSON-RPC response (id + result/error, no method):
	// return 202 Accepted without waking child (L2).
	if !req.HasID || (!req.HasMethod && (req.HasResult || req.HasError)) {
		cancelBridgeRequest(r.Context(), req)
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !req.HasMethod {
		writeJSONRPCStatusError(w, http.StatusBadRequest, req.ID, -32600, "missing method in JSON-RPC request")
		return
	}

	// Enforce ToolFilter on tools/call before waking or hitting child.
	if req.Method == "tools/call" && filter.Active() {
		if strings.TrimSpace(req.ToolName) == "" {
			writeJSONRPCStatusError(w, http.StatusBadRequest, req.ID, -32600, "tools/call requires a non-empty string params.name")
			return
		}
		hint := b.cachedToolReadOnlyHint(req.ToolName, nil)
		if !filter.Allows(req.ToolName, hint) {
			WriteToolDeniedJSONRPC(w, req.ID, req.ToolName)
			return
		}
	}

	// Enforce MaxConcurrent semaphore.
	select {
	case b.sem <- struct{}{}:
		defer func() { <-b.sem }()
	default:
		writeJSONRPCStatusError(w, http.StatusTooManyRequests, req.ID, -32000, "bridge concurrency limit exceeded")
		return
	}

	b.beginActiveRequest()
	defer b.endActiveRequest()

	respBytes, statusCode, rpcErrCode, rpcErrMsg := b.executeSingleRequest(r.Context(), req, filter, r.Header.Get(noWakeHeader) == "1")
	if statusCode == 0 {
		// Client disconnected.
		return
	}
	if statusCode == http.StatusAccepted {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if statusCode != http.StatusOK {
		if rpcErrCode != 0 {
			writeJSONRPCStatusError(w, statusCode, req.ID, rpcErrCode, rpcErrMsg)
		} else {
			writeJSONError(w, statusCode, rpcErrMsg)
		}
		return
	}
	if req.Method == "initialize" && r.Header.Get("Mcp-Session-Id") == "" {
		sessionID, ok := b.startSession(scope)
		if !ok {
			writeJSONRPCStatusError(w, http.StatusServiceUnavailable, req.ID, -32000, "bridge session limit exceeded")
			return
		}
		w.Header().Set("Mcp-Session-Id", sessionID)
	}
	writeRawJSON(w, http.StatusOK, respBytes)
}

func (b *StdioBridge) handleBatchPOST(w http.ResponseWriter, r *http.Request, reqs []StrictMCPItem, filter ToolFilter) {
	if len(reqs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "empty_batch")
		return
	}

	hasNonNotif := false
	needsChild := false
	for _, pr := range reqs {
		if !pr.HasID || (!pr.HasMethod && (pr.HasResult || pr.HasError)) {
			cancelBridgeRequest(r.Context(), pr)
			continue
		}
		hasNonNotif = true
		if !pr.HasMethod {
			continue
		}
		if pr.Method == "tools/call" && filter.Active() {
			if strings.TrimSpace(pr.ToolName) == "" || !filter.Allows(pr.ToolName, b.cachedToolReadOnlyHint(pr.ToolName, nil)) {
				continue
			}
		}
		needsChild = true
	}

	if !hasNonNotif {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if needsChild {
		select {
		case b.sem <- struct{}{}:
			defer func() { <-b.sem }()
		default:
			writeJSONRPCStatusError(w, http.StatusTooManyRequests, json.RawMessage("null"), -32000, "bridge concurrency limit exceeded")
			return
		}
		b.beginActiveRequest()
		defer b.endActiveRequest()
	}

	var out bytes.Buffer
	out.WriteByte('[')
	appendResult := func(resp []byte) bool {
		if len(resp) > b.opts.MaxLineBytes-out.Len()-2 {
			writeJSONRPCStatusError(w, http.StatusBadGateway, nil, -32000, "batch responses exceeded maxLineBytes")
			return false
		}
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		out.Write(resp)
		return true
	}
	for _, req := range reqs {
		if !req.HasID || (!req.HasMethod && (req.HasResult || req.HasError)) {
			continue
		}
		if !req.HasMethod {
			errObj, _ := marshalCompactJSON(jsonRPCErrorEnvelope{
				JSONRPC: "2.0",
				ID:      normalizeJSONRPCID(req.ID),
				Error:   jsonRPCErrorBody{Code: -32600, Message: "missing method in JSON-RPC request"},
			})
			if !appendResult(errObj) {
				return
			}
			continue
		}
		if req.Method == "tools/call" && filter.Active() {
			if strings.TrimSpace(req.ToolName) == "" {
				errObj, _ := marshalCompactJSON(jsonRPCErrorEnvelope{
					JSONRPC: "2.0",
					ID:      normalizeJSONRPCID(req.ID),
					Error:   jsonRPCErrorBody{Code: -32600, Message: "tools/call requires a non-empty string params.name"},
				})
				if !appendResult(errObj) {
					return
				}
				continue
			}
			if !needsChild || !filter.Allows(req.ToolName, b.cachedToolReadOnlyHint(req.ToolName, nil)) {
				denied, _ := marshalCompactJSON(buildToolDeniedPayload(req.ID, req.ToolName))
				if !appendResult(denied) {
					return
				}
				continue
			}
		}
		respBytes, statusCode, rpcErrCode, rpcErrMsg := b.executeSingleRequest(r.Context(), req, filter, r.Header.Get(noWakeHeader) == "1")
		if statusCode == 0 {
			return
		}
		if statusCode == http.StatusAccepted {
			continue
		}
		if (statusCode == http.StatusServiceUnavailable || statusCode == http.StatusConflict) && rpcErrCode == 0 {
			writeJSONError(w, statusCode, rpcErrMsg)
			return
		}
		if statusCode != http.StatusOK {
			code := rpcErrCode
			if code == 0 {
				code = -32000
			}
			errObj, _ := marshalCompactJSON(jsonRPCErrorEnvelope{
				JSONRPC: "2.0",
				ID:      normalizeJSONRPCID(req.ID),
				Error: jsonRPCErrorBody{
					Code:    code,
					Message: rpcErrMsg,
				},
			})
			if !appendResult(errObj) {
				return
			}
			continue
		}
		if !appendResult(respBytes) {
			return
		}
	}

	if out.Len() == 1 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	out.WriteByte(']')
	writeRawJSON(w, http.StatusOK, out.Bytes())
}

// executeSingleRequest runs one non-notification JSON-RPC request against the child bridge.
// Returns (responseBytes, httpStatus, jsonRPCErrCode, errMsg). httpStatus == 0 means client ctx canceled.
func (b *StdioBridge) executeSingleRequest(parentCtx context.Context, req StrictMCPItem, filter ToolFilter, noWake bool) ([]byte, int, int, string) {
	if req.Method == "server/discover" {
		// The bridge implements the initialize lifecycle. Forwarding discover could
		// cause a newer child to advertise a lifecycle the HTTP bridge cannot serve.
		out, err := marshalCompactJSON(jsonRPCErrorEnvelope{
			JSONRPC: "2.0",
			ID:      normalizeJSONRPCID(req.ID),
			Error:   jsonRPCErrorBody{Code: -32601, Message: "bridge supports the initialize lifecycle"},
		})
		if err != nil {
			return nil, http.StatusInternalServerError, -32603, b.redactError(err)
		}
		return out, http.StatusOK, 0, ""
	}
	if session, _ := parentCtx.Value(bridgeSessionContextKey{}).(*bridgeSession); session != nil {
		sessionCtx, cancelSessionRequest := context.WithCancelCause(parentCtx)
		defer cancelSessionRequest(nil)
		id := string(req.ID)
		if !session.register(id, cancelSessionRequest) {
			return nil, http.StatusConflict, -32600, "duplicate request id or closed bridge session"
		}
		defer session.finish(id)
		parentCtx = sessionCtx
	}
	reqCtx, cancelReq := context.WithTimeout(parentCtx, b.opts.RequestTimeout)
	defer cancelReq()

	// Initialize: if already running with cached result, return immediately without re-initializing child;
	// if sleeping, wake child once so InitializeResult reflects real server capabilities.
	if req.Method == "initialize" {
		b.mu.Lock()
		running := b.state == "running" && b.proc != nil && !b.proc.exited
		cached := cloneRawMessage(b.cachedInitResult)
		b.mu.Unlock()
		if running && len(cached) > 0 {
			out, err := buildJSONRPCResult(req.ID, cached)
			if err != nil {
				return nil, http.StatusInternalServerError, -32603, b.redactError(err)
			}
			return out, http.StatusOK, 0, ""
		}

		if _, err := b.ensureRunningProc(reqCtx, noWake, true); err != nil {
			if errors.Is(context.Cause(parentCtx), errBridgeClientCancelled) {
				return nil, http.StatusAccepted, 0, ""
			}
			if errors.Is(err, ErrBridgeSleeping) {
				return nil, http.StatusConflict, 0, "service_sleeping"
			}
			if errors.Is(err, ErrBridgeStopped) {
				return nil, http.StatusServiceUnavailable, 0, "service_stopped"
			}
			if errors.Is(err, ErrBridgeDegraded) {
				return nil, http.StatusServiceUnavailable, 0, "service_degraded"
			}
			if parentCtx.Err() != nil {
				return nil, 0, 0, ""
			}
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, http.StatusGatewayTimeout, -32000, "request timed out"
			}
			return nil, http.StatusBadGateway, -32000, b.redactError(err)
		}

		b.mu.Lock()
		cached = cloneRawMessage(b.cachedInitResult)
		b.mu.Unlock()
		if len(cached) == 0 {
			return nil, http.StatusBadGateway, -32000, "missing cached initialize result"
		}
		out, err := buildJSONRPCResult(req.ID, cached)
		if err != nil {
			return nil, http.StatusInternalServerError, -32603, b.redactError(err)
		}
		return out, http.StatusOK, 0, ""
	}

	proc, err := b.ensureRunningProc(reqCtx, noWake, true)
	if err != nil {
		if errors.Is(context.Cause(parentCtx), errBridgeClientCancelled) {
			return nil, http.StatusAccepted, 0, ""
		}
		if errors.Is(err, ErrBridgeSleeping) {
			return nil, http.StatusConflict, 0, "service_sleeping"
		}
		if errors.Is(err, ErrBridgeStopped) {
			return nil, http.StatusServiceUnavailable, 0, "service_stopped"
		}
		if errors.Is(err, ErrBridgeDegraded) {
			return nil, http.StatusServiceUnavailable, 0, "service_degraded"
		}
		if parentCtx.Err() != nil {
			return nil, 0, 0, ""
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, http.StatusGatewayTimeout, -32000, "request timed out"
		}
		return nil, http.StatusBadGateway, -32000, b.redactError(err)
	}

	if req.Method == "tools/call" && filter.ReadOnlyOnly && !filter.Allows(req.ToolName, b.cachedToolReadOnlyHint(req.ToolName, proc)) {
		denied, _ := marshalCompactJSON(buildToolDeniedPayload(req.ID, req.ToolName))
		return denied, http.StatusOK, 0, ""
	}
	b.mu.Lock()
	revision := b.catalogRevision
	b.mu.Unlock()
	callCtx, cancelCall := context.WithCancel(reqCtx)
	defer cancelCall()
	stopAfter := context.AfterFunc(proc.procCtx, cancelCall)
	defer stopAfter()
	if proc.procCtx.Err() != nil {
		return nil, http.StatusBadGateway, -32000, "service stopped while handling request"
	}
	respBytes, err := proc.call(callCtx, req.Raw, req.ID, "request timeout")
	if err != nil {
		if errors.Is(context.Cause(parentCtx), errBridgeClientCancelled) {
			return nil, http.StatusAccepted, 0, ""
		}
		if parentCtx.Err() != nil {
			return nil, 0, 0, ""
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, http.StatusGatewayTimeout, -32000, "request timed out"
		}
		return nil, http.StatusBadGateway, -32000, b.redactError(err)
	}

	if req.Method == "tools/list" {
		b.cacheToolsReadOnlyHints(respBytes, proc, revision)
		if filter.Active() {
			filtered, mod, ferr := FilterToolsListJSON(respBytes, filter)
			if ferr != nil {
				return nil, http.StatusBadGateway, -32000, "tools_list_filter_failed"
			}
			if mod {
				respBytes = filtered
			}
		}
	}

	return respBytes, http.StatusOK, 0, ""
}

func buildJSONRPCResult(id json.RawMessage, result json.RawMessage) ([]byte, error) {
	return marshalCompactJSON(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{
		JSONRPC: "2.0",
		ID:      normalizeJSONRPCID(id),
		Result:  result,
	})
}

func writeRawJSON(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeJSONRPCStatusError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	raw, err := marshalCompactJSON(jsonRPCErrorEnvelope{
		JSONRPC: "2.0",
		ID:      normalizeJSONRPCID(id),
		Error: jsonRPCErrorBody{
			Code:    code,
			Message: msg,
		},
	})
	if err != nil {
		writeJSONError(w, status, msg)
		return
	}
	writeRawJSON(w, status, raw)
}
