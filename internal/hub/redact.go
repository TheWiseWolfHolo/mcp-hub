package hub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	// RedactedPlaceholder replaces detected secrets in log streams and diagnostics.
	RedactedPlaceholder = "[REDACTED]"

	minSecretLen          = 8
	redactorMaxBufferSize = 32 * 1024 // 32 KiB
	overlongLogLine       = RedactedPlaceholder + " (overlong log line)\n"
)

var (
	// Matches HTTP headers or JSON key/value pairs carrying an Authorization Bearer token.
	redactAuthHeaderRe = regexp.MustCompile(`(?i)(authorization["']?\s*:\s*["']?bearer\s+)([^\s"',;}\]]+)`)
	// Matches standalone "Bearer <token>" values covering all printable non-whitespace ASCII tokens.
	redactBearerRe = regexp.MustCompile(`(?i)(\bbearer\s+)([\x21-\x7E]{6,})`)
	// Matches sensitive query parameters.
	redactQueryParamRe  = regexp.MustCompile(`(?i)([?&](?:token|api_key|apikey|key|secret|access_token|password|passwd)=)([^&\s"']+)`)
	redactURLUserInfoRe = regexp.MustCompile(`(?i)((?:https?|tcp)://)[^/@\s"'\\]+@`)
	// Matches common API key / PAT prefixes.
	redactKeyPrefixRe = regexp.MustCompile(`(?:\bsk-[A-Za-z0-9_-]{8,}|\bghp_[A-Za-z0-9]{8,}|\bgithub_pat_[A-Za-z0-9_]{8,}|\bfc-[A-Za-z0-9_-]{8,})`)
	// Matches 3-part base64url JWTs beginning with eyJ.
	redactJWTRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{4,}\b`)
)

// Redactor scrubs known secret values (including URL-encoded forms) and common
// credential patterns from strings, byte slices, and streaming io.Writers.
// It is safe for concurrent use by multiple goroutines.
type Redactor struct {
	mu       sync.Mutex
	variants map[string]struct{}
	replacer atomic.Pointer[strings.Replacer]
}

// NewRedactor creates a Redactor with regex scrubbers active and no exact secrets registered.
func NewRedactor() *Redactor {
	return &Redactor{}
}

// SetSecrets atomically replaces the exact-match secret list.
// Non-empty secrets of length >= 8 are deduplicated, expanded with url.QueryEscape
// and url.PathEscape variants, and sorted longest-first before compiling a Replacer.
func (r *Redactor) SetSecrets(secrets []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.variants = make(map[string]struct{}, len(secrets)*3)
	r.addSecretsLocked(secrets)
	r.publishLocked()
}

// AddSecrets keeps prior credentials masked over the lifetime of the daemon. Older
// processes and connections may still emit them after a failed or partial reload.
func (r *Redactor) AddSecrets(secrets []string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.variants == nil {
		r.variants = make(map[string]struct{}, len(secrets)*3)
	}
	oldLen := len(r.variants)
	r.addSecretsLocked(secrets)
	if len(r.variants) != oldLen {
		r.publishLocked()
	}
}

func (r *Redactor) addSecretsLocked(secrets []string) {
	for _, raw := range secrets {
		if len(raw) < minSecretLen || strings.TrimSpace(raw) == "" {
			continue
		}
		r.variants[raw] = struct{}{}
		r.variants[url.QueryEscape(raw)] = struct{}{}
		r.variants[url.PathEscape(raw)] = struct{}{}
	}
}

func (r *Redactor) publishLocked() {
	if len(r.variants) == 0 {
		r.replacer.Store(nil)
		return
	}
	variants := make([]string, 0, len(r.variants))
	for v := range r.variants {
		variants = append(variants, v)
	}
	sort.Slice(variants, func(i, j int) bool {
		if len(variants[i]) != len(variants[j]) {
			return len(variants[i]) > len(variants[j])
		}
		return variants[i] < variants[j]
	})
	pairs := make([]string, 0, len(variants)*2)
	for _, v := range variants {
		pairs = append(pairs, v, RedactedPlaceholder)
	}
	r.replacer.Store(strings.NewReplacer(pairs...))
}

// Redact applies exact secret replacement and regex scrubbers to s.
func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	if r != nil {
		if rep := r.replacer.Load(); rep != nil {
			s = rep.Replace(s)
		}
	}
	s = redactAuthHeaderRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := redactAuthHeaderRe.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		if isSafeBearerPlaceholder(sub[2]) {
			return match
		}
		return sub[1] + RedactedPlaceholder
	})
	s = redactBearerRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := redactBearerRe.FindStringSubmatch(match)
		if len(sub) != 3 {
			return match
		}
		if isSafeBearerPlaceholder(sub[2]) {
			return match
		}
		return sub[1] + RedactedPlaceholder
	})
	s = redactQueryParamRe.ReplaceAllString(s, "${1}"+RedactedPlaceholder)
	s = redactURLUserInfoRe.ReplaceAllString(s, "${1}"+RedactedPlaceholder+"@")
	s = redactKeyPrefixRe.ReplaceAllString(s, RedactedPlaceholder)
	s = redactJWTRe.ReplaceAllString(s, RedactedPlaceholder)
	return s
}

func isSafeBearerPlaceholder(token string) bool {
	if token == "<TOKEN_FROM:" {
		return true
	}
	if rest, ok := strings.CutPrefix(token, RedactedPlaceholder); ok && strings.Trim(rest, `"',;})]`) == "" {
		return true
	}
	return false
}

// RedactBytes redacts b and returns a redacted byte slice without mutating b.
func (r *Redactor) RedactBytes(b []byte) []byte {
	if len(b) == 0 {
		if b == nil {
			return nil
		}
		return []byte{}
	}
	return []byte(r.Redact(string(b)))
}

// RedactJSON scrubs decoded string values and keys so escaped credentials cannot
// bypass redaction. UseNumber preserves large integer fields in diagnostics.
func (r *Redactor) RedactJSON(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(r.redactJSONValue(value))
}

func (r *Redactor) redactJSONValue(value any) any {
	switch value := value.(type) {
	case string:
		return r.Redact(value)
	case []any:
		for i, item := range value {
			value[i] = r.redactJSONValue(item)
		}
		return value
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[r.Redact(key)] = r.redactJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func diagnosticURL(redactor *Redactor, raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return RedactedPlaceholder
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return redactor.Redact(u.String())
}

// NewWriter redacts complete log lines and replaces oversized lines with a fixed marker.
// Flush and Close emit a final unterminated line if it fit in the buffer.
func (r *Redactor) NewWriter(dst io.Writer) io.Writer {
	if dst == nil {
		dst = io.Discard
	}
	return &redactingWriter{
		redactor: r,
		dst:      dst,
	}
}

type redactingWriter struct {
	mu       sync.Mutex
	redactor *Redactor
	dst      io.Writer
	buf      []byte
	discard  bool
}

func (w *redactingWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	rem := p
	for len(rem) > 0 {
		end := bytes.IndexByte(rem, '\n')
		if end < 0 {
			end = len(rem)
		} else {
			end++
		}
		chunk := rem[:end]
		rem = rem[end:]
		complete := chunk[len(chunk)-1] == '\n'
		if w.discard {
			w.discard = !complete
			continue
		}
		if len(chunk) > redactorMaxBufferSize-len(w.buf) {
			w.buf = w.buf[:0]
			w.discard = !complete
			if _, err := io.WriteString(w.dst, overlongLogLine); err != nil {
				return len(p) - len(rem), err
			}
			continue
		}
		w.buf = append(w.buf, chunk...)
		if complete {
			if err := w.flushLocked(); err != nil {
				return len(p) - len(rem), err
			}
		}
	}
	return len(p), nil
}

// Flush redacts and writes any buffered partial line to dst.
func (w *redactingWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.discard = false
	return w.flushLocked()
}

// Close flushes any buffered remainder to dst without closing dst itself.
func (w *redactingWriter) Close() error {
	return w.Flush()
}

func (w *redactingWriter) flushLocked() error {
	if len(w.buf) == 0 {
		return nil
	}
	out := w.redactor.RedactBytes(w.buf)
	w.buf = w.buf[:0]
	if len(out) == 0 {
		return nil
	}
	_, err := w.dst.Write(out)
	return err
}

// bestEffortWriter wraps a secondary log sink (such as RotatingFileWriter) so transient
// disk or lock errors never propagate back to os/exec's stdout/stderr pipe copier.
// The first error is reported once to onErr (if provided).
type bestEffortWriter struct {
	dst    io.Writer
	onErr  func(error)
	logged atomic.Bool
}

func newBestEffortWriter(dst io.Writer, onErr func(error)) io.Writer {
	if dst == nil {
		return io.Discard
	}
	return &bestEffortWriter{dst: dst, onErr: onErr}
}

func (w *bestEffortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if _, err := w.dst.Write(p); err != nil {
		if w.logged.CompareAndSwap(false, true) && w.onErr != nil {
			w.onErr(err)
		}
	}
	return len(p), nil
}
