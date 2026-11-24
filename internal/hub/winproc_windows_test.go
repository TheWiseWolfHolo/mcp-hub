//go:build windows

package hub

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestProcessJob_StructSizes(t *testing.T) {
	wantPMC := uintptr(80)
	if runtime.GOARCH == "386" {
		wantPMC = 44
	}
	if got := unsafe.Sizeof(processMemoryCountersEx{}); got != wantPMC {
		t.Fatalf("unsafe.Sizeof(processMemoryCountersEx) = %d, want %d (arch=%s)", got, wantPMC, runtime.GOARCH)
	}
	if got := unsafe.Sizeof(jobObjectBasicProcessIDListHeader{}); got != 8 {
		t.Fatalf("unsafe.Sizeof(jobObjectBasicProcessIDListHeader) = %d, want 8", got)
	}
}

func TestAcquireInstanceMutex_SingleInstance(t *testing.T) {
	name := fmt.Sprintf(`Local\McpHubTestMutex_%d_%d`, os.Getpid(), time.Now().UnixNano())

	m1, err := AcquireInstanceMutex(name)
	if err != nil {
		t.Fatalf("first AcquireInstanceMutex: %v", err)
	}

	m2, err := AcquireInstanceMutex(name)
	if !errors.Is(err, ErrAlreadyRunning) {
		if m2 != nil {
			_ = m2.Close()
		}
		_ = m1.Close()
		t.Fatalf("second AcquireInstanceMutex err = %v, want ErrAlreadyRunning", err)
	}

	if err := m1.Close(); err != nil {
		t.Fatalf("close first mutex: %v", err)
	}

	m3, err := AcquireInstanceMutex(name)
	if err != nil {
		t.Fatalf("re-acquire after Close failed: %v", err)
	}
	_ = m3.Close()
}

func TestProcessJob_KillOnCloseTerminatesChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}

	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "HELPER_MODE=sleep")
	configureCmd(cmd)

	if err := cmd.Start(); err != nil {
		_ = job.Close()
		t.Fatalf("start helper sleep process: %v", err)
	}
	if err := job.AssignPID(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = job.Close()
		t.Fatalf("AssignPID: %v", err)
	}

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- cmd.Wait()
	}()

	// Give the child a moment to ensure it is actively sleeping (not exiting early).
	select {
	case err := <-waitDone:
		_ = job.Close()
		t.Fatalf("helper exited unexpectedly before job close: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	// Closing the Job Object handle alone MUST kill the child via JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
	if err := job.Close(); err != nil {
		_ = cmd.Process.Kill()
		t.Fatalf("job.Close: %v", err)
	}

	select {
	case <-waitDone:
		// Child terminated immediately on Job Object close!
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child process was NOT terminated when KillOnClose Job Object closed")
	}
}

func TestProcessJob_MemoryStatsOnRealChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	job, err := NewKillOnCloseJob()
	if err != nil {
		t.Fatalf("NewKillOnCloseJob: %v", err)
	}
	defer job.TerminateAndClose()

	cmd := exec.Command(exe, "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "HELPER_MODE=sleep")
	configureCmd(cmd)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper sleep process: %v", err)
	}
	defer func() {
		_ = job.TerminateAndClose()
		_ = cmd.Wait()
	}()

	if err := job.AssignPID(cmd.Process.Pid); err != nil {
		t.Fatalf("AssignPID: %v", err)
	}

	// Poll briefly until the child process has initialized its working set.
	var stats JobMemoryStats
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats, err = job.MemoryStats()
		if err != nil {
			t.Fatalf("MemoryStats: %v", err)
		}
		if stats.ProcessCount >= 1 && stats.WorkingSetBytes > 0 && stats.PrivateBytes > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}

	if stats.ProcessCount != 1 {
		t.Fatalf("ProcessCount = %d, want 1 (stats=%+v)", stats.ProcessCount, stats)
	}
	if stats.WorkingSetBytes == 0 {
		t.Errorf("WorkingSetBytes = 0, want > 0")
	}
	if stats.PrivateBytes == 0 {
		t.Errorf("PrivateBytes = 0, want > 0")
	}
	if stats.PeakBytes == 0 {
		t.Errorf("PeakBytes = 0, want > 0")
	}
}

func TestACL_WritePrivateFileAtomicAndAuditRoundTrip(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.token")

	if err := WritePrivateFileAtomic(secretPath, []byte("top-secret-token-value\n")); err != nil {
		t.Fatalf("WritePrivateFileAtomic: %v", err)
	}
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("read private file: %v", err)
	}
	if string(got) != "top-secret-token-value\n" {
		t.Fatalf("content = %q, want %q", string(got), "top-secret-token-value\n")
	}

	audit := AuditFileACL(secretPath)
	if !audit.Exists || !audit.Secure || len(audit.Issues) != 0 {
		t.Fatalf("expected WritePrivateFileAtomic output to pass AuditFileACL, got %+v", audit)
	}

	// Overwrite atomically and verify private ACL still holds.
	if err := WritePrivateFileAtomic(secretPath, []byte("rotated-secret-value\n")); err != nil {
		t.Fatalf("second WritePrivateFileAtomic: %v", err)
	}
	audit2 := AuditFileACL(secretPath)
	if !audit2.Exists || !audit2.Secure || len(audit2.Issues) != 0 {
		t.Fatalf("expected overwritten private file to pass AuditFileACL, got %+v", audit2)
	}

	// Apply a permissive DACL granting Everyone (WD) and BUILTIN\Users (BU) and verify AuditFileACL flags both.
	curSID, err := currentUserSIDString()
	if err != nil {
		t.Fatalf("currentUserSIDString: %v", err)
	}
	permissiveSDDL := "D:P(A;;FA;;;" + curSID + ")(A;;FR;;;WD)(A;;FR;;;BU)"
	if err := setSDDLForTest(secretPath, permissiveSDDL); err != nil {
		t.Fatalf("set permissive SDDL: %v", err)
	}
	badAudit := AuditFileACL(secretPath)
	if !badAudit.Exists || badAudit.Secure {
		t.Fatalf("expected permissive file to fail AuditFileACL, got %+v", badAudit)
	}
	joined := strings.Join(badAudit.Issues, "; ")
	if !strings.Contains(joined, "Everyone") || !strings.Contains(joined, "Users") {
		t.Errorf("expected Everyone and Users issues, got %v", badAudit.Issues)
	}
	if err := ResetFileACL(secretPath, false); err != nil {
		t.Fatalf("ResetFileACL: %v", err)
	}
	if got := AuditFileACL(secretPath); !got.Secure {
		t.Fatalf("repaired ACL failed audit: %v", got.Issues)
	}
	if err := WritePrivateFileAtomicNew(secretPath, []byte("must-not-replace")); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing path should fail no-clobber write, got %v", err)
	}
	if got, err := os.ReadFile(secretPath); err != nil || string(got) != "rotated-secret-value\n" {
		t.Fatalf("no-clobber write changed existing data: %v", err)
	}
	unlock, err := LockTokenFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if release, err := LockTokenFile(strings.ToUpper(secretPath)); err == nil {
		release()
		t.Fatal("duplicate token lock was granted")
	}

	// Non-existent path must report Exists=false, Secure=false.
	missing := AuditFileACL(filepath.Join(dir, "does-not-exist.token"))
	if missing.Exists || missing.Secure || len(missing.Issues) == 0 {
		t.Fatalf("expected missing file audit failure, got %+v", missing)
	}

	// Verify ResetFileACL on a file uses single current-user ACE (no SYSTEM ACE).
	if aces, err := listAllowAceSIDsForTest(secretPath); err != nil {
		t.Fatalf("listAllowAceSIDsForTest(file): %v", err)
	} else if len(aces) != 1 || aces[0] != curSID {
		t.Fatalf("file allow ACE SIDs after ResetFileACL = %v, want [%s]", aces, curSID)
	}

	// Verify ResetFileACL on a directory sets protected current-user-only OICI inheritance.
	privDir := filepath.Join(dir, "private-dir")
	if err := os.MkdirAll(privDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setSDDLForTest(privDir, permissiveSDDL); err != nil {
		t.Fatalf("set permissive dir SDDL: %v", err)
	}
	if err := ResetFileACL(privDir, true); err != nil {
		t.Fatalf("ResetFileACL(dir): %v", err)
	}
	if got := AuditFileACL(privDir); !got.Secure {
		t.Fatalf("repaired dir ACL failed audit: %v", got.Issues)
	}
	if aces, err := listAllowAceSIDsForTest(privDir); err != nil {
		t.Fatalf("listAllowAceSIDsForTest(dir): %v", err)
	} else if len(aces) != 1 || aces[0] != curSID {
		t.Fatalf("dir allow ACE SIDs after ResetFileACL = %v, want [%s]", aces, curSID)
	}
	inheritedChild := filepath.Join(privDir, "inherited.txt")
	if err := os.WriteFile(inheritedChild, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := AuditFileACL(inheritedChild); !got.Secure {
		t.Fatalf("child created inside OICI private dir failed audit: %v", got.Issues)
	}

	// Verify currentProcessMemoryStats returns live memory usage.
	ws, priv := currentProcessMemoryStats()
	if ws == 0 || priv == 0 {
		t.Fatalf("expected positive current process memory stats, got ws=%d priv=%d", ws, priv)
	}
}

func listAllowAceSIDsForTest(path string) ([]string, error) {
	pathPtr, err := syscall.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var pDacl, pSD uintptr
	ret, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		seFileObject,
		daclSecurityInformation,
		0, 0, uintptr(unsafe.Pointer(&pDacl)), 0, uintptr(unsafe.Pointer(&pSD)),
	)
	if ret != 0 {
		return nil, syscall.Errno(ret)
	}
	if pSD != 0 {
		defer syscall.LocalFree(syscall.Handle(pSD))
	}
	var sizeInfo aclSizeInformation
	ok, _, errno := procGetAclInformation.Call(pDacl, uintptr(unsafe.Pointer(&sizeInfo)), unsafe.Sizeof(sizeInfo), aclSizeInformationClass)
	if ok == 0 {
		return nil, errno
	}
	var sids []string
	for i := uint32(0); i < sizeInfo.AceCount; i++ {
		var acePtr unsafe.Pointer
		if ok, _, _ := procGetAce.Call(pDacl, uintptr(i), uintptr(unsafe.Pointer(&acePtr))); ok == 0 || acePtr == nil {
			continue
		}
		sid, permissive, err := inspectAllowAceSID(acePtr)
		if err != nil {
			return nil, err
		}
		if permissive {
			sids = append(sids, sid)
		}
	}
	return sids, nil
}

func setSDDLForTest(path, sddl string) error {
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return err
	}
	var sd uintptr
	ok, _, errno := procConvertStringSecurityDescriptorToSDW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	if ok == 0 || sd == 0 {
		return errno
	}
	defer syscall.LocalFree(syscall.Handle(sd))

	var daclPresent int32
	var dacl uintptr
	var daclDefaulted int32
	ok, _, errno = procGetSecurityDescriptorDacl.Call(
		sd,
		uintptr(unsafe.Pointer(&daclPresent)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&daclDefaulted)),
	)
	if ok == 0 || daclPresent == 0 {
		return errno
	}
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	ret, _, _ := procSetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		seFileObject,
		daclSecurityInformation|protectedDaclSecurityInfo,
		0, 0, dacl, 0,
	)
	if ret != 0 {
		return syscall.Errno(ret)
	}
	return nil
}
