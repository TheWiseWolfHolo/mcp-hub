//go:build windows

package hub

import (
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub/trayui"
)

var procSendMessageW = user32.NewProc("SendMessageW")

func TestTray_Win32StructSizes(t *testing.T) {
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if got := unsafe.Sizeof(notifyIconDataW{}); got != 976 {
			t.Fatalf("unsafe.Sizeof(notifyIconDataW) = %d, want 976", got)
		}
		if got := unsafe.Sizeof(notifyIconIdentifier{}); got != 40 {
			t.Fatalf("unsafe.Sizeof(notifyIconIdentifier) = %d, want 40", got)
		}
		if got := unsafe.Sizeof(wndClassExW{}); got != 80 {
			t.Fatalf("unsafe.Sizeof(wndClassExW) = %d, want 80", got)
		}
		if got := unsafe.Sizeof(winMsg{}); got != 48 {
			t.Fatalf("unsafe.Sizeof(winMsg) = %d, want 48", got)
		}
		if got := unsafe.Sizeof(iconInfo{}); got != 32 {
			t.Fatalf("unsafe.Sizeof(iconInfo) = %d, want 32", got)
		}
		if got := unsafe.Sizeof(trackMouseEvent{}); got != 24 {
			t.Fatalf("unsafe.Sizeof(trackMouseEvent) = %d, want 24", got)
		}
	} else {
		if got := unsafe.Sizeof(notifyIconDataW{}); got != 956 {
			t.Fatalf("unsafe.Sizeof(notifyIconDataW) = %d, want 956", got)
		}
		if got := unsafe.Sizeof(notifyIconIdentifier{}); got != 28 {
			t.Fatalf("unsafe.Sizeof(notifyIconIdentifier) = %d, want 28", got)
		}
	}
	if got := unsafe.Sizeof(bitmapInfoHeader{}); got != 40 {
		t.Fatalf("unsafe.Sizeof(bitmapInfoHeader) = %d, want 40", got)
	}
	if got := unsafe.Sizeof(monitorInfo{}); got != 40 {
		t.Fatalf("unsafe.Sizeof(monitorInfo) = %d, want 40", got)
	}
	if got := unsafe.Sizeof(blendFunction{}); got != 4 {
		t.Fatalf("unsafe.Sizeof(blendFunction) = %d, want 4", got)
	}
}

func TestTray_CopyUTF16Truncated(t *testing.T) {
	var buf [128]uint16
	longASCII := strings.Repeat("x", 200)
	copyUTF16Truncated(buf[:], longASCII)
	if buf[127] != 0 {
		t.Fatalf("expected NUL terminator at index 127, got %d", buf[127])
	}
	decoded := string(utf16.Decode(buf[:127]))
	if len(decoded) != 127 {
		t.Fatalf("decoded len = %d, want 127", len(decoded))
	}

	// Surrogate pair boundary check: 126 ASCII chars + 1 supplementary rune (2 UTF-16 units)
	// must NOT leave an unpaired high surrogate at index 126.
	surrogateEdge := strings.Repeat("a", 126) + "🚀"
	copyUTF16Truncated(buf[:], surrogateEdge)
	if buf[126] != 0 || buf[127] != 0 {
		t.Fatalf("expected high surrogate backed off cleanly, got buf[126]=0x%x buf[127]=0x%x", buf[126], buf[127])
	}
}

func TestTray_CreateIconFromCanvasAllStatesAndSizes(t *testing.T) {
	states := []string{"ok", "sleeping", "starting", "degraded", "error"}
	for _, light := range []bool{false, true} {
		for _, sz := range []int{16, 20, 24, 32} {
			for _, st := range states {
				canvas := trayui.RenderTrayIcon(sz, st, light)
				hIcon, err := createIconFromCanvas(canvas)
				if err != nil || hIcon == 0 {
					t.Fatalf("createIconFromCanvas(%d,%q,light=%v): hIcon=%v err=%v", sz, st, light, hIcon, err)
				}
				ok, _, errno := procDestroyIcon.Call(hIcon)
				if ok == 0 {
					t.Fatalf("DestroyIcon(%v): %v", hIcon, errno)
				}
			}
		}
	}
}

func TestWinLifecycle_SettingChangeAndDpiSafeWithoutLParamDeref(t *testing.T) {
	wl, err := StartWindowsLifecycle(false, LifecycleCallbacks{}, nil)
	if err != nil {
		t.Fatalf("StartWindowsLifecycle: %v", err)
	}
	defer wl.Close()

	wl.mu.Lock()
	hwnd := wl.hwnd
	wl.mu.Unlock()

	// Passing an invalid/non-pointer lParam must NEVER crash WM_SETTINGCHANGE handling.
	_, _, _ = procSendMessageW.Call(hwnd, wmSettingChange, 0, 0x1234)
	_, _, _ = procSendMessageW.Call(hwnd, wmDpiChanged, (144<<16)|144, 0)

	// NotifyBalloon when tray is disabled must be a safe no-op.
	wl.NotifyBalloon("Test", "Hello", "warning")
}

func TestWinLifecycle_StartPowerDedupAndCloseCleanly(t *testing.T) {
	var suspendCount atomic.Int32
	var resumeCount atomic.Int32

	wl, err := StartWindowsLifecycle(false, LifecycleCallbacks{
		OnSuspend: func() { suspendCount.Add(1) },
		OnResume:  func() { resumeCount.Add(1) },
	}, nil)
	if err != nil {
		t.Fatalf("StartWindowsLifecycle(false): %v", err)
	}

	wl.mu.Lock()
	hwnd := wl.hwnd
	wl.mu.Unlock()
	if hwnd == 0 {
		_ = wl.Close()
		t.Fatal("expected non-zero hidden window handle")
	}

	// Fire two rapid suspend broadcasts and two rapid resume broadcasts (AUTOMATIC + SUSPEND);
	// deduplication must ensure each callback fires exactly once.
	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmSuspend, 0)
	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmSuspend, 0)
	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmResumeAutomatic, 0)
	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmResumeSuspend, 0)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if suspendCount.Load() >= 1 && resumeCount.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := suspendCount.Load(); got != 1 {
		t.Errorf("suspendCount = %d, want 1", got)
	}
	if got := resumeCount.Load(); got != 1 {
		t.Errorf("resumeCount = %d, want 1", got)
	}

	// Close must be fast and idempotent.
	if err := wl.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := wl.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestWinLifecycle_QueryEndSessionAndEndSession(t *testing.T) {
	deadlineCh := make(chan time.Time, 1)
	var wl *WindowsLifecycle
	var err error

	wl, err = StartWindowsLifecycle(false, LifecycleCallbacks{
		OnSessionEnd: func(deadline time.Time) {
			deadlineCh <- deadline
			wl.NotifyShutdownComplete()
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartWindowsLifecycle: %v", err)
	}
	defer wl.Close()

	wl.mu.Lock()
	hwnd := wl.hwnd
	wl.mu.Unlock()

	ret, _, _ := procSendMessageW.Call(hwnd, wmQueryEndSession, 0, 0)
	if ret != 1 {
		t.Fatalf("WM_QUERYENDSESSION returned %d, want 1", ret)
	}

	started := time.Now()
	_, _, _ = procPostMessageW.Call(hwnd, wmEndSession, 1, 0)

	select {
	case <-wl.doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for lifecycle loop to exit on WM_ENDSESSION")
	}

	select {
	case deadline := <-deadlineCh:
		if budget := deadline.Sub(started); budget <= 0 || budget >= 5*time.Second {
			t.Fatalf("session-end budget = %s, want between 0 and 5 seconds", budget)
		}
	default:
		t.Fatal("OnSessionEnd was not called")
	}
}

func TestWinLifecycle_PowerQueueKeepsLatestState(t *testing.T) {
	wl := &WindowsLifecycle{powerCh: make(chan uint32, 1)}
	wl.handlePowerEvent(pbtApmSuspend)
	wl.handlePowerEvent(pbtApmResumeAutomatic)
	if got := <-wl.powerCh; got != pbtApmResumeAutomatic {
		t.Fatalf("queued power event = %d, want resume", got)
	}
}

func TestWinLifecycle_OrderedPowerEventsAndCallbackPanicRecovery(t *testing.T) {
	var eventsMu sync.Mutex
	var events []string
	resumeDone := make(chan struct{})

	wl, err := StartWindowsLifecycle(false, LifecycleCallbacks{
		OnSuspend: func() {
			time.Sleep(120 * time.Millisecond)
			eventsMu.Lock()
			events = append(events, "suspend")
			eventsMu.Unlock()
		},
		OnResume: func() {
			eventsMu.Lock()
			events = append(events, "resume")
			eventsMu.Unlock()
			select {
			case <-resumeDone:
			default:
				close(resumeDone)
			}
		},
		GetTraySnapshot: func() TraySnapshot {
			panic("simulated tray snapshot panic")
		},
	}, nil)
	if err != nil {
		t.Fatalf("StartWindowsLifecycle: %v", err)
	}
	defer wl.Close()

	wl.mu.Lock()
	hwnd := wl.hwnd
	wl.mu.Unlock()

	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmSuspend, 0)
	_, _, _ = procSendMessageW.Call(hwnd, wmPowerBroadcast, pbtApmResumeAutomatic, 0)

	select {
	case <-resumeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ordered resume event")
	}

	eventsMu.Lock()
	got := append([]string(nil), events...)
	eventsMu.Unlock()
	if len(got) != 2 || got[0] != "suspend" || got[1] != "resume" {
		t.Fatalf("power callback order = %v, want [suspend resume]", got)
	}

	// Verify panic recovery inside WndProc does not crash the process.
	wl.enableTray = true
	_, _, _ = procSendMessageW.Call(hwnd, wmAppRefreshTray, 0, 0)
}
