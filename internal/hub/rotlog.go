package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	rotationRetryBumpBytes int64 = 1 << 20 // 1 MiB
	rotationRetryCooldown        = 60 * time.Second
)

// RotatingFileWriter is a thread-safe size-based rotating log writer.
// When the active file would exceed maxBytes, it closes the file, moves it to
// "<path>.old" (replacing any previous backup), and opens a fresh file.
type RotatingFileWriter struct {
	mu              sync.Mutex
	path            string
	maxBytes        int64
	nextRotateBytes int64
	retryAfter      time.Time
	now             func() time.Time
	file            *os.File
	size            int64
}

// NewRotatingFileWriter opens or creates path for append-only logging with rotation.
func NewRotatingFileWriter(path string, maxBytes int64) (*RotatingFileWriter, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultLogMaxBytes
	}
	cleanPath := filepath.Clean(path)
	if err := os.MkdirAll(filepath.Dir(cleanPath), 0o700); err != nil {
		return nil, fmt.Errorf("create log dir for %q: %w", cleanPath, err)
	}
	f, err := os.OpenFile(cleanPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", cleanPath, err)
	}
	var size int64
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	return &RotatingFileWriter{
		path:            cleanPath,
		maxBytes:        maxBytes,
		nextRotateBytes: maxBytes,
		now:             time.Now,
		file:            f,
		size:            size,
	}, nil
}

// Write appends p to the log file, rotating first when the write would exceed maxBytes.
func (w *RotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.file == nil {
		return 0, os.ErrClosed
	}
	if w.shouldRotateLocked(int64(len(p))) {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotatingFileWriter) shouldRotateLocked(writeLen int64) bool {
	if w.size <= 0 {
		return false
	}
	projected := w.size + writeLen
	if projected <= w.maxBytes {
		return false
	}
	if w.retryAfter.IsZero() {
		return true
	}
	nowFn := w.now
	if nowFn == nil {
		nowFn = time.Now
	}
	if !nowFn().Before(w.retryAfter) {
		return true
	}
	return projected > w.nextRotateBytes
}

func (w *RotatingFileWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	backupPath := w.path + ".old"
	if err := replaceFileAtomic(w.path, backupPath); err != nil && !os.IsNotExist(err) {
		// On Windows a log viewer or AV may temporarily lock the file during rename.
		// Reopen the active file in append mode and defer the next rotation attempt
		// so os/exec's pipe io.Copy goroutine is never aborted by a transient lock.
		f, openErr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			return fmt.Errorf("rotate log %q -> %q: %w (reopen failed: %v)", w.path, backupPath, err, openErr)
		}
		w.file = f
		if info, statErr := f.Stat(); statErr == nil {
			w.size = info.Size()
		}
		nowFn := w.now
		if nowFn == nil {
			nowFn = time.Now
		}
		w.nextRotateBytes = w.size + rotationRetryBumpBytes
		w.retryAfter = nowFn().Add(rotationRetryCooldown)
		return nil
	}
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("reopen log %q after rotation: %w", w.path, err)
	}
	w.file = f
	w.size = 0
	w.nextRotateBytes = w.maxBytes
	w.retryAfter = time.Time{}
	return nil
}

// Path returns the active log file path.
func (w *RotatingFileWriter) Path() string {
	return w.path
}

// Close closes the underlying file handle.
func (w *RotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}
