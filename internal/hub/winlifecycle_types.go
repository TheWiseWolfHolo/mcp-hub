package hub

import "time"

// TrayServiceItem describes one managed service shown in the System Tray UI.
type TrayServiceItem struct {
	Name          string
	State         string // "running", "sleeping", "starting", "stopping", "stopped", "degraded"
	Healthy       bool
	Running       bool
	HasReload     bool
	CanSleep      bool
	Mode          string // "http" or "stdio-bridge"
	StartMode     string // "eager" or "lazy"
	PID           int
	Port          int
	PrivateMB     float64
	Restarts      int
	CrashRestarts int
	WakeCount     int
	ActiveReqs    int64
	ActiveSSE     int64
	TotalReqs     int64
	LogFile       string
	ClientURL     string
	LastError     string
}

// TraySnapshot describes the current System Tray icon state, tooltip, and service state.
type TraySnapshot struct {
	State          string // "ok", "sleeping", "starting", "degraded", "error"
	Tooltip        string
	Header         string
	Version        string
	UptimeSec      int64
	TotalPrivateMB float64
	ControlAddr    string
	ConfigPath     string
	LogDir         string
	RunningCount   int
	SleepingCount  int
	StoppedCount   int
	StartingCount  int
	ErrorCount     int
	Services       []TrayServiceItem
}

// LifecycleCallbacks connects OS session, power, and System Tray events to the supervisor.
type LifecycleCallbacks struct {
	OnShutdown       func()
	OnSessionEnd     func(deadline time.Time)
	OnSuspend        func()
	OnResume         func()
	GetTraySnapshot  func() TraySnapshot
	OnServiceStart   func(name string)
	OnServiceStop    func(name string)
	OnServiceRestart func(name string)
	OnServiceReload  func(name string)
	OnServiceSleep   func(name string)
	OnServiceWake    func(name string)
	OnStartAll       func()
	OnRestartAll     func()
	OnReloadConfig   func()
	OnOpenDashboard  func()
	OnOpenLogs       func()
	OnOpenConfig     func()
	OnOpenServiceLog func(name string)
	OnExit           func()
}
