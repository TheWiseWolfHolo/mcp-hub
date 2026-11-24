package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRotatingFileWriter(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "svc.log")

	w, err := NewRotatingFileWriter(logPath, 64)
	if err != nil {
		t.Fatalf("NewRotatingFileWriter: %v", err)
	}
	defer w.Close()

	// Write enough bytes to trigger multiple rotations.
	for i := 0; i < 6; i++ {
		line := fmt.Sprintf("entry-%02d-abcdefghijklmnopqrstuvwxyz\n", i)
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
	}

	active, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active log: %v", err)
	}
	backup, err := os.ReadFile(logPath + ".old")
	if err != nil {
		t.Fatalf("read backup log: %v", err)
	}
	if len(active) == 0 || int64(len(active)) > 64 {
		t.Errorf("active log size = %d, want (0, 64]", len(active))
	}
	if len(backup) == 0 {
		t.Errorf("expected non-empty .old backup")
	}
	if !strings.Contains(string(active), "entry-05") {
		t.Errorf("expected latest entry in active log, got %q", string(active))
	}

	// Verify concurrent writes stay safe without race or error.
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = w.Write([]byte(fmt.Sprintf("g%d-msg-%d\n", id, j)))
			}
		}(g)
	}
	wg.Wait()
}

func TestRotatingFileWriter_RotationFailureFallback(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "locked.log")

	w, err := NewRotatingFileWriter(logPath, 64)
	if err != nil {
		t.Fatalf("NewRotatingFileWriter: %v", err)
	}
	defer w.Close()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	w.now = func() time.Time { return now }

	// Seed the active file below maxBytes.
	line1 := "line-1-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" // 40 bytes
	if n, err := w.Write([]byte(line1)); err != nil || n != len(line1) {
		t.Fatalf("Write line1: n=%d err=%v", n, err)
	}

	// Place a non-empty directory at "<path>.old" so os.Remove and os.Rename fail deterministically,
	// simulating a Windows log viewer / AV lock while keeping fallback append reopen possible.
	blockerDir := logPath + ".old"
	if err := os.Mkdir(blockerDir, 0o700); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blockerDir, "hold"), []byte("lock"), 0o600); err != nil {
		t.Fatalf("write blocker file: %v", err)
	}

	// Next write crosses maxBytes: rotation fails, fallback reopen succeeds, Write MUST return nil.
	line2 := "line-2-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" // 40 bytes -> total 80 > 64
	if n, err := w.Write([]byte(line2)); err != nil || n != len(line2) {
		t.Fatalf("Write line2 on rotation failure: n=%d err=%v (want nil error)", n, err)
	}
	firstRetryAfter := w.retryAfter
	if !firstRetryAfter.Equal(now.Add(rotationRetryCooldown)) {
		t.Fatalf("retryAfter = %v, want %v", firstRetryAfter, now.Add(rotationRetryCooldown))
	}
	if w.nextRotateBytes != int64(len(line1))+rotationRetryBumpBytes {
		t.Fatalf("nextRotateBytes = %d, want %d", w.nextRotateBytes, int64(len(line1))+rotationRetryBumpBytes)
	}

	// Advance time slightly (still within cooldown) and write more lines; all must succeed
	// without re-triggering rotation on every write.
	now = now.Add(5 * time.Second)
	for i := 3; i <= 5; i++ {
		msg := fmt.Sprintf("line-%d-cccccccccccccccccccccccccccccccc\n", i)
		if n, err := w.Write([]byte(msg)); err != nil || n != len(msg) {
			t.Fatalf("Write line%d during cooldown: n=%d err=%v", i, n, err)
		}
	}
	if !w.retryAfter.Equal(firstRetryAfter) {
		t.Fatalf("expected rotation to remain deferred during cooldown (retryAfter changed from %v to %v)", firstRetryAfter, w.retryAfter)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log during fallback: %v", err)
	}
	for _, want := range []string{"line-1-", "line-2-", "line-3-", "line-4-", "line-5-"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("expected %q in active log during fallback; got %q", want, string(content))
		}
	}

	// Clear the blocker and advance past cooldown; next write must rotate cleanly.
	if err := os.RemoveAll(blockerDir); err != nil {
		t.Fatalf("remove blocker dir: %v", err)
	}
	now = firstRetryAfter.Add(time.Second)
	line6 := "line-6-after-recovery\n"
	if n, err := w.Write([]byte(line6)); err != nil || n != len(line6) {
		t.Fatalf("Write line6 after cooldown: n=%d err=%v", n, err)
	}

	active, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active log after recovery: %v", err)
	}
	backup, err := os.ReadFile(logPath + ".old")
	if err != nil {
		t.Fatalf("read backup log after recovery: %v", err)
	}
	if string(active) != line6 {
		t.Errorf("active log after recovery = %q, want %q", string(active), line6)
	}
	if !strings.Contains(string(backup), "line-5-") {
		t.Errorf("expected backup log to contain prior entries, got %q", string(backup))
	}
	if !w.retryAfter.IsZero() {
		t.Errorf("expected retryAfter reset to zero after successful rotation, got %v", w.retryAfter)
	}
}

func TestRotatingFileWriter_PreservesOldBackupWhenActiveLocked(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows sharing-violation lock semantics only")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "preserve.log")
	backupPath := logPath + ".old"

	const sentinelBackup = "previous-backup-must-survive\n"
	if err := os.WriteFile(backupPath, []byte(sentinelBackup), 0o600); err != nil {
		t.Fatalf("seed .old backup: %v", err)
	}

	w, err := NewRotatingFileWriter(logPath, 32)
	if err != nil {
		t.Fatalf("NewRotatingFileWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("01234567890123456789\n")); err != nil {
		t.Fatalf("seed active log: %v", err)
	}

	// Open the active log file without FILE_SHARE_DELETE (default os.Open on Windows)
	// so MoveFileExW fails with a sharing violation when rotation is attempted.
	lockHandle, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("open lock handle on active log: %v", err)
	}
	defer lockHandle.Close()

	// Cross maxBytes: replaceFileAtomic fails, fallback keeps writing, and .old MUST remain intact!
	if _, err := w.Write([]byte("trigger-rotation-while-locked\n")); err != nil {
		t.Fatalf("Write while locked: %v", err)
	}

	gotBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read preserved .old backup: %v", err)
	}
	if string(gotBackup) != sentinelBackup {
		t.Fatalf(".old backup content = %q, want %q", string(gotBackup), sentinelBackup)
	}
}
