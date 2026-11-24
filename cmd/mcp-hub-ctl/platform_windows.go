//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	shell32DLL        = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteW = shell32DLL.NewProc("ShellExecuteW")
)

const (
	winDetachedProcess       = 0x00000008
	winCreateNewProcessGroup = 0x00000200
	winCreateNoWindow        = 0x08000000
	winSWShowNormal          = 1
	winSEErrNoAssoc          = 31
)

func hubBinaryName() string {
	return "mcp-hub.exe"
}

func launchHubDetached(binary, configPath string) (int, error) {
	cmd := exec.Command(binary, "run", "--config", configPath)
	cmd.Dir = filepath.Dir(binary)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: winCreateNoWindow | winDetachedProcess | winCreateNewProcessGroup,
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func openExternal(target string) error {
	verbPtr, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	targetPtr, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	ret, _, callErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(verbPtr)),
		uintptr(unsafe.Pointer(targetPtr)),
		0,
		0,
		uintptr(winSWShowNormal),
	)
	if ret > 32 {
		return nil
	}
	// If there is no file association for a local regular file (e.g. .log), open it with notepad.exe.
	if ret == winSEErrNoAssoc && !strings.HasPrefix(strings.ToLower(target), "http://") && !strings.HasPrefix(strings.ToLower(target), "https://") {
		if st, statErr := os.Stat(target); statErr == nil && !st.IsDir() {
			cmd := exec.Command("notepad.exe", target)
			if err := cmd.Start(); err == nil {
				_ = cmd.Process.Release()
				return nil
			}
		}
	}
	if callErr != nil && callErr != syscall.Errno(0) {
		return fmt.Errorf("ShellExecuteW(%q) failed (code %d): %w", target, ret, callErr)
	}
	return fmt.Errorf("ShellExecuteW(%q) failed (code %d)", target, ret)
}
