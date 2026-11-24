package trayui

// Rect mirrors Win32 RECT coordinates (can be negative on secondary monitors).
type Rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

// Width returns the horizontal span of r.
func (r Rect) Width() int32 { return r.Right - r.Left }

// Height returns the vertical span of r.
func (r Rect) Height() int32 { return r.Bottom - r.Top }

// PlacePopup computes the top-left window coordinates (including shadowPad offset) for
// a layered tray popup (Control Popover or Quick Menu), anchored cleanly against the
// tray icon rect or cursor point and clamped strictly within the monitor work area.
//
// It handles bottom, top, left, and right taskbars as well as negative multi-monitor coordinates.
func PlacePopup(
	anchor Rect,
	hasAnchor bool,
	cursorX, cursorY int32,
	cardW, cardH, shadowPad int32,
	workArea Rect,
	isQuickMenu bool,
) (winX, winY int32) {
	const margin int32 = 4
	const gap int32 = 6

	minX := workArea.Left + margin
	maxX := workArea.Right - cardW - margin
	if maxX < minX {
		maxX = minX
	}
	minY := workArea.Top + margin
	maxY := workArea.Bottom - cardH - margin
	if maxY < minY {
		maxY = minY
	}

	refX, refY := cursorX, cursorY
	if hasAnchor && anchor.Right > anchor.Left && anchor.Bottom > anchor.Top {
		refX = anchor.Left + (anchor.Right-anchor.Left)/2
		refY = anchor.Top + (anchor.Bottom-anchor.Top)/2
	}

	var cardX, cardY int32

	switch {
	case refY < workArea.Top:
		// Top taskbar: open below workArea.Top.
		if isQuickMenu {
			cardX = cursorX - cardW + 8
		} else {
			cardX = refX - cardW/2
		}
		cardY = workArea.Top + gap

	case refX < workArea.Left:
		// Left taskbar: open to the right of workArea.Left.
		cardX = workArea.Left + gap
		if isQuickMenu {
			cardY = cursorY - cardH + 8
		} else {
			cardY = refY - cardH/2
		}

	case refX >= workArea.Right:
		// Right taskbar: open to the left of workArea.Right.
		cardX = workArea.Right - cardW - gap
		if isQuickMenu {
			cardY = cursorY - cardH + 8
		} else {
			cardY = refY - cardH/2
		}

	default:
		// Bottom taskbar (or inside work area, e.g., tray overflow flyout):
		if isQuickMenu {
			cardX = cursorX - cardW + 8
			cardY = cursorY - cardH - gap
			if cardY < minY {
				// If cursor is near top of screen, flip below cursor.
				cardY = cursorY + gap
			}
		} else {
			cardX = refX - cardW/2
			if hasAnchor && anchor.Top > workArea.Top {
				anchorTop := anchor.Top
				if anchorTop > workArea.Bottom {
					anchorTop = workArea.Bottom
				}
				cardY = anchorTop - cardH - gap
			} else {
				cardY = workArea.Bottom - cardH - gap
			}
			if cardY < minY {
				cardY = workArea.Top + gap
			}
		}
	}

	cardX = clampInt32(cardX, minX, maxX)
	cardY = clampInt32(cardY, minY, maxY)
	return cardX - shadowPad, cardY - shadowPad
}

func clampInt32(v, lo, hi int32) int32 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
