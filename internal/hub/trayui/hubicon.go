package trayui

import "math"

// RenderTrayIcon renders the dynamic Tri-Node Hub tray icon at native taskbar DPI size
// (16, 20, 24, 32+ px), adapting emblem ink to systemLightTheme and compositing one of
// five distinct semantic status badges in the bottom-right corner with a clean alpha-knockout moat.
func RenderTrayIcon(size int, state string, systemLightTheme bool) *Canvas {
	if size < 16 {
		size = 16
	}
	c := NewCanvas(size, size)
	scale := float64(size) / 24.0

	emblemColor := RGBA{0xF5, 0xF6, 0xF8, 0xFF} // Dark taskbar -> crisp ivory
	if systemLightTheme {
		emblemColor = RGBA{0x15, 0x16, 0x19, 0xFF} // Light taskbar -> obsidian ink
	}

	// Optically shift the Tri-Node Hub slightly top-left to balance the bottom-right badge.
	// Downward-Y topology (-150°, -30°, 90°) naturally cradles the bottom-right status badge
	// without colliding with any of the three satellite nodes!
	cx := 10.6 * scale
	cy := 10.4 * scale

	// Stroke compensation at small sizes (16px) keeps lines razor-sharp rather than wispy.
	strokeW := 2.0 * scale
	if size <= 18 {
		strokeW = math.Max(strokeW, 1.45)
	}
	hubR := 2.8 * scale
	spokeLen := 6.7 * scale
	nodeR := 1.95 * scale
	if size <= 18 {
		nodeR = math.Max(nodeR, 1.42)
	}

	angles := [3]float64{-150, -30, 90}

	// Render the Tri-Node Hub emblem using a single unified SDF field (no joint double-blend).
	halfS := strokeW * 0.5
	for py := 0; py < size; py++ {
		fy := float64(py) + 0.5
		for px := 0; px < size; px++ {
			fx := float64(px) + 0.5
			minD := math.Abs(math.Hypot(fx-cx, fy-cy)-hubR) - halfS
			for _, deg := range angles {
				rad := deg * math.Pi / 180.0
				cosA, sinA := math.Cos(rad), math.Sin(rad)
				sx := cx + cosA*hubR
				sy := cy + sinA*hubR
				nx := cx + cosA*spokeLen
				ny := cy + sinA*spokeLen
				dSpoke := sdfSegment(fx, fy, sx, sy, nx, ny) - halfS
				dNode := math.Hypot(fx-nx, fy-ny) - nodeR
				if dSpoke < minD {
					minD = dSpoke
				}
				if dNode < minD {
					minD = dNode
				}
			}
			if minD < 0.5 {
				c.BlendPixel(px, py, emblemColor, clampFloat(0.5-minD, 0, 1))
			}
		}
	}

	// Bottom-right status badge with clean transparent alpha-knockout moat.
	bx := 18.2 * scale
	by := 18.2 * scale
	badgeR := 4.4 * scale
	moatR := 5.75 * scale
	if size <= 18 {
		bx = float64(size) - 3.7
		by = float64(size) - 3.7
		badgeR = 3.1
		moatR = 4.25
	}

	c.KnockoutCircle(bx, by, moatR)

	theme := ThemeFor(systemLightTheme)
	glyphInk := RGBA{0xFF, 0xFF, 0xFF, 0xFF}
	if !systemLightTheme {
		glyphInk = RGBA{0x18, 0x19, 0x1C, 0xFF}
	}

	switch state {
	case "ok", "":
		// 1. Emerald filled badge
		c.FillCircle(bx, by, badgeR, theme.Success)

	case "sleeping":
		// 2. Calm slate hollow standby ring (unmistakably distinct from filled/warning)
		ringStroke := math.Max(1.35, 1.85*scale)
		ringR := badgeR - ringStroke*0.45
		c.StrokeCircle(bx, by, ringR, ringStroke, theme.Sleeping)

	case "starting":
		// 3. Cobalt 270-degree open progress arc
		arcW := math.Max(1.35, 1.85*scale)
		arcR := badgeR - arcW*0.45
		c.StrokeArc(bx, by, arcR, -90, 270, arcW, theme.Accent)

	case "degraded":
		// 4. Amber badge with crisp horizontal minus mark (>=20px)
		c.FillCircle(bx, by, badgeR, theme.Warning)
		if size >= 20 {
			halfBar := badgeR * 0.48
			barW := math.Max(1.25, 1.5*scale)
			c.StrokeLine(bx-halfBar, by, bx+halfBar, by, barW, glyphInk)
		}

	case "error":
		// 5. Crimson badge with crisp '!' mark (>=20px)
		c.FillCircle(bx, by, badgeR, theme.Error)
		if size >= 20 {
			barW := math.Max(1.2, 1.45*scale)
			topY := by - badgeR*0.52
			midY := by + badgeR*0.04
			dotY := by + badgeR*0.48
			c.StrokeLine(bx, topY, bx, midY, barW, glyphInk)
			c.FillCircle(bx, dotY, barW*0.58, glyphInk)
		}

	default:
		c.FillCircle(bx, by, badgeR, theme.Warning)
	}

	return c
}

// RenderAppIcon renders the flagship McpHub application icon at any square resolution
// (16, 20, 24, 32, 48, 128, 256 px): dark obsidian rounded squircle with subtle rim,
// centered ivory Tri-Node Hub emblem, and glowing cobalt #60A5FA hub core.
func RenderAppIcon(size int) *Canvas {
	if size < 16 {
		size = 16
	}
	c := NewCanvas(size, size)
	s := float64(size)

	// Margin leaves crisp anti-aliased edge on larger sizes while staying full-bleed at 16px.
	margin := s * 0.04
	if size <= 20 {
		margin = 0.5
	}
	boxW := s - margin*2
	boxH := s - margin*2
	cornerR := boxW * 0.235

	// Dark obsidian rounded squircle with subtle vertical sheen around #18191C.
	topCol := RGBA{0x20, 0x22, 0x28, 0xFF}
	botCol := RGBA{0x13, 0x14, 0x17, 0xFF}
	c.FillRoundedRectGradientV(margin, margin, boxW, boxH, cornerR, topCol, botCol)

	// Subtle ambient cobalt aura behind the hub core.
	cx := s * 0.5
	cy := s * 0.485
	if size >= 24 {
		c.FillRadialGlow(cx, cy, boxW*0.42, RGBA{0x60, 0xA5, 0xFA, 44})
	}

	// Crisp hairline rim (#383D4A).
	rimW := math.Max(1.0, s*0.014)
	if size <= 24 {
		rimW = 1.0
	}
	inset := rimW * 0.5
	c.StrokeRoundedRect(margin+inset, margin+inset, boxW-rimW, boxH-rimW, math.Max(1, cornerR-inset), rimW, RGBA{0x38, 0x3D, 0x4A, 0xFF})

	// Centered ivory Tri-Node Hub emblem.
	unit := boxW / 24.0
	hubR := 3.55 * unit
	spokeLen := 7.65 * unit
	nodeR := 2.2 * unit
	strokeW := 2.05 * unit
	if size <= 20 {
		strokeW = math.Max(strokeW, 1.35)
		nodeR = math.Max(nodeR, 1.45)
	}

	ivory := RGBA{0xF5, 0xF6, 0xF8, 0xFF}
	angles := [3]float64{-150, -30, 90}

	// Render unified Tri-Node Hub structure (central ring + 3 radial spokes + 3 satellite nodes).
	halfS := strokeW * 0.5
	for py := 0; py < size; py++ {
		fy := float64(py) + 0.5
		for px := 0; px < size; px++ {
			fx := float64(px) + 0.5
			minD := math.Abs(math.Hypot(fx-cx, fy-cy)-hubR) - halfS
			for _, deg := range angles {
				rad := deg * math.Pi / 180.0
				cosA, sinA := math.Cos(rad), math.Sin(rad)
				sx := cx + cosA*hubR
				sy := cy + sinA*hubR
				nx := cx + cosA*spokeLen
				ny := cy + sinA*spokeLen
				dSpoke := sdfSegment(fx, fy, sx, sy, nx, ny) - halfS
				dNode := math.Hypot(fx-nx, fy-ny) - nodeR
				if dSpoke < minD {
					minD = dSpoke
				}
				if dNode < minD {
					minD = dNode
				}
			}
			if minD < 0.5 {
				c.BlendPixel(px, py, ivory, clampFloat(0.5-minD, 0, 1))
			}
		}
	}

	// On larger icons (>=48px), add a subtle obsidian pupil inside each satellite node for depth.
	if size >= 48 {
		for _, deg := range angles {
			rad := deg * math.Pi / 180.0
			nx := cx + math.Cos(rad)*spokeLen
			ny := cy + math.Sin(rad)*spokeLen
			c.FillCircle(nx, ny, nodeR*0.36, RGBA{0x18, 0x19, 0x1C, 0xFF})
		}
	}

	// Bright cobalt #60A5FA glowing hub core.
	cobalt := RGBA{0x60, 0xA5, 0xFA, 0xFF}
	coreR := 1.95 * unit
	if size <= 20 {
		coreR = math.Max(coreR, 1.35)
	}
	if size >= 32 {
		c.FillRadialGlow(cx, cy, hubR*1.45, RGBA{0x60, 0xA5, 0xFA, 78})
	}
	c.FillCircle(cx, cy, coreR, cobalt)
	if size >= 48 {
		// Subtle specular core highlight for flagship polish.
		c.FillCircle(cx-coreR*0.24, cy-coreR*0.24, coreR*0.42, RGBA{0xBF, 0xDB, 0xFE, 210})
	}

	return c
}
