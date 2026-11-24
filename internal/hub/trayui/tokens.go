package trayui

// RGBA represents a non-premultiplied 8-bit sRGB color with alpha.
type RGBA struct {
	R, G, B, A uint8
}

// WithAlpha returns a copy of c with alpha replaced by a.
func (c RGBA) WithAlpha(a uint8) RGBA {
	c.A = a
	return c
}

// Theme defines the complete Obsidian (Dark) / Alabaster (Light) visual token set.
type Theme struct {
	IsDark bool

	Canvas   RGBA
	Surface  RGBA
	Elevated RGBA
	Border   RGBA
	Divider  RGBA

	Ink      RGBA
	Muted    RGBA
	Icon     RGBA
	IconDim  RGBA
	RowHover RGBA

	Accent     RGBA
	AccentText RGBA
	AccentSoft RGBA
	OnAccent   RGBA
	Focus      RGBA

	Success     RGBA
	SuccessText RGBA
	SuccessSoft RGBA

	Warning     RGBA
	WarningText RGBA
	WarningSoft RGBA

	Error     RGBA
	ErrorText RGBA
	ErrorSoft RGBA

	Sleeping     RGBA
	SleepingSoft RGBA
}

// LightTheme matches AppsUseLightTheme == 1 (Alabaster Developer Craft).
var LightTheme = Theme{
	IsDark: false,

	Canvas:   RGBA{0xFA, 0xF9, 0xF6, 0xFF}, // #FAF9F6
	Surface:  RGBA{0xF2, 0xF1, 0xEC, 0xFF}, // #F2F1EC
	Elevated: RGBA{0xFF, 0xFF, 0xFF, 0xFF}, // #FFFFFF
	Border:   RGBA{0xDD, 0xDA, 0xD2, 0xFF}, // #DDDAD2
	Divider:  RGBA{0xE5, 0xE3, 0xDC, 0xFF}, // #E5E3DC

	Ink:      RGBA{0x15, 0x16, 0x19, 0xFF}, // #151619
	Muted:    RGBA{0x5C, 0x60, 0x6B, 0xFF}, // #5C606B
	Icon:     RGBA{0x15, 0x16, 0x19, 235},  // rgba(21,22,25,0.92)
	IconDim:  RGBA{0x5C, 0x60, 0x6B, 115},  // rgba(92,96,107,0.45)
	RowHover: RGBA{0x15, 0x16, 0x19, 14},   // rgba(21,22,25,0.055)

	Accent:     RGBA{0x25, 0x63, 0xEB, 0xFF}, // #2563EB
	AccentText: RGBA{0x1D, 0x4E, 0xD8, 0xFF}, // #1D4ED8
	AccentSoft: RGBA{0xEB, 0xF2, 0xFF, 0xFF}, // #EBF2FF
	OnAccent:   RGBA{0xFF, 0xFF, 0xFF, 0xFF}, // #FFFFFF
	Focus:      RGBA{0x25, 0x63, 0xEB, 0xFF}, // #2563EB

	Success:     RGBA{0x16, 0xA3, 0x4A, 0xFF}, // #16A34A
	SuccessText: RGBA{0x15, 0x80, 0x3D, 0xFF}, // #15803D
	SuccessSoft: RGBA{0xE8, 0xF8, 0xEE, 0xFF}, // #E8F8EE

	Warning:     RGBA{0xD9, 0x77, 0x06, 0xFF}, // #D97706
	WarningText: RGBA{0xB4, 0x53, 0x09, 0xFF}, // #B45309
	WarningSoft: RGBA{0xFE, 0xF3, 0xDC, 0xFF}, // #FEF3DC

	Error:     RGBA{0xDC, 0x26, 0x26, 0xFF}, // #DC2626
	ErrorText: RGBA{0xB9, 0x1C, 0x1C, 0xFF}, // #B91C1C
	ErrorSoft: RGBA{0xFE, 0xEC, 0xEB, 0xFF}, // #FEECEB

	Sleeping:     RGBA{0x64, 0x74, 0x8B, 0xFF}, // #64748B
	SleepingSoft: RGBA{0xF1, 0xF5, 0xF9, 0xFF}, // #F1F5F9
}

// DarkTheme matches AppsUseLightTheme == 0 (Obsidian Developer Craft).
var DarkTheme = Theme{
	IsDark: true,

	Canvas:   RGBA{0x18, 0x19, 0x1C, 0xFF}, // #18191C
	Surface:  RGBA{0x20, 0x22, 0x27, 0xFF}, // #202227
	Elevated: RGBA{0x26, 0x29, 0x30, 0xFF}, // #262930
	Border:   RGBA{0x32, 0x36, 0x40, 0xFF}, // #323640
	Divider:  RGBA{0x2B, 0x2E, 0x36, 0xFF}, // #2B2E36

	Ink:      RGBA{0xF5, 0xF6, 0xF8, 0xFF}, // #F5F6F8
	Muted:    RGBA{0x9C, 0xA3, 0xB2, 0xFF}, // #9CA3B2
	Icon:     RGBA{0xF5, 0xF6, 0xF8, 235},  // rgba(245,246,248,0.92)
	IconDim:  RGBA{0x9C, 0xA3, 0xB2, 107},  // rgba(156,163,178,0.42)
	RowHover: RGBA{0xF5, 0xF6, 0xF8, 17},   // rgba(245,246,248,0.065)

	Accent:     RGBA{0x60, 0xA5, 0xFA, 0xFF}, // #60A5FA
	AccentText: RGBA{0x7C, 0xB5, 0xFF, 0xFF}, // #7CB5FF
	AccentSoft: RGBA{0x1E, 0x2D, 0x45, 0xFF}, // #1E2D45
	OnAccent:   RGBA{0x0B, 0x11, 0x20, 0xFF}, // #0B1120
	Focus:      RGBA{0x60, 0xA5, 0xFA, 0xFF}, // #60A5FA

	Success:     RGBA{0x34, 0xD3, 0x99, 0xFF}, // #34D399
	SuccessText: RGBA{0x6E, 0xE7, 0xB7, 0xFF}, // #6EE7B7
	SuccessSoft: RGBA{0x16, 0x33, 0x29, 0xFF}, // #163329

	Warning:     RGBA{0xFB, 0xBF, 0x24, 0xFF}, // #FBBF24
	WarningText: RGBA{0xFC, 0xD3, 0x4D, 0xFF}, // #FCD34D
	WarningSoft: RGBA{0x36, 0x2A, 0x14, 0xFF}, // #362A14

	Error:     RGBA{0xF8, 0x71, 0x71, 0xFF}, // #F87171
	ErrorText: RGBA{0xFC, 0xA5, 0xA5, 0xFF}, // #FCA5A5
	ErrorSoft: RGBA{0x3B, 0x1D, 0x20, 0xFF}, // #3B1D20

	Sleeping:     RGBA{0x94, 0xA3, 0xB8, 0xFF}, // #94A3B8
	SleepingSoft: RGBA{0x26, 0x2B, 0x35, 0xFF}, // #262B35
}

// ThemeFor returns LightTheme when light is true, otherwise DarkTheme.
func ThemeFor(light bool) Theme {
	if light {
		return LightTheme
	}
	return DarkTheme
}

// Shared logical geometry constants (in 96-DPI logical pixels).
const (
	CardCorner      = 11.0
	ShadowPad       = 8
	HoverPillRadius = 7.0
	HoverInsetX     = 6
	MicroRadius     = 6.0

	PopoverLogicalWidth   = 356
	QuickMenuLogicalWidth = 248
)

// ServiceItem is the trayui view-model for a single managed service.
type ServiceItem struct {
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

// Snapshot is the trayui view-model for the hub's current state.
type Snapshot struct {
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
	Services       []ServiceItem
}

// ActionKind identifies a user-triggered action from the Popover or Quick Menu.
type ActionKind int

const (
	ActionNone ActionKind = iota
	ActionOpenPopover
	ActionOpenDashboard
	ActionOpenLogs
	ActionOpenConfig
	ActionReloadConfig
	ActionCopyStatus
	ActionStartAll
	ActionRestartAll
	ActionExit
	ActionToggleServiceDrawer
	ActionServiceStart
	ActionServiceStop
	ActionServiceRestart
	ActionServiceReload
	ActionServiceSleep
	ActionServiceWake
	ActionServiceOpenLog
	ActionServiceCopyURL
)

// Action represents a resolved UI invocation.
type Action struct {
	Kind    ActionKind
	Service string
	Payload string // e.g., ClientURL for ActionServiceCopyURL
}
