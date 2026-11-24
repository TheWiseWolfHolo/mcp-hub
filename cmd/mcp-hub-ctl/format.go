package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/TheWiseWolfHolo/mcp-hub/internal/hub"
)

const (
	ansiReset       = "\x1b[0m"
	ansiBold        = "\x1b[1m"
	ansiDim         = "\x1b[2m"
	ansiCrimson     = "\x1b[31m"
	ansiCrimsonBold = "\x1b[1;31m"
	ansiEmerald     = "\x1b[32m"
	ansiEmeraldBold = "\x1b[1;32m"
	ansiAmber       = "\x1b[33m"
	ansiAmberBold   = "\x1b[1;33m"
	ansiMagenta     = "\x1b[35m"
	ansiCyan        = "\x1b[36m"
	ansiSlate       = "\x1b[90m"
)

type termStyle struct {
	color bool
	isTTY bool
	dumb  bool
	vt    bool
	width int
}

func newTermStyle(w io.Writer, asJSON bool) termStyle {
	dumb := strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
	tty := isTerminalWriter(w)
	st := termStyle{
		isTTY: tty,
		dumb:  dumb,
		width: terminalWidth(w),
	}
	if tty && !dumb {
		st.vt = enableVirtualTerminal(w)
	}
	if asJSON {
		return st
	}
	if os.Getenv("NO_COLOR") != "" {
		return st
	}
	if strings.TrimSpace(os.Getenv("CLICOLOR")) == "0" {
		return st
	}
	if dumb {
		return st
	}
	if force := strings.TrimSpace(os.Getenv("CLICOLOR_FORCE")); force != "" && force != "0" {
		_ = enableVirtualTerminal(w)
		st.color = true
		return st
	}
	if st.vt {
		st.color = true
	}
	return st
}

func (s termStyle) canUseVTWatch() bool {
	return s.vt
}

func (s termStyle) wrap(code, text string) string {
	if !s.color || text == "" {
		return text
	}
	return code + text + ansiReset
}

func (s termStyle) bold(text string) string        { return s.wrap(ansiBold, text) }
func (s termStyle) dim(text string) string         { return s.wrap(ansiDim, text) }
func (s termStyle) emerald(text string) string     { return s.wrap(ansiEmerald, text) }
func (s termStyle) emeraldBold(text string) string { return s.wrap(ansiEmeraldBold, text) }
func (s termStyle) slate(text string) string       { return s.wrap(ansiSlate, text) }
func (s termStyle) amber(text string) string       { return s.wrap(ansiAmber, text) }
func (s termStyle) amberBold(text string) string   { return s.wrap(ansiAmberBold, text) }
func (s termStyle) crimson(text string) string     { return s.wrap(ansiCrimson, text) }
func (s termStyle) crimsonBold(text string) string { return s.wrap(ansiCrimsonBold, text) }
func (s termStyle) cyan(text string) string        { return s.wrap(ansiCyan, text) }
func (s termStyle) magenta(text string) string     { return s.wrap(ansiMagenta, text) }

func (s termStyle) stateBadge(state string) string {
	d := dash(state)
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "ok", "healthy":
		return s.emerald(d)
	case "sleeping", "stopped":
		return s.slate(d)
	case "starting", "stopping":
		return s.amber(d)
	case "degraded", "error", "failed":
		return s.crimsonBold(d)
	default:
		if d == "-" {
			return s.dim(d)
		}
		return d
	}
}

func (s termStyle) healthBadge(word string) string {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "ok", "healthy":
		return s.emeraldBold(word)
	case "degraded", "down", "error":
		return s.crimsonBold(word)
	case "starting":
		return s.amber(word)
	case "-":
		return s.dim(word)
	default:
		return word
	}
}

func (s termStyle) resultBadge(result string) string {
	up := strings.ToUpper(strings.TrimSpace(result))
	switch up {
	case "PASS", "OK", "FIXED":
		return s.emeraldBold(up)
	case "SKIP":
		return s.slate(up)
	case "WARN":
		return s.amberBold(up)
	case "FAIL", "VIOLATION", "ERROR":
		return s.crimsonBold(up)
	default:
		return up
	}
}

func (s termStyle) boolBadge(v bool) string {
	if v {
		return s.emerald("true")
	}
	return s.crimson("false")
}

func (s termStyle) crashWakeCell(crashes, wakes int) string {
	if crashes == 0 && wakes == 0 {
		return s.dim("0/0")
	}
	cStr := fmt.Sprintf("%d", crashes)
	if crashes > 0 {
		cStr = s.crimsonBold(cStr)
	} else {
		cStr = s.dim(cStr)
	}
	wStr := fmt.Sprintf("%d", wakes)
	if wakes > 0 {
		wStr = s.slate(wStr)
	} else {
		wStr = s.dim(wStr)
	}
	return cStr + s.dim("/") + wStr
}

func (s termStyle) toolHintBadge(readOnly *bool) string {
	if readOnly == nil {
		return s.dim("-")
	}
	if *readOnly {
		return s.emerald("read-only")
	}
	return s.amber("write")
}

func (s termStyle) section(title string) string {
	return s.bold(title)
}

func (s termStyle) effectiveWidth() int {
	if s.width >= 60 {
		return s.width
	}
	return 116
}

// ---------------------------------------------------------------------------
// ANSI stripping & CJK/East-Asian display width
// ---------------------------------------------------------------------------

// stripANSI removes ANSI CSI escape sequences from s.
func stripANSI(s string) string {
	if !strings.ContainsRune(s, '\x1b') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			i += 2
			for i < len(s) {
				c := s[i]
				i++
				if c >= 0x40 && c <= 0x7e {
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// runeWidth returns the terminal cell width (0, 1, or 2) of r.
func runeWidth(r rune) int {
	if r == 0 || r < 0x20 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || unicode.Is(unicode.Cf, r) {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) || // Hangul Jamo init
		r == 0x2329 || r == 0x232a ||
		(r >= 0x2e80 && r <= 0x303e) || // CJK Radicals .. CJK Symbols
		(r >= 0x3040 && r <= 0xa4cf && r != 0x303f) || // Hiragana, Katakana, Bopomofo, CJK Unified
		(r >= 0xac00 && r <= 0xd7a3) || // Hangul Syllables
		(r >= 0xf900 && r <= 0xfaff) || // CJK Compatibility Ideographs
		(r >= 0xfe10 && r <= 0xfe19) || // Vertical forms
		(r >= 0xfe30 && r <= 0xfe6f) || // CJK Compatibility Forms
		(r >= 0xff00 && r <= 0xff60) || // Fullwidth ASCII / punctuation
		(r >= 0xffe0 && r <= 0xffe6) || // Fullwidth signs
		(r >= 0x1f300 && r <= 0x1faff) || // Emoji / Pictographs
		(r >= 0x20000 && r <= 0x3fffd) // CJK Extensions B..G
}

// visibleWidth returns the terminal column width of s, ignoring ANSI escape codes
// and counting CJK / East-Asian wide characters as width 2.
func visibleWidth(s string) int {
	clean := stripANSI(s)
	w := 0
	for _, r := range clean {
		w += runeWidth(r)
	}
	return w
}

// truncateDisplay shortens plain string s so its terminal display width does not exceed maxCols.
func truncateDisplay(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if visibleWidth(s) <= maxCols {
		return s
	}
	if maxCols <= 3 {
		var b strings.Builder
		w := 0
		for _, r := range s {
			rw := runeWidth(r)
			if w+rw > maxCols {
				break
			}
			b.WriteRune(r)
			w += rw
		}
		return b.String()
	}
	limit := maxCols - 3
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > limit {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	b.WriteString("...")
	return b.String()
}

// truncateMiddle keeps both prefix and suffix of s when display width exceeds maxCols.
func truncateMiddle(s string, maxCols int) string {
	if maxCols <= 0 {
		return ""
	}
	if visibleWidth(s) <= maxCols {
		return s
	}
	if maxCols <= 5 {
		return truncateDisplay(s, maxCols)
	}
	runes := []rune(s)
	budget := maxCols - 3
	headBudget := budget / 2
	tailBudget := budget - headBudget

	var head []rune
	hw := 0
	for _, r := range runes {
		rw := runeWidth(r)
		if hw+rw > headBudget {
			break
		}
		head = append(head, r)
		hw += rw
	}
	var tail []rune
	tw := 0
	for i := len(runes) - 1; i >= len(head); i-- {
		rw := runeWidth(runes[i])
		if tw+rw > tailBudget {
			break
		}
		tail = append(tail, runes[i])
		tw += rw
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	return string(head) + "..." + string(tail)
}

// compactAuditTarget keeps audit TARGET values readable without pushing RESULT/DETAIL off-screen.
func compactAuditTarget(target, dataDir string, maxCols int) string {
	s := oneLine(target)
	if dataDir != "" {
		cleanBase := filepath.Clean(dataDir)
		// Replace matching dataDir prefix inside "<label> <path>" with relative path.
		if idx := strings.IndexByte(s, ' '); idx > 0 {
			label := s[:idx]
			rest := strings.TrimSpace(s[idx+1:])
			if rel, ok := shortRelPath(cleanBase, rest); ok {
				s = label + " " + rel
			}
		} else if rel, ok := shortRelPath(cleanBase, s); ok {
			s = rel
		}
	}
	return truncateMiddle(s, maxCols)
}

func shortRelPath(baseDir, rawPath string) (string, bool) {
	if strings.TrimSpace(rawPath) == "" {
		return "", false
	}
	if !filepath.IsAbs(rawPath) {
		return filepath.ToSlash(rawPath), false
	}
	cleanTarget := filepath.Clean(rawPath)
	if samePath(baseDir, cleanTarget) {
		return filepath.Base(cleanTarget) + "/", true
	}
	rel, err := filepath.Rel(baseDir, cleanTarget)
	if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel), true
	}
	// Outside dataDir: keep last 2 path components with leading ".../".
	slash := filepath.ToSlash(cleanTarget)
	parts := strings.Split(strings.Trim(slash, "/"), "/")
	if len(parts) >= 3 {
		return ".../" + parts[len(parts)-2] + "/" + parts[len(parts)-1], true
	}
	return slash, false
}

// compactProbeTarget strips redundant http:// prefix in human tables and bounds column width.
func compactProbeTarget(target string, maxCols int) string {
	s := oneLine(target)
	s = strings.Replace(s, " http://", " ", 1)
	s = strings.TrimPrefix(s, "http://")
	return truncateMiddle(s, maxCols)
}

// ---------------------------------------------------------------------------
// ANSI-aware & CJK-width-aware column table renderer
// ---------------------------------------------------------------------------

type colAlign int

const (
	alignLeft colAlign = iota
	alignRight
)

type tableCol struct {
	Header string
	Align  colAlign
}

type colTable struct {
	cols  []tableCol
	rows  [][]string
	style termStyle
	gap   int
}

func newColTable(st termStyle, cols ...tableCol) *colTable {
	return &colTable{
		cols:  cols,
		style: st,
		gap:   2,
	}
}

func (t *colTable) addRow(cells ...string) {
	row := make([]string, len(t.cols))
	copy(row, cells)
	t.rows = append(t.rows, row)
}

func (t *colTable) render(w io.Writer) {
	if len(t.cols) == 0 {
		return
	}
	widths := make([]int, len(t.cols))
	for j, c := range t.cols {
		widths[j] = visibleWidth(c.Header)
	}
	for _, row := range t.rows {
		for j := 0; j < len(t.cols) && j < len(row); j++ {
			if cw := visibleWidth(row[j]); cw > widths[j] {
				widths[j] = cw
			}
		}
	}
	gapStr := strings.Repeat(" ", t.gap)
	writeRow := func(cells []string, isHeader bool) {
		var line strings.Builder
		for j, col := range t.cols {
			if j > 0 {
				line.WriteString(gapStr)
			}
			cell := ""
			if j < len(cells) {
				cell = cells[j]
			}
			cw := visibleWidth(cell)
			pad := widths[j] - cw
			if pad < 0 {
				pad = 0
			}
			rendered := cell
			if isHeader {
				rendered = t.style.dim(cell)
			}
			isLast := j == len(t.cols)-1
			if col.Align == alignRight {
				if pad > 0 {
					line.WriteString(strings.Repeat(" ", pad))
				}
				line.WriteString(rendered)
			} else {
				line.WriteString(rendered)
				if !isLast && pad > 0 {
					line.WriteString(strings.Repeat(" ", pad))
				}
			}
		}
		line.WriteByte('\n')
		_, _ = io.WriteString(w, line.String())
	}

	headers := make([]string, len(t.cols))
	for j, c := range t.cols {
		headers[j] = c.Header
	}
	writeRow(headers, true)
	for _, row := range t.rows {
		writeRow(row, false)
	}
}

// ---------------------------------------------------------------------------
// Log line coloring & Watch frame rendering
// ---------------------------------------------------------------------------

var logHighlightRe = regexp.MustCompile(`(\[REDACTED\]|\b(?:FATAL|ERROR|WARN|WARNING)\b)`)

func formatLogLine(st termStyle, ln hub.LogLine) string {
	ts := ln.Timestamp
	if ts == "" {
		ts = "-"
	}
	text := colorizeLogText(st, ln.Text)
	if ln.Stream != "" {
		tag := "[" + ln.Stream + "]"
		switch strings.ToLower(ln.Stream) {
		case "stdout":
			tag = st.cyan(tag)
		case "stderr":
			tag = st.amber(tag)
		case "hub":
			tag = st.emerald(tag)
		default:
			tag = st.dim(tag)
		}
		return fmt.Sprintf("%s %s %s\n", st.dim(ts), tag, text)
	}
	return fmt.Sprintf("%s %s\n", st.dim(ts), text)
}

func colorizeLogText(st termStyle, text string) string {
	if !st.color || text == "" {
		return text
	}
	return logHighlightRe.ReplaceAllStringFunc(text, func(tok string) string {
		switch tok {
		case hub.RedactedPlaceholder:
			return st.magenta(tok)
		case "FATAL", "ERROR":
			return st.crimsonBold(tok)
		case "WARN", "WARNING":
			return st.amberBold(tok)
		default:
			return tok
		}
	})
}

// formatWatchFrame wraps a pre-rendered status buffer for flicker-free VT repaint:
// cursor home (\x1b[H), clear-to-EOL (\x1b[K) per line, and clear-to-EOS (\x1b[J) at the bottom.
func formatWatchFrame(rendered []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(rendered) + 128)
	out.WriteString("\x1b[H")
	lines := bytes.Split(rendered, []byte{'\n'})
	// If rendered ended with '\n', the last split element is empty; avoid extra trailing blank line.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for _, ln := range lines {
		out.Write(ln)
		out.WriteString("\x1b[K\n")
	}
	out.WriteString("\x1b[J")
	return out.Bytes()
}
