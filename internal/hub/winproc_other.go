//go:build !windows

package hub

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning indicates another mcp-hub supervisor holds the instance lock.
var ErrAlreadyRunning = errors.New("another mcp-hub instance is already running")

// JobMemoryStats aggregates live process count and memory telemetry for a Job Object.
type JobMemoryStats struct {
	ProcessCount    int    `json:"processCount"`
	WorkingSetBytes uint64 `json:"workingSetBytes"`
	PrivateBytes    uint64 `json:"privateBytes"`
	PeakBytes       uint64 `json:"peakBytes"`
}

// ACLAuditResult reports the permission audit result for a sensitive file.
type ACLAuditResult struct {
	Path   string   `json:"path"`
	Exists bool     `json:"exists"`
	Secure bool     `json:"secure"`
	Issues []string `json:"issues,omitempty"`
}

func replaceFileAtomic(src, dst string) error {
	return os.Rename(src, dst)
}

type InstanceMutex struct{}

func AcquireInstanceMutex(name string) (*InstanceMutex, error) {
	return &InstanceMutex{}, nil
}

func (m *InstanceMutex) Close() error { return nil }

// LockTokenFile locks the containing directory, serializing token operations without a lock file.
func LockTokenFile(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock token directory %q: %w", dir, err)
	}
	return func() { _ = f.Close() }, nil
}

type ProcessJob struct{}

func NewKillOnCloseJob() (*ProcessJob, error)              { return &ProcessJob{}, nil }
func (j *ProcessJob) AssignPID(pid int) error              { return nil }
func (j *ProcessJob) MemoryStats() (JobMemoryStats, error) { return JobMemoryStats{}, nil }
func (j *ProcessJob) TerminateAndClose() error             { return nil }
func (j *ProcessJob) Close() error                         { return nil }

// currentProcessMemoryStats returns approximate working set and private bytes for the current process.
func currentProcessMemoryStats() (workingSetBytes, privateBytes uint64) {
	if raw, err := os.ReadFile("/proc/self/status"); err == nil {
		var vmRSS, rssAnon uint64
		for _, line := range strings.Split(string(raw), "\n") {
			if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
				fields := strings.Fields(rest)
				if len(fields) >= 1 {
					if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
						vmRSS = kb * 1024
					}
				}
			} else if rest, ok := strings.CutPrefix(line, "RssAnon:"); ok {
				fields := strings.Fields(rest)
				if len(fields) >= 1 {
					if kb, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
						rssAnon = kb * 1024
					}
				}
			}
		}
		if vmRSS > 0 {
			if rssAnon == 0 {
				rssAnon = vmRSS
			}
			return vmRSS, rssAnon
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys, ms.HeapInuse
}

func configureCmd(cmd *exec.Cmd) {}

func startCmdInJob(cmd *exec.Cmd, job *ProcessJob) error {
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", cmd.Path, err)
	}
	if err := job.AssignPID(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = job.TerminateAndClose()
		_ = cmd.Wait()
		return fmt.Errorf("assign pid %d to job object: %w", cmd.Process.Pid, err)
	}
	return nil
}

// WritePrivateFileAtomic writes data to a 0600 temp file in path's directory and atomically renames it.
func WritePrivateFileAtomic(path string, data []byte) error {
	return writePrivateFileAtomic(path, data, true)
}

// WritePrivateFileAtomicNew publishes a private file only if path does not exist.
func WritePrivateFileAtomicNew(path string, data []byte) error {
	return writePrivateFileAtomic(path, data, false)
}

func writePrivateFileAtomic(path string, data []byte, replace bool) error {
	cleanPath := filepath.Clean(path)
	dir := filepath.Dir(cleanPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-private-*")
	if err != nil {
		return fmt.Errorf("create temp file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if !replace {
		return os.Link(tmpPath, cleanPath)
	}
	if err := replaceFileAtomic(tmpPath, cleanPath); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// ResetFileACL grants access only to the current owner of one regular file or directory.
func ResetFileACL(path string, isDir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if isDir && info.IsDir() {
		return os.Chmod(path, 0o700)
	}
	if !isDir && info.Mode().IsRegular() {
		return os.Chmod(path, 0o600)
	}
	return fmt.Errorf("unexpected ACL target type: %q", path)
}

// AuditFileACL checks Unix file permissions, flagging group or world access bits.
func AuditFileACL(path string) ACLAuditResult {
	cleanPath := filepath.Clean(path)
	res := ACLAuditResult{Path: cleanPath}
	info, err := os.Stat(cleanPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Issues = []string{"file does not exist"}
			return res
		}
		res.Issues = []string{fmt.Sprintf("stat failed: %v", err)}
		return res
	}
	res.Exists = true
	if info.Mode().Perm()&0o077 != 0 {
		res.Issues = []string{fmt.Sprintf("file permissions %04o grant group/other access", info.Mode().Perm())}
		return res
	}
	res.Secure = true
	return res
}
