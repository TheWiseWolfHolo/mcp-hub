//go:build !windows

package hub

import "log"

// WindowsLifecycle is a no-op stub on non-Windows platforms.
type WindowsLifecycle struct{}

// StartWindowsLifecycle returns a no-op WindowsLifecycle on non-Windows platforms.
func StartWindowsLifecycle(enableTray bool, cb LifecycleCallbacks, logger *log.Logger) (*WindowsLifecycle, error) {
	return &WindowsLifecycle{}, nil
}

// RefreshTray is a no-op on non-Windows platforms.
func (wl *WindowsLifecycle) RefreshTray() {}

// NotifyBalloon is a no-op on non-Windows platforms.
func (wl *WindowsLifecycle) NotifyBalloon(title, message, severity string) {}

// NotifyShutdownComplete is a no-op on non-Windows platforms.
func (wl *WindowsLifecycle) NotifyShutdownComplete() {}

// Close is a no-op on non-Windows platforms.
func (wl *WindowsLifecycle) Close() error { return nil }
