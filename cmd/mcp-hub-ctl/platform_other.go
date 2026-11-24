//go:build !windows

package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
)

func hubBinaryName() string {
	return "mcp-hub"
}

func launchHubDetached(binary, configPath string) (int, error) {
	cmd := exec.Command(binary, "run", "--config", configPath)
	cmd.Dir = filepath.Dir(binary)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func openExternal(target string) error {
	opener := "xdg-open"
	if runtime.GOOS == "darwin" {
		opener = "open"
	}
	cmd := exec.Command(opener, target)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}
