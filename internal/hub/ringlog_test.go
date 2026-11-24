package hub

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRingLog_OverflowOrderAndUTF8Truncation(t *testing.T) {
	rl := NewRingLog(4, 10)
	fixed := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	rl.now = func() time.Time { return fixed }

	for i := 1; i <= 6; i++ {
		rl.Append("stderr", fmt.Sprintf("line-%d\r\n", i))
	}

	got := rl.Tail(10)
	if len(got) != 4 {
		t.Fatalf("Tail len = %d, want 4", len(got))
	}
	wantTexts := []string{"line-3", "line-4", "line-5", "line-6"}
	wantSeqs := []uint64{3, 4, 5, 6}
	for i, w := range wantTexts {
		if got[i].Seq != wantSeqs[i] {
			t.Errorf("got[%d].Seq = %d, want %d", i, got[i].Seq, wantSeqs[i])
		}
		if got[i].Text != w {
			t.Errorf("got[%d].Text = %q, want %q", i, got[i].Text, w)
		}
		if got[i].Stream != "stderr" {
			t.Errorf("got[%d].Stream = %q, want stderr", i, got[i].Stream)
		}
		if got[i].Timestamp != "2026-11-20T10:00:00Z" {
			t.Errorf("got[%d].Timestamp = %q", i, got[i].Timestamp)
		}
	}

	// Tail(2) returns the newest 2 lines in chronological order.
	last2 := rl.TailText(2)
	if len(last2) != 2 || last2[0] != "line-5" || last2[1] != "line-6" {
		t.Fatalf("TailText(2) = %v, want [line-5 line-6]", last2)
	}

	// Multi-byte UTF-8 truncation must never split a rune or exceed maxLineBytes.
	// Each Chinese rune is 3 bytes; 4 runes = 12 bytes > limit 10 -> must truncate to 3 runes (9 bytes).
	rl.Append("stdout", "你好世界")
	trunc := rl.TailText(1)
	if len(trunc) != 1 || len(trunc[0]) > 10 || !utf8.ValidString(trunc[0]) || trunc[0] != "你好世" {
		t.Fatalf("UTF-8 truncation got %q (valid=%v, bytes=%d), want %q", trunc[0], utf8.ValidString(trunc[0]), len(trunc[0]), "你好世")
	}
}

func TestRingLog_WriterAndCrashTail(t *testing.T) {
	rl := NewRingLog(8, 64)
	w := rl.Writer("stdout")

	if _, err := w.Write([]byte("alpha\nbeta\r\ngam")); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if _, err := w.Write([]byte("ma\ndelta")); err != nil {
		t.Fatalf("Write 2: %v", err)
	}
	if flusher, ok := w.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}

	got := rl.TailText(10)
	want := []string{"alpha", "beta", "gamma", "delta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("TailText = %v, want %v", got, want)
	}

	// Freeze last 2 lines; subsequent appends must not alter the frozen crash tail.
	frozen := rl.FreezeCrashTail(2)
	if strings.Join(frozen, ",") != "gamma,delta" {
		t.Fatalf("FreezeCrashTail(2) = %v, want [gamma delta]", frozen)
	}
	rl.Append("stderr", "post-restart-1")
	rl.Append("stderr", "post-restart-2")

	stillFrozen := rl.CrashTail()
	if strings.Join(stillFrozen, ",") != "gamma,delta" {
		t.Fatalf("CrashTail after appends = %v, want [gamma delta]", stillFrozen)
	}

	rl.ClearCrashTail()
	if ct := rl.CrashTail(); ct != nil {
		t.Fatalf("CrashTail after ClearCrashTail = %v, want nil", ct)
	}
}

func TestRingLog_SubscribeSlowDropAndIdempotentUnsubscribe(t *testing.T) {
	rl := NewRingLog(128, 256)
	ch, unsub := rl.Subscribe()
	if cap(ch) != 64 {
		t.Fatalf("subscriber chan cap = %d, want 64", cap(ch))
	}

	// Publish 100 messages without reading; Append must never block and must retain latest 64.
	for i := 0; i < 100; i++ {
		rl.Append("stderr", fmt.Sprintf("msg-%03d", i))
	}

	var received []string
	for len(received) < 64 {
		select {
		case item := <-ch:
			received = append(received, item.Text)
		default:
			t.Fatalf("expected 64 buffered items, drained only %d", len(received))
		}
	}
	if received[0] != "msg-036" || received[len(received)-1] != "msg-099" {
		t.Fatalf("expected newest 64 [msg-036..msg-099], got [%s..%s]", received[0], received[len(received)-1])
	}

	// Unsubscribe must be safe to call multiple times and close the channel cleanly.
	unsub()
	unsub()

	// Appending after unsubscribe must not panic.
	rl.Append("stderr", "after-unsub")

	if _, ok := <-ch; ok {
		t.Fatalf("expected subscriber channel to be closed after unsubscribe")
	}
}
