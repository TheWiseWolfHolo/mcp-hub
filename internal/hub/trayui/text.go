package trayui

import (
	"math"
	"strings"
)

// FontFamily selects the semantic typography role.
type FontFamily int

const (
	FontSans FontFamily = iota
	FontMono
)

// FontSpec defines a text style in physical pixels (already DPI-scaled).
type FontSpec struct {
	Family FontFamily
	SizePx float64
	Weight int // 400 normal, 500 medium, 600 semibold, 700 bold
}

// TextDraw describes one positioned text draw operation queued for a frame.
type TextDraw struct {
	Text   string
	X      int
	Y      int
	MaxW   int // <= 0 means unclipped
	Color  RGBA
	Spec   FontSpec
	AlignR bool  // if true, X is the right edge and text is right-aligned
	Clip   *Rect // optional clipping rectangle (e.g. scrollable list viewport)
}

// TextSize holds measured text dimensions in physical pixels.
type TextSize struct {
	W      int
	H      int
	Ascent int
}

type textRun struct {
	text  string
	isCJK bool
}

// splitTextRuns partitions s into contiguous Latin/symbol and CJK runs so GDI
// renders CJK cleanly in Microsoft YaHei UI while keeping Latin in Segoe UI / Cascadia Mono.
func splitTextRuns(s string) []textRun {
	if s == "" {
		return nil
	}
	var runs []textRun
	start := 0
	var curCJK bool
	first := true
	for i, r := range s {
		cjk := isCJKRune(r)
		if first {
			curCJK = cjk
			first = false
			continue
		}
		if cjk != curCJK {
			runs = append(runs, textRun{text: s[start:i], isCJK: curCJK})
			start = i
			curCJK = cjk
		}
	}
	runs = append(runs, textRun{text: s[start:], isCJK: curCJK})
	return runs
}

func isCJKRune(r rune) bool {
	switch {
	case r >= 0x2E80 && r <= 0x9FFF: // CJK Radicals, Kangxi, Punctuation, Hiragana, Katakana, Bopomofo, Ideographs
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul Syllables
		return true
	case r >= 0xF900 && r <= 0xFAFF: // CJK Compatibility Ideographs
		return true
	case r >= 0xFE30 && r <= 0xFE4F: // CJK Compatibility Forms
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // Fullwidth Forms
		return true
	case r >= 0x20000 && r <= 0x2FA1F: // CJK Extension B-F
		return true
	default:
		return false
	}
}

// TruncateText shortens s with an ellipsis ("…") if its measured width exceeds maxW.
func TruncateText(s string, spec FontSpec, maxW int) string {
	if s == "" || maxW <= 0 {
		return s
	}
	if MeasureText(s, spec).W <= maxW {
		return s
	}
	const ell = "…"
	ellW := MeasureText(ell, spec).W
	if ellW >= maxW {
		return ""
	}
	runes := []rune(s)
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		cand := string(runes[:mid]) + ell
		if MeasureText(cand, spec).W <= maxW {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return ell
	}
	return strings.TrimRight(string(runes[:lo]), " ") + ell
}

// WrapText wraps s into at most maxLines lines fitting within maxW pixels.
func WrapText(s string, spec FontSpec, maxW int, maxLines int) []string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
	if s == "" || maxLines <= 0 {
		return nil
	}
	if maxW <= 0 || MeasureText(s, spec).W <= maxW {
		return []string{s}
	}
	var lines []string
	rem := s
	for len(lines) < maxLines && rem != "" {
		if len(lines) == maxLines-1 {
			lines = append(lines, TruncateText(rem, spec, maxW))
			break
		}
		runes := []rune(rem)
		lo, hi := 1, len(runes)
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if MeasureText(string(runes[:mid]), spec).W <= maxW {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		cut := lo
		// Prefer breaking on whitespace when reasonable.
		if cut < len(runes) {
			for j := cut; j > cut/2; j-- {
				if runes[j-1] == ' ' || runes[j-1] == '/' || runes[j-1] == ':' || runes[j-1] == '-' {
					cut = j
					break
				}
			}
		}
		line := strings.TrimSpace(string(runes[:cut]))
		if line != "" {
			lines = append(lines, line)
		}
		rem = strings.TrimSpace(string(runes[cut:]))
	}
	return lines
}

func pxRound(v float64) int {
	return int(math.Round(v))
}
