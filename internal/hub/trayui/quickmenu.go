package trayui

import (
	"fmt"
	"math"
	"strings"
)

// QuickMenuState holds interactive hover and keyboard focus state for the Right-Click Quick Menu.
type QuickMenuState struct {
	HoverIndex int // -1 = none
	FocusIndex int // -1 = none
}

// QuickMenuItem describes one actionable entry in the Right-Click Quick Menu.
type QuickMenuItem struct {
	ID        string
	Label     string
	Shortcut  string
	Icon      IconID
	Action    Action
	SepBefore bool
	Danger    bool
}

type quickMenuRowLayout struct {
	item    QuickMenuItem
	rowRect Rect
	sepY    int
}

// QuickMenuLayout holds physical pixel geometry for the Right-Click Quick Menu.
type QuickMenuLayout struct {
	Scale      float64
	WinW       int
	WinH       int
	ShadowPad  int
	CardRect   Rect
	HeaderRect Rect
	HeaderText string
	Rows       []quickMenuRowLayout
}

// InCard reports whether (x, y) lies inside the visible menu card.
func (l *QuickMenuLayout) InCard(x, y int) bool {
	return ptInRect(x, y, l.CardRect)
}

// HitTest returns the index of the hovered Quick Menu item, or -1.
func (l *QuickMenuLayout) HitTest(x, y int) int {
	if !l.InCard(x, y) {
		return -1
	}
	for i := range l.Rows {
		if ptInRect(x, y, l.Rows[i].rowRect) {
			return i
		}
	}
	return -1
}

// RowAction returns the Action for row index idx, or ActionNone if out of range.
func (l *QuickMenuLayout) RowAction(idx int) Action {
	if idx < 0 || idx >= len(l.Rows) {
		return Action{Kind: ActionNone}
	}
	return l.Rows[idx].item.Action
}

// QuickMenuItems returns the canonical ordered items for the Right-Click Quick Menu.
func QuickMenuItems() []QuickMenuItem {
	return []QuickMenuItem{
		{ID: "qm:panel", Label: "Open Control Panel", Icon: IconPanel, Action: Action{Kind: ActionOpenPopover}},
		{ID: "qm:dashboard", Label: "Open Web Dashboard", Icon: IconDashboard, Action: Action{Kind: ActionOpenDashboard}},
		{ID: "qm:startall", Label: "Start All Services", Icon: IconStartAll, Action: Action{Kind: ActionStartAll}, SepBefore: true},
		{ID: "qm:restartall", Label: "Restart All Services", Icon: IconRestart, Action: Action{Kind: ActionRestartAll}},
		{ID: "qm:reload", Label: "Reload Config", Shortcut: "Ctrl+R", Icon: IconReload, Action: Action{Kind: ActionReloadConfig}},
		{ID: "qm:logs", Label: "Open Logs Folder", Icon: IconFolder, Action: Action{Kind: ActionOpenLogs}, SepBefore: true},
		{ID: "qm:config", Label: "Open Config File", Icon: IconFileCode, Action: Action{Kind: ActionOpenConfig}},
		{ID: "qm:copy", Label: "Copy Status", Shortcut: "Ctrl+C", Icon: IconCopy, Action: Action{Kind: ActionCopyStatus}},
		{ID: "qm:exit", Label: "Exit McpHub", Icon: IconPower, Action: Action{Kind: ActionExit}, SepBefore: true, Danger: true},
	}
}

// LayoutQuickMenu computes physical pixel coordinates for the Right-Click Quick Menu at dpi.
func LayoutQuickMenu(snap Snapshot, dpi int) *QuickMenuLayout {
	if dpi <= 0 {
		dpi = 96
	}
	s := float64(dpi) / 96.0
	sp := int(math.Ceil(float64(ShadowPad) * s))
	minCardW := pxRound(float64(QuickMenuLogicalWidth) * s)
	maxCardW := pxRound(296 * s)

	padX := pxRound(14 * s)
	iconPadX := pxRound(38 * s)
	insetX := pxRound(float64(HoverInsetX) * s)
	padY := pxRound(6 * s)
	headerH := pxRound(32 * s)
	rowH := pxRound(31 * s)
	sepH := pxRound(9 * s)
	shortcutGap := pxRound(20 * s)

	headerSpec := FontSpec{Family: FontMono, SizePx: 10.6 * s, Weight: 500}
	labelSpec := FontSpec{Family: FontSans, SizePx: 12.8 * s, Weight: 400}
	scSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}

	ver := strings.TrimSpace(snap.Version)
	if ver == "" {
		ver = "0.3.0"
	}
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	headerText := fmt.Sprintf("McpHub %s · %s", ver, formatHeaderSummary(snap))
	headerW := padX + pxRound(12*s) + MeasureText(headerText, headerSpec).W + padX

	items := QuickMenuItems()
	maxItemW := 0
	for _, it := range items {
		w := iconPadX + MeasureText(it.Label, labelSpec).W + padX
		if it.Shortcut != "" {
			w += shortcutGap + MeasureText(it.Shortcut, scSpec).W
		}
		if w > maxItemW {
			maxItemW = w
		}
	}

	cardW := minCardW
	if headerW > cardW {
		cardW = headerW
	}
	if maxItemW > cardW {
		cardW = maxItemW
	}
	if cardW > maxCardW {
		cardW = maxCardW
	}

	y := sp + headerH + 1 + padY
	rows := make([]quickMenuRowLayout, 0, len(items))
	for _, it := range items {
		sepY := -1
		if it.SepBefore {
			sepY = y + sepH/2
			y += sepH
		}
		rr := Rect{
			Left:   int32(sp + insetX),
			Top:    int32(y),
			Right:  int32(sp + cardW - insetX),
			Bottom: int32(y + rowH),
		}
		rows = append(rows, quickMenuRowLayout{
			item:    it,
			rowRect: rr,
			sepY:    sepY,
		})
		y += rowH
	}
	cardH := (y - sp) + padY
	winW := cardW + sp*2
	winH := cardH + sp*2

	return &QuickMenuLayout{
		Scale:     s,
		WinW:      winW,
		WinH:      winH,
		ShadowPad: sp,
		CardRect: Rect{
			Left:   int32(sp),
			Top:    int32(sp),
			Right:  int32(sp + cardW),
			Bottom: int32(sp + cardH),
		},
		HeaderRect: Rect{
			Left:   int32(sp),
			Top:    int32(sp),
			Right:  int32(sp + cardW),
			Bottom: int32(sp + headerH),
		},
		HeaderText: headerText,
		Rows:       rows,
	}
}

// RenderQuickMenu renders the Right-Click Quick Menu into a premultiplied BGRA Canvas.
func RenderQuickMenu(snap Snapshot, state QuickMenuState, theme Theme, dpi int) (*Canvas, *QuickMenuLayout) {
	l := LayoutQuickMenu(snap, dpi)
	s := l.Scale
	c := NewCanvas(l.WinW, l.WinH)

	cx := float64(l.CardRect.Left)
	cy := float64(l.CardRect.Top)
	cw := float64(l.CardRect.Width())
	ch := float64(l.CardRect.Height())
	corner := CardCorner * s

	// 1. Soft 3-pass elevation shadow.
	c.DrawCardShadow(cx, cy, cw, ch, corner, theme.IsDark, s)

	// 2. Opaque card fill + crisp 1px (+0.5px inset) border hairline.
	c.FillRoundedRect(cx, cy, cw, ch, corner, theme.Canvas)
	c.StrokeRoundedRect(cx+0.5, cy+0.5, cw-1, ch-1, math.Max(1, corner-0.5), 1.0, theme.Border)

	var texts []TextDraw
	padX := pxRound(14 * s)
	headerSpec := FontSpec{Family: FontMono, SizePx: 10.6 * s, Weight: 500}
	labelSpec := FontSpec{Family: FontSans, SizePx: 12.8 * s, Weight: 400}
	scSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}

	// 3. Quiet mono header + semantic dot.
	{
		hMidY := int(l.HeaderRect.Top) + int(l.HeaderRect.Height())/2
		_, dotCol, _ := headerSummaryPalette(snap.State, theme)
		dotCX := float64(l.CardRect.Left+int32(padX)) + 3.2*s
		dotCY := float64(hMidY)
		if snap.State == "sleeping" {
			c.StrokeCircle(dotCX, dotCY, 2.8*s, math.Max(1.2, 1.4*s), dotCol)
		} else {
			c.FillCircle(dotCX, dotCY, 3.1*s, dotCol)
		}

		hTextX := int(l.CardRect.Left) + padX + pxRound(12*s)
		hSz := MeasureText(l.HeaderText, headerSpec)
		texts = append(texts, TextDraw{
			Text:  l.HeaderText,
			X:     hTextX,
			Y:     hMidY - hSz.H/2,
			MaxW:  int(l.CardRect.Right) - padX - hTextX,
			Color: theme.Muted,
			Spec:  headerSpec,
		})
	}

	// Header divider.
	c.FillRect(int(l.CardRect.Left)+1, int(l.HeaderRect.Bottom), int(l.CardRect.Width())-2, 1, theme.Divider)

	// 4. Menu rows.
	rightEdge := int(l.CardRect.Right) - padX
	for i, row := range l.Rows {
		if row.sepY >= 0 {
			c.FillRect(int(l.CardRect.Left)+padX, row.sepY, int(l.CardRect.Width())-padX*2, 1, theme.Divider)
		}
		hovered := state.HoverIndex == i
		focused := state.FocusIndex == i

		if hovered || focused {
			bg := theme.RowHover
			if row.item.Danger {
				bg = theme.ErrorSoft
			}
			c.FillRoundedRect(
				float64(row.rowRect.Left),
				float64(row.rowRect.Top)+1,
				float64(row.rowRect.Width()),
				float64(row.rowRect.Height())-2,
				HoverPillRadius*s,
				bg,
			)
		}
		if focused {
			c.StrokeRoundedRect(
				float64(row.rowRect.Left)+1,
				float64(row.rowRect.Top)+2,
				float64(row.rowRect.Width())-2,
				float64(row.rowRect.Height())-4,
				HoverPillRadius*s-0.5,
				math.Max(1.5, 1.75*s),
				theme.Focus,
			)
		}

		icSz := 14.5 * s
		icX := float64(l.CardRect.Left) + 14.0*s
		icY := float64(row.rowRect.Top) + (float64(row.rowRect.Height())-icSz)*0.5
		icCol := theme.Icon
		lblCol := theme.Ink
		if row.item.Danger {
			if hovered || focused {
				icCol = theme.Error
				lblCol = theme.ErrorText
			} else {
				icCol = theme.Error.WithAlpha(205)
				lblCol = theme.ErrorText
			}
		} else if hovered || focused {
			icCol = theme.Accent
		}
		DrawIcon(c, row.item.Icon, icX, icY, icSz, icCol)

		lblSz := MeasureText(row.item.Label, labelSpec)
		texts = append(texts, TextDraw{
			Text:  row.item.Label,
			X:     int(l.CardRect.Left) + pxRound(38*s),
			Y:     int(row.rowRect.Top) + (int(row.rowRect.Height())-lblSz.H)/2,
			Color: lblCol,
			Spec:  labelSpec,
		})

		if row.item.Shortcut != "" {
			scSz := MeasureText(row.item.Shortcut, scSpec)
			scCol := theme.Muted
			if row.item.Danger && (hovered || focused) {
				scCol = theme.ErrorText
			}
			texts = append(texts, TextDraw{
				Text:   row.item.Shortcut,
				X:      rightEdge,
				Y:      int(row.rowRect.Top) + (int(row.rowRect.Height())-scSz.H)/2,
				Color:  scCol,
				Spec:   scSpec,
				AlignR: true,
			})
		}
	}

	// 5. Flush all ClearType text onto opaque card interior.
	DrawTexts(c, texts)
	return c, l
}
