//go:build windows

package hub

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// ErrAlreadyRunning indicates another mcp-hub supervisor holds the named mutex.
var ErrAlreadyRunning = errors.New("another mcp-hub instance is already running")

var (
	kernel32                      = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutexW              = kernel32.NewProc("CreateMutexW")
	procCreateJobObjectW          = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject   = kernel32.NewProc("SetInformationJobObject")
	procQueryInformationJobObject = kernel32.NewProc("QueryInformationJobObject")
	procAssignProcessToJobObject  = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject        = kernel32.NewProc("TerminateJobObject")
	procMoveFileExW               = kernel32.NewProc("MoveFileExW")
	procK32GetProcessMemoryInfo   = kernel32.NewProc("K32GetProcessMemoryInfo")

	ntdll               = syscall.NewLazyDLL("ntdll.dll")
	procNtResumeProcess = ntdll.NewProc("NtResumeProcess")

	advapi32                                 = syscall.NewLazyDLL("advapi32.dll")
	procConvertStringSecurityDescriptorToSDW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDacl            = advapi32.NewProc("GetSecurityDescriptorDacl")
	procSetKernelObjectSecurity              = advapi32.NewProc("SetKernelObjectSecurity")
	procGetSecurityInfo                      = advapi32.NewProc("GetSecurityInfo")
	procSetNamedSecurityInfoW                = advapi32.NewProc("SetNamedSecurityInfoW")
	procGetNamedSecurityInfoW                = advapi32.NewProc("GetNamedSecurityInfoW")
	procGetAclInformation                    = advapi32.NewProc("GetAclInformation")
	procGetAce                               = advapi32.NewProc("GetAce")
	procIsValidSid                           = advapi32.NewProc("IsValidSid")
)

const (
	errorAccessDenied                 = syscall.Errno(5)
	errorSharingViolation             = syscall.Errno(32)
	errorFileExists                   = syscall.Errno(80)
	errorAlreadyExists                = syscall.Errno(183)
	errorMoreData                     = syscall.Errno(234)
	createSuspended                   = 0x00000004
	createNoWindow                    = 0x08000000
	processSetQuota                   = 0x0100
	processSuspendResume              = 0x0800
	processQueryLimitedInformation    = 0x1000
	jobObjectBasicProcessIDList       = 3
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000

	movefileReplaceExisting = 0x00000001
	movefileWriteThrough    = 0x00000008

	sddlRevision1                 = 1
	seFileObject                  = 1
	ownerSecurityInformation      = 0x00000001
	daclSecurityInformation       = 0x00000004
	protectedDaclSecurityInfo     = 0x80000000
	aclSizeInformationClass       = 2
	accessAllowedAceType          = 0
	accessAllowedObjectAceType    = 5
	accessAllowedCallbackAceType  = 9
	accessAllowedCallbackObjType  = 11
	aceObjectTypePresent          = 0x1
	aceInheritedObjectTypePresent = 0x2

	sidLocalSystem        = "S-1-5-18"
	sidEveryone           = "S-1-1-0"
	sidAuthenticatedUsers = "S-1-5-11"
	sidBuiltinUsers       = "S-1-5-32-545"
	sidBuiltinAdmins      = "S-1-5-32-544"
)

type jobObjectBasicLimitInformation struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type ioCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobObjectExtendedLimitInfo struct {
	BasicLimitInformation jobObjectBasicLimitInformation
	IoInfo                ioCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

type jobObjectBasicProcessIDListHeader struct {
	NumberOfAssignedProcesses uint32
	NumberOfProcessIdsInList  uint32
}

type processMemoryCountersEx struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
	PrivateUsage               uintptr
}

type aclSizeInformation struct {
	AceCount      uint32
	AclBytesInUse uint32
	AclBytesFree  uint32
}

type aceHeader struct {
	AceType  uint8
	AceFlags uint8
	AceSize  uint16
}

// JobMemoryStats aggregates live process count and memory telemetry for a Job Object.
type JobMemoryStats struct {
	ProcessCount    int    `json:"processCount"`
	WorkingSetBytes uint64 `json:"workingSetBytes"`
	PrivateBytes    uint64 `json:"privateBytes"`
	PeakBytes       uint64 `json:"peakBytes"`
}

// ACLAuditResult reports the NTFS permission audit result for a sensitive file.
type ACLAuditResult struct {
	Path   string   `json:"path"`
	Exists bool     `json:"exists"`
	Secure bool     `json:"secure"`
	Issues []string `json:"issues,omitempty"`
}

// replaceFileAtomic atomically moves src onto dst using MoveFileExW so dst is never
// pre-deleted if src is locked or inaccessible on Windows.
// Retries transient ERROR_ACCESS_DENIED (5) and ERROR_SHARING_VIOLATION (32) with backoff (~1.5s total).
func replaceFileAtomic(src, dst string) error {
	return moveFileAtomic(src, dst, movefileReplaceExisting|movefileWriteThrough)
}

func moveFileAtomic(src, dst string, flags uintptr) error {
	srcPtr, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return &os.LinkError{Op: "replace", Old: src, New: dst, Err: err}
	}
	dstPtr, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return &os.LinkError{Op: "replace", Old: src, New: dst, Err: err}
	}

	backoffs := []time.Duration{
		10 * time.Millisecond,
		25 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		700 * time.Millisecond,
	}
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		ok, _, errno := procMoveFileExW.Call(
			uintptr(unsafe.Pointer(srcPtr)),
			uintptr(unsafe.Pointer(dstPtr)),
			flags,
		)
		if ok != 0 {
			return nil
		}
		lastErr = errno
		if (errno != errorAccessDenied && errno != errorSharingViolation) || attempt == len(backoffs) {
			break
		}
		time.Sleep(backoffs[attempt])
	}
	return &os.LinkError{Op: "replace", Old: src, New: dst, Err: lastErr}
}

// InstanceMutex guards single-instance execution using a Win32 Named Mutex.
type InstanceMutex struct {
	mu     sync.Mutex
	handle syscall.Handle
}

// AcquireInstanceMutex creates or opens a Win32 named mutex.
// If another instance already holds it, ErrAlreadyRunning is returned.
func AcquireInstanceMutex(name string) (*InstanceMutex, error) {
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("encode mutex name %q: %w", name, err)
	}
	// bInitialOwner = 0 avoids Win32 thread-affinity constraints in Go goroutines;
	// holding the kernel object handle open keeps the named mutex alive until Close/exit.
	h, _, errno := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(namePtr)))
	if h == 0 {
		if errno == errorAccessDenied {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("CreateMutexW(%q): %w", name, errno)
	}
	if errno == errorAlreadyExists {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return nil, ErrAlreadyRunning
	}
	return &InstanceMutex{handle: syscall.Handle(h)}, nil
}

// Close releases the Win32 named mutex handle.
func (m *InstanceMutex) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handle == 0 {
		return nil
	}
	err := syscall.CloseHandle(m.handle)
	m.handle = 0
	return err
}

// LockTokenFile acquires an exclusive lock for a token operation across CLI processes.
func LockTokenFile(path string) (func(), error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(absolute)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(filepath.Join(dir, filepath.Base(absolute)))
	digest := sha256.Sum256([]byte(key))
	mutex, err := AcquireInstanceMutex(`Global\McpHubToken-` + hex.EncodeToString(digest[:]))
	if errors.Is(err, ErrAlreadyRunning) {
		return nil, fmt.Errorf("another token operation is in progress for %q: %w", path, err)
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = mutex.Close() }, nil
}

// ProcessJob owns a Windows Job Object configured with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE.
// Closing the job (or supervisor exit/crash) terminates all processes in the job tree.
type ProcessJob struct {
	mu     sync.RWMutex
	handle syscall.Handle
}

// NewKillOnCloseJob creates a Job Object that kills all assigned processes on handle close.
func NewKillOnCloseJob() (*ProcessJob, error) {
	h, _, errno := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return nil, fmt.Errorf("CreateJobObjectW: %w", errno)
	}
	var info jobObjectExtendedLimitInfo
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	ok, _, errno := procSetInformationJobObject.Call(
		h,
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	if ok == 0 {
		_ = syscall.CloseHandle(syscall.Handle(h))
		return nil, fmt.Errorf("SetInformationJobObject: %w", errno)
	}
	return &ProcessJob{handle: syscall.Handle(h)}, nil
}

// AssignPID binds a running process (and any descendants it spawns) to this Job Object.
func (j *ProcessJob) AssignPID(pid int) error {
	if j == nil || pid <= 0 {
		return nil
	}
	j.mu.RLock()
	defer j.mu.RUnlock()
	hJob := j.handle
	if hJob == 0 {
		return errors.New("job object is closed")
	}

	hProc, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE|processSetQuota, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer syscall.CloseHandle(hProc)

	ok, _, errno := procAssignProcessToJobObject.Call(uintptr(hJob), uintptr(hProc))
	if ok == 0 {
		return fmt.Errorf("AssignProcessToJobObject(%d): %w", pid, errno)
	}
	return nil
}

// MemoryStats queries the Job Object's active PID list, peak memory usage, and sums
// per-process WorkingSetSize and PrivateUsage across all live processes in the job.
func (j *ProcessJob) MemoryStats() (JobMemoryStats, error) {
	if j == nil {
		return JobMemoryStats{}, nil
	}

	var stats JobMemoryStats
	j.mu.RLock()
	hJob := j.handle
	if hJob == 0 {
		j.mu.RUnlock()
		return JobMemoryStats{}, errors.New("job object is closed")
	}

	var extInfo jobObjectExtendedLimitInfo
	if ok, _, _ := procQueryInformationJobObject.Call(
		uintptr(hJob),
		jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&extInfo)),
		unsafe.Sizeof(extInfo),
		0,
	); ok != 0 {
		stats.PeakBytes = uint64(extInfo.PeakJobMemoryUsed)
	}

	pids, err := queryJobProcessIDs(hJob)
	j.mu.RUnlock()
	if err != nil {
		return stats, err
	}

	for _, pid := range pids {
		if pid == 0 {
			continue
		}
		hProc, openErr := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
		if openErr != nil {
			// Process may have exited between listing PIDs and opening the handle.
			continue
		}
		var pmc processMemoryCountersEx
		pmc.CB = uint32(unsafe.Sizeof(pmc))
		ok, _, _ := procK32GetProcessMemoryInfo.Call(
			uintptr(hProc),
			uintptr(unsafe.Pointer(&pmc)),
			uintptr(pmc.CB),
		)
		_ = syscall.CloseHandle(hProc)
		if ok == 0 {
			continue
		}
		stats.ProcessCount++
		stats.WorkingSetBytes += uint64(pmc.WorkingSetSize)
		stats.PrivateBytes += uint64(pmc.PrivateUsage)
	}
	return stats, nil
}

func queryJobProcessIDs(hJob syscall.Handle) ([]uintptr, error) {
	const (
		hdrSize      = int(unsafe.Sizeof(jobObjectBasicProcessIDListHeader{}))
		ptrSize      = int(unsafe.Sizeof(uintptr(0)))
		maxCapPIDs   = 65536
		maxGrowTries = 8
	)
	capPIDs := 16
	for attempt := 0; attempt < maxGrowTries; attempt++ {
		byteLen := hdrSize + capPIDs*ptrSize
		// Allocate uint64 backing slice for guaranteed 8-byte alignment on all architectures.
		buf := make([]uint64, (byteLen+7)/8)
		var retLen uint32
		ok, _, errno := procQueryInformationJobObject.Call(
			uintptr(hJob),
			jobObjectBasicProcessIDList,
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)*8),
			uintptr(unsafe.Pointer(&retLen)),
		)
		base := unsafe.Pointer(&buf[0])
		hdr := (*jobObjectBasicProcessIDListHeader)(base)
		if ok != 0 {
			n := int(hdr.NumberOfProcessIdsInList)
			maxFit := (len(buf)*8 - hdrSize) / ptrSize
			if n > maxFit {
				n = maxFit
			}
			if n <= 0 {
				return nil, nil
			}
			src := unsafe.Slice((*uintptr)(unsafe.Add(base, hdrSize)), n)
			out := make([]uintptr, n)
			copy(out, src)
			return out, nil
		}
		if errno != errorMoreData {
			return nil, fmt.Errorf("QueryInformationJobObject(BasicProcessIdList): %w", errno)
		}
		nextCap := int(hdr.NumberOfAssignedProcesses) + 8
		if nextCap <= capPIDs {
			nextCap = capPIDs * 2
		}
		if nextCap > maxCapPIDs {
			nextCap = maxCapPIDs
		}
		capPIDs = nextCap
	}
	return nil, errors.New("QueryInformationJobObject(BasicProcessIdList): exceeded retry limit")
}

// TerminateAndClose immediately terminates every process in the job tree and closes the handle.
func (j *ProcessJob) TerminateAndClose() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	hJob := j.handle
	j.handle = 0
	if hJob == 0 {
		return nil
	}
	_, _, _ = procTerminateJobObject.Call(uintptr(hJob), 1)
	return syscall.CloseHandle(hJob)
}

// Close closes the Job Object handle (triggering JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE).
func (j *ProcessJob) Close() error {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	hJob := j.handle
	j.handle = 0
	if hJob == 0 {
		return nil
	}
	return syscall.CloseHandle(hJob)
}

func configureCmd(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}

// startCmdInJob launches cmd suspended so the child is assigned to the Job Object BEFORE
// executing user code (preventing grandchild escape), then resumes the process.
func startCmdInJob(cmd *exec.Cmd, job *ProcessJob) error {
	useSuspended := job != nil && procNtResumeProcess.Find() == nil
	if useSuspended {
		if cmd.SysProcAttr == nil {
			configureCmd(cmd)
		}
		cmd.SysProcAttr.CreationFlags |= createSuspended
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %q: %w", cmd.Path, err)
	}

	// Fail closed if Job Object assignment fails: terminate child immediately.
	if err := job.AssignPID(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = job.TerminateAndClose()
		_ = cmd.Wait()
		return fmt.Errorf("assign pid %d to job object: %w", cmd.Process.Pid, err)
	}

	if useSuspended {
		if err := resumeProcessPID(cmd.Process.Pid); err != nil {
			_ = cmd.Process.Kill()
			_ = job.TerminateAndClose()
			_ = cmd.Wait()
			return fmt.Errorf("resume pid %d after job assignment: %w", cmd.Process.Pid, err)
		}
	}
	return nil
}

func resumeProcessPID(pid int) error {
	hProc, err := syscall.OpenProcess(processSuspendResume, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("OpenProcess(PROCESS_SUSPEND_RESUME, %d): %w", pid, err)
	}
	defer syscall.CloseHandle(hProc)

	status, _, _ := procNtResumeProcess.Call(uintptr(hProc))
	if status != 0 {
		return fmt.Errorf("NtResumeProcess(%d) returned NTSTATUS 0x%x", pid, status)
	}
	return nil
}

func currentUserSIDString() (string, error) {
	tok, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("OpenCurrentProcessToken: %w", err)
	}
	defer tok.Close()

	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("GetTokenUser: %w", err)
	}
	if tu == nil || tu.User.Sid == nil {
		return "", errors.New("GetTokenUser returned empty SID")
	}
	sidStr, err := tu.User.Sid.String()
	if err != nil {
		return "", fmt.Errorf("ConvertSidToStringSidW: %w", err)
	}
	return sidStr, nil
}

func securityDescriptor(sddl string) (uintptr, error) {
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return 0, fmt.Errorf("encode SDDL: %w", err)
	}

	var sd uintptr
	ok, _, errno := procConvertStringSecurityDescriptorToSDW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		sddlRevision1,
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	if ok == 0 || sd == 0 {
		return 0, fmt.Errorf("ConvertStringSecurityDescriptorToSecurityDescriptorW: %w", errno)
	}
	return sd, nil
}

func createPrivateTempFile(dir string) (*os.File, string, error) {
	sidStr, err := currentUserSIDString()
	if err != nil {
		return nil, "", err
	}
	// Set the owner and protected DACL before creating the file.
	sd, err := securityDescriptor("O:" + sidStr + "D:P(A;;FA;;;" + sidStr + ")")
	if err != nil {
		return nil, "", err
	}
	defer syscall.LocalFree(syscall.Handle(sd))

	sa := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}

	var randBytes [12]byte
	for attempt := 0; attempt < 32; attempt++ {
		if _, err := rand.Read(randBytes[:]); err != nil {
			return nil, "", fmt.Errorf("generate temp filename: %w", err)
		}
		tmpPath := filepath.Join(dir, ".tmp-private-"+hex.EncodeToString(randBytes[:]))
		tmpPtr, err := syscall.UTF16PtrFromString(tmpPath)
		if err != nil {
			return nil, "", fmt.Errorf("encode temp path %q: %w", tmpPath, err)
		}
		h, createErr := syscall.CreateFile(
			tmpPtr,
			syscall.GENERIC_READ|syscall.GENERIC_WRITE,
			0, // exclusive sharing while writing secret
			&sa,
			syscall.CREATE_NEW,
			syscall.FILE_ATTRIBUTE_NORMAL,
			0,
		)
		if createErr == nil {
			return os.NewFile(uintptr(h), tmpPath), tmpPath, nil
		}
		if errors.Is(createErr, errorFileExists) || errors.Is(createErr, errorAlreadyExists) {
			continue
		}
		return nil, "", fmt.Errorf("CreateFileW(%q): %w", tmpPath, createErr)
	}
	return nil, "", fmt.Errorf("failed to allocate unique temp file in %q", dir)
}

// WritePrivateFileAtomic creates a temporary file directly with a protected current-user-only
// NTFS SECURITY_ATTRIBUTES descriptor via CreateFileW(CREATE_NEW) (zero TOCTOU window),
// writes and syncs data, and atomically replaces path via MoveFileExW.
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

	tmp, tmpPath, err := createPrivateTempFile(dir)
	if err != nil {
		return fmt.Errorf("create private temp file in %q: %w", dir, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpPath, err)
	}
	flags := uintptr(movefileWriteThrough)
	if replace {
		flags |= movefileReplaceExisting
	}
	if err := moveFileAtomic(tmpPath, cleanPath, flags); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// currentProcessMemoryStats returns the current process's WorkingSetSize and PrivateUsage in bytes.
func currentProcessMemoryStats() (workingSetBytes, privateBytes uint64) {
	hProc, err := syscall.GetCurrentProcess()
	if err != nil {
		return 0, 0
	}
	var pmc processMemoryCountersEx
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ok, _, _ := procK32GetProcessMemoryInfo.Call(
		uintptr(hProc),
		uintptr(unsafe.Pointer(&pmc)),
		uintptr(pmc.CB),
	)
	if ok == 0 {
		return 0, 0
	}
	return uint64(pmc.WorkingSetSize), uint64(pmc.PrivateUsage)
}

func handleOwnerSID(h syscall.Handle) (string, error) {
	var pOwner *syscall.SID
	var pSD uintptr
	ret, _, _ := procGetSecurityInfo.Call(
		uintptr(h),
		seFileObject,
		ownerSecurityInformation,
		uintptr(unsafe.Pointer(&pOwner)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&pSD)),
	)
	if ret != 0 {
		return "", syscall.Errno(ret)
	}
	if pSD != 0 {
		defer syscall.LocalFree(syscall.Handle(pSD))
	}
	if pOwner == nil {
		return "", errors.New("empty owner SID")
	}
	valid, _, _ := procIsValidSid.Call(uintptr(unsafe.Pointer(pOwner)))
	if valid == 0 {
		return "", errors.New("invalid owner SID")
	}
	return pOwner.String()
}

// ResetFileACL sets the current-user owner (when different) and replaces the DACL on one file or
// directory without following its final reparse point.
// Files receive a protected current-user-only ACE; directories receive the same ACE with OICI inheritance.
func ResetFileACL(path string, isDir bool) error {
	sid, err := currentUserSIDString()
	if err != nil {
		return err
	}
	sddl := "O:" + sid + "D:P(A;;FA;;;" + sid + ")"
	if isDir {
		sddl = "O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")"
	}
	sd, err := securityDescriptor(sddl)
	if err != nil {
		return err
	}
	defer syscall.LocalFree(syscall.Handle(sd))

	name, err := syscall.UTF16PtrFromString(filepath.Clean(path))
	if err != nil {
		return err
	}
	const (
		readControl    = 0x00020000
		writeDACL      = 0x00040000
		writeOwner     = 0x00080000
		readAttributes = 0x00000080
	)
	openFlags := uint32(syscall.FILE_FLAG_OPEN_REPARSE_POINT | syscall.FILE_FLAG_BACKUP_SEMANTICS)
	shareMode := uint32(syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE)
	hasWriteOwner := true
	h, err := syscall.CreateFile(name, readControl|writeDACL|writeOwner|readAttributes,
		shareMode, nil, syscall.OPEN_EXISTING, openFlags, 0)
	if err != nil {
		if errors.Is(err, errorAccessDenied) {
			hasWriteOwner = false
			h, err = syscall.CreateFile(name, readControl|writeDACL|readAttributes,
				shareMode, nil, syscall.OPEN_EXISTING, openFlags, 0)
		}
		if err != nil {
			return fmt.Errorf("open ACL target %q: %w", path, err)
		}
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("inspect ACL target %q: %w", path, err)
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		(info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != isDir {
		return fmt.Errorf("unexpected ACL target type: %q", path)
	}

	secInfo := uintptr(protectedDaclSecurityInfo | daclSecurityInformation)
	ownerSID, ownerErr := handleOwnerSID(h)
	if ownerErr != nil || ownerSID != sid {
		if !hasWriteOwner {
			return fmt.Errorf("set owner SID for %q: %w", path, errorAccessDenied)
		}
		secInfo |= ownerSecurityInformation
	}
	ok, _, errno := procSetKernelObjectSecurity.Call(uintptr(h), secInfo, sd)
	if ok == 0 {
		return fmt.Errorf("set protected ACL for %q: %w", path, errno)
	}
	return nil
}

// AuditFileACL inspects path's NTFS owner and DACL, flagging unexpected owners,
// NULL DACLs, or any permissive ACE granting access outside the current user and LOCAL SYSTEM.
func AuditFileACL(path string) ACLAuditResult {
	cleanPath := filepath.Clean(path)
	res := ACLAuditResult{Path: cleanPath}

	if _, err := os.Stat(cleanPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			res.Issues = []string{"file does not exist"}
			return res
		}
		res.Issues = []string{fmt.Sprintf("stat failed: %v", err)}
		return res
	}
	res.Exists = true

	curSID, err := currentUserSIDString()
	if err != nil {
		res.Issues = []string{fmt.Sprintf("resolve current user SID: %v", err)}
		return res
	}

	pathPtr, err := syscall.UTF16PtrFromString(cleanPath)
	if err != nil {
		res.Issues = []string{fmt.Sprintf("encode path: %v", err)}
		return res
	}

	var pOwner *syscall.SID
	var pDacl uintptr
	var pSD uintptr
	ret, _, _ := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		seFileObject,
		ownerSecurityInformation|daclSecurityInformation,
		uintptr(unsafe.Pointer(&pOwner)),
		0,
		uintptr(unsafe.Pointer(&pDacl)),
		0,
		uintptr(unsafe.Pointer(&pSD)),
	)
	if ret != 0 {
		res.Issues = []string{fmt.Sprintf("GetNamedSecurityInfoW failed: %v", syscall.Errno(ret))}
		return res
	}
	if pSD != 0 {
		defer syscall.LocalFree(syscall.Handle(pSD))
	}

	seenIssues := make(map[string]struct{})
	addIssue := func(msg string) {
		if _, dup := seenIssues[msg]; !dup {
			seenIssues[msg] = struct{}{}
			res.Issues = append(res.Issues, msg)
		}
	}

	if pOwner == nil {
		addIssue("file has no owner SID")
	} else {
		validOwner, _, _ := procIsValidSid.Call(uintptr(unsafe.Pointer(pOwner)))
		if validOwner == 0 {
			addIssue("invalid file owner SID")
		} else if ownerSID, convErr := pOwner.String(); convErr != nil {
			addIssue(fmt.Sprintf("convert owner SID to string: %v", convErr))
		} else if ownerSID != curSID && ownerSID != sidLocalSystem {
			addIssue(fmt.Sprintf("unexpected file owner SID %s", ownerSID))
		}
	}

	if pDacl == 0 {
		addIssue("null DACL grants full access to everyone")
		return res
	}

	var sizeInfo aclSizeInformation
	ok, _, errno := procGetAclInformation.Call(
		pDacl,
		uintptr(unsafe.Pointer(&sizeInfo)),
		unsafe.Sizeof(sizeInfo),
		aclSizeInformationClass,
	)
	if ok == 0 {
		addIssue(fmt.Sprintf("GetAclInformation failed: %v", errno))
		return res
	}

	for i := uint32(0); i < sizeInfo.AceCount; i++ {
		var acePtr unsafe.Pointer
		ok, _, _ := procGetAce.Call(pDacl, uintptr(i), uintptr(unsafe.Pointer(&acePtr)))
		if ok == 0 || acePtr == nil {
			addIssue(fmt.Sprintf("GetAce(%d) failed", i))
			continue
		}
		sidStr, permissive, parseErr := inspectAllowAceSID(acePtr)
		if parseErr != nil {
			addIssue(parseErr.Error())
			continue
		}
		if !permissive {
			continue
		}
		if sidStr == curSID || sidStr == sidLocalSystem {
			continue
		}
		switch sidStr {
		case sidEveryone:
			addIssue("permissive ACE grants access to Everyone (S-1-1-0)")
		case sidAuthenticatedUsers:
			addIssue("permissive ACE grants access to Authenticated Users (S-1-5-11)")
		case sidBuiltinUsers:
			addIssue(`permissive ACE grants access to BUILTIN\Users (S-1-5-32-545)`)
		case sidBuiltinAdmins:
			addIssue(`permissive ACE grants access to BUILTIN\Administrators (S-1-5-32-544)`)
		default:
			addIssue(fmt.Sprintf("permissive ACE grants access to unexpected SID %s", sidStr))
		}
	}

	res.Secure = len(res.Issues) == 0
	return res
}

// inspectAllowAceSID reads the SID from a Windows ACE pointer returned by GetAce.
func inspectAllowAceSID(acePtr unsafe.Pointer) (sidStr string, permissive bool, err error) {
	hdr := (*aceHeader)(acePtr)
	var sidOffset int
	switch hdr.AceType {
	case accessAllowedAceType, accessAllowedCallbackAceType:
		sidOffset = 8
	case accessAllowedObjectAceType, accessAllowedCallbackObjType:
		if int(hdr.AceSize) < 12 {
			return "", false, errors.New("malformed object ACE header")
		}
		flags := *(*uint32)(unsafe.Add(acePtr, 8))
		sidOffset = 12
		if flags&aceObjectTypePresent != 0 {
			sidOffset += 16
		}
		if flags&aceInheritedObjectTypePresent != 0 {
			sidOffset += 16
		}
	default:
		// Deny, audit, or other non-grant ACEs do not broaden permissions.
		return "", false, nil
	}
	if sidOffset+8 > int(hdr.AceSize) {
		return "", false, errors.New("malformed allow ACE SID offset")
	}
	sidPtr := unsafe.Add(acePtr, sidOffset)
	valid, _, _ := procIsValidSid.Call(uintptr(sidPtr))
	if valid == 0 {
		return "", false, errors.New("invalid SID in allow ACE")
	}
	s, convErr := (*syscall.SID)(sidPtr).String()
	if convErr != nil {
		return "", false, fmt.Errorf("convert ACE SID to string: %w", convErr)
	}
	return s, true, nil
}
