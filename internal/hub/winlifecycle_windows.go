//go:build windows

package hub

import (
	"fmt"
	"log"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub/trayui"
)

var (
	user32                          = syscall.NewLazyDLL("user32.dll")
	procRegisterClassExW            = user32.NewProc("RegisterClassExW")
	procUnregisterClassW            = user32.NewProc("UnregisterClassW")
	procCreateWindowExW             = user32.NewProc("CreateWindowExW")
	procDestroyWindow               = user32.NewProc("DestroyWindow")
	procDefWindowProcW              = user32.NewProc("DefWindowProcW")
	procGetMessageW                 = user32.NewProc("GetMessageW")
	procTranslateMessage            = user32.NewProc("TranslateMessage")
	procDispatchMessageW            = user32.NewProc("DispatchMessageW")
	procPostMessageW                = user32.NewProc("PostMessageW")
	procPostQuitMessage             = user32.NewProc("PostQuitMessage")
	procRegisterWindowMessageW      = user32.NewProc("RegisterWindowMessageW")
	procShutdownBlockReasonCreate   = user32.NewProc("ShutdownBlockReasonCreate")
	procShutdownBlockReasonDestroy  = user32.NewProc("ShutdownBlockReasonDestroy")
	procCreateIconIndirect          = user32.NewProc("CreateIconIndirect")
	procDestroyIcon                 = user32.NewProc("DestroyIcon")
	procSetForegroundWindow         = user32.NewProc("SetForegroundWindow")
	procSetFocus                    = user32.NewProc("SetFocus")
	procShowWindow                  = user32.NewProc("ShowWindow")
	procSetWindowPos                = user32.NewProc("SetWindowPos")
	procGetWindowRect               = user32.NewProc("GetWindowRect")
	procUpdateLayeredWindow         = user32.NewProc("UpdateLayeredWindow")
	procGetDC                       = user32.NewProc("GetDC")
	procReleaseDC                   = user32.NewProc("ReleaseDC")
	procGetCursorPos                = user32.NewProc("GetCursorPos")
	procFindWindowW                 = user32.NewProc("FindWindowW")
	procGetDpiForWindow             = user32.NewProc("GetDpiForWindow")
	procGetSystemMetrics            = user32.NewProc("GetSystemMetrics")
	procGetSystemMetricsForDpi      = user32.NewProc("GetSystemMetricsForDpi")
	procSetProcessDpiAwarenessCtx   = user32.NewProc("SetProcessDpiAwarenessContext")
	procChangeWindowMessageFilterEx = user32.NewProc("ChangeWindowMessageFilterEx")
	procMonitorFromRect             = user32.NewProc("MonitorFromRect")
	procGetMonitorInfoW             = user32.NewProc("GetMonitorInfoW")
	procLoadCursorW                 = user32.NewProc("LoadCursorW")
	procSetCursor                   = user32.NewProc("SetCursor")
	procTrackMouseEvent             = user32.NewProc("TrackMouseEvent")
	procSetTimer                    = user32.NewProc("SetTimer")
	procKillTimer                   = user32.NewProc("KillTimer")
	procGetKeyState                 = user32.NewProc("GetKeyState")
	procOpenClipboard               = user32.NewProc("OpenClipboard")
	procEmptyClipboard              = user32.NewProc("EmptyClipboard")
	procSetClipboardData            = user32.NewProc("SetClipboardData")
	procCloseClipboard              = user32.NewProc("CloseClipboard")

	gdi32                  = syscall.NewLazyDLL("gdi32.dll")
	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap       = gdi32.NewProc("CreateBitmap")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	shell32                    = syscall.NewLazyDLL("shell32.dll")
	procShellNotifyIcon        = shell32.NewProc("Shell_NotifyIconW")
	procShellNotifyIconGetRect = shell32.NewProc("Shell_NotifyIconGetRect")

	shcoreTray           = syscall.NewLazyDLL("shcore.dll")
	procGetDpiForMonitor = shcoreTray.NewProc("GetDpiForMonitor")

	advapi32Tray      = syscall.NewLazyDLL("advapi32.dll")
	procRegOpenKeyExW = advapi32Tray.NewProc("RegOpenKeyExW")
	procRegQueryValEx = advapi32Tray.NewProc("RegQueryValueExW")
	procRegCloseKey   = advapi32Tray.NewProc("RegCloseKey")

	powrprof                                     = syscall.NewLazyDLL("powrprof.dll")
	procPowerRegisterSuspendResumeNotification   = powrprof.NewProc("PowerRegisterSuspendResumeNotification")
	procPowerUnregisterSuspendResumeNotification = powrprof.NewProc("PowerUnregisterSuspendResumeNotification")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

const (
	wmDestroy         = 0x0002
	wmActivate        = 0x0006
	wmKillFocus       = 0x0008
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmSettingChange   = 0x001A
	wmSetCursor       = 0x0020
	wmContextMenu     = 0x007B
	wmKeyDown         = 0x0100
	wmTimer           = 0x0113
	wmMouseMove       = 0x0200
	wmLButtonDown     = 0x0201
	wmLButtonUp       = 0x0202
	wmLButtonDblClk   = 0x0203
	wmRButtonUp       = 0x0205
	wmMouseWheel      = 0x020A
	wmPowerBroadcast  = 0x0218
	wmMouseLeave      = 0x02A3
	wmDpiChanged      = 0x02E0
	wmApp             = 0x8000

	wmAppRefreshTray  = wmApp + 1
	wmAppTrayCallback = wmApp + 2
	wmAppClose        = wmApp + 3
	wmAppBalloon      = wmApp + 4

	wsOverlapped   = 0x00000000
	wsPopup        = 0x80000000
	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080
	wsExLayered    = 0x00080000
	csDblClks      = 0x0008

	swShow     = 5
	swpNoMove  = 0x0002
	swpNoSize  = 0x0001
	swpNoZOrd  = 0x0004
	swpNoActiv = 0x0010
	hwndTopMst = ^uintptr(0) // (HWND)-1

	ulwAlpha   = 0x00000002
	acSrcOver  = 0x00
	acSrcAlpha = 0x01

	pbtApmSuspend         = 4
	pbtApmResumeSuspend   = 7
	pbtApmResumeAutomatic = 18

	deviceNotifyCallback = 2

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010

	niifInfo             = 0x00000001
	niifWarning          = 0x00000002
	niifError            = 0x00000003
	niifRespectQuietTime = 0x00000080

	trayIconID = 1

	biRGB        = 0
	dibRGBColors = 0
	msgfltAllow  = 1

	smCxSmIcon               = 49
	monitorDefaultToNearest  = 2
	mdtEffectiveDPI          = 0
	tmeLeave                 = 0x00000002
	htClient                 = 1
	idcArrow                 = 32512
	idcHand                  = 32649
	vkControl                = 0x11
	vkEscape                 = 0x1B
	vkReturn                 = 0x0D
	vkSpace                  = 0x20
	vkLeft                   = 0x25
	vkUp                     = 0x26
	vkRight                  = 0x27
	vkDown                   = 0x28
	cfUnicodeText            = 13
	gmemMoveable             = 0x0002
	hkeyCurrentUser          = 0x80000001
	keyQueryValue            = 0x0001
	regDword                 = 4
	dpiAwarenessPerMonitorV2 = ^uintptr(3) // (DPI_AWARENESS_CONTEXT)-4

	timerTrayRetry   = 0x7A01
	timerCopiedReset = 0x7A02
	trayRetryMs      = 5000
	trayRetryMax     = 36
	copiedResetMs    = 1500

	// Windows can terminate an app with no visible window after five seconds.
	winShutdownWaitTimeout = 4 * time.Second
	powerEventDedupWindow  = 2 * time.Second
	winCloseWaitTimeout    = 5 * time.Second
	popupToggleDebounce    = 180 * time.Millisecond
)

type wndClassExW struct {
	CBSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CBClsExtra    int32
	CBWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type winPoint struct {
	X int32
	Y int32
}

type winSize struct {
	CX int32
	CY int32
}

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      winPoint
}

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type notifyIconDataW struct {
	CBSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         winGUID
	HBalloonIcon     uintptr
}

type notifyIconIdentifier struct {
	CBSize   uint32
	HWnd     uintptr
	UID      uint32
	GuidItem winGUID
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type bitmapInfo struct {
	BmiHeader bitmapInfoHeader
	BmiColors [1]uint32
}

type iconInfo struct {
	FIcon    int32
	XHotspot uint32
	YHotspot uint32
	HbmMask  uintptr
	HbmColor uintptr
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

type monitorInfo struct {
	CBSize    uint32
	RcMonitor winRect
	RcWork    winRect
	DwFlags   uint32
}

type trackMouseEvent struct {
	CBSize      uint32
	DwFlags     uint32
	HwndTrack   uintptr
	DwHoverTime uint32
}

type deviceNotifySubscribeParams struct {
	Callback uintptr
	Context  uintptr
}

type balloonMsg struct {
	title    string
	message  string
	severity string
}

type popupKind int

const (
	popupNone popupKind = iota
	popupPopover
	popupQuickMenu
)

// trayPopup holds transient state for the modeless WS_EX_LAYERED popup window.
// All resources (HWND, HDC, HBITMAP) are allocated ONLY while visible and freed on close.
type trayPopup struct {
	kind popupKind
	hwnd uintptr
	dpi  int

	anchorRect trayui.Rect
	hasAnchor  bool
	cursorPt   winPoint
	workArea   trayui.Rect

	memDC   uintptr
	dibBmp  uintptr
	dibBits unsafe.Pointer
	dibW    int
	dibH    int

	snap           trayui.Snapshot
	popoverState   trayui.PopoverState
	popoverLayout  *trayui.PopoverLayout
	quickMenuState trayui.QuickMenuState
	quickLayout    *trayui.QuickMenuLayout

	mouseTracking bool
	overClickable bool
	closing       bool
}

// Package-level callback dispatch registry so syscall.NewCallback is allocated once
// per process and no Go pointers are stored inside Win32/C memory.
var (
	dpiAwareOnce          sync.Once
	lifecycleCallbackOnce sync.Once
	wndProcCallbackPtr    uintptr
	popupProcCallbackPtr  uintptr
	powerCallbackPtr      uintptr

	lifecycleSeq atomic.Uint64

	lifecycleRegistryMu sync.RWMutex
	lifecycleByHWND     = map[uintptr]*WindowsLifecycle{}
	popupByHWND         = map[uintptr]*WindowsLifecycle{}
	lifecycleByToken    = map[uintptr]*WindowsLifecycle{}
)

func initDPIAwareness() {
	dpiAwareOnce.Do(func() {
		if procSetProcessDpiAwarenessCtx.Find() == nil {
			_, _, _ = procSetProcessDpiAwarenessCtx.Call(dpiAwarenessPerMonitorV2)
		}
	})
}

func initLifecycleCallbacks() {
	lifecycleCallbackOnce.Do(func() {
		wndProcCallbackPtr = syscall.NewCallback(lifecycleWndProc)
		popupProcCallbackPtr = syscall.NewCallback(popupWndProc)
		powerCallbackPtr = syscall.NewCallback(lifecyclePowerCallback)
	})
}

func lifecycleWndProc(hwnd, msg, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			lifecycleRegistryMu.RLock()
			wl := lifecycleByHWND[hwnd]
			lifecycleRegistryMu.RUnlock()
			if wl != nil && wl.logger != nil {
				wl.logger.Printf("[winlifecycle] panic recovered in WndProc (msg=0x%x): %v", msg, r)
			}
			ret, _, _ = procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		}
	}()

	lifecycleRegistryMu.RLock()
	wl := lifecycleByHWND[hwnd]
	lifecycleRegistryMu.RUnlock()
	if wl != nil {
		if handled, r := wl.handleMessage(hwnd, uint32(msg), wParam, lParam); handled {
			return r
		}
	}
	ret, _, _ = procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return ret
}

func popupWndProc(hwnd, msg, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			lifecycleRegistryMu.RLock()
			wl := popupByHWND[hwnd]
			lifecycleRegistryMu.RUnlock()
			if wl != nil && wl.logger != nil {
				wl.logger.Printf("[winlifecycle] panic recovered in PopupWndProc (msg=0x%x): %v", msg, r)
			}
			ret, _, _ = procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		}
	}()

	lifecycleRegistryMu.RLock()
	wl := popupByHWND[hwnd]
	lifecycleRegistryMu.RUnlock()
	if wl != nil {
		if handled, r := wl.handlePopupMessage(hwnd, uint32(msg), wParam, lParam); handled {
			return r
		}
	}
	ret, _, _ = procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return ret
}

func lifecyclePowerCallback(ctxToken, eventType, _ uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			lifecycleRegistryMu.RLock()
			wl := lifecycleByToken[ctxToken]
			lifecycleRegistryMu.RUnlock()
			if wl != nil && wl.logger != nil {
				wl.logger.Printf("[winlifecycle] panic recovered in PowerCallback (event=%d): %v", eventType, r)
			}
			ret = 0
		}
	}()

	lifecycleRegistryMu.RLock()
	wl := lifecycleByToken[ctxToken]
	lifecycleRegistryMu.RUnlock()
	if wl != nil {
		wl.handlePowerEvent(uint32(eventType))
	}
	return 0
}

// WindowsLifecycle hosts a hidden top-level Win32 overlapped window on a dedicated
// OS-locked thread to receive WM_QUERYENDSESSION/WM_ENDSESSION, suspend/resume power
// events, and optional high-DPI custom-drawn System Tray interactions.
type WindowsLifecycle struct {
	enableTray bool
	cb         LifecycleCallbacks
	logger     *log.Logger
	token      uintptr

	closing        atomic.Bool
	refreshPending atomic.Bool

	mu         sync.Mutex
	hwnd       uintptr
	closed     bool
	trayAdded  bool
	icons      map[string]uintptr
	iconSize   int
	iconLight  bool
	wmTaskbar  uint32
	powerReg   uintptr
	powerParam deviceNotifySubscribeParams

	// Message-loop thread owned fields (accessed only on runLoop OS thread).
	hInst             uintptr
	popupClassNamePtr *uint16
	cursorArrow       uintptr
	cursorHand        uintptr
	appsLightTheme    bool
	systemLightTheme  bool
	trayRetryCount    int
	popup             *trayPopup
	lastPopupClose    time.Time
	lastClosedKind    popupKind

	balloonMu    sync.Mutex
	balloonQueue []balloonMsg

	powerMu     sync.Mutex
	lastSuspend time.Time
	lastResume  time.Time
	powerCh     chan uint32
	powerStopCh chan struct{}
	powerDone   chan struct{}

	shutdownOnce     sync.Once
	shutdownDoneOnce sync.Once
	shutdownDone     chan struct{}
	closeOnce        sync.Once
	doneCh           chan struct{}
}

// StartWindowsLifecycle launches the hidden top-level Win32 message loop and optional
// System Tray icon. It returns only after the Win32 window is created and ready.
func StartWindowsLifecycle(enableTray bool, cb LifecycleCallbacks, logger *log.Logger) (*WindowsLifecycle, error) {
	initDPIAwareness()
	initLifecycleCallbacks()

	seq := lifecycleSeq.Add(1)
	wl := &WindowsLifecycle{
		enableTray:   enableTray,
		cb:           cb,
		logger:       logger,
		token:        uintptr(seq),
		icons:        make(map[string]uintptr, 5),
		powerCh:      make(chan uint32, 16),
		powerStopCh:  make(chan struct{}),
		powerDone:    make(chan struct{}),
		shutdownDone: make(chan struct{}),
		doneCh:       make(chan struct{}),
	}

	go wl.runPowerWorker()

	readyCh := make(chan error, 1)
	go wl.runLoop(seq, readyCh)

	if err := <-readyCh; err != nil {
		close(wl.powerStopCh)
		<-wl.powerDone
		return nil, err
	}
	return wl, nil
}

func (wl *WindowsLifecycle) runLoop(seq uint64, readyCh chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(wl.doneCh)

	hInst, _, _ := procGetModuleHandleW.Call(0)
	wl.hInst = hInst
	wl.cursorArrow, _, _ = procLoadCursorW.Call(0, idcArrow)
	wl.cursorHand, _, _ = procLoadCursorW.Call(0, idcHand)

	className := fmt.Sprintf("McpHubLifecycleWnd_%d_%d", os.Getpid(), seq)
	classNamePtr, err := syscall.UTF16PtrFromString(className)
	if err != nil {
		readyCh <- fmt.Errorf("encode window class name: %w", err)
		return
	}
	popupClassName := fmt.Sprintf("McpHubTrayPopup_%d_%d", os.Getpid(), seq)
	popupClassNamePtr, err := syscall.UTF16PtrFromString(popupClassName)
	if err != nil {
		readyCh <- fmt.Errorf("encode popup class name: %w", err)
		return
	}
	wl.popupClassNamePtr = popupClassNamePtr

	titlePtr, err := syscall.UTF16PtrFromString("McpHub")
	if err != nil {
		readyCh <- fmt.Errorf("encode window title: %w", err)
		return
	}

	wc := wndClassExW{
		CBSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   wndProcCallbackPtr,
		HInstance:     hInst,
		LpszClassName: classNamePtr,
	}
	atom, _, errno := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		readyCh <- fmt.Errorf("RegisterClassExW: %w", errno)
		return
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(classNamePtr)), hInst)

	pwc := wndClassExW{
		CBSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		Style:         csDblClks,
		LpfnWndProc:   popupProcCallbackPtr,
		HInstance:     hInst,
		HCursor:       wl.cursorArrow,
		LpszClassName: popupClassNamePtr,
	}
	pAtom, _, pErrno := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&pwc)))
	if pAtom == 0 {
		readyCh <- fmt.Errorf("RegisterClassExW(popup): %w", pErrno)
		return
	}
	defer procUnregisterClassW.Call(uintptr(unsafe.Pointer(popupClassNamePtr)), hInst)

	// Top-level overlapped window with hWndParent=0 and NO WS_VISIBLE (never HWND_MESSAGE)
	// so Windows delivers WM_QUERYENDSESSION, WM_ENDSESSION, WM_POWERBROADCAST, WM_SETTINGCHANGE, and TaskbarCreated.
	hwnd, _, errno := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		wsOverlapped,
		0, 0, 0, 0,
		0,
		0,
		hInst,
		0,
	)
	if hwnd == 0 {
		readyCh <- fmt.Errorf("CreateWindowExW: %w", errno)
		return
	}

	taskbarMsgName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	wmTaskbar, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(taskbarMsgName)))
	if procChangeWindowMessageFilterEx.Find() == nil {
		// Allow TaskbarCreated and wmAppTrayCallback across UIPI if hub runs elevated while Explorer is standard integrity.
		if wmTaskbar != 0 {
			_, _, _ = procChangeWindowMessageFilterEx.Call(hwnd, wmTaskbar, msgfltAllow, 0)
		}
		_, _, _ = procChangeWindowMessageFilterEx.Call(hwnd, wmAppTrayCallback, msgfltAllow, 0)
	}

	wl.appsLightTheme, wl.systemLightTheme = readPersonalizeThemes()

	wl.mu.Lock()
	wl.hwnd = hwnd
	wl.wmTaskbar = uint32(wmTaskbar)
	wl.mu.Unlock()

	lifecycleRegistryMu.Lock()
	lifecycleByHWND[hwnd] = wl
	lifecycleByToken[wl.token] = wl
	lifecycleRegistryMu.Unlock()

	defer func() {
		wl.closing.Store(true)
		wl.closePopup(false)
		_, _, _ = procKillTimer.Call(hwnd, timerTrayRetry)
		wl.unregisterPowerNotifications()
		wl.destroyTrayAndIcons(hwnd)

		lifecycleRegistryMu.Lock()
		delete(lifecycleByHWND, hwnd)
		delete(lifecycleByToken, wl.token)
		lifecycleRegistryMu.Unlock()

		wl.mu.Lock()
		wl.hwnd = 0
		wl.closed = true
		wl.mu.Unlock()

		close(wl.powerStopCh)
		<-wl.powerDone
	}()

	wl.registerPowerNotifications()

	if wl.enableTray {
		wl.ensureTrayIcons(hwnd)
		wl.ensureTrayIconWithRetry(hwnd, true)
	}

	readyCh <- nil

	var msg winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (wl *WindowsLifecycle) registerPowerNotifications() {
	if err := procPowerRegisterSuspendResumeNotification.Find(); err != nil {
		return
	}
	wl.powerParam = deviceNotifySubscribeParams{
		Callback: powerCallbackPtr,
		Context:  wl.token,
	}
	var reg uintptr
	ret, _, _ := procPowerRegisterSuspendResumeNotification.Call(
		deviceNotifyCallback,
		uintptr(unsafe.Pointer(&wl.powerParam)),
		uintptr(unsafe.Pointer(&reg)),
	)
	if ret == 0 && reg != 0 {
		wl.mu.Lock()
		wl.powerReg = reg
		wl.mu.Unlock()
	} else if wl.logger != nil {
		wl.logger.Printf("[winlifecycle] PowerRegisterSuspendResumeNotification returned %d (continuing with WM_POWERBROADCAST)", ret)
	}
}

func (wl *WindowsLifecycle) unregisterPowerNotifications() {
	wl.mu.Lock()
	reg := wl.powerReg
	wl.powerReg = 0
	wl.mu.Unlock()
	if reg == 0 {
		return
	}
	if err := procPowerUnregisterSuspendResumeNotification.Find(); err == nil {
		_, _, _ = procPowerUnregisterSuspendResumeNotification.Call(reg)
	}
}

func (wl *WindowsLifecycle) handleMessage(hwnd uintptr, msg uint32, wParam, lParam uintptr) (bool, uintptr) {
	if wl.wmTaskbar != 0 && msg == wl.wmTaskbar {
		if wl.enableTray && !wl.closing.Load() {
			wl.trayRetryCount = 0
			wl.ensureTrayIcons(hwnd)
			wl.ensureTrayIconWithRetry(hwnd, true)
		}
		return true, 0
	}

	switch msg {
	case wmQueryEndSession:
		reason, _ := syscall.UTF16PtrFromString("McpHub is shutting down managed MCP services...")
		_, _, _ = procShutdownBlockReasonCreate.Call(hwnd, uintptr(unsafe.Pointer(reason)))
		return true, 1

	case wmEndSession:
		if wParam != 0 {
			wl.closing.Store(true)
			wl.closePopup(false)
			if wl.cb.OnSessionEnd != nil || wl.cb.OnShutdown != nil {
				deadline := time.Now().Add(winShutdownWaitTimeout)
				wl.shutdownOnce.Do(func() { go wl.dispatchSessionEnd(deadline) })
				timer := time.NewTimer(time.Until(deadline))
				defer timer.Stop()
				select {
				case <-wl.shutdownDone:
				case <-timer.C:
				}
			}
			_, _, _ = procShutdownBlockReasonDestroy.Call(hwnd)
			wl.destroyTrayAndIcons(hwnd)
			_, _, _ = procDestroyWindow.Call(hwnd)
		} else {
			_, _, _ = procShutdownBlockReasonDestroy.Call(hwnd)
		}
		return true, 0

	case wmPowerBroadcast:
		wl.handlePowerEvent(uint32(wParam))
		return true, 1

	case wmSettingChange:
		// Never dereference lParam here: simply re-read theme preference from HKCU safely.
		appsLight, sysLight := readPersonalizeThemes()
		appsChanged := appsLight != wl.appsLightTheme
		sysChanged := sysLight != wl.systemLightTheme
		wl.appsLightTheme = appsLight
		wl.systemLightTheme = sysLight
		if wl.enableTray && !wl.closing.Load() {
			if sysChanged {
				wl.ensureTrayIcons(hwnd)
				wl.addOrUpdateTrayIcon(hwnd, false)
			}
			if appsChanged && wl.popup != nil {
				wl.presentPopup(false)
			}
		}
		return true, 0

	case wmDpiChanged:
		if wl.enableTray && !wl.closing.Load() {
			wl.ensureTrayIcons(hwnd)
			wl.addOrUpdateTrayIcon(hwnd, false)
			if wl.popup != nil {
				wl.popup.dpi = monitorDPIForRect(wl.popup.anchorRect, hwnd)
				wl.presentPopup(true)
			}
		}
		return true, 0

	case wmTimer:
		if wParam == timerTrayRetry {
			if wl.enableTray && !wl.closing.Load() {
				wl.ensureTrayIconWithRetry(hwnd, true)
			} else {
				_, _, _ = procKillTimer.Call(hwnd, timerTrayRetry)
			}
			return true, 0
		}

	case wmAppRefreshTray:
		wl.refreshPending.Store(false)
		if wl.enableTray && !wl.closing.Load() {
			wl.ensureTrayIcons(hwnd)
			wl.addOrUpdateTrayIcon(hwnd, false)
			if wl.popup != nil && wl.popup.kind == popupPopover {
				wl.popup.snap = toTrayUISnapshot(wl.currentSnapshot())
				wl.presentPopup(true)
			}
		}
		return true, 0

	case wmAppBalloon:
		if wl.enableTray && !wl.closing.Load() {
			wl.drainAndShowBalloons(hwnd)
		}
		return true, 0

	case wmAppTrayCallback:
		if wl.enableTray && !wl.closing.Load() {
			ev := uint32(lParam & 0xFFFF)
			switch ev {
			case wmLButtonUp:
				wl.togglePopup(hwnd, popupPopover)
			case wmRButtonUp, wmContextMenu:
				wl.togglePopup(hwnd, popupQuickMenu)
			}
		}
		return true, 0

	case wmAppClose:
		wl.closing.Store(true)
		wl.closePopup(false)
		_, _, _ = procKillTimer.Call(hwnd, timerTrayRetry)
		wl.destroyTrayAndIcons(hwnd)
		_, _, _ = procDestroyWindow.Call(hwnd)
		return true, 0

	case wmDestroy:
		_, _, _ = procPostQuitMessage.Call(0)
		return true, 0
	}
	return false, 0
}

func (wl *WindowsLifecycle) togglePopup(owner uintptr, kind popupKind) {
	if wl.popup != nil {
		curKind := wl.popup.kind
		wl.closePopup(true)
		if curKind == kind {
			return
		}
	} else if wl.lastClosedKind == kind && time.Since(wl.lastPopupClose) < popupToggleDebounce {
		// Prevent immediate reopen when user clicked tray icon to dismiss an open popup.
		return
	}
	wl.openPopup(owner, kind)
}

func (wl *WindowsLifecycle) openPopup(owner uintptr, kind popupKind) {
	wl.appsLightTheme, wl.systemLightTheme = readPersonalizeThemes()

	var pt winPoint
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	anchor, hasAnchor := getTrayIconRect(owner)
	refRect := anchor
	if !hasAnchor {
		refRect = trayui.Rect{Left: pt.X, Top: pt.Y, Right: pt.X + 1, Bottom: pt.Y + 1}
	}
	workArea := monitorWorkAreaForRect(refRect)
	dpi := monitorDPIForRect(refRect, owner)

	snap := toTrayUISnapshot(wl.currentSnapshot())

	p := &trayPopup{
		kind:       kind,
		dpi:        dpi,
		anchorRect: anchor,
		hasAnchor:  hasAnchor,
		cursorPt:   pt,
		workArea:   workArea,
		snap:       snap,
		popoverState: trayui.PopoverState{
			FocusIndex: -1,
		},
		quickMenuState: trayui.QuickMenuState{
			HoverIndex: -1,
			FocusIndex: -1,
		},
	}

	canvas, winX, winY := wl.renderPopupFrame(p)
	if canvas == nil {
		return
	}

	titlePtr, _ := syscall.UTF16PtrFromString("McpHub Tray")
	hwnd, _, errno := procCreateWindowExW.Call(
		wsExLayered|wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(wl.popupClassNamePtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		wsPopup,
		uintptr(int32(winX)),
		uintptr(int32(winY)),
		uintptr(int32(canvas.Width)),
		uintptr(int32(canvas.Height)),
		owner,
		0,
		wl.hInst,
		0,
	)
	if hwnd == 0 {
		if wl.logger != nil {
			wl.logger.Printf("[winlifecycle] CreateWindowExW(popup) failed: %v", errno)
		}
		return
	}

	p.hwnd = hwnd
	wl.popup = p

	lifecycleRegistryMu.Lock()
	popupByHWND[hwnd] = wl
	lifecycleRegistryMu.Unlock()

	if !wl.uploadCanvasToPopup(p, canvas, winX, winY) {
		wl.closePopup(false)
		return
	}

	_, _, _ = procShowWindow.Call(hwnd, swShow)
	_, _, _ = procSetForegroundWindow.Call(hwnd)
	_, _, _ = procSetWindowPos.Call(hwnd, hwndTopMst, 0, 0, 0, 0, swpNoMove|swpNoSize)
	_, _, _ = procSetFocus.Call(hwnd)
}

func (wl *WindowsLifecycle) renderPopupFrame(p *trayPopup) (canvas *trayui.Canvas, winX, winY int32) {
	theme := trayui.ThemeFor(wl.appsLightTheme)
	maxCardH := int(p.workArea.Height()) - 24
	if maxCardH < 220 {
		maxCardH = 220
	}

	switch p.kind {
	case popupPopover:
		c, layout := trayui.RenderPopover(p.snap, p.popoverState, theme, p.dpi, maxCardH)
		p.popoverLayout = layout
		if p.popoverState.ScrollOffset > layout.MaxScroll {
			p.popoverState.ScrollOffset = layout.MaxScroll
		}
		wx, wy := trayui.PlacePopup(
			p.anchorRect, p.hasAnchor,
			p.cursorPt.X, p.cursorPt.Y,
			layout.CardRect.Width(), layout.CardRect.Height(),
			int32(layout.ShadowPad), p.workArea, false,
		)
		return c, wx, wy

	case popupQuickMenu:
		c, layout := trayui.RenderQuickMenu(p.snap, p.quickMenuState, theme, p.dpi)
		p.quickLayout = layout
		wx, wy := trayui.PlacePopup(
			p.anchorRect, p.hasAnchor,
			p.cursorPt.X, p.cursorPt.Y,
			layout.CardRect.Width(), layout.CardRect.Height(),
			int32(layout.ShadowPad), p.workArea, true,
		)
		return c, wx, wy
	default:
		return nil, 0, 0
	}
}

func (wl *WindowsLifecycle) presentPopup(reposition bool) {
	p := wl.popup
	if p == nil || p.hwnd == 0 || p.closing {
		return
	}
	canvas, newX, newY := wl.renderPopupFrame(p)
	if canvas == nil {
		return
	}
	if !reposition {
		var wr winRect
		if ok, _, _ := procGetWindowRect.Call(p.hwnd, uintptr(unsafe.Pointer(&wr))); ok != 0 {
			newX, newY = wr.Left, wr.Top
		}
	}
	wl.uploadCanvasToPopup(p, canvas, newX, newY)
}

func (wl *WindowsLifecycle) uploadCanvasToPopup(p *trayPopup, canvas *trayui.Canvas, winX, winY int32) bool {
	if p.memDC == 0 || p.dibBmp == 0 || p.dibW != canvas.Width || p.dibH != canvas.Height {
		freePopupDIB(p)
		memDC, _, _ := procCreateCompatibleDC.Call(0)
		if memDC == 0 {
			return false
		}
		var bmi bitmapInfo
		bmi.BmiHeader = bitmapInfoHeader{
			BiSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			BiWidth:       int32(canvas.Width),
			BiHeight:      -int32(canvas.Height),
			BiPlanes:      1,
			BiBitCount:    32,
			BiCompression: biRGB,
		}
		var bits unsafe.Pointer
		hBmp, _, _ := procCreateDIBSection.Call(
			memDC,
			uintptr(unsafe.Pointer(&bmi)),
			dibRGBColors,
			uintptr(unsafe.Pointer(&bits)),
			0,
			0,
		)
		if hBmp == 0 || bits == nil {
			_, _, _ = procDeleteDC.Call(memDC)
			return false
		}
		_, _, _ = procSelectObject.Call(memDC, hBmp)
		p.memDC = memDC
		p.dibBmp = hBmp
		p.dibBits = bits
		p.dibW = canvas.Width
		p.dibH = canvas.Height
	}

	dst := unsafe.Slice((*byte)(p.dibBits), len(canvas.Pix))
	copy(dst, canvas.Pix)

	hdcScreen, _, _ := procGetDC.Call(0)
	if hdcScreen == 0 {
		return false
	}
	defer procReleaseDC.Call(0, hdcScreen)

	ptDst := winPoint{X: winX, Y: winY}
	sz := winSize{CX: int32(canvas.Width), CY: int32(canvas.Height)}
	ptSrc := winPoint{X: 0, Y: 0}
	blend := blendFunction{
		BlendOp:             acSrcOver,
		BlendFlags:          0,
		SourceConstantAlpha: 255,
		AlphaFormat:         acSrcAlpha,
	}
	ok, _, _ := procUpdateLayeredWindow.Call(
		p.hwnd,
		hdcScreen,
		uintptr(unsafe.Pointer(&ptDst)),
		uintptr(unsafe.Pointer(&sz)),
		p.memDC,
		uintptr(unsafe.Pointer(&ptSrc)),
		0,
		uintptr(unsafe.Pointer(&blend)),
		ulwAlpha,
	)
	return ok != 0
}

func freePopupDIB(p *trayPopup) {
	if p == nil {
		return
	}
	if p.dibBmp != 0 {
		_, _, _ = procDeleteObject.Call(p.dibBmp)
		p.dibBmp = 0
	}
	if p.memDC != 0 {
		_, _, _ = procDeleteDC.Call(p.memDC)
		p.memDC = 0
	}
	p.dibBits = nil
	p.dibW = 0
	p.dibH = 0
}

func (wl *WindowsLifecycle) closePopup(recordDebounce bool) {
	p := wl.popup
	if p == nil {
		return
	}
	if p.closing {
		return
	}
	p.closing = true
	wl.popup = nil
	if recordDebounce {
		wl.lastPopupClose = time.Now()
		wl.lastClosedKind = p.kind
	}

	hwnd := p.hwnd
	p.hwnd = 0
	if hwnd != 0 {
		_, _, _ = procKillTimer.Call(hwnd, timerCopiedReset)
		lifecycleRegistryMu.Lock()
		delete(popupByHWND, hwnd)
		lifecycleRegistryMu.Unlock()
		_, _, _ = procDestroyWindow.Call(hwnd)
	}
	freePopupDIB(p)
}

func (wl *WindowsLifecycle) handlePopupMessage(hwnd uintptr, msg uint32, wParam, lParam uintptr) (bool, uintptr) {
	p := wl.popup
	if p == nil || p.hwnd != hwnd {
		return false, 0
	}

	switch msg {
	case wmActivate:
		if uint16(wParam&0xFFFF) == 0 {
			wl.closePopup(true)
		}
		return true, 0

	case wmKillFocus:
		wl.closePopup(true)
		return true, 0

	case wmSetCursor:
		if uint16(lParam&0xFFFF) == htClient {
			cur := wl.cursorArrow
			if p.overClickable && wl.cursorHand != 0 {
				cur = wl.cursorHand
			}
			if cur != 0 {
				_, _, _ = procSetCursor.Call(cur)
				return true, 1
			}
		}
		return false, 0

	case wmMouseMove:
		if !p.mouseTracking {
			tme := trackMouseEvent{
				CBSize:    uint32(unsafe.Sizeof(trackMouseEvent{})),
				DwFlags:   tmeLeave,
				HwndTrack: hwnd,
			}
			_, _, _ = procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
			p.mouseTracking = true
		}
		x, y := decodeMouseXY(lParam)
		wl.updatePopupHover(p, x, y)
		return true, 0

	case wmMouseLeave:
		p.mouseTracking = false
		p.overClickable = false
		changed := false
		if p.kind == popupPopover {
			if p.popoverState.HoveredService != "" || p.popoverState.HoveredTargetID != "" {
				p.popoverState.HoveredService = ""
				p.popoverState.HoveredTargetID = ""
				changed = true
			}
		} else if p.kind == popupQuickMenu {
			if p.quickMenuState.HoverIndex != -1 {
				p.quickMenuState.HoverIndex = -1
				changed = true
			}
		}
		if changed {
			wl.presentPopup(false)
		}
		return true, 0

	case wmMouseWheel:
		if p.kind == popupPopover && p.popoverLayout != nil && p.popoverLayout.MaxScroll > 0 {
			delta := int16((wParam >> 16) & 0xFFFF)
			step := int(math.Round(36.0 * p.popoverLayout.Scale))
			next := p.popoverState.ScrollOffset - int(delta)*step/120
			if next < 0 {
				next = 0
			}
			if next > p.popoverLayout.MaxScroll {
				next = p.popoverLayout.MaxScroll
			}
			if next != p.popoverState.ScrollOffset {
				p.popoverState.ScrollOffset = next
				wl.presentPopup(false)
			}
		}
		return true, 0

	case wmLButtonDown:
		x, y := decodeMouseXY(lParam)
		if !popupPointInCard(p, x, y) {
			wl.closePopup(true)
			return true, 0
		}
		wl.updatePopupHover(p, x, y)
		return true, 0

	case wmLButtonUp, wmLButtonDblClk:
		x, y := decodeMouseXY(lParam)
		if !popupPointInCard(p, x, y) {
			wl.closePopup(true)
			return true, 0
		}
		wl.handlePopupClick(p, x, y)
		return true, 0

	case wmTimer:
		if wParam == timerCopiedReset {
			_, _, _ = procKillTimer.Call(hwnd, timerCopiedReset)
			if p.popoverState.CopiedStatus || p.popoverState.CopiedServiceURL != "" {
				p.popoverState.CopiedStatus = false
				p.popoverState.CopiedServiceURL = ""
				wl.presentPopup(false)
			}
			return true, 0
		}

	case wmKeyDown:
		wl.handlePopupKeyDown(p, uint32(wParam))
		return true, 0

	case wmDpiChanged:
		newDPI := int(uint16(wParam & 0xFFFF))
		if newDPI >= 96 {
			p.dpi = newDPI
			wl.presentPopup(true)
		}
		return true, 0

	case wmDestroy:
		lifecycleRegistryMu.Lock()
		delete(popupByHWND, hwnd)
		lifecycleRegistryMu.Unlock()
		return true, 0
	}

	return false, 0
}

func decodeMouseXY(lParam uintptr) (int, int) {
	x := int(int16(lParam & 0xFFFF))
	y := int(int16((lParam >> 16) & 0xFFFF))
	return x, y
}

func popupPointInCard(p *trayPopup, x, y int) bool {
	if p.kind == popupPopover && p.popoverLayout != nil {
		return p.popoverLayout.InCard(x, y)
	}
	if p.kind == popupQuickMenu && p.quickLayout != nil {
		return p.quickLayout.InCard(x, y)
	}
	return false
}

func (wl *WindowsLifecycle) updatePopupHover(p *trayPopup, x, y int) {
	switch p.kind {
	case popupPopover:
		if p.popoverLayout == nil {
			return
		}
		target, hovSvc := p.popoverLayout.HitTest(x, y)
		targetID := ""
		if target != nil {
			targetID = target.ID
		}
		p.overClickable = target != nil
		if hovSvc != p.popoverState.HoveredService || targetID != p.popoverState.HoveredTargetID {
			p.popoverState.HoveredService = hovSvc
			p.popoverState.HoveredTargetID = targetID
			wl.presentPopup(false)
		}

	case popupQuickMenu:
		if p.quickLayout == nil {
			return
		}
		idx := p.quickLayout.HitTest(x, y)
		p.overClickable = idx >= 0
		if idx != p.quickMenuState.HoverIndex {
			p.quickMenuState.HoverIndex = idx
			wl.presentPopup(false)
		}
	}
}

func (wl *WindowsLifecycle) handlePopupClick(p *trayPopup, x, y int) {
	switch p.kind {
	case popupPopover:
		if p.popoverLayout == nil {
			return
		}
		target, _ := p.popoverLayout.HitTest(x, y)
		if target != nil {
			wl.executePopupAction(p, target.Action)
		}

	case popupQuickMenu:
		if p.quickLayout == nil {
			return
		}
		idx := p.quickLayout.HitTest(x, y)
		if act := p.quickLayout.RowAction(idx); act.Kind != trayui.ActionNone {
			wl.executePopupAction(p, act)
		}
	}
}

func (wl *WindowsLifecycle) handlePopupKeyDown(p *trayPopup, vk uint32) {
	ctrlDown := int16(getKeyState(vkControl)) < 0
	if ctrlDown {
		switch vk {
		case 'R':
			wl.executePopupAction(p, trayui.Action{Kind: trayui.ActionReloadConfig})
			return
		case 'C':
			wl.executePopupAction(p, trayui.Action{Kind: trayui.ActionCopyStatus})
			return
		}
	}

	switch vk {
	case vkEscape:
		wl.closePopup(true)

	case vkUp, vkDown:
		if p.kind == popupPopover && p.popoverLayout != nil {
			n := len(p.popoverLayout.FocusIDs)
			if n == 0 {
				return
			}
			cur := p.popoverState.FocusIndex
			if vk == vkDown {
				if cur < 0 || cur >= n-1 {
					cur = 0
				} else {
					cur++
				}
			} else {
				if cur <= 0 {
					cur = n - 1
				} else {
					cur--
				}
			}
			p.popoverState.FocusIndex = cur
			wl.ensureFocusedServiceVisible(p)
			wl.presentPopup(false)
		} else if p.kind == popupQuickMenu && p.quickLayout != nil {
			n := len(p.quickLayout.Rows)
			if n == 0 {
				return
			}
			cur := p.quickMenuState.FocusIndex
			if vk == vkDown {
				if cur < 0 || cur >= n-1 {
					cur = 0
				} else {
					cur++
				}
			} else {
				if cur <= 0 {
					cur = n - 1
				} else {
					cur--
				}
			}
			p.quickMenuState.FocusIndex = cur
			p.quickMenuState.HoverIndex = cur
			wl.presentPopup(false)
		}

	case vkRight:
		if p.kind == popupPopover && p.popoverLayout != nil {
			if t := p.popoverLayout.FocusedTarget(p.popoverState.FocusIndex); t != nil && t.Kind == trayui.TargetServiceRow {
				if p.popoverState.ExpandedService != t.ServiceName {
					p.popoverState.ExpandedService = t.ServiceName
					wl.presentPopup(true)
				}
			}
		}

	case vkLeft:
		if p.kind == popupPopover && p.popoverState.ExpandedService != "" {
			p.popoverState.ExpandedService = ""
			wl.presentPopup(true)
		}

	case vkReturn, vkSpace:
		if p.kind == popupPopover && p.popoverLayout != nil {
			if t := p.popoverLayout.FocusedTarget(p.popoverState.FocusIndex); t != nil {
				wl.executePopupAction(p, t.Action)
			}
		} else if p.kind == popupQuickMenu && p.quickLayout != nil {
			idx := p.quickMenuState.FocusIndex
			if idx < 0 {
				idx = p.quickMenuState.HoverIndex
			}
			if act := p.quickLayout.RowAction(idx); act.Kind != trayui.ActionNone {
				wl.executePopupAction(p, act)
			}
		}
	}
}

func (wl *WindowsLifecycle) ensureFocusedServiceVisible(p *trayPopup) {
	if p.popoverLayout == nil || p.popoverLayout.MaxScroll <= 0 {
		return
	}
	rowRect, ok := p.popoverLayout.ServiceRowRect(p.popoverState.FocusIndex)
	if !ok {
		return
	}
	if rowRect.Top < p.popoverLayout.ListRect.Top {
		p.popoverState.ScrollOffset -= int(p.popoverLayout.ListRect.Top - rowRect.Top)
	} else if rowRect.Bottom > p.popoverLayout.ListRect.Bottom {
		p.popoverState.ScrollOffset += int(rowRect.Bottom - p.popoverLayout.ListRect.Bottom)
	}
	if p.popoverState.ScrollOffset < 0 {
		p.popoverState.ScrollOffset = 0
	}
	if p.popoverState.ScrollOffset > p.popoverLayout.MaxScroll {
		p.popoverState.ScrollOffset = p.popoverLayout.MaxScroll
	}
}

func (wl *WindowsLifecycle) executePopupAction(p *trayPopup, action trayui.Action) {
	isPopover := p.kind == popupPopover

	switch action.Kind {
	case trayui.ActionNone:
		return

	case trayui.ActionOpenPopover:
		wl.mu.Lock()
		owner := wl.hwnd
		wl.mu.Unlock()
		wl.closePopup(false)
		if owner != 0 {
			wl.openPopup(owner, popupPopover)
		}
		return

	case trayui.ActionToggleServiceDrawer:
		if p.popoverState.ExpandedService == action.Service {
			p.popoverState.ExpandedService = ""
		} else {
			p.popoverState.ExpandedService = action.Service
		}
		wl.presentPopup(true)
		return

	case trayui.ActionCopyStatus:
		setClipboardText(p.hwnd, trayui.FormatStatusSummary(p.snap))
		if isPopover {
			p.popoverState.CopiedStatus = true
			_, _, _ = procSetTimer.Call(p.hwnd, timerCopiedReset, copiedResetMs, 0)
			wl.presentPopup(false)
		} else {
			wl.closePopup(true)
		}
		return

	case trayui.ActionServiceCopyURL:
		if action.Payload != "" {
			setClipboardText(p.hwnd, action.Payload)
		}
		if isPopover {
			p.popoverState.CopiedServiceURL = action.Service
			_, _, _ = procSetTimer.Call(p.hwnd, timerCopiedReset, copiedResetMs, 0)
			wl.presentPopup(false)
		} else {
			wl.closePopup(true)
		}
		return

	case trayui.ActionServiceStart:
		wl.dispatchAsync("service-start", func() {
			if wl.cb.OnServiceStart != nil {
				wl.cb.OnServiceStart(action.Service)
			}
		})
	case trayui.ActionServiceStop:
		wl.dispatchAsync("service-stop", func() {
			if wl.cb.OnServiceStop != nil {
				wl.cb.OnServiceStop(action.Service)
			}
		})
	case trayui.ActionServiceRestart:
		wl.dispatchAsync("service-restart", func() {
			if wl.cb.OnServiceRestart != nil {
				wl.cb.OnServiceRestart(action.Service)
			}
		})
	case trayui.ActionServiceReload:
		wl.dispatchAsync("service-reload", func() {
			if wl.cb.OnServiceReload != nil {
				wl.cb.OnServiceReload(action.Service)
			}
		})
	case trayui.ActionServiceSleep:
		wl.dispatchAsync("service-sleep", func() {
			if wl.cb.OnServiceSleep != nil {
				wl.cb.OnServiceSleep(action.Service)
			}
		})
	case trayui.ActionServiceWake:
		wl.dispatchAsync("service-wake", func() {
			if wl.cb.OnServiceWake != nil {
				wl.cb.OnServiceWake(action.Service)
			}
		})
	case trayui.ActionServiceOpenLog:
		wl.closePopup(true)
		wl.dispatchAsync("service-open-log", func() {
			if wl.cb.OnOpenServiceLog != nil {
				wl.cb.OnOpenServiceLog(action.Service)
			}
		})
		return
	case trayui.ActionStartAll:
		wl.closePopup(true)
		wl.dispatchAsync("start-all", wl.cb.OnStartAll)
		return
	case trayui.ActionRestartAll:
		wl.closePopup(true)
		wl.dispatchAsync("restart-all", wl.cb.OnRestartAll)
		return
	case trayui.ActionReloadConfig:
		if !isPopover {
			wl.closePopup(true)
		}
		wl.dispatchAsync("reload-config", wl.cb.OnReloadConfig)
		return
	case trayui.ActionOpenDashboard:
		wl.closePopup(true)
		wl.dispatchAsync("open-dashboard", wl.cb.OnOpenDashboard)
		return
	case trayui.ActionOpenLogs:
		wl.closePopup(true)
		wl.dispatchAsync("open-logs", wl.cb.OnOpenLogs)
		return
	case trayui.ActionOpenConfig:
		wl.closePopup(true)
		wl.dispatchAsync("open-config", wl.cb.OnOpenConfig)
		return
	case trayui.ActionExit:
		wl.closePopup(true)
		wl.dispatchAsync("exit", wl.cb.OnExit)
		return
	}

	if !isPopover {
		wl.closePopup(true)
	}
}

func (wl *WindowsLifecycle) dispatchAsync(label string, fn func()) {
	if fn == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil && wl.logger != nil {
				wl.logger.Printf("[winlifecycle] panic recovered in tray action %q: %v", label, r)
			}
		}()
		fn()
	}()
}

func (wl *WindowsLifecycle) dispatchSessionEnd(deadline time.Time) {
	defer func() {
		if r := recover(); r != nil && wl.logger != nil {
			wl.logger.Printf("[winlifecycle] panic recovered in session end callback: %v", r)
		}
	}()
	if wl.cb.OnSessionEnd != nil {
		wl.cb.OnSessionEnd(deadline)
		return
	}
	wl.cb.OnShutdown()
}

func (wl *WindowsLifecycle) runPowerWorker() {
	defer close(wl.powerDone)
	for {
		select {
		case <-wl.powerStopCh:
			return
		case ev := <-wl.powerCh:
			if wl.closing.Load() {
				return
			}
			wl.dispatchPowerEvent(ev)
		}
	}
}

func (wl *WindowsLifecycle) dispatchPowerEvent(eventType uint32) {
	defer func() {
		if r := recover(); r != nil && wl.logger != nil {
			wl.logger.Printf("[winlifecycle] panic recovered in power worker (event=%d): %v", eventType, r)
		}
	}()
	switch eventType {
	case pbtApmSuspend:
		if wl.cb.OnSuspend != nil {
			wl.cb.OnSuspend()
		}
	case pbtApmResumeAutomatic, pbtApmResumeSuspend:
		if wl.cb.OnResume != nil {
			wl.cb.OnResume()
		}
	}
}

func (wl *WindowsLifecycle) handlePowerEvent(eventType uint32) {
	wl.powerMu.Lock()
	defer wl.powerMu.Unlock()
	if wl.closing.Load() {
		return
	}
	now := time.Now()
	switch eventType {
	case pbtApmSuspend:
		if !wl.lastSuspend.IsZero() && now.Sub(wl.lastSuspend) < powerEventDedupWindow {
			return
		}
		wl.lastSuspend = now
		wl.lastResume = time.Time{} // Allow immediate resume after suspend.
	case pbtApmResumeAutomatic, pbtApmResumeSuspend:
		if !wl.lastResume.IsZero() && now.Sub(wl.lastResume) < powerEventDedupWindow {
			return
		}
		wl.lastResume = now
		wl.lastSuspend = time.Time{} // Allow rapid Modern Standby suspend->resume->suspend cycle.
	default:
		return
	}

	select {
	case wl.powerCh <- eventType:
	default:
		select {
		case <-wl.powerCh:
		default:
		}
		wl.powerCh <- eventType
	}
}

func (wl *WindowsLifecycle) ensureTrayIcons(hwnd uintptr) {
	size := taskbarSmallIconSize(hwnd)
	light := wl.systemLightTheme

	wl.mu.Lock()
	if len(wl.icons) == 5 && wl.iconSize == size && wl.iconLight == light {
		wl.mu.Unlock()
		return
	}
	oldIcons := wl.icons
	wl.icons = make(map[string]uintptr, 5)
	wl.iconSize = size
	wl.iconLight = light
	wl.mu.Unlock()

	states := []string{"ok", "sleeping", "starting", "degraded", "error"}
	built := make(map[string]uintptr, len(states))
	for _, st := range states {
		canvas := trayui.RenderTrayIcon(size, st, light)
		hIcon, err := createIconFromCanvas(canvas)
		if err != nil {
			if wl.logger != nil {
				wl.logger.Printf("[winlifecycle] create tray icon %q (%dpx) failed: %v", st, size, err)
			}
			continue
		}
		built[st] = hIcon
	}

	wl.mu.Lock()
	wl.icons = built
	wl.mu.Unlock()

	for _, hIcon := range oldIcons {
		if hIcon != 0 {
			_, _, _ = procDestroyIcon.Call(hIcon)
		}
	}
}

func (wl *WindowsLifecycle) selectIconLocked(state string) uintptr {
	switch state {
	case "ok", "":
		return wl.icons["ok"]
	case "sleeping":
		if h := wl.icons["sleeping"]; h != 0 {
			return h
		}
		return wl.icons["ok"]
	case "starting":
		if h := wl.icons["starting"]; h != 0 {
			return h
		}
		return wl.icons["ok"]
	case "error":
		return wl.icons["error"]
	case "degraded":
		return wl.icons["degraded"]
	default:
		if h := wl.icons["degraded"]; h != 0 {
			return h
		}
		return wl.icons["ok"]
	}
}

func (wl *WindowsLifecycle) currentSnapshot() TraySnapshot {
	if wl.cb.GetTraySnapshot != nil {
		return wl.cb.GetTraySnapshot()
	}
	return TraySnapshot{State: "ok", Tooltip: "McpHub", Version: Version}
}

func (wl *WindowsLifecycle) ensureTrayIconWithRetry(hwnd uintptr, forceAdd bool) {
	if wl.addOrUpdateTrayIcon(hwnd, forceAdd) {
		wl.trayRetryCount = 0
		_, _, _ = procKillTimer.Call(hwnd, timerTrayRetry)
		return
	}
	if wl.trayRetryCount >= trayRetryMax {
		_, _, _ = procKillTimer.Call(hwnd, timerTrayRetry)
		return
	}
	wl.trayRetryCount++
	_, _, _ = procSetTimer.Call(hwnd, timerTrayRetry, trayRetryMs, 0)
}

func (wl *WindowsLifecycle) addOrUpdateTrayIcon(hwnd uintptr, forceAdd bool) bool {
	snap := wl.currentSnapshot()
	tooltip := snap.Tooltip
	if tooltip == "" {
		tooltip = "McpHub"
	}

	wl.mu.Lock()
	hIcon := wl.selectIconLocked(snap.State)
	alreadyAdded := wl.trayAdded
	wl.mu.Unlock()

	var nid notifyIconDataW
	nid.CBSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = trayIconID
	nid.UFlags = nifMessage | nifTip
	nid.UCallbackMessage = wmAppTrayCallback
	if hIcon != 0 {
		nid.UFlags |= nifIcon
		nid.HIcon = hIcon
	}
	copyUTF16Truncated(nid.SzTip[:], tooltip)

	op := uintptr(nimModify)
	if forceAdd || !alreadyAdded {
		op = nimAdd
	}
	ok, _, errno := procShellNotifyIcon.Call(op, uintptr(unsafe.Pointer(&nid)))
	if ok == 0 && op == nimModify {
		ok, _, errno = procShellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	} else if ok == 0 && op == nimAdd {
		ok, _, errno = procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
	}
	if ok != 0 {
		wl.mu.Lock()
		wl.trayAdded = true
		wl.mu.Unlock()
		return true
	}
	if wl.logger != nil {
		wl.logger.Printf("[winlifecycle] Shell_NotifyIconW failed (op=%d): %v", op, errno)
	}
	return false
}

func (wl *WindowsLifecycle) destroyTrayAndIcons(hwnd uintptr) {
	wl.mu.Lock()
	added := wl.trayAdded
	wl.trayAdded = false
	icons := wl.icons
	wl.icons = nil
	wl.mu.Unlock()

	if added && hwnd != 0 {
		var nid notifyIconDataW
		nid.CBSize = uint32(unsafe.Sizeof(nid))
		nid.HWnd = hwnd
		nid.UID = trayIconID
		_, _, _ = procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	}
	for _, hIcon := range icons {
		if hIcon != 0 {
			_, _, _ = procDestroyIcon.Call(hIcon)
		}
	}
}

func (wl *WindowsLifecycle) drainAndShowBalloons(hwnd uintptr) {
	wl.balloonMu.Lock()
	batch := wl.balloonQueue
	wl.balloonQueue = nil
	wl.balloonMu.Unlock()

	if len(batch) == 0 {
		return
	}
	// Show the most recent notification to avoid balloon backlog spam.
	b := batch[len(batch)-1]

	wl.mu.Lock()
	added := wl.trayAdded
	wl.mu.Unlock()
	if !added {
		return
	}

	var flags uint32 = niifInfo | niifRespectQuietTime
	switch strings.ToLower(strings.TrimSpace(b.severity)) {
	case "warning", "warn", "degraded":
		flags = niifWarning | niifRespectQuietTime
	case "error", "err", "critical":
		flags = niifError | niifRespectQuietTime
	}

	var nid notifyIconDataW
	nid.CBSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = trayIconID
	nid.UFlags = nifInfo
	nid.DwInfoFlags = flags
	copyUTF16Truncated(nid.SzInfoTitle[:], b.title)
	copyUTF16Truncated(nid.SzInfo[:], b.message)
	_, _, _ = procShellNotifyIcon.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

// NotifyBalloon queues a non-blocking Windows System Tray toast/balloon notification (NIF_INFO).
// severity may be "info", "warning", or "error". Safe to call from any goroutine.
func (wl *WindowsLifecycle) NotifyBalloon(title, message, severity string) {
	if wl == nil || !wl.enableTray || wl.closing.Load() {
		return
	}
	if strings.TrimSpace(title) == "" && strings.TrimSpace(message) == "" {
		return
	}
	wl.mu.Lock()
	hwnd := wl.hwnd
	closed := wl.closed
	wl.mu.Unlock()
	if closed || hwnd == 0 {
		return
	}

	wl.balloonMu.Lock()
	if len(wl.balloonQueue) >= 4 {
		wl.balloonQueue = wl.balloonQueue[len(wl.balloonQueue)-3:]
	}
	wl.balloonQueue = append(wl.balloonQueue, balloonMsg{
		title:    title,
		message:  message,
		severity: severity,
	})
	wl.balloonMu.Unlock()

	_, _, _ = procPostMessageW.Call(hwnd, wmAppBalloon, 0, 0)
}

// RefreshTray posts a coalesced non-blocking notification to refresh the System Tray icon,
// tooltip, and any currently open Control Popover.
func (wl *WindowsLifecycle) RefreshTray() {
	if wl == nil || !wl.enableTray || wl.closing.Load() {
		return
	}
	if !wl.refreshPending.CompareAndSwap(false, true) {
		return
	}
	wl.mu.Lock()
	hwnd := wl.hwnd
	closed := wl.closed
	wl.mu.Unlock()
	if closed || hwnd == 0 || wl.closing.Load() {
		wl.refreshPending.Store(false)
		return
	}
	ok, _, _ := procPostMessageW.Call(hwnd, wmAppRefreshTray, 0, 0)
	if ok == 0 {
		wl.refreshPending.Store(false)
	}
}

// NotifyShutdownComplete signals WM_ENDSESSION that supervisor cleanup has finished.
func (wl *WindowsLifecycle) NotifyShutdownComplete() {
	if wl == nil {
		return
	}
	wl.shutdownDoneOnce.Do(func() {
		close(wl.shutdownDone)
	})
}

// Close destroys the hidden window, closes any open popup, removes the tray icon (if enabled),
// unregisters power notifications, and waits (bounded) for the Win32 message-loop thread to exit.
func (wl *WindowsLifecycle) Close() error {
	if wl == nil {
		return nil
	}
	wl.closeOnce.Do(func() {
		wl.closing.Store(true)
		wl.mu.Lock()
		hwnd := wl.hwnd
		closed := wl.closed
		wl.mu.Unlock()

		waitDur := winCloseWaitTimeout
		if !closed && hwnd != 0 {
			ok, _, errno := procPostMessageW.Call(hwnd, wmAppClose, 0, 0)
			if ok == 0 {
				if wl.logger != nil {
					wl.logger.Printf("[winlifecycle] PostMessageW(wmAppClose) failed: %v", errno)
				}
				waitDur = 500 * time.Millisecond
			}
		}
		select {
		case <-wl.doneCh:
		case <-time.After(waitDur):
			if wl.logger != nil {
				wl.logger.Printf("[winlifecycle] timed out after %s waiting for lifecycle loop exit", waitDur)
			}
		}
	})
	return nil
}

// createIconFromCanvas converts a premultiplied BGRA trayui.Canvas into a Win32 HICON.
func createIconFromCanvas(canvas *trayui.Canvas) (uintptr, error) {
	if canvas == nil || canvas.Width <= 0 || canvas.Height <= 0 {
		return 0, fmt.Errorf("empty icon canvas")
	}
	w, h := canvas.Width, canvas.Height

	var bmi bitmapInfo
	bmi.BmiHeader = bitmapInfoHeader{
		BiSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		BiWidth:       int32(w),
		BiHeight:      -int32(h), // Top-down DIB
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: biRGB,
	}

	var bitsPtr unsafe.Pointer
	hColor, _, errno := procCreateDIBSection.Call(
		0,
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bitsPtr)),
		0,
		0,
	)
	if hColor == 0 || bitsPtr == nil {
		return 0, fmt.Errorf("CreateDIBSection: %w", errno)
	}
	defer procDeleteObject.Call(hColor)

	dst := unsafe.Slice((*byte)(bitsPtr), w*h*4)
	copy(dst, canvas.Pix)

	maskBits := make([]byte, ((w+15)/16)*2*h)
	hMask, _, errno := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, uintptr(unsafe.Pointer(&maskBits[0])))
	if hMask == 0 {
		return 0, fmt.Errorf("CreateBitmap: %w", errno)
	}
	defer procDeleteObject.Call(hMask)

	ii := iconInfo{
		FIcon:    1,
		HbmMask:  hMask,
		HbmColor: hColor,
	}
	hIcon, _, errno := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	if hIcon == 0 {
		return 0, fmt.Errorf("CreateIconIndirect: %w", errno)
	}
	return hIcon, nil
}

func readPersonalizeThemes() (appsLight bool, systemLight bool) {
	// Defaults: Windows apps light=true, taskbar dark=false if registry keys are absent.
	appsLight = true
	systemLight = false

	subkey, err := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`)
	if err != nil {
		return appsLight, systemLight
	}
	var hKey uintptr
	r, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(subkey)), 0, keyQueryValue, uintptr(unsafe.Pointer(&hKey)))
	if r != 0 || hKey == 0 {
		return appsLight, systemLight
	}
	defer procRegCloseKey.Call(hKey)

	if v, ok := queryRegDWORD(hKey, "AppsUseLightTheme"); ok {
		appsLight = v != 0
	}
	if v, ok := queryRegDWORD(hKey, "SystemUsesLightTheme"); ok {
		systemLight = v != 0
	}
	return appsLight, systemLight
}

func queryRegDWORD(hKey uintptr, name string) (uint32, bool) {
	valPtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return 0, false
	}
	var typ uint32
	var val uint32
	size := uint32(unsafe.Sizeof(val))
	r, _, _ := procRegQueryValEx.Call(
		hKey,
		uintptr(unsafe.Pointer(valPtr)),
		0,
		uintptr(unsafe.Pointer(&typ)),
		uintptr(unsafe.Pointer(&val)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r != 0 || typ != regDword {
		return 0, false
	}
	return val, true
}

func taskbarSmallIconSize(fallbackHWND uintptr) int {
	dpi := taskbarDPI(fallbackHWND)
	if procGetSystemMetricsForDpi.Find() == nil {
		sz, _, _ := procGetSystemMetricsForDpi.Call(smCxSmIcon, uintptr(dpi))
		if sz >= 16 && sz <= 64 {
			return int(sz)
		}
	}
	sz := int(math.Round(16.0 * float64(dpi) / 96.0))
	if sz < 16 {
		sz = 16
	}
	if sz > 64 {
		sz = 64
	}
	return sz
}

func taskbarDPI(fallbackHWND uintptr) int {
	if procGetDpiForWindow.Find() == nil {
		if cls, err := syscall.UTF16PtrFromString("Shell_TrayWnd"); err == nil {
			trayHWND, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
			if trayHWND != 0 {
				if d, _, _ := procGetDpiForWindow.Call(trayHWND); d >= 96 {
					return int(d)
				}
			}
		}
		if fallbackHWND != 0 {
			if d, _, _ := procGetDpiForWindow.Call(fallbackHWND); d >= 96 {
				return int(d)
			}
		}
	}
	return 96
}

func getTrayIconRect(hwnd uintptr) (trayui.Rect, bool) {
	if procShellNotifyIconGetRect.Find() != nil || hwnd == 0 {
		return trayui.Rect{}, false
	}
	id := notifyIconIdentifier{
		CBSize: uint32(unsafe.Sizeof(notifyIconIdentifier{})),
		HWnd:   hwnd,
		UID:    trayIconID,
	}
	var rc winRect
	hr, _, _ := procShellNotifyIconGetRect.Call(uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&rc)))
	if hr != 0 || rc.Right <= rc.Left || rc.Bottom <= rc.Top {
		return trayui.Rect{}, false
	}
	return trayui.Rect{Left: rc.Left, Top: rc.Top, Right: rc.Right, Bottom: rc.Bottom}, true
}

func monitorWorkAreaForRect(r trayui.Rect) trayui.Rect {
	rc := winRect{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom}
	if rc.Right <= rc.Left {
		rc.Right = rc.Left + 1
	}
	if rc.Bottom <= rc.Top {
		rc.Bottom = rc.Top + 1
	}
	hMon, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&rc)), monitorDefaultToNearest)
	if hMon != 0 {
		var mi monitorInfo
		mi.CBSize = uint32(unsafe.Sizeof(mi))
		if ok, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
			return trayui.Rect{
				Left:   mi.RcWork.Left,
				Top:    mi.RcWork.Top,
				Right:  mi.RcWork.Right,
				Bottom: mi.RcWork.Bottom,
			}
		}
	}
	w, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	h, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	if w == 0 {
		w = 1920
	}
	if h == 0 {
		h = 1040
	}
	return trayui.Rect{Left: 0, Top: 0, Right: int32(w), Bottom: int32(h)}
}

func monitorDPIForRect(r trayui.Rect, fallbackHWND uintptr) int {
	rc := winRect{Left: r.Left, Top: r.Top, Right: r.Right, Bottom: r.Bottom}
	if rc.Right <= rc.Left {
		rc.Right = rc.Left + 1
	}
	if rc.Bottom <= rc.Top {
		rc.Bottom = rc.Top + 1
	}
	hMon, _, _ := procMonitorFromRect.Call(uintptr(unsafe.Pointer(&rc)), monitorDefaultToNearest)
	if hMon != 0 && procGetDpiForMonitor.Find() == nil {
		var dpiX, dpiY uint32
		hr, _, _ := procGetDpiForMonitor.Call(hMon, mdtEffectiveDPI, uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY)))
		if hr == 0 && dpiX >= 96 {
			return int(dpiX)
		}
	}
	return taskbarDPI(fallbackHWND)
}

func getKeyState(vk uint32) uint16 {
	r, _, _ := procGetKeyState.Call(uintptr(vk))
	return uint16(r)
}

func setClipboardText(hwnd uintptr, text string) {
	if text == "" {
		return
	}
	u16 := utf16.Encode([]rune(text))
	u16 = append(u16, 0)
	nBytes := uintptr(len(u16) * 2)

	ok, _, _ := procOpenClipboard.Call(hwnd)
	if ok == 0 {
		return
	}
	defer procCloseClipboard.Call()
	_, _, _ = procEmptyClipboard.Call()

	hMem, _, _ := procGlobalAlloc.Call(gmemMoveable, nBytes)
	if hMem == 0 {
		return
	}
	ptr, _, _ := procGlobalLock.Call(hMem)
	if ptr == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
		return
	}
	_, _, _ = procRtlMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&u16[0])), nBytes)
	_, _, _ = procGlobalUnlock.Call(hMem)

	res, _, _ := procSetClipboardData.Call(cfUnicodeText, hMem)
	if res == 0 {
		_, _, _ = procGlobalFree.Call(hMem)
	}
}

func toTrayUISnapshot(snap TraySnapshot) trayui.Snapshot {
	svcs := make([]trayui.ServiceItem, len(snap.Services))
	for i, s := range snap.Services {
		svcs[i] = trayui.ServiceItem{
			Name:          s.Name,
			State:         s.State,
			Healthy:       s.Healthy,
			Running:       s.Running,
			HasReload:     s.HasReload,
			CanSleep:      s.CanSleep,
			Mode:          s.Mode,
			StartMode:     s.StartMode,
			PID:           s.PID,
			Port:          s.Port,
			PrivateMB:     s.PrivateMB,
			Restarts:      s.Restarts,
			CrashRestarts: s.CrashRestarts,
			WakeCount:     s.WakeCount,
			ActiveReqs:    s.ActiveReqs,
			ActiveSSE:     s.ActiveSSE,
			TotalReqs:     s.TotalReqs,
			LogFile:       s.LogFile,
			ClientURL:     s.ClientURL,
			LastError:     s.LastError,
		}
	}
	ver := snap.Version
	if ver == "" {
		ver = Version
	}
	return trayui.Snapshot{
		State:          snap.State,
		Tooltip:        snap.Tooltip,
		Header:         snap.Header,
		Version:        ver,
		UptimeSec:      snap.UptimeSec,
		TotalPrivateMB: snap.TotalPrivateMB,
		ControlAddr:    snap.ControlAddr,
		ConfigPath:     snap.ConfigPath,
		LogDir:         snap.LogDir,
		RunningCount:   snap.RunningCount,
		SleepingCount:  snap.SleepingCount,
		StoppedCount:   snap.StoppedCount,
		StartingCount:  snap.StartingCount,
		ErrorCount:     snap.ErrorCount,
		Services:       svcs,
	}
}

// copyUTF16Truncated encodes s as UTF-16 into dst, NUL-terminating and truncating cleanly
// at len(dst)-1 code units without splitting a UTF-16 surrogate pair.
func copyUTF16Truncated(dst []uint16, s string) {
	if len(dst) == 0 {
		return
	}
	for i := range dst {
		dst[i] = 0
	}
	maxUnits := len(dst) - 1
	if maxUnits == 0 || s == "" {
		return
	}
	encoded := utf16.Encode([]rune(s))
	if len(encoded) > maxUnits {
		encoded = encoded[:maxUnits]
		// Avoid leaving a dangling high surrogate (0xD800..0xDBFF) at the end.
		if len(encoded) > 0 && encoded[len(encoded)-1] >= 0xD800 && encoded[len(encoded)-1] <= 0xDBFF {
			encoded = encoded[:len(encoded)-1]
		}
	}
	copy(dst, encoded)
	dst[len(encoded)] = 0
}
