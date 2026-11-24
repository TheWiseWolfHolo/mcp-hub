package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub"
)

func main() {
	runtime.GOMAXPROCS(min(runtime.NumCPU(), 4))
	debug.SetMemoryLimit(64 << 20)

	args := os.Args[1:]
	subcmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcmd = args[0]
		args = args[1:]
	}

	switch subcmd {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		configPath := fs.String("config", hub.DefaultConfigPath, "Path to config.json")
		mutexName := fs.String("mutex", hub.DefaultMutexName, "Windows single-instance named mutex")
		if err := fs.Parse(args); err != nil {
			recordFatal(hub.DefaultLogDir, fmt.Sprintf("parse flags: %v", err))
			os.Exit(2)
		}
		if err := runSupervisor(*configPath, *mutexName); err != nil {
			os.Exit(1)
		}

	case "version", "--version", "-v":
		if hub.GitCommit != "" {
			fmt.Printf("mcp-hub %s (%s)\n", hub.Version, hub.GitCommit)
		} else {
			fmt.Printf("mcp-hub %s\n", hub.Version)
		}

	case "help", "-h", "--help":
		fmt.Printf("mcp-hub %s - headless Windows MCP daemon supervisor\n\nUsage:\n  mcp-hub run [--config %s]\n\nUse mcp-hub-ctl for status, health, catalog, logs, start, stop, restart, reload, sleep, wake, ui, and validate.\n", hub.Version, hub.DefaultConfigPath)

	default:
		recordFatal(hub.DefaultLogDir, fmt.Sprintf("unknown subcommand %q (expected 'run'; use mcp-hub-ctl for control commands)", subcmd))
		os.Exit(2)
	}
}

func runSupervisor(configPath, mutexName string) error {
	cfg, err := hub.LoadConfig(configPath)
	if err != nil {
		recordFatal(hub.DefaultLogDir, fmt.Sprintf("load config %q: %v", configPath, err))
		return err
	}

	mutex, err := hub.AcquireInstanceMutex(mutexName)
	if err != nil {
		if errors.Is(err, hub.ErrAlreadyRunning) {
			recordFatal(cfg.LogDir, fmt.Sprintf("instance already running (mutex %q held)", mutexName))
		} else {
			recordFatal(cfg.LogDir, fmt.Sprintf("acquire mutex %q: %v", mutexName, err))
		}
		return err
	}
	defer mutex.Close()

	sup, err := hub.NewSupervisor(cfg, configPath)
	if err != nil {
		recordFatal(cfg.LogDir, fmt.Sprintf("init supervisor: %v", err))
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := sup.Start(ctx); err != nil {
		sup.Shutdown()
		recordFatal(cfg.LogDir, fmt.Sprintf("supervisor failed to start: %v", err))
		return err
	}

	var wlPtr atomic.Pointer[hub.WindowsLifecycle]
	redactor := hub.NewRedactor()
	alerts := newDegradedAlertTracker(2 * time.Minute)

	notifyErr := func(title string, err error) {
		if err == nil {
			return
		}
		if cur := wlPtr.Load(); cur != nil {
			cur.NotifyBalloon(title, redactor.Redact(err.Error()), "warning")
		}
	}

	refreshTray := func() {
		cur := wlPtr.Load()
		if cur == nil {
			return
		}
		cur.RefreshTray()
		if title, msg, ok := alerts.Observe(sup.TraySnapshot()); ok {
			cur.NotifyBalloon(title, redactor.Redact(msg), "warning")
		}
	}

	cb := hub.LifecycleCallbacks{
		OnShutdown:      sup.RequestStop,
		OnSessionEnd:    sup.RequestSessionEnd,
		OnSuspend:       sup.OnSuspend,
		OnResume:        sup.OnResume,
		GetTraySnapshot: sup.TraySnapshot,
		OnServiceStart: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, _, err := sup.StartService(opCtx, name); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] start %q failed: %v", name, err)
				}
				notifyErr("Start "+name+" failed", err)
			}
			refreshTray()
		},
		OnServiceStop: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, _, err := sup.StopService(opCtx, name); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] stop %q failed: %v", name, err)
				}
				notifyErr("Stop "+name+" failed", err)
			}
			refreshTray()
		},
		OnServiceRestart: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if _, _, err := sup.RestartService(opCtx, name, false); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] restart %q failed: %v", name, err)
				}
				notifyErr("Restart "+name+" failed", err)
			}
			refreshTray()
		},
		OnServiceReload: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			if _, _, err := sup.ReloadService(opCtx, name); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] reload %q failed: %v", name, err)
				}
				notifyErr("Reload "+name+" failed", err)
			}
			refreshTray()
		},
		OnServiceSleep: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, _, err := sup.SleepService(opCtx, name); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] sleep %q failed: %v", name, err)
				}
				notifyErr("Sleep "+name+" failed", err)
			}
			refreshTray()
		},
		OnServiceWake: func(name string) {
			opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, _, err := sup.WakeService(opCtx, name); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] wake %q failed: %v", name, err)
				}
				notifyErr("Wake "+name+" failed", err)
			}
			refreshTray()
		},
		OnStartAll: func() {
			opCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			results := sup.StartAllServices(opCtx)
			var failed []string
			for name, res := range results {
				if !res.OK && res.Error != "" {
					failed = append(failed, fmt.Sprintf("%s (%s)", name, res.Error))
				}
			}
			if len(failed) > 0 {
				msg := strings.Join(failed, "; ")
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] start-all had errors: %s", msg)
				}
				if cur := wlPtr.Load(); cur != nil {
					cur.NotifyBalloon("Start All completed with errors", redactor.Redact(msg), "warning")
				}
			}
			refreshTray()
		},
		OnRestartAll: func() {
			opCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			results := sup.RestartAllServices(opCtx, false)
			var failed []string
			for name, res := range results {
				if !res.OK && res.Error != "" {
					failed = append(failed, fmt.Sprintf("%s (%s)", name, res.Error))
				}
			}
			if len(failed) > 0 {
				msg := strings.Join(failed, "; ")
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] restart-all had errors: %s", msg)
				}
				if cur := wlPtr.Load(); cur != nil {
					cur.NotifyBalloon("Restart All completed with errors", redactor.Redact(msg), "warning")
				}
			}
			refreshTray()
		},
		OnReloadConfig: func() {
			opCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := sup.ReloadConfig(opCtx); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] reload config failed: %v", err)
				}
				notifyErr("Reload Config failed", err)
			}
			refreshTray()
		},
		OnOpenDashboard: func() {
			launchURL, err := sup.CreateDashboardLaunchURL()
			if err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] open dashboard failed: %v", err)
				}
				notifyErr("Open Web Dashboard failed", err)
				return
			}
			if err := openWithShell(launchURL); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] launch browser failed: %v", err)
				}
				notifyErr("Open Web Dashboard failed", err)
			}
		},
		OnOpenLogs: func() {
			logDir := cfg.LogDir
			if snapDir := strings.TrimSpace(sup.TraySnapshot().LogDir); snapDir != "" {
				logDir = snapDir
			}
			openLogsFolder(logDir, sup.Logger())
		},
		OnOpenConfig: func() {
			target := configPath
			if snapCfg := strings.TrimSpace(sup.TraySnapshot().ConfigPath); snapCfg != "" {
				target = snapCfg
			}
			if err := openWithShell(filepath.Clean(target)); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] open config %q failed: %v", target, err)
				}
				notifyErr("Open Config failed", err)
			}
		},
		OnOpenServiceLog: func(name string) {
			snap := sup.TraySnapshot()
			var logFile string
			for _, svc := range snap.Services {
				if svc.Name == name && strings.TrimSpace(svc.LogFile) != "" {
					logFile = svc.LogFile
					break
				}
			}
			logDir := cfg.LogDir
			if strings.TrimSpace(snap.LogDir) != "" {
				logDir = snap.LogDir
			}
			if logFile == "" {
				logFile = filepath.Join(logDir, name+".log")
			} else if !filepath.IsAbs(logFile) {
				logFile = filepath.Join(logDir, logFile)
			}
			if _, statErr := os.Stat(logFile); statErr != nil {
				openLogsFolder(logDir, sup.Logger())
				return
			}
			if err := openWithShell(filepath.Clean(logFile)); err != nil {
				if sup.Logger() != nil {
					sup.Logger().Printf("[tray] open service log %q failed: %v", logFile, err)
				}
				notifyErr("Open Log failed", err)
			}
		},
		OnExit: sup.RequestStop,
	}

	wl, err := hub.StartWindowsLifecycle(cfg.ShouldShowTray(), cb, sup.Logger())
	if err != nil {
		if sup.Logger() != nil {
			sup.Logger().Printf("[hub] warning: StartWindowsLifecycle failed (continuing headless): %v", err)
		}
		wl = nil
	} else {
		wlPtr.Store(wl)
		alerts.Seed(sup.TraySnapshot())
		sup.SetOnStateChange(refreshTray)
	}

	var trayStop chan struct{}
	if wl != nil && cfg.ShouldShowTray() {
		trayStop = make(chan struct{})
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-trayStop:
					return
				case <-ticker.C:
					refreshTray()
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
	case <-sup.StopCh():
	}

	if trayStop != nil {
		close(trayStop)
	}

	sup.Shutdown()
	if wl != nil {
		wl.NotifyShutdownComplete()
		_ = wl.Close()
	}
	return nil
}

// degradedAlertTracker emits rate-limited tray notifications only when a service
// transitions into the degraded (circuit-breaker open) state.
type degradedAlertTracker struct {
	mu        sync.Mutex
	cooldown  time.Duration
	prevState map[string]string
	lastAlert map[string]time.Time
}

func newDegradedAlertTracker(cooldown time.Duration) *degradedAlertTracker {
	return &degradedAlertTracker{
		cooldown:  cooldown,
		prevState: make(map[string]string),
		lastAlert: make(map[string]time.Time),
	}
}

func (d *degradedAlertTracker) Seed(snap hub.TraySnapshot) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, svc := range snap.Services {
		d.prevState[svc.Name] = strings.ToLower(strings.TrimSpace(svc.State))
	}
}

func (d *degradedAlertTracker) Observe(snap hub.TraySnapshot) (title, msg string, fire bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for _, svc := range snap.Services {
		cur := strings.ToLower(strings.TrimSpace(svc.State))
		prev := d.prevState[svc.Name]
		d.prevState[svc.Name] = cur
		if cur == "degraded" && prev != "degraded" && !fire {
			if last, ok := d.lastAlert[svc.Name]; !ok || now.Sub(last) >= d.cooldown {
				d.lastAlert[svc.Name] = now
				title = fmt.Sprintf("McpHub: %s degraded", svc.Name)
				if strings.TrimSpace(svc.LastError) != "" {
					msg = strings.TrimSpace(svc.LastError)
				} else if svc.CrashRestarts > 0 {
					msg = fmt.Sprintf("Circuit breaker tripped after %d crash restarts.", svc.CrashRestarts)
				} else {
					msg = "Service entered degraded state. Open McpHub tray to inspect logs."
				}
				fire = true
			}
		}
	}
	return title, msg, fire
}

func openLogsFolder(logDir string, logger interface{ Printf(string, ...any) }) {
	dir := filepath.Clean(strings.TrimSpace(logDir))
	if dir == "" {
		return
	}
	if runtime.GOOS == "windows" {
		cmd := exec.Command("explorer.exe", dir)
		if err := cmd.Start(); err != nil {
			if logger != nil {
				logger.Printf("[tray] open logs folder %q failed: %v", dir, err)
			}
			return
		}
		go func() { _ = cmd.Wait() }()
	}
}

func openWithShell(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return errors.New("empty target")
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("shell open not supported on %s", runtime.GOOS)
	}
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func recordFatal(logDir, msg string) {
	redacted := hub.NewRedactor().Redact(msg)
	line := fmt.Sprintf("%s [hub] FATAL: %s\n", time.Now().UTC().Format(time.RFC3339), redacted)
	_, _ = os.Stderr.WriteString(line)
	if strings.TrimSpace(logDir) == "" {
		return
	}
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(logDir, "mcp-hub.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}
