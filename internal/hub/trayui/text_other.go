//go:build !windows

package trayui

import "math"

// MeasureText provides deterministic font metrics on non-Windows platforms
// so cross-platform tests and layout verification run cleanly.
func MeasureText(s string, spec FontSpec) TextSize {
	h := int(math.Round(spec.SizePx * 1.25))
	if h < 8 {
		h = 8
	}
	ascent := int(math.Round(float64(h) * 0.78))
	if s == "" {
		return TextSize{W: 0, H: h, Ascent: ascent}
	}
	adv := spec.SizePx * 0.56
	if spec.Family == FontMono {
		adv = spec.SizePx * 0.60
	}
	var w float64
	for _, r := range s {
		if isCJKRune(r) {
			w += spec.SizePx * 1.0
		} else {
			w += adv
		}
	}
	return TextSize{W: int(math.Ceil(w)), H: h, Ascent: ascent}
}

// DrawTexts renders simple crisp glyph blocks on non-Windows platforms so
// offscreen layout tests verify content coverage without Windows GDI.
func DrawTexts(c *Canvas, draws []TextDraw) {
	if c == nil || len(draws) == 0 {
		return
	}
	for _, d := range draws {
		if d.Text == "" || d.Color.A == 0 {
			continue
		}
		text := d.Text
		if d.MaxW > 0 {
			text = TruncateText(text, d.Spec, d.MaxW)
		}
		if text == "" {
			continue
		}
		sz := MeasureText(text, d.Spec)
		x := d.X
		if d.AlignR {
			x = d.X - sz.W
		}
		adv := d.Spec.SizePx * 0.56
		if d.Spec.Family == FontMono {
			adv = d.Spec.SizePx * 0.60
		}
		glyphH := maxInt(2, int(math.Round(d.Spec.SizePx*0.62)))
		gy := d.Y + maxInt(1, (sz.H-glyphH)/2)
		curX := float64(x)
		for _, r := range text {
			w := adv
			if isCJKRune(r) {
				w = d.Spec.SizePx
			}
			if r != ' ' {
				gw := maxInt(1, int(math.Round(w*0.65)))
				c.FillRect(int(math.Round(curX)), gy, gw, glyphH, d.Color)
			}
			curX += w
		}
	}
}
