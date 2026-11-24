package trayui

import "math"

// IconID identifies a 24x24 geometric UI icon.
type IconID int

const (
	IconNone IconID = iota
	IconHub
	IconDashboard
	IconPlay
	IconStop
	IconRestart
	IconReload
	IconSleep
	IconWake
	IconLog
	IconFolder
	IconFileCode
	IconCopy
	IconCheck
	IconPower
	IconChevronRight
	IconChevronDown
	IconPanel
	IconStartAll
)

// AllIcons returns every defined 24x24 UI icon for validation and contact sheets.
func AllIcons() []IconID {
	return []IconID{
		IconHub,
		IconDashboard,
		IconPlay,
		IconStop,
		IconRestart,
		IconReload,
		IconSleep,
		IconWake,
		IconLog,
		IconFolder,
		IconFileCode,
		IconCopy,
		IconCheck,
		IconPower,
		IconChevronRight,
		IconChevronDown,
		IconPanel,
		IconStartAll,
	}
}

// Name returns a human-readable identifier for id.
func (id IconID) Name() string {
	switch id {
	case IconHub:
		return "hub"
	case IconDashboard:
		return "dashboard"
	case IconPlay:
		return "play"
	case IconStop:
		return "stop"
	case IconRestart:
		return "restart"
	case IconReload:
		return "reload"
	case IconSleep:
		return "sleep"
	case IconWake:
		return "wake"
	case IconLog:
		return "log"
	case IconFolder:
		return "folder"
	case IconFileCode:
		return "file-code"
	case IconCopy:
		return "copy"
	case IconCheck:
		return "check"
	case IconPower:
		return "power"
	case IconChevronRight:
		return "chevron-right"
	case IconChevronDown:
		return "chevron-down"
	case IconPanel:
		return "panel"
	case IconStartAll:
		return "start-all"
	default:
		return "none"
	}
}

type iconOpKind int

const (
	opLine iconOpKind = iota
	opCircleStroke
	opCircleFill
	opArcStroke
	opRoundRectStroke
	opRoundRectFill
	opTriangleStroke
	opTriangleFill
	opCrescentStroke
)

// iconOp defines one SDF primitive in 24x24 design coordinates.
type iconOp struct {
	kind       iconOpKind
	a, b, c, d float64
	e, f       float64
}

// iconSpec describes a 24x24 icon rendered as a unified SDF field (zero joint overlap).
type iconSpec struct {
	strokeWidth float64 // default 2.0 on 24x24 grid
	ops         []iconOp
}

func line(x1, y1, x2, y2 float64) iconOp {
	return iconOp{kind: opLine, a: x1, b: y1, c: x2, d: y2}
}

func circleStroke(cx, cy, r float64) iconOp {
	return iconOp{kind: opCircleStroke, a: cx, b: cy, c: r}
}

func circleFill(cx, cy, r float64) iconOp {
	return iconOp{kind: opCircleFill, a: cx, b: cy, c: r}
}

func arcStroke(cx, cy, r, startDeg, sweepDeg float64) iconOp {
	return iconOp{kind: opArcStroke, a: cx, b: cy, c: r, d: startDeg, e: sweepDeg}
}

func roundRectStroke(x, y, w, h, r float64) iconOp {
	return iconOp{kind: opRoundRectStroke, a: x, b: y, c: w, d: h, e: r}
}

func triangleStroke(ax, ay, bx, by, cx, cy float64) iconOp {
	return iconOp{kind: opTriangleStroke, a: ax, b: ay, c: bx, d: by, e: cx, f: cy}
}

func crescentStroke(cx1, cy1, r1, cx2, cy2, r2 float64) iconOp {
	return iconOp{kind: opCrescentStroke, a: cx1, b: cy1, c: r1, d: cx2, e: cy2, f: r2}
}

func polyline(pts ...[2]float64) []iconOp {
	ops := make([]iconOp, 0, len(pts)-1)
	for i := 0; i+1 < len(pts); i++ {
		ops = append(ops, line(pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1]))
	}
	return ops
}

func specFor(id IconID) iconSpec {
	switch id {
	case IconHub:
		// Tri-Node Hub on 24x24 grid: central ring + 3 radial spokes + 3 satellite nodes.
		// Downward-Y symmetry: top-left (-150°), top-right (-30°), bottom (90°).
		const cx, cy = 12.0, 11.4
		const hubR = 2.6
		const spokeLen = 6.7
		const nodeR = 1.65
		angles := [3]float64{-150, -30, 90}
		ops := []iconOp{circleStroke(cx, cy, hubR)}
		for _, deg := range angles {
			rad := deg * math.Pi / 180.0
			cosA, sinA := math.Cos(rad), math.Sin(rad)
			sx := cx + cosA*(hubR+0.2)
			sy := cy + sinA*(hubR+0.2)
			nx := cx + cosA*spokeLen
			ny := cy + sinA*spokeLen
			ops = append(ops,
				line(sx, sy, nx-cosA*(nodeR*0.6), ny-sinA*(nodeR*0.6)),
				circleFill(nx, ny, nodeR),
			)
		}
		return iconSpec{strokeWidth: 2.0, ops: ops}

	case IconDashboard:
		// Clean 2x2 geometric dashboard grid inside [4.2, 19.8] (~24% ink coverage).
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				roundRectStroke(4.4, 4.4, 6.0, 6.0, 1.4),
				roundRectStroke(13.6, 4.4, 6.0, 6.0, 1.4),
				roundRectStroke(4.4, 13.6, 6.0, 6.0, 1.4),
				roundRectStroke(13.6, 13.6, 6.0, 6.0, 1.4),
			},
		}

	case IconPlay:
		// Crisp rounded play triangle optically centered slightly right.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				triangleStroke(7.4, 5.2, 18.6, 12.0, 7.4, 18.8),
			},
		}

	case IconStop:
		// Crisp rounded square.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				roundRectStroke(5.5, 5.5, 13.0, 13.0, 2.5),
			},
		}

	case IconRestart:
		// 285-degree circular arc + crisp top-right arrowhead.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				arcStroke(12.0, 12.0, 7.0, -45, 285),
				line(16.95, 7.05, 16.95, 3.8),
				line(16.95, 7.05, 13.7, 7.05),
			},
		}

	case IconReload:
		// Dual opposing refresh arcs with arrowheads.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				arcStroke(12.0, 12.0, 7.0, -155, 120),
				line(17.7, 8.0, 18.0, 4.6),
				line(17.7, 8.0, 14.5, 8.4),
				arcStroke(12.0, 12.0, 7.0, 25, 120),
				line(6.3, 16.0, 6.0, 19.4),
				line(6.3, 16.0, 9.5, 15.6),
			},
		}

	case IconSleep:
		// Exact mathematically joined crescent moon: C1(12,12,r=7.2) & C2(16.8,7.2,r=5.8)
		// meet to 0.00px precision at (19.18, 12.49) and (11.51, 4.82).
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				arcStroke(12.0, 12.0, 7.2, 3.88, 262.24),
				arcStroke(16.8, 7.2, 5.8, 65.74, 138.52),
			},
		}

	case IconWake:
		// Sharp lightning bolt (wake from standby).
		return iconSpec{
			strokeWidth: 2.0,
			ops: polyline(
				[2]float64{13.4, 3.8},
				[2]float64{6.4, 12.8},
				[2]float64{12.0, 12.8},
				[2]float64{10.6, 20.2},
				[2]float64{17.6, 11.2},
				[2]float64{12.0, 11.2},
				[2]float64{13.4, 3.8},
			),
		}

	case IconLog:
		// Terminal / log window with prompt `>_`.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				roundRectStroke(3.8, 4.8, 16.4, 14.4, 2.2),
				line(7.4, 9.5, 10.2, 12.0),
				line(10.2, 12.0, 7.4, 14.5),
				line(12.6, 14.5, 16.4, 14.5),
			},
		}

	case IconFolder:
		// Classic crisp folder outline.
		return iconSpec{
			strokeWidth: 2.0,
			ops: polyline(
				[2]float64{4.0, 6.4},
				[2]float64{4.0, 17.8},
				[2]float64{20.0, 17.8},
				[2]float64{20.0, 8.6},
				[2]float64{11.6, 8.6},
				[2]float64{9.8, 6.4},
				[2]float64{4.0, 6.4},
			),
		}

	case IconFileCode:
		// Document with folded corner & two config lines.
		ops := polyline(
			[2]float64{5.6, 4.0},
			[2]float64{14.0, 4.0},
			[2]float64{18.4, 8.4},
			[2]float64{18.4, 20.0},
			[2]float64{5.6, 20.0},
			[2]float64{5.6, 4.0},
		)
		ops = append(ops,
			line(14.0, 4.0, 14.0, 8.4),
			line(14.0, 8.4, 18.4, 8.4),
			line(8.8, 12.6, 15.2, 12.6),
			line(8.8, 16.0, 13.2, 16.0),
		)
		return iconSpec{strokeWidth: 2.0, ops: ops}

	case IconCopy:
		// Two overlapping rounded sheets.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				roundRectStroke(9.0, 9.0, 10.8, 10.8, 2.0),
				line(15.0, 9.0, 15.0, 6.2),
				line(15.0, 6.2, 13.0, 4.2),
				line(13.0, 4.2, 6.2, 4.2),
				line(6.2, 4.2, 4.2, 6.2),
				line(4.2, 6.2, 4.2, 13.0),
				line(4.2, 13.0, 6.2, 15.0),
				line(6.2, 15.0, 9.0, 15.0),
			},
		}

	case IconCheck:
		// Crisp checkmark.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				line(5.2, 12.6, 9.8, 17.2),
				line(9.8, 17.2, 18.8, 7.2),
			},
		}

	case IconPower:
		// Clean power symbol: vertical top stem + 280-degree open ring.
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				line(12.0, 3.8, 12.0, 11.4),
				arcStroke(12.0, 12.6, 6.8, -50, 280),
			},
		}

	case IconChevronRight:
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				line(9.2, 6.2, 15.0, 12.0),
				line(15.0, 12.0, 9.2, 17.8),
			},
		}

	case IconChevronDown:
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				line(6.2, 9.2, 12.0, 15.0),
				line(12.0, 15.0, 17.8, 9.2),
			},
		}

	case IconPanel:
		// Control panel: rounded frame with top bar + clean content line (~25% ink coverage).
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				roundRectStroke(4.6, 4.8, 14.8, 14.4, 2.2),
				line(4.6, 9.6, 19.4, 9.6),
				line(8.2, 14.4, 15.8, 14.4),
			},
		}

	case IconStartAll:
		// Play triangle + vertical bar (start all).
		return iconSpec{
			strokeWidth: 2.0,
			ops: []iconOp{
				triangleStroke(5.2, 5.8, 14.2, 12.0, 5.2, 18.2),
				line(18.2, 5.8, 18.2, 18.2),
			},
		}

	default:
		return iconSpec{}
	}
}

// DrawIcon renders a 24x24 UI icon onto canvas c at top-left (x, y) with pixel extent size.
// All strokes in an icon share a single unified SDF field so overlapping joints never double-blend.
func DrawIcon(c *Canvas, id IconID, x, y, size float64, col RGBA) {
	if c == nil || id == IconNone || size <= 0 || col.A == 0 {
		return
	}
	spec := specFor(id)
	if len(spec.ops) == 0 {
		return
	}

	scale := size / 24.0
	sw := spec.strokeWidth
	if sw <= 0 {
		sw = 2.0
	}
	halfS := sw * 0.5
	// Ensure strokes stay crisp and readable at small logical sizes (e.g. 13-16px).
	pxScale := 1.0 / scale
	halfPx := 0.5 * pxScale

	x0 := maxInt(0, int(math.Floor(x)))
	y0 := maxInt(0, int(math.Floor(y)))
	x1 := minInt(c.Width, int(math.Ceil(x+size)))
	y1 := minInt(c.Height, int(math.Ceil(y+size)))

	for py := y0; py < y1; py++ {
		gy := (float64(py) + 0.5 - y) / scale
		for px := x0; px < x1; px++ {
			gx := (float64(px) + 0.5 - x) / scale
			minD := 1e9
			for _, op := range spec.ops {
				var d float64
				switch op.kind {
				case opLine:
					d = sdfSegment(gx, gy, op.a, op.b, op.c, op.d) - halfS
				case opCircleStroke:
					d = math.Abs(math.Hypot(gx-op.a, gy-op.b)-op.c) - halfS
				case opCircleFill:
					d = math.Hypot(gx-op.a, gy-op.b) - op.c
				case opArcStroke:
					d = sdfArc(gx, gy, op.a, op.b, op.c, op.d, op.e) - halfS
				case opRoundRectStroke:
					cx, cy := op.a+op.c*0.5, op.b+op.d*0.5
					d = math.Abs(sdfRoundedRect(gx, gy, cx, cy, op.c*0.5, op.d*0.5, op.e)) - halfS
				case opRoundRectFill:
					cx, cy := op.a+op.c*0.5, op.b+op.d*0.5
					d = sdfRoundedRect(gx, gy, cx, cy, op.c*0.5, op.d*0.5, op.e)
				case opTriangleStroke:
					d1 := sdfSegment(gx, gy, op.a, op.b, op.c, op.d)
					d2 := sdfSegment(gx, gy, op.c, op.d, op.e, op.f)
					d3 := sdfSegment(gx, gy, op.e, op.f, op.a, op.b)
					d = math.Min(d1, math.Min(d2, d3)) - halfS
				case opTriangleFill:
					d = sdfTriangle(gx, gy, op.a, op.b, op.c, op.d, op.e, op.f)
				case opCrescentStroke:
					dOuter := math.Hypot(gx-op.a, gy-op.b) - op.c
					dInner := math.Hypot(gx-op.d, gy-op.e) - op.f
					// Boolean difference: max(dOuter, -dInner)
					dCrescent := math.Max(dOuter, -dInner)
					d = math.Abs(dCrescent) - halfS
				}
				if d < minD {
					minD = d
				}
			}
			if minD < halfPx {
				cov := clampFloat((halfPx-minD)/(2.0*halfPx), 0, 1)
				c.BlendPixel(px, py, col, cov)
			}
		}
	}
}
