package trayui

import (
	"image"
	"image/color"
	"math"
)

// Canvas is a 32-bit premultiplied BGRA pixel buffer suitable for direct upload
// to Win32 UpdateLayeredWindow / CreateIconIndirect DIBs and PNG encoding.
type Canvas struct {
	Width  int
	Height int
	Pix    []byte // len == Width * Height * 4, premultiplied [B, G, R, A]
	Clip   *Rect  // optional active clipping rectangle
}

// NewCanvas allocates a transparent canvas of the given pixel dimensions.
func NewCanvas(w, h int) *Canvas {
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return &Canvas{
		Width:  w,
		Height: h,
		Pix:    make([]byte, w*h*4),
	}
}

// Fill fills the entire canvas with a solid color (overwriting previous contents).
func (c *Canvas) Fill(col RGBA) {
	a := float64(col.A) / 255.0
	pb := uint8(math.Round(float64(col.B) * a))
	pg := uint8(math.Round(float64(col.G) * a))
	pr := uint8(math.Round(float64(col.R) * a))
	pa := col.A
	for i := 0; i < len(c.Pix); i += 4 {
		c.Pix[i+0] = pb
		c.Pix[i+1] = pg
		c.Pix[i+2] = pr
		c.Pix[i+3] = pa
	}
}

// BlendPixel composites non-premultiplied color col scaled by coverage [0..1]
// onto pixel (x, y) using Porter-Duff SrcOver in premultiplied BGRA space.
func (c *Canvas) BlendPixel(x, y int, col RGBA, cov float64) {
	if x < 0 || y < 0 || x >= c.Width || y >= c.Height || cov <= 0 {
		return
	}
	if c.Clip != nil {
		if int32(x) < c.Clip.Left || int32(x) >= c.Clip.Right || int32(y) < c.Clip.Top || int32(y) >= c.Clip.Bottom {
			return
		}
	}
	if cov > 1 {
		cov = 1
	}
	sa := (float64(col.A) / 255.0) * cov
	if sa <= 0 {
		return
	}
	idx := (y*c.Width + x) * 4
	if sa >= 1.0-1e-6 {
		c.Pix[idx+0] = col.B
		c.Pix[idx+1] = col.G
		c.Pix[idx+2] = col.R
		c.Pix[idx+3] = 255
		return
	}
	inv := 1.0 - sa
	sb := float64(col.B) * sa
	sg := float64(col.G) * sa
	sr := float64(col.R) * sa

	db := float64(c.Pix[idx+0])
	dg := float64(c.Pix[idx+1])
	dr := float64(c.Pix[idx+2])
	da := float64(c.Pix[idx+3])

	c.Pix[idx+0] = clampByte(sb + db*inv)
	c.Pix[idx+1] = clampByte(sg + dg*inv)
	c.Pix[idx+2] = clampByte(sr + dr*inv)
	c.Pix[idx+3] = clampByte(sa*255.0 + da*inv)
}

// KnockoutPixel erases destination alpha proportionally to cov [0..1] (Porter-Duff DstOut).
func (c *Canvas) KnockoutPixel(x, y int, cov float64) {
	if x < 0 || y < 0 || x >= c.Width || y >= c.Height || cov <= 0 {
		return
	}
	if cov > 1 {
		cov = 1
	}
	idx := (y*c.Width + x) * 4
	if cov >= 1.0-1e-6 {
		c.Pix[idx+0] = 0
		c.Pix[idx+1] = 0
		c.Pix[idx+2] = 0
		c.Pix[idx+3] = 0
		return
	}
	inv := 1.0 - cov
	c.Pix[idx+0] = clampByte(float64(c.Pix[idx+0]) * inv)
	c.Pix[idx+1] = clampByte(float64(c.Pix[idx+1]) * inv)
	c.Pix[idx+2] = clampByte(float64(c.Pix[idx+2]) * inv)
	c.Pix[idx+3] = clampByte(float64(c.Pix[idx+3]) * inv)
}

// CompositeOver draws src onto c at integer offset (dx, dy) using premultiplied SrcOver.
func (c *Canvas) CompositeOver(src *Canvas, dx, dy int) {
	if src == nil {
		return
	}
	for sy := 0; sy < src.Height; sy++ {
		ty := dy + sy
		if ty < 0 || ty >= c.Height {
			continue
		}
		for sx := 0; sx < src.Width; sx++ {
			tx := dx + sx
			if tx < 0 || tx >= c.Width {
				continue
			}
			si := (sy*src.Width + sx) * 4
			saByte := src.Pix[si+3]
			if saByte == 0 {
				continue
			}
			di := (ty*c.Width + tx) * 4
			if saByte == 255 {
				copy(c.Pix[di:di+4], src.Pix[si:si+4])
				continue
			}
			inv := 1.0 - float64(saByte)/255.0
			c.Pix[di+0] = clampByte(float64(src.Pix[si+0]) + float64(c.Pix[di+0])*inv)
			c.Pix[di+1] = clampByte(float64(src.Pix[si+1]) + float64(c.Pix[di+1])*inv)
			c.Pix[di+2] = clampByte(float64(src.Pix[si+2]) + float64(c.Pix[di+2])*inv)
			c.Pix[di+3] = clampByte(float64(saByte) + float64(c.Pix[di+3])*inv)
		}
	}
}

// ToNRGBA converts the premultiplied BGRA buffer to a standard non-premultiplied *image.NRGBA.
func (c *Canvas) ToNRGBA() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, c.Width, c.Height))
	for y := 0; y < c.Height; y++ {
		for x := 0; x < c.Width; x++ {
			i := (y*c.Width + x) * 4
			b, g, r, a := c.Pix[i+0], c.Pix[i+1], c.Pix[i+2], c.Pix[i+3]
			if a == 0 {
				continue
			}
			if a == 255 {
				img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
				continue
			}
			af := float64(a) / 255.0
			img.SetNRGBA(x, y, color.NRGBA{
				R: clampByte(float64(r) / af),
				G: clampByte(float64(g) / af),
				B: clampByte(float64(b) / af),
				A: a,
			})
		}
	}
	return img
}

// FillRect fills an integer-aligned rectangle with crisp edges.
func (c *Canvas) FillRect(x, y, w, h int, col RGBA) {
	if w <= 0 || h <= 0 || col.A == 0 {
		return
	}
	x0 := maxInt(0, x)
	y0 := maxInt(0, y)
	x1 := minInt(c.Width, x+w)
	y1 := minInt(c.Height, y+h)
	for py := y0; py < y1; py++ {
		for px := x0; px < x1; px++ {
			c.BlendPixel(px, py, col, 1.0)
		}
	}
}

// FillRoundedRect fills a rounded rectangle using analytical SDF anti-aliasing.
func (c *Canvas) FillRoundedRect(x, y, w, h, r float64, col RGBA) {
	if w <= 0 || h <= 0 || col.A == 0 {
		return
	}
	r = clampFloat(r, 0, math.Min(w, h)*0.5)
	cx, cy := x+w*0.5, y+h*0.5
	hw, hh := w*0.5, h*0.5

	x0 := maxInt(0, int(math.Floor(x-1)))
	y0 := maxInt(0, int(math.Floor(y-1)))
	x1 := minInt(c.Width, int(math.Ceil(x+w+1)))
	y1 := minInt(c.Height, int(math.Ceil(y+h+1)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := sdfRoundedRect(fx, fy, cx, cy, hw, hh, r)
			if d < 0.5 {
				cov := clampFloat(0.5-d, 0, 1)
				c.BlendPixel(px, py, col, cov)
			}
		}
	}
}

// FillRoundedRectGradientV fills a rounded rectangle with a smooth vertical linear gradient.
func (c *Canvas) FillRoundedRectGradientV(x, y, w, h, r float64, top, bottom RGBA) {
	if w <= 0 || h <= 0 {
		return
	}
	r = clampFloat(r, 0, math.Min(w, h)*0.5)
	cx, cy := x+w*0.5, y+h*0.5
	hw, hh := w*0.5, h*0.5

	x0 := maxInt(0, int(math.Floor(x-1)))
	y0 := maxInt(0, int(math.Floor(y-1)))
	x1 := minInt(c.Width, int(math.Ceil(x+w+1)))
	y1 := minInt(c.Height, int(math.Ceil(y+h+1)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		t := clampFloat((fy-y)/h, 0, 1)
		col := lerpRGBA(top, bottom, t)
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := sdfRoundedRect(fx, fy, cx, cy, hw, hh, r)
			if d < 0.5 {
				cov := clampFloat(0.5-d, 0, 1)
				c.BlendPixel(px, py, col, cov)
			}
		}
	}
}

// StrokeRoundedRect strokes a rounded rectangle centered on its perimeter using analytical SDF.
// For a crisp 1px inset hairline on a card [x, y, w, h, r], pass [x+0.5, y+0.5, w-1, h-1, r-0.5, 1.0].
func (c *Canvas) StrokeRoundedRect(x, y, w, h, r, strokeW float64, col RGBA) {
	if w <= 0 || h <= 0 || strokeW <= 0 || col.A == 0 {
		return
	}
	r = clampFloat(r, 0, math.Min(w, h)*0.5)
	cx, cy := x+w*0.5, y+h*0.5
	hw, hh := w*0.5, h*0.5
	halfS := strokeW * 0.5
	pad := halfS + 1.5

	x0 := maxInt(0, int(math.Floor(x-pad)))
	y0 := maxInt(0, int(math.Floor(y-pad)))
	x1 := minInt(c.Width, int(math.Ceil(x+w+pad)))
	y1 := minInt(c.Height, int(math.Ceil(y+h+pad)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := math.Abs(sdfRoundedRect(fx, fy, cx, cy, hw, hh, r)) - halfS
			if d < 0.5 {
				cov := clampFloat(0.5-d, 0, 1)
				c.BlendPixel(px, py, col, cov)
			}
		}
	}
}

// DrawCardShadow paints a refined 3-pass soft drop shadow (offsetY +2px) around a card.
func (c *Canvas) DrawCardShadow(x, y, w, h, r float64, isDark bool, scale float64) {
	type shadowPass struct {
		expand float64
		blur   float64
		alpha  uint8
	}
	var passes [3]shadowPass
	if isDark {
		passes = [3]shadowPass{
			{expand: 4.5, blur: 5.5, alpha: 24},
			{expand: 2.2, blur: 3.0, alpha: 34},
			{expand: 0.6, blur: 1.4, alpha: 42},
		}
	} else {
		passes = [3]shadowPass{
			{expand: 4.5, blur: 5.5, alpha: 11},
			{expand: 2.2, blur: 3.0, alpha: 16},
			{expand: 0.6, blur: 1.4, alpha: 22},
		}
	}
	shiftY := 2.0 * scale

	for _, p := range passes {
		ex := p.expand * scale
		blur := math.Max(1.0, p.blur*scale)
		sx := x - ex*0.4
		sy := y - ex*0.3 + shiftY
		sw := w + ex*0.8
		sh := h + ex*0.8
		sr := r + ex*0.4
		cx, cy := sx+sw*0.5, sy+sh*0.5
		hw, hh := sw*0.5, sh*0.5

		x0 := maxInt(0, int(math.Floor(sx-blur-1)))
		y0 := maxInt(0, int(math.Floor(sy-blur-1)))
		x1 := minInt(c.Width, int(math.Ceil(sx+sw+blur+1)))
		y1 := minInt(c.Height, int(math.Ceil(sy+sh+blur+1)))

		col := RGBA{0, 0, 0, p.alpha}
		for py := y0; py < y1; py++ {
			fy := float64(py) + 0.5
			for px := x0; px < x1; px++ {
				fx := float64(px) + 0.5
				d := sdfRoundedRect(fx, fy, cx, cy, hw, hh, sr)
				if d >= blur {
					continue
				}
				var cov float64
				if d <= -blur*0.3 {
					cov = 1.0
				} else {
					t := clampFloat((blur-d)/(blur*1.3), 0, 1)
					cov = t * t * (3 - 2*t) // smoothstep feather
				}
				c.BlendPixel(px, py, col, cov)
			}
		}
	}
}

// FillCircle paints an analytically anti-aliased filled circle.
func (c *Canvas) FillCircle(cx, cy, r float64, col RGBA) {
	if r <= 0 || col.A == 0 {
		return
	}
	x0 := maxInt(0, int(math.Floor(cx-r-1.5)))
	y0 := maxInt(0, int(math.Floor(cy-r-1.5)))
	x1 := minInt(c.Width, int(math.Ceil(cx+r+1.5)))
	y1 := minInt(c.Height, int(math.Ceil(cy+r+1.5)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := math.Hypot(fx-cx, fy-cy) - r
			if d < 0.5 {
				c.BlendPixel(px, py, col, clampFloat(0.5-d, 0, 1))
			}
		}
	}
}

// FillRadialGlow paints a soft radial glow centered at (cx, cy) with radius r.
func (c *Canvas) FillRadialGlow(cx, cy, r float64, col RGBA) {
	if r <= 0 || col.A == 0 {
		return
	}
	x0 := maxInt(0, int(math.Floor(cx-r-1)))
	y0 := maxInt(0, int(math.Floor(cy-r-1)))
	x1 := minInt(c.Width, int(math.Ceil(cx+r+1)))
	y1 := minInt(c.Height, int(math.Ceil(cy+r+1)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			dist := math.Hypot(fx-cx, fy-cy)
			if dist >= r {
				continue
			}
			t := 1.0 - dist/r
			cov := t * t
			c.BlendPixel(px, py, col, cov)
		}
	}
}

// KnockoutCircle erases canvas pixels inside a circle with analytical anti-aliasing.
func (c *Canvas) KnockoutCircle(cx, cy, r float64) {
	if r <= 0 {
		return
	}
	x0 := maxInt(0, int(math.Floor(cx-r-1.5)))
	y0 := maxInt(0, int(math.Floor(cy-r-1.5)))
	x1 := minInt(c.Width, int(math.Ceil(cx+r+1.5)))
	y1 := minInt(c.Height, int(math.Ceil(cy+r+1.5)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := math.Hypot(fx-cx, fy-cy) - r
			if d < 0.5 {
				c.KnockoutPixel(px, py, clampFloat(0.5-d, 0, 1))
			}
		}
	}
}

// StrokeCircle paints an analytically anti-aliased stroked circle ring.
func (c *Canvas) StrokeCircle(cx, cy, r, strokeW float64, col RGBA) {
	if r <= 0 || strokeW <= 0 || col.A == 0 {
		return
	}
	halfS := strokeW * 0.5
	pad := r + halfS + 1.5
	x0 := maxInt(0, int(math.Floor(cx-pad)))
	y0 := maxInt(0, int(math.Floor(cy-pad)))
	x1 := minInt(c.Width, int(math.Ceil(cx+pad)))
	y1 := minInt(c.Height, int(math.Ceil(cy+pad)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := math.Abs(math.Hypot(fx-cx, fy-cy)-r) - halfS
			if d < 0.5 {
				c.BlendPixel(px, py, col, clampFloat(0.5-d, 0, 1))
			}
		}
	}
}

// StrokeArc paints an analytically anti-aliased open circular arc with round caps.
// Angles are in degrees: 0 = right (+X), 90 = down (+Y), -90 = up (-Y).
func (c *Canvas) StrokeArc(cx, cy, r, startDeg, sweepDeg, strokeW float64, col RGBA) {
	if r <= 0 || strokeW <= 0 || col.A == 0 || sweepDeg == 0 {
		return
	}
	halfS := strokeW * 0.5
	pad := r + halfS + 1.5
	x0 := maxInt(0, int(math.Floor(cx-pad)))
	y0 := maxInt(0, int(math.Floor(cy-pad)))
	x1 := minInt(c.Width, int(math.Ceil(cx+pad)))
	y1 := minInt(c.Height, int(math.Ceil(cy+pad)))

	for py := y0; py < y1; py++ {
		fy := float64(py) + 0.5
		for px := x0; px < x1; px++ {
			fx := float64(px) + 0.5
			d := sdfArc(fx, fy, cx, cy, r, startDeg, sweepDeg) - halfS
			if d < 0.5 {
				c.BlendPixel(px, py, col, clampFloat(0.5-d, 0, 1))
			}
		}
	}
}

// StrokeLine paints an analytically anti-aliased line segment with round caps.
func (c *Canvas) StrokeLine(x1, y1, x2, y2, strokeW float64, col RGBA) {
	if strokeW <= 0 || col.A == 0 {
		return
	}
	halfS := strokeW * 0.5
	pad := halfS + 1.5
	minX := maxInt(0, int(math.Floor(math.Min(x1, x2)-pad)))
	minY := maxInt(0, int(math.Floor(math.Min(y1, y2)-pad)))
	maxX := minInt(c.Width, int(math.Ceil(math.Max(x1, x2)+pad)))
	maxY := minInt(c.Height, int(math.Ceil(math.Max(y1, y2)+pad)))

	for py := minY; py < maxY; py++ {
		fy := float64(py) + 0.5
		for px := minX; px < maxX; px++ {
			fx := float64(px) + 0.5
			d := sdfSegment(fx, fy, x1, y1, x2, y2) - halfS
			if d < 0.5 {
				c.BlendPixel(px, py, col, clampFloat(0.5-d, 0, 1))
			}
		}
	}
}

// SDF primitives in pixel coordinates.

func sdfRoundedRect(px, py, cx, cy, halfW, halfH, r float64) float64 {
	qx := math.Abs(px-cx) - (halfW - r)
	qy := math.Abs(py-cy) - (halfH - r)
	outD := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
	inD := math.Min(math.Max(qx, qy), 0)
	return outD + inD - r
}

func sdfSegment(px, py, ax, ay, bx, by float64) float64 {
	pax, pay := px-ax, py-ay
	bax, bay := bx-ax, by-ay
	denom := bax*bax + bay*bay
	var h float64
	if denom > 1e-12 {
		h = (pax*bax + pay*bay) / denom
		if h < 0 {
			h = 0
		} else if h > 1 {
			h = 1
		}
	}
	return math.Hypot(pax-bax*h, pay-bay*h)
}

func sdfArc(px, py, cx, cy, r, startDeg, sweepDeg float64) float64 {
	if math.Abs(sweepDeg) >= 360 {
		return math.Abs(math.Hypot(px-cx, py-cy) - r)
	}
	midRad := (startDeg + sweepDeg*0.5) * math.Pi / 180.0
	halfRad := math.Abs(sweepDeg) * 0.5 * math.Pi / 180.0
	cosM, sinM := math.Cos(midRad), math.Sin(midRad)

	dx, dy := px-cx, py-cy
	rx := dx*cosM + dy*sinM
	ry := math.Abs(-dx*sinM + dy*cosM)

	if math.Atan2(ry, rx) <= halfRad {
		return math.Abs(math.Hypot(rx, ry) - r)
	}
	ex := r * math.Cos(halfRad)
	ey := r * math.Sin(halfRad)
	return math.Hypot(rx-ex, ry-ey)
}

func sdfTriangle(px, py, ax, ay, bx, by, cx, cy float64) float64 {
	d1 := sdfSegment(px, py, ax, ay, bx, by)
	d2 := sdfSegment(px, py, bx, by, cx, cy)
	d3 := sdfSegment(px, py, cx, cy, ax, ay)
	d := math.Min(d1, math.Min(d2, d3))

	c1 := (bx-ax)*(py-ay) - (by-ay)*(px-ax)
	c2 := (cx-bx)*(py-by) - (cy-by)*(px-bx)
	c3 := (ax-cx)*(py-cy) - (ay-cy)*(px-cx)
	if (c1 >= 0 && c2 >= 0 && c3 >= 0) || (c1 <= 0 && c2 <= 0 && c3 <= 0) {
		return -d
	}
	return d
}

func clampByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(math.Round(v))
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func lerpRGBA(a, b RGBA, t float64) RGBA {
	return RGBA{
		R: clampByte(float64(a.R) + (float64(b.R)-float64(a.R))*t),
		G: clampByte(float64(a.G) + (float64(b.G)-float64(a.G))*t),
		B: clampByte(float64(a.B) + (float64(b.B)-float64(a.B))*t),
		A: clampByte(float64(a.A) + (float64(b.A)-float64(a.A))*t),
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
