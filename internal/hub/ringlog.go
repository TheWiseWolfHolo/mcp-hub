package hub

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultRingLogCapacity is the default number of lines retained in a RingLog.
	DefaultRingLogCapacity = 128
	// DefaultRingLogMaxLineBytes is the default per-line byte limit in a RingLog.
	DefaultRingLogMaxLineBytes = 1024

	ringLogSubscriberBuf = 64
)

// LogLine represents a single timestamped log line captured in a RingLog.
type LogLine struct {
	Seq       uint64 `json:"seq,omitempty"`
	Timestamp string `json:"timestamp"`
	Stream    string `json:"stream"`
	Text      string `json:"text"`
}

// RingLog stores a bounded in-memory ring buffer of recent log lines,
// supports freezing a crash-tail snapshot, and fans out live lines to SSE subscribers.
type RingLog struct {
	mu           sync.Mutex
	buf          []LogLine
	head         int
	count        int
	nextSeq      uint64
	maxLineBytes int
	crashTail    []string
	nextSubID    uint64
	subs         map[uint64]chan LogLine
	now          func() time.Time
}

// NewRingLog creates a RingLog with the given capacity and per-line byte limit.
// Non-positive arguments fall back to DefaultRingLogCapacity (128) and DefaultRingLogMaxLineBytes (1024).
func NewRingLog(capacity, maxLineBytes int) *RingLog {
	if capacity <= 0 {
		capacity = DefaultRingLogCapacity
	}
	if maxLineBytes <= 0 {
		maxLineBytes = DefaultRingLogMaxLineBytes
	}
	return &RingLog{
		buf:          make([]LogLine, capacity),
		maxLineBytes: maxLineBytes,
		subs:         make(map[uint64]chan LogLine),
		now:          time.Now,
	}
}

// Append trims trailing CR/LF, truncates line safely on UTF-8 rune boundaries to maxLineBytes,
// records a UTC RFC3339 timestamp, appends to the ring buffer, and broadcasts non-blocking
// to all active subscribers.
func (r *RingLog) Append(stream, line string) {
	if r == nil {
		return
	}
	cleaned := strings.TrimRight(line, "\r\n")
	cleaned = truncateUTF8(cleaned, r.maxLineBytes)

	r.mu.Lock()
	nowFn := r.now
	if nowFn == nil {
		nowFn = time.Now
	}
	r.nextSeq++
	entry := LogLine{
		Seq:       r.nextSeq,
		Timestamp: nowFn().UTC().Format(time.RFC3339),
		Stream:    stream,
		Text:      cleaned,
	}
	r.buf[r.head] = entry
	r.head = (r.head + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
	for _, ch := range r.subs {
		select {
		case ch <- entry:
		default:
			// Subscriber is slow: drop oldest queued message, then enqueue latest.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- entry:
			default:
			}
		}
	}
	r.mu.Unlock()
}

// Writer returns an io.Writer that splits incoming bytes on '\n' and appends each line
// tagged with stream. The returned writer also implements Flush() error and Close() error
// to emit any unterminated trailing fragment.
func (r *RingLog) Writer(stream string) io.Writer {
	return &ringLogWriter{
		ring:   r,
		stream: stream,
	}
}

// Tail returns up to the last n buffered lines in chronological order.
// If n <= 0 or n > buffered count, all buffered lines are returned.
func (r *RingLog) Tail(n int) []LogLine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tailLocked(n)
}

func (r *RingLog) tailLocked(n int) []LogLine {
	if r.count == 0 {
		return nil
	}
	if n <= 0 || n > r.count {
		n = r.count
	}
	capLen := len(r.buf)
	start := (r.head - n + capLen) % capLen
	out := make([]LogLine, n)
	for i := 0; i < n; i++ {
		out[i] = r.buf[(start+i)%capLen]
	}
	return out
}

// TailText returns the Text field of up to the last n buffered lines in chronological order.
func (r *RingLog) TailText(n int) []string {
	lines := r.Tail(n)
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

// FreezeCrashTail snapshots the last n lines' text as the frozen crash tail and returns a copy.
// Subsequent Append calls do not overwrite the frozen snapshot until FreezeCrashTail or ClearCrashTail is called.
func (r *RingLog) FreezeCrashTail(n int) []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	lines := r.tailLocked(n)
	if len(lines) == 0 {
		r.crashTail = nil
		return nil
	}
	snap := make([]string, len(lines))
	for i, l := range lines {
		snap[i] = l.Text
	}
	r.crashTail = snap
	return append([]string(nil), snap...)
}

// CrashTail returns a copy of the frozen crash-tail snapshot, or nil if none is frozen.
func (r *RingLog) CrashTail() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.crashTail) == 0 {
		return nil
	}
	return append([]string(nil), r.crashTail...)
}

// ClearCrashTail clears any frozen crash-tail snapshot.
func (r *RingLog) ClearCrashTail() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.crashTail = nil
	r.mu.Unlock()
}

// Subscribe returns a buffered channel (capacity 64) receiving future LogLine entries
// and an idempotent unsubscribe function that removes and closes the channel safely.
func (r *RingLog) Subscribe() (<-chan LogLine, func()) {
	if r == nil {
		ch := make(chan LogLine)
		close(ch)
		return ch, func() {}
	}
	ch := make(chan LogLine, ringLogSubscriberBuf)
	r.mu.Lock()
	r.nextSubID++
	id := r.nextSubID
	r.subs[id] = ch
	r.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			r.mu.Lock()
			if existing, ok := r.subs[id]; ok {
				delete(r.subs, id)
				close(existing)
			}
			r.mu.Unlock()
		})
	}
	return ch, unsub
}

type ringLogWriter struct {
	mu         sync.Mutex
	ring       *RingLog
	stream     string
	partial    []byte
	overflowed bool
}

func (w *ringLogWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	// Cap buffered partial bytes slightly above maxLineBytes so pathological streams
	// without newlines cannot consume unbounded memory while still preserving the prefix.
	limit := DefaultRingLogMaxLineBytes + 4
	if w.ring != nil && w.ring.maxLineBytes > 0 {
		limit = w.ring.maxLineBytes + 4
	}

	rem := p
	for len(rem) > 0 {
		nl := bytes.IndexByte(rem, '\n')
		if nl < 0 {
			w.appendPartialLocked(rem, limit)
			break
		}
		w.appendPartialLocked(rem[:nl], limit)
		line := string(w.partial)
		w.partial = w.partial[:0]
		w.overflowed = false
		if w.ring != nil {
			w.ring.Append(w.stream, line)
		}
		rem = rem[nl+1:]
	}
	return len(p), nil
}

func (w *ringLogWriter) appendPartialLocked(chunk []byte, limit int) {
	if w.overflowed || len(chunk) == 0 {
		return
	}
	avail := limit - len(w.partial)
	if avail <= 0 {
		w.overflowed = true
		return
	}
	if len(chunk) > avail {
		w.partial = append(w.partial, chunk[:avail]...)
		w.overflowed = true
		return
	}
	w.partial = append(w.partial, chunk...)
}

// Flush appends any buffered unterminated partial line to the RingLog.
func (w *ringLogWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.partial) == 0 {
		return nil
	}
	line := string(w.partial)
	w.partial = w.partial[:0]
	w.overflowed = false
	if w.ring != nil {
		w.ring.Append(w.stream, line)
	}
	return nil
}

// Close flushes any buffered partial line.
func (w *ringLogWriter) Close() error {
	return w.Flush()
}

func truncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
