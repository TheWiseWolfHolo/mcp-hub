package hub

import (
	"bytes"
	"io"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestRedact_ExactAndURLEncodedAndLongestFirst(t *testing.T) {
	r := NewRedactor()
	shortSecret := "short77" // len 7 -> must be ignored
	baseSecret := "super+secret/value=123"
	longerSecret := "super+secret/value=123-extended"
	r.SetSecrets([]string{
		"",
		"       ",
		shortSecret,
		baseSecret,
		longerSecret,
		baseSecret, // duplicate
	})

	// Longest-first avoids leaving "-extended" behind.
	if got := r.Redact("tok=" + longerSecret); got != "tok=[REDACTED]" {
		t.Fatalf("longest-first failed: got %q", got)
	}
	// Short secret (<8 chars) should not be scrubbed.
	if got := r.Redact("keep " + shortSecret); got != "keep "+shortSecret {
		t.Fatalf("short secret unexpectedly redacted: got %q", got)
	}
	// Raw, QueryEscape, and PathEscape variants must all be redacted.
	variants := []string{
		baseSecret,
		url.QueryEscape(baseSecret),
		url.PathEscape(baseSecret),
	}
	for _, v := range variants {
		in := "https://example.local/cb?x=" + v + "&ok=1"
		got := r.Redact(in)
		if strings.Contains(got, v) || !strings.Contains(got, RedactedPlaceholder) {
			t.Errorf("variant %q not redacted in %q -> %q", v, in, got)
		}
	}

	// Clearing secrets disables exact replacement while keeping regex scrubbers.
	r.SetSecrets(nil)
	if got := r.Redact("raw " + baseSecret); got != "raw "+baseSecret {
		t.Errorf("expected cleared exact secret not to redact, got %q", got)
	}
}

func TestRedact_RegexScrubbers(t *testing.T) {
	r := NewRedactor()
	cases := []struct {
		name      string
		in        string
		forbidden string
		wantHas   string
	}{
		{
			name:      "authorization header",
			in:        "Authorization: Bearer top_secret_hdr_value",
			forbidden: "top_secret_hdr_value",
			wantHas:   "Authorization: Bearer [REDACTED]",
		},
		{
			name:      "json authorization",
			in:        `{"Authorization": "Bearer abc123xyz999"}`,
			forbidden: "abc123xyz999",
			wantHas:   `Bearer [REDACTED]`,
		},
		{
			name:      "standalone bearer",
			in:        "using Bearer myTokenVal123456 for upstream",
			forbidden: "myTokenVal123456",
			wantHas:   "Bearer [REDACTED]",
		},
		{
			name:      "standalone bearer with printable ASCII symbols",
			in:        "upstream failed Bearer p@ss!w#rd$%^&*:123 retry",
			forbidden: "p@ss!w#rd",
			wantHas:   "upstream failed Bearer [REDACTED] retry",
		},
		{
			name:      "query params",
			in:        "GET /mcp?api_key=my_api_key_99&token=tok_abc&apikey=ak_1&key=k_2&secret=sec_3&access_token=at_4&ok=yes",
			forbidden: "my_api_key_99",
			wantHas:   "api_key=[REDACTED]&token=[REDACTED]&apikey=[REDACTED]&key=[REDACTED]&secret=[REDACTED]&access_token=[REDACTED]&ok=yes",
		},
		{
			name:      "openai sk prefix",
			in:        "key=sk-proj-1234567890abcdef done",
			forbidden: "sk-proj-1234567890abcdef",
			wantHas:   "[REDACTED]",
		},
		{
			name:      "github ghp prefix",
			in:        "ghp_1234567890ABCDEF",
			forbidden: "ghp_1234567890ABCDEF",
			wantHas:   "[REDACTED]",
		},
		{
			name:      "github_pat prefix",
			in:        "github_pat_11ABCDEF_0123456789",
			forbidden: "github_pat_11ABCDEF_0123456789",
			wantHas:   "[REDACTED]",
		},
		{
			name:      "firecrawl fc prefix",
			in:        "firecrawl=fc-abcdef1234567890",
			forbidden: "fc-abcdef1234567890",
			wantHas:   "[REDACTED]",
		},
		{
			name:      "jwt token",
			in:        "jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c ok",
			forbidden: "eyJhbGci",
			wantHas:   "jwt [REDACTED] ok",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Redact(tc.in)
			if strings.Contains(got, tc.forbidden) {
				t.Errorf("secret %q leaked in %q", tc.forbidden, got)
			}
			if !strings.Contains(got, tc.wantHas) {
				t.Errorf("got %q, want substring %q", got, tc.wantHas)
			}
			gotBytes := r.RedactBytes([]byte(tc.in))
			if string(gotBytes) != got {
				t.Errorf("RedactBytes mismatch: %q vs %q", string(gotBytes), got)
			}
		})
	}
	if got := r.Redact(`{"Authorization": "Bearer abc123xyz999"}`); got != `{"Authorization": "Bearer [REDACTED]"}` {
		t.Fatalf("JSON authorization redaction corrupted closing delimiters: got %q", got)
	}
}

func TestRedact_WriterBufferingAndFlush(t *testing.T) {
	r := NewRedactor()
	r.SetSecrets([]string{"my-split-secret-value"})

	var dst bytes.Buffer
	w := r.NewWriter(&dst)

	// Split a secret across two writes before the newline; line buffering must catch it.
	if n, err := w.Write([]byte("prefix my-split-")); err != nil || n != len("prefix my-split-") {
		t.Fatalf("first Write: n=%d err=%v", n, err)
	}
	if dst.Len() != 0 {
		t.Fatalf("expected partial line to remain buffered until newline, got %q", dst.String())
	}
	if n, err := w.Write([]byte("secret-value suffix\npartial-no-nl")); err != nil || n != len("secret-value suffix\npartial-no-nl") {
		t.Fatalf("second Write: n=%d err=%v", n, err)
	}
	if got := dst.String(); got != "prefix [REDACTED] suffix\n" {
		t.Fatalf("after newline flush: got %q", got)
	}

	// Close() flushes remainder.
	closer, ok := w.(io.Closer)
	if !ok {
		t.Fatalf("expected NewWriter to implement io.Closer")
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := dst.String(); got != "prefix [REDACTED] suffix\npartial-no-nl" {
		t.Fatalf("after Close flush: got %q", got)
	}

	// Exceeding 32 KiB without a newline emits a fixed marker.
	dst.Reset()
	big := strings.Repeat("a", 33*1024)
	if n, err := w.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("big Write: n=%d err=%v", n, err)
	}
	if dst.String() != overlongLogLine {
		t.Fatalf("expected one overlong line marker, got %d bytes", dst.Len())
	}
	if flusher, ok := w.(interface{ Flush() error }); ok {
		if err := flusher.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
	}
	if dst.String() != overlongLogLine {
		t.Fatalf("Flush changed the already discarded line: %d bytes", dst.Len())
	}
}

func TestRedact_ConcurrentSetAndRedact(t *testing.T) {
	r := NewRedactor()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if id%2 == 0 {
					r.SetSecrets([]string{"concurrent-secret-token-1234"})
				} else {
					_ = r.Redact("Bearer tok_12345678 concurrent-secret-token-1234")
				}
			}
		}(i)
	}
	wg.Wait()
}
