package trayui

import (
	"fmt"
	"math"
	"strings"
)

// PopoverState holds interactive UI state for the Left-Click Control Popover.
type PopoverState struct {
	ExpandedService  string // Name of expanded service detail drawer ("" = none)
	HoveredService   string // Service row currently under mouse cursor
	HoveredTargetID  string // Exact hit-target ID under mouse cursor
	FocusIndex       int    // Keyboard focus index into PopoverLayout.FocusIDs (-1 = none)
	ScrollOffset     int    // Physical pixel scroll offset within the services viewport
	CopiedStatus     bool   // True for ~1.5s after Copy Status is triggered
	CopiedServiceURL string // Service name showing "Copied" pill for ~1.5s after Copy URL
}

// TargetKind classifies an interactive region inside the Popover.
type TargetKind int

const (
	TargetHeaderButton TargetKind = iota
	TargetServiceRow
	TargetServiceInlineButton
	TargetDrawerButton
	TargetFooterRow
)

// HitTarget represents one clickable/hoverable/focusable rectangle in window pixel coordinates.
type HitTarget struct {
	ID          string
	Kind        TargetKind
	Rect        Rect
	Action      Action
	ServiceName string
	Icon        IconID
	Label       string
	Danger      bool
	Tooltip     string
}

type drawerLayout struct {
	boxRect   Rect
	metaY     int
	metaText  string
	errRect   Rect
	errLines  []string
	buttons   []HitTarget
	hasError  bool
	drawerEnd int
}

type serviceRowLayout struct {
	index        int
	svc          ServiceItem
	rowRect      Rect
	pillRect     Rect
	inlineBtns   []HitTarget
	expanded     bool
	drawer       drawerLayout
	inlineActive bool
}

type footerItemSpec struct {
	id        string
	label     string
	shortcut  string
	icon      IconID
	action    Action
	sepBefore bool
	danger    bool
}

type footerRowLayout struct {
	spec    footerItemSpec
	rowRect Rect
	sepY    int
}

// PopoverLayout holds the computed physical pixel geometry of a Popover frame.
type PopoverLayout struct {
	Scale      float64
	WinW       int
	WinH       int
	ShadowPad  int
	CardRect   Rect
	HeaderRect Rect
	ListRect   Rect
	FooterRect Rect

	MaxScroll     int
	TotalListH    int
	HeaderDashBtn HitTarget
	Services      []serviceRowLayout
	FooterRows    []footerRowLayout
	Targets       []HitTarget
	FocusIDs      []string
}

// InCard reports whether window-relative point (x, y) lies inside the visible card.
func (l *PopoverLayout) InCard(x, y int) bool {
	return x >= int(l.CardRect.Left) && x < int(l.CardRect.Right) &&
		y >= int(l.CardRect.Top) && y < int(l.CardRect.Bottom)
}

// HitTest resolves the highest-priority interactive target and hovered service at (x, y).
func (l *PopoverLayout) HitTest(x, y int) (target *HitTarget, hoveredService string) {
	if !l.InCard(x, y) {
		return nil, ""
	}
	// First check if point is inside the visible services viewport.
	inList := x >= int(l.ListRect.Left) && x < int(l.ListRect.Right) &&
		y >= int(l.ListRect.Top) && y < int(l.ListRect.Bottom)

	if inList {
		for i := range l.Services {
			sr := &l.Services[i]
			if ptInRect(x, y, sr.rowRect) {
				hoveredService = sr.svc.Name
				// Check inline buttons on this row first so right-side clicks execute cleanly.
				for j := range sr.inlineBtns {
					if ptInRect(x, y, sr.inlineBtns[j].Rect) {
						return &sr.inlineBtns[j], hoveredService
					}
				}
				return &HitTarget{
					ID:          "svc:" + sr.svc.Name,
					Kind:        TargetServiceRow,
					Rect:        sr.rowRect,
					Action:      Action{Kind: ActionToggleServiceDrawer, Service: sr.svc.Name},
					ServiceName: sr.svc.Name,
				}, hoveredService
			}
			if sr.expanded && ptInRect(x, y, sr.drawer.boxRect) {
				for j := range sr.drawer.buttons {
					if ptInRect(x, y, sr.drawer.buttons[j].Rect) {
						return &sr.drawer.buttons[j], ""
					}
				}
				return nil, ""
			}
		}
	}

	if ptInRect(x, y, l.HeaderDashBtn.Rect) {
		t := l.HeaderDashBtn
		return &t, ""
	}

	for i := range l.FooterRows {
		fr := &l.FooterRows[i]
		if ptInRect(x, y, fr.rowRect) {
			return &HitTarget{
				ID:     fr.spec.id,
				Kind:   TargetFooterRow,
				Rect:   fr.rowRect,
				Action: fr.spec.action,
				Icon:   fr.spec.icon,
				Label:  fr.spec.label,
				Danger: fr.spec.danger,
			}, ""
		}
	}

	return nil, ""
}

// FocusedTarget returns the HitTarget corresponding to FocusIndex, or nil.
func (l *PopoverLayout) FocusedTarget(focusIndex int) *HitTarget {
	if focusIndex < 0 || focusIndex >= len(l.FocusIDs) {
		return nil
	}
	id := l.FocusIDs[focusIndex]
	for i := range l.Targets {
		if l.Targets[i].ID == id {
			return &l.Targets[i]
		}
	}
	return nil
}

// ServiceRowRect returns the bounding rect of service row idx, if in range.
func (l *PopoverLayout) ServiceRowRect(idx int) (Rect, bool) {
	if idx < 0 || idx >= len(l.Services) {
		return Rect{}, false
	}
	return l.Services[idx].rowRect, true
}

// LayoutPopover computes all physical pixel coordinates for the Control Popover at dpi.
// maxCardHeightPx caps the card height (<= 0 uses a generous default of 600 logical px).
func LayoutPopover(snap Snapshot, state PopoverState, dpi int, maxCardHeightPx int) *PopoverLayout {
	if dpi <= 0 {
		dpi = 96
	}
	s := float64(dpi) / 96.0
	sp := int(math.Ceil(float64(ShadowPad) * s))
	cardW := pxRound(float64(PopoverLogicalWidth) * s)

	headerH := pxRound(52 * s)
	listPadY := pxRound(6 * s)
	rowH := pxRound(48 * s)
	footerPadY := pxRound(6 * s)
	footerRowH := pxRound(31 * s)
	sepH := pxRound(9 * s)
	padX := pxRound(14 * s)
	insetX := pxRound(float64(HoverInsetX) * s)

	footerSpecs := popoverFooterSpecs()
	footerH := footerPadY * 2
	for _, fs := range footerSpecs {
		if fs.sepBefore {
			footerH += sepH
		}
		footerH += footerRowH
	}

	// Determine focused service name (if keyboard focus is on a service row).
	focusedSvc := ""
	if state.FocusIndex >= 0 && state.FocusIndex < len(snap.Services) {
		focusedSvc = snap.Services[state.FocusIndex].Name
	}

	// Measure total services list height (including expanded drawer if present).
	emptyListH := pxRound(44 * s)
	errSpec := FontSpec{Family: FontSans, SizePx: 11.2 * s, Weight: 400}
	errLH := MeasureText("Ag", errSpec).H
	btnSpec := FontSpec{Family: FontSans, SizePx: 11.5 * s, Weight: 500}
	metaSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}
	metaLH := MeasureText("PID", metaSpec).H

	drawerPadX := pxRound(10 * s)
	drawerPadY := pxRound(9 * s)
	drawerInsetX := pxRound(12 * s)
	drawerGapTop := pxRound(2 * s)
	drawerGapBot := pxRound(6 * s)
	drawerBtnH := pxRound(24 * s)
	drawerW := cardW - drawerInsetX*2
	drawerInnerW := drawerW - drawerPadX*2

	// Precompute each service's height and drawer lines.
	type svcPre struct {
		expanded bool
		drawerH  int
		metaText string
		errLines []string
		errBoxH  int
		btns     []drawerActionSpec
	}
	pre := make([]svcPre, len(snap.Services))
	totalListH := listPadY * 2
	if len(snap.Services) == 0 {
		totalListH += emptyListH
	} else {
		for i, svc := range snap.Services {
			totalListH += rowH
			if state.ExpandedService != "" && state.ExpandedService == svc.Name {
				p := svcPre{
					expanded: true,
					metaText: formatServiceMetrics(svc),
					btns:     serviceDrawerActions(svc),
				}
				dh := drawerPadY + metaLH
				if strings.TrimSpace(svc.LastError) != "" {
					errPadX := pxRound(8 * s)
					errPadY := pxRound(6 * s)
					p.errLines = WrapText(svc.LastError, errSpec, drawerInnerW-errPadX*2, 2)
					if len(p.errLines) > 0 {
						p.errBoxH = errPadY*2 + len(p.errLines)*errLH + maxInt(0, len(p.errLines)-1)*pxRound(2*s)
						dh += pxRound(7*s) + p.errBoxH
					}
				}
				dh += pxRound(8*s) + drawerBtnH + drawerPadY
				p.drawerH = dh
				pre[i] = p
				totalListH += drawerGapTop + dh + drawerGapBot
			}
		}
	}

	if maxCardHeightPx <= 0 {
		maxCardHeightPx = pxRound(700 * s)
	}
	maxListH := maxCardHeightPx - headerH - footerH - 2
	if maxListH < pxRound(140*s) {
		maxListH = pxRound(140 * s)
	}
	listH := totalListH
	if listH > maxListH {
		listH = maxListH
	}
	maxScroll := totalListH - listH
	if maxScroll < 0 {
		maxScroll = 0
	}
	scrollY := state.ScrollOffset
	if scrollY < 0 {
		scrollY = 0
	}
	if scrollY > maxScroll {
		scrollY = maxScroll
	}

	cardH := headerH + 1 + listH + 1 + footerH
	winW := cardW + sp*2
	winH := cardH + sp*2

	cardRect := Rect{
		Left:   int32(sp),
		Top:    int32(sp),
		Right:  int32(sp + cardW),
		Bottom: int32(sp + cardH),
	}
	headerRect := Rect{
		Left:   cardRect.Left,
		Top:    cardRect.Top,
		Right:  cardRect.Right,
		Bottom: cardRect.Top + int32(headerH),
	}
	listRect := Rect{
		Left:   cardRect.Left,
		Top:    headerRect.Bottom + 1,
		Right:  cardRect.Right,
		Bottom: headerRect.Bottom + 1 + int32(listH),
	}
	footerRect := Rect{
		Left:   cardRect.Left,
		Top:    listRect.Bottom + 1,
		Right:  cardRect.Right,
		Bottom: cardRect.Bottom,
	}

	// Header Dashboard icon button on right edge.
	dashSize := pxRound(26 * s)
	dashX := int(headerRect.Right) - padX - dashSize
	dashY := int(headerRect.Top) + (headerH-dashSize)/2
	dashBtn := HitTarget{
		ID:      "hdr:dashboard",
		Kind:    TargetHeaderButton,
		Rect:    Rect{Left: int32(dashX), Top: int32(dashY), Right: int32(dashX + dashSize), Bottom: int32(dashY + dashSize)},
		Action:  Action{Kind: ActionOpenDashboard},
		Icon:    IconDashboard,
		Tooltip: "Open Web Dashboard",
	}

	l := &PopoverLayout{
		Scale:         s,
		WinW:          winW,
		WinH:          winH,
		ShadowPad:     sp,
		CardRect:      cardRect,
		HeaderRect:    headerRect,
		ListRect:      listRect,
		FooterRect:    footerRect,
		MaxScroll:     maxScroll,
		TotalListH:    totalListH,
		HeaderDashBtn: dashBtn,
	}

	// Layout service rows + inline icon buttons + optional expanded drawer.
	inlineBtnSize := pxRound(24 * s)
	inlineBtnGap := pxRound(4 * s)
	scrollBarReserve := 0
	if maxScroll > 0 {
		scrollBarReserve = pxRound(6 * s)
	}

	curY := int(listRect.Top) + listPadY - scrollY
	for i, svc := range snap.Services {
		rr := Rect{
			Left:   cardRect.Left + int32(insetX),
			Top:    int32(curY),
			Right:  cardRect.Right - int32(insetX) - int32(scrollBarReserve),
			Bottom: int32(curY + rowH),
		}
		inlineActive := state.HoveredService == svc.Name || focusedSvc == svc.Name

		// Position context-aware inline icon buttons from right edge inward.
		actions := serviceInlineActions(svc)
		inlineBtns := make([]HitTarget, len(actions))
		btnRight := int(rr.Right) - pxRound(8*s)
		btnY := curY + (rowH-inlineBtnSize)/2
		for idx := len(actions) - 1; idx >= 0; idx-- {
			a := actions[idx]
			bx := btnRight - inlineBtnSize
			inlineBtns[idx] = HitTarget{
				ID:          fmt.Sprintf("svcbtn:%s:%d", svc.Name, a.action.Kind),
				Kind:        TargetServiceInlineButton,
				Rect:        Rect{Left: int32(bx), Top: int32(btnY), Right: int32(bx + inlineBtnSize), Bottom: int32(btnY + inlineBtnSize)},
				Action:      a.action,
				ServiceName: svc.Name,
				Icon:        a.icon,
				Danger:      a.danger,
				Tooltip:     a.tooltip,
			}
			btnRight = bx - inlineBtnGap
		}

		sr := serviceRowLayout{
			index:        i,
			svc:          svc,
			rowRect:      rr,
			inlineBtns:   inlineBtns,
			expanded:     pre[i].expanded,
			inlineActive: inlineActive,
		}

		rowTarget := HitTarget{
			ID:          "svc:" + svc.Name,
			Kind:        TargetServiceRow,
			Rect:        rr,
			Action:      Action{Kind: ActionToggleServiceDrawer, Service: svc.Name},
			ServiceName: svc.Name,
		}
		l.Targets = append(l.Targets, rowTarget)
		l.FocusIDs = append(l.FocusIDs, rowTarget.ID)
		for _, ib := range inlineBtns {
			l.Targets = append(l.Targets, ib)
		}

		curY += rowH

		if pre[i].expanded {
			p := pre[i]
			dy := curY + drawerGapTop
			box := Rect{
				Left:   cardRect.Left + int32(drawerInsetX),
				Top:    int32(dy),
				Right:  cardRect.Right - int32(drawerInsetX) - int32(scrollBarReserve),
				Bottom: int32(dy + p.drawerH),
			}
			dl := drawerLayout{
				boxRect:   box,
				metaY:     dy + drawerPadY,
				metaText:  p.metaText,
				drawerEnd: dy + p.drawerH,
			}
			yCursor := dy + drawerPadY + metaLH
			if len(p.errLines) > 0 {
				yCursor += pxRound(7 * s)
				dl.hasError = true
				dl.errLines = p.errLines
				dl.errRect = Rect{
					Left:   box.Left + int32(drawerPadX),
					Top:    int32(yCursor),
					Right:  box.Right - int32(drawerPadX),
					Bottom: int32(yCursor + p.errBoxH),
				}
				yCursor += p.errBoxH
			}
			yCursor += pxRound(8 * s)

			// Layout labeled pill buttons inside the drawer.
			bx := int(box.Left) + drawerPadX
			btnGap := pxRound(6 * s)
			btnPadX := pxRound(8 * s)
			btnIconSz := pxRound(12 * s)
			btnIconGap := pxRound(5 * s)
			maxRight := int(box.Right) - drawerPadX
			for _, da := range p.btns {
				lbl := da.label
				ic := da.icon
				if da.action.Kind == ActionServiceCopyURL && state.CopiedServiceURL == svc.Name {
					lbl = "Copied"
					ic = IconCheck
				}
				tw := MeasureText(lbl, btnSpec).W
				bw := btnPadX + btnIconSz + btnIconGap + tw + btnPadX
				if bx+bw > maxRight {
					break // Keep action bar cleanly on one tidy row without overflow.
				}
				bt := HitTarget{
					ID:          fmt.Sprintf("drawer:%s:%d", svc.Name, da.action.Kind),
					Kind:        TargetDrawerButton,
					Rect:        Rect{Left: int32(bx), Top: int32(yCursor), Right: int32(bx + bw), Bottom: int32(yCursor + drawerBtnH)},
					Action:      da.action,
					ServiceName: svc.Name,
					Icon:        ic,
					Label:       lbl,
					Danger:      da.danger,
				}
				dl.buttons = append(dl.buttons, bt)
				l.Targets = append(l.Targets, bt)
				bx += bw + btnGap
			}

			sr.drawer = dl
			curY = dy + p.drawerH + drawerGapBot
		}

		l.Services = append(l.Services, sr)
	}

	// Layout footer menu rows.
	fy := int(footerRect.Top) + footerPadY
	for _, fs := range footerSpecs {
		sepY := -1
		if fs.sepBefore {
			sepY = fy + sepH/2
			fy += sepH
		}
		rr := Rect{
			Left:   cardRect.Left + int32(insetX),
			Top:    int32(fy),
			Right:  cardRect.Right - int32(insetX),
			Bottom: int32(fy + footerRowH),
		}
		l.FooterRows = append(l.FooterRows, footerRowLayout{
			spec:    fs,
			rowRect: rr,
			sepY:    sepY,
		})
		ft := HitTarget{
			ID:     fs.id,
			Kind:   TargetFooterRow,
			Rect:   rr,
			Action: fs.action,
			Icon:   fs.icon,
			Label:  fs.label,
			Danger: fs.danger,
		}
		l.Targets = append(l.Targets, ft)
		l.FocusIDs = append(l.FocusIDs, ft.ID)
		fy += footerRowH
	}

	l.Targets = append(l.Targets, dashBtn)
	return l
}

// RenderPopover renders the Left-Click Control Popover into a premultiplied BGRA Canvas.
func RenderPopover(snap Snapshot, state PopoverState, theme Theme, dpi int, maxCardHeightPx int) (*Canvas, *PopoverLayout) {
	l := LayoutPopover(snap, state, dpi, maxCardHeightPx)
	s := l.Scale
	c := NewCanvas(l.WinW, l.WinH)

	cx := float64(l.CardRect.Left)
	cy := float64(l.CardRect.Top)
	cw := float64(l.CardRect.Width())
	ch := float64(l.CardRect.Height())
	corner := CardCorner * s

	// 1. Soft 3-pass elevation drop shadow.
	c.DrawCardShadow(cx, cy, cw, ch, corner, theme.IsDark, s)

	// 2. Opaque card surface + crisp 1px (+0.5px inset) border hairline.
	c.FillRoundedRect(cx, cy, cw, ch, corner, theme.Canvas)
	c.StrokeRoundedRect(cx+0.5, cy+0.5, cw-1, ch-1, math.Max(1, corner-0.5), 1.0, theme.Border)

	var texts []TextDraw
	padX := pxRound(14 * s)

	// Typography specs.
	titleSpec := FontSpec{Family: FontSans, SizePx: 13.8 * s, Weight: 600}
	verSpec := FontSpec{Family: FontMono, SizePx: 10.5 * s, Weight: 500}
	summarySpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 500}
	svcNameSpec := FontSpec{Family: FontSans, SizePx: 13.0 * s, Weight: 600}
	svcSubSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}
	badgeSpec := FontSpec{Family: FontMono, SizePx: 10.2 * s, Weight: 500}
	drawerMetaSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}
	drawerErrSpec := FontSpec{Family: FontSans, SizePx: 11.2 * s, Weight: 400}
	drawerBtnSpec := FontSpec{Family: FontSans, SizePx: 11.5 * s, Weight: 500}
	footerSpec := FontSpec{Family: FontSans, SizePx: 12.8 * s, Weight: 400}
	shortcutSpec := FontSpec{Family: FontMono, SizePx: 10.8 * s, Weight: 400}

	// Determine focused target ID.
	focusID := ""
	if state.FocusIndex >= 0 && state.FocusIndex < len(l.FocusIDs) {
		focusID = l.FocusIDs[state.FocusIndex]
	}

	// 3. Header: Tri-Node Hub emblem + McpHub + v0.3.0 pill + live summary badge + Dashboard button.
	{
		hx := int(l.HeaderRect.Left) + padX
		hMidY := int(l.HeaderRect.Top) + int(l.HeaderRect.Height())/2

		// Mini rounded emblem tile.
		tileSz := pxRound(24 * s)
		tileY := hMidY - tileSz/2
		c.FillRoundedRect(float64(hx), float64(tileY), float64(tileSz), float64(tileSz), MicroRadius*s, theme.AccentSoft)
		icSz := 16.0 * s
		DrawIcon(c, IconHub, float64(hx)+(float64(tileSz)-icSz)*0.5, float64(tileY)+(float64(tileSz)-icSz)*0.5, icSz, theme.Accent)
		hx += tileSz + pxRound(8*s)

		title := "McpHub"
		titleSz := MeasureText(title, titleSpec)
		texts = append(texts, TextDraw{
			Text:  title,
			X:     hx,
			Y:     hMidY - titleSz.H/2,
			Color: theme.Ink,
			Spec:  titleSpec,
		})
		hx += titleSz.W + pxRound(6*s)

		ver := strings.TrimSpace(snap.Version)
		if ver == "" {
			ver = "0.3.0"
		}
		if !strings.HasPrefix(ver, "v") {
			ver = "v" + ver
		}
		verSz := MeasureText(ver, verSpec)
		verPadX := pxRound(6 * s)
		verH := pxRound(19 * s)
		verW := verSz.W + verPadX*2
		verY := hMidY - verH/2
		c.FillRoundedRect(float64(hx), float64(verY), float64(verW), float64(verH), MicroRadius*s, theme.Surface)
		c.StrokeRoundedRect(float64(hx)+0.5, float64(verY)+0.5, float64(verW)-1, float64(verH)-1, math.Max(1, MicroRadius*s-0.5), 1.0, theme.Border)
		texts = append(texts, TextDraw{
			Text:  ver,
			X:     hx + verPadX,
			Y:     verY + (verH-verSz.H)/2,
			Color: theme.Muted,
			Spec:  verSpec,
		})

		// Right-side Dashboard quick icon button.
		db := l.HeaderDashBtn
		dbHovered := state.HoveredTargetID == db.ID
		if dbHovered {
			c.FillRoundedRect(float64(db.Rect.Left), float64(db.Rect.Top), float64(db.Rect.Width()), float64(db.Rect.Height()), MicroRadius*s, theme.RowHover)
		}
		dbIcSz := 15.0 * s
		dbIcCol := theme.Icon
		if dbHovered {
			dbIcCol = theme.Accent
		}
		DrawIcon(c, IconDashboard,
			float64(db.Rect.Left)+(float64(db.Rect.Width())-dbIcSz)*0.5,
			float64(db.Rect.Top)+(float64(db.Rect.Height())-dbIcSz)*0.5,
			dbIcSz, dbIcCol,
		)

		// Live summary pill to the left of Dashboard button.
		sumText := formatHeaderSummary(snap)
		sumSz := MeasureText(sumText, summarySpec)
		dotSz := 6.0 * s
		dotGap := pxRound(5 * s)
		sumPadX := pxRound(7 * s)
		sumH := pxRound(21 * s)
		sumW := sumPadX + int(math.Ceil(dotSz)) + dotGap + sumSz.W + sumPadX
		sumX := int(db.Rect.Left) - pxRound(6*s) - sumW
		sumY := hMidY - sumH/2

		sumBg, sumDot, sumTxt := headerSummaryPalette(snap.State, theme)
		if sumX > hx+verW+pxRound(6*s) {
			c.FillRoundedRect(float64(sumX), float64(sumY), float64(sumW), float64(sumH), MicroRadius*s, sumBg)
			dotCX := float64(sumX+sumPadX) + dotSz*0.5
			dotCY := float64(hMidY)
			if snap.State == "sleeping" {
				c.StrokeCircle(dotCX, dotCY, dotSz*0.42, math.Max(1.2, 1.4*s), sumDot)
			} else {
				c.FillCircle(dotCX, dotCY, dotSz*0.48, sumDot)
			}
			texts = append(texts, TextDraw{
				Text:  sumText,
				X:     sumX + sumPadX + int(math.Ceil(dotSz)) + dotGap,
				Y:     sumY + (sumH-sumSz.H)/2,
				Color: sumTxt,
				Spec:  summarySpec,
			})
		}
	}

	// Header / List divider.
	c.FillRect(int(l.CardRect.Left)+1, int(l.HeaderRect.Bottom), int(l.CardRect.Width())-2, 1, theme.Divider)

	// 4. Services List (clipped to ListRect when scrollable).
	if len(l.Services) == 0 {
		msg := "No services configured"
		msz := MeasureText(msg, footerSpec)
		texts = append(texts, TextDraw{
			Text:  msg,
			X:     int(l.CardRect.Left) + padX,
			Y:     int(l.ListRect.Top) + (int(l.ListRect.Height())-msz.H)/2,
			Color: theme.Muted,
			Spec:  footerSpec,
		})
	} else {
		listClip := l.ListRect
		listTop := int(l.ListRect.Top)
		listBot := int(l.ListRect.Bottom)
		c.Clip = &listClip

		for i := range l.Services {
			sr := &l.Services[i]
			// Skip completely off-screen rows.
			rowBottom := int(sr.rowRect.Bottom)
			if sr.expanded {
				rowBottom = sr.drawer.drawerEnd
			}
			if rowBottom <= listTop || int(sr.rowRect.Top) >= listBot {
				continue
			}

			rowVisible := int(sr.rowRect.Bottom) > listTop && int(sr.rowRect.Top) < listBot
			if rowVisible {
				isHovered := sr.inlineActive
				isFocused := focusID == "svc:"+sr.svc.Name

				if isHovered || isFocused || sr.expanded {
					bg := theme.RowHover
					if sr.expanded && !isHovered && !isFocused {
						bg = theme.RowHover.WithAlpha(theme.RowHover.A * 2 / 3)
					}
					c.FillRoundedRect(
						float64(sr.rowRect.Left),
						float64(sr.rowRect.Top)+1,
						float64(sr.rowRect.Width()),
						float64(sr.rowRect.Height())-2,
						HoverPillRadius*s,
						bg,
					)
				}
				if isFocused {
					c.StrokeRoundedRect(
						float64(sr.rowRect.Left)+1,
						float64(sr.rowRect.Top)+2,
						float64(sr.rowRect.Width())-2,
						float64(sr.rowRect.Height())-4,
						HoverPillRadius*s-0.5,
						math.Max(1.5, 1.75*s),
						theme.Focus,
					)
				}

				// Left semantic status dot.
				dotCX := float64(sr.rowRect.Left) + 14.0*s
				dotCY := float64(sr.rowRect.Top) + float64(sr.rowRect.Height())*0.5
				drawServiceStatusDot(c, sr.svc, dotCX, dotCY, s, theme)

				// Right side: resting badge + chevron OR inline 24x24 icon buttons on hover/focus.
				textLeft := int(sr.rowRect.Left) + pxRound(26*s)
				var textRightLimit int

				if sr.inlineActive && len(sr.inlineBtns) > 0 {
					textRightLimit = int(sr.inlineBtns[0].Rect.Left) - pxRound(8*s)
					for _, btn := range sr.inlineBtns {
						bHov := state.HoveredTargetID == btn.ID
						if bHov {
							bBg := theme.Elevated
							bBorder := theme.Border
							if btn.Danger {
								bBg = theme.ErrorSoft
								bBorder = theme.Error.WithAlpha(90)
							}
							c.FillRoundedRect(float64(btn.Rect.Left), float64(btn.Rect.Top), float64(btn.Rect.Width()), float64(btn.Rect.Height()), MicroRadius*s, bBg)
							c.StrokeRoundedRect(float64(btn.Rect.Left)+0.5, float64(btn.Rect.Top)+0.5, float64(btn.Rect.Width())-1, float64(btn.Rect.Height())-1, math.Max(1, MicroRadius*s-0.5), 1.0, bBorder)
						}
						icCol := theme.Icon
						if btn.Danger {
							if bHov {
								icCol = theme.Error
							} else {
								icCol = theme.Error.WithAlpha(200)
							}
						} else if bHov {
							icCol = theme.Accent
						}
						icSz := 14.0 * s
						DrawIcon(c, btn.Icon,
							float64(btn.Rect.Left)+(float64(btn.Rect.Width())-icSz)*0.5,
							float64(btn.Rect.Top)+(float64(btn.Rect.Height())-icSz)*0.5,
							icSz, icCol,
						)
					}
				} else {
					// Resting state: subtle chevron + right-side RAM/state pill.
					chevSz := 12.0 * s
					chevX := float64(sr.rowRect.Right) - 8.0*s - chevSz
					chevY := float64(sr.rowRect.Top) + (float64(sr.rowRect.Height())-chevSz)*0.5
					chevIcon := IconChevronRight
					if sr.expanded {
						chevIcon = IconChevronDown
					}
					DrawIcon(c, chevIcon, chevX, chevY, chevSz, theme.IconDim)

					bText, bBg, bFg := serviceRightBadge(sr.svc, theme)
					bSz := MeasureText(bText, badgeSpec)
					bPadX := pxRound(6 * s)
					bH := pxRound(20 * s)
					bW := bSz.W + bPadX*2
					bX := int(math.Floor(chevX)) - pxRound(5*s) - bW
					bY := int(sr.rowRect.Top) + (int(sr.rowRect.Height())-bH)/2
					c.FillRoundedRect(float64(bX), float64(bY), float64(bW), float64(bH), MicroRadius*s, bBg)
					texts = append(texts, TextDraw{
						Text:  bText,
						X:     bX + bPadX,
						Y:     bY + (bH-bSz.H)/2,
						Color: bFg,
						Spec:  badgeSpec,
						Clip:  &listClip,
					})
					textRightLimit = bX - pxRound(8*s)
				}

				// Service name & mono subtitle.
				availW := maxInt(pxRound(40*s), textRightLimit-textLeft)
				nameSz := MeasureText(sr.svc.Name, svcNameSpec)
				subText, subCol := formatServiceSubtitle(sr.svc, theme)
				subSz := MeasureText(subText, svcSubSpec)
				lineGap := pxRound(2 * s)
				blockH := nameSz.H + lineGap + subSz.H
				blockY := int(sr.rowRect.Top) + (int(sr.rowRect.Height())-blockH)/2

				texts = append(texts,
					TextDraw{
						Text:  sr.svc.Name,
						X:     textLeft,
						Y:     blockY,
						MaxW:  availW,
						Color: theme.Ink,
						Spec:  svcNameSpec,
						Clip:  &listClip,
					},
					TextDraw{
						Text:  subText,
						X:     textLeft,
						Y:     blockY + nameSz.H + lineGap,
						MaxW:  availW,
						Color: subCol,
						Spec:  svcSubSpec,
						Clip:  &listClip,
					},
				)
			}

			// Expanded inline inset detail drawer.
			if sr.expanded && int(sr.drawer.boxRect.Bottom) > listTop && int(sr.drawer.boxRect.Top) < listBot {
				dl := &sr.drawer
				bx := float64(dl.boxRect.Left)
				by := float64(dl.boxRect.Top)
				bw := float64(dl.boxRect.Width())
				bh := float64(dl.boxRect.Height())
				drRad := 8.0 * s
				c.FillRoundedRect(bx, by, bw, bh, drRad, theme.Surface)
				c.StrokeRoundedRect(bx+0.5, by+0.5, bw-1, bh-1, math.Max(1, drRad-0.5), 1.0, theme.Border)

				dPadX := pxRound(10 * s)
				texts = append(texts, TextDraw{
					Text:  dl.metaText,
					X:     int(dl.boxRect.Left) + dPadX,
					Y:     dl.metaY,
					MaxW:  int(dl.boxRect.Width()) - dPadX*2,
					Color: theme.Muted,
					Spec:  drawerMetaSpec,
					Clip:  &listClip,
				})

				if dl.hasError && len(dl.errLines) > 0 {
					ex := float64(dl.errRect.Left)
					ey := float64(dl.errRect.Top)
					ew := float64(dl.errRect.Width())
					eh := float64(dl.errRect.Height())
					c.FillRoundedRect(ex, ey, ew, eh, MicroRadius*s, theme.ErrorSoft)
					c.StrokeRoundedRect(ex+0.5, ey+0.5, ew-1, eh-1, math.Max(1, MicroRadius*s-0.5), 1.0, theme.Error.WithAlpha(70))
					errLH := MeasureText("Ag", drawerErrSpec).H
					eyTxt := int(dl.errRect.Top) + pxRound(6*s)
					for _, line := range dl.errLines {
						texts = append(texts, TextDraw{
							Text:  line,
							X:     int(dl.errRect.Left) + pxRound(8*s),
							Y:     eyTxt,
							MaxW:  int(dl.errRect.Width()) - pxRound(16*s),
							Color: theme.ErrorText,
							Spec:  drawerErrSpec,
							Clip:  &listClip,
						})
						eyTxt += errLH + pxRound(2*s)
					}
				}

				// Drawer action pills.
				for _, bt := range dl.buttons {
					bHov := state.HoveredTargetID == bt.ID
					isCopied := bt.Action.Kind == ActionServiceCopyURL && state.CopiedServiceURL == sr.svc.Name
					bBg := theme.Elevated
					bBorder := theme.Border
					bTxt := theme.Ink
					bIc := theme.Icon

					if isCopied {
						bBg = theme.SuccessSoft
						bBorder = theme.Success.WithAlpha(90)
						bTxt = theme.SuccessText
						bIc = theme.Success
					} else if bt.Danger {
						if bHov {
							bBg = theme.ErrorSoft
							bBorder = theme.Error.WithAlpha(110)
							bTxt = theme.ErrorText
							bIc = theme.Error
						} else {
							bTxt = theme.ErrorText
							bIc = theme.Error
						}
					} else if bHov {
						bBg = theme.AccentSoft
						bBorder = theme.Accent.WithAlpha(110)
						bTxt = theme.AccentText
						bIc = theme.Accent
					}

					c.FillRoundedRect(float64(bt.Rect.Left), float64(bt.Rect.Top), float64(bt.Rect.Width()), float64(bt.Rect.Height()), MicroRadius*s, bBg)
					c.StrokeRoundedRect(float64(bt.Rect.Left)+0.5, float64(bt.Rect.Top)+0.5, float64(bt.Rect.Width())-1, float64(bt.Rect.Height())-1, math.Max(1, MicroRadius*s-0.5), 1.0, bBorder)

					icSz := 12.0 * s
					icX := float64(bt.Rect.Left) + 8.0*s
					icY := float64(bt.Rect.Top) + (float64(bt.Rect.Height())-icSz)*0.5
					DrawIcon(c, bt.Icon, icX, icY, icSz, bIc)

					lblSz := MeasureText(bt.Label, drawerBtnSpec)
					texts = append(texts, TextDraw{
						Text:  bt.Label,
						X:     int(math.Round(icX+icSz)) + pxRound(5*s),
						Y:     int(bt.Rect.Top) + (int(bt.Rect.Height())-lblSz.H)/2,
						Color: bTxt,
						Spec:  drawerBtnSpec,
						Clip:  &listClip,
					})
				}
			}
		}

		// Sleek scrollbar thumb when list overflows.
		if l.MaxScroll > 0 && l.TotalListH > 0 {
			trackH := float64(l.ListRect.Height()) - 12.0*s
			thumbH := math.Max(22.0*s, trackH*(float64(l.ListRect.Height())/float64(l.TotalListH)))
			frac := float64(clampInt(state.ScrollOffset, 0, l.MaxScroll)) / float64(l.MaxScroll)
			thumbY := float64(l.ListRect.Top) + 6.0*s + frac*(trackH-thumbH)
			thumbW := math.Max(3.0, 3.5*s)
			thumbX := float64(l.CardRect.Right) - 4.0*s - thumbW
			c.FillRoundedRect(thumbX, thumbY, thumbW, thumbH, thumbW*0.5, theme.IconDim)
		}
		c.Clip = nil
	}

	// List / Footer divider.
	c.FillRect(int(l.CardRect.Left)+1, int(l.ListRect.Bottom), int(l.CardRect.Width())-2, 1, theme.Divider)

	// 5. Footer actions.
	for _, fr := range l.FooterRows {
		if fr.sepY >= 0 {
			c.FillRect(int(l.CardRect.Left)+padX, fr.sepY, int(l.CardRect.Width())-padX*2, 1, theme.Divider)
		}
		hovered := state.HoveredTargetID == fr.spec.id
		focused := focusID == fr.spec.id

		if hovered || focused {
			bg := theme.RowHover
			if fr.spec.danger {
				bg = theme.ErrorSoft
			}
			c.FillRoundedRect(
				float64(fr.rowRect.Left),
				float64(fr.rowRect.Top)+1,
				float64(fr.rowRect.Width()),
				float64(fr.rowRect.Height())-2,
				HoverPillRadius*s,
				bg,
			)
		}
		if focused {
			c.StrokeRoundedRect(
				float64(fr.rowRect.Left)+1,
				float64(fr.rowRect.Top)+2,
				float64(fr.rowRect.Width())-2,
				float64(fr.rowRect.Height())-4,
				HoverPillRadius*s-0.5,
				math.Max(1.5, 1.75*s),
				theme.Focus,
			)
		}

		icSz := 14.5 * s
		icX := float64(l.CardRect.Left) + 14.0*s
		icY := float64(fr.rowRect.Top) + (float64(fr.rowRect.Height())-icSz)*0.5
		icCol := theme.Icon
		lblCol := theme.Ink
		if fr.spec.danger {
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

		iconToDraw := fr.spec.icon
		if fr.spec.action.Kind == ActionCopyStatus && state.CopiedStatus {
			iconToDraw = IconCheck
			icCol = theme.Success
		}
		DrawIcon(c, iconToDraw, icX, icY, icSz, icCol)

		lblSz := MeasureText(fr.spec.label, footerSpec)
		texts = append(texts, TextDraw{
			Text:  fr.spec.label,
			X:     int(l.CardRect.Left) + pxRound(38*s),
			Y:     int(fr.rowRect.Top) + (int(fr.rowRect.Height())-lblSz.H)/2,
			Color: lblCol,
			Spec:  footerSpec,
		})

		rightEdge := int(l.CardRect.Right) - padX
		if fr.spec.action.Kind == ActionCopyStatus && state.CopiedStatus {
			// Inline "Copied" pill confirmation without closing.
			cpText := "Copied"
			cpSz := MeasureText(cpText, badgeSpec)
			cpPadX := pxRound(7 * s)
			cpH := pxRound(19 * s)
			cpW := cpSz.W + cpPadX*2
			cpX := rightEdge - cpW
			cpY := int(fr.rowRect.Top) + (int(fr.rowRect.Height())-cpH)/2
			c.FillRoundedRect(float64(cpX), float64(cpY), float64(cpW), float64(cpH), MicroRadius*s, theme.SuccessSoft)
			texts = append(texts, TextDraw{
				Text:  cpText,
				X:     cpX + cpPadX,
				Y:     cpY + (cpH-cpSz.H)/2,
				Color: theme.SuccessText,
				Spec:  badgeSpec,
			})
		} else if fr.spec.shortcut != "" {
			scSz := MeasureText(fr.spec.shortcut, shortcutSpec)
			texts = append(texts, TextDraw{
				Text:   fr.spec.shortcut,
				X:      rightEdge,
				Y:      int(fr.rowRect.Top) + (int(fr.rowRect.Height())-scSz.H)/2,
				Color:  theme.Muted,
				Spec:   shortcutSpec,
				AlignR: true,
			})
		}
	}

	// 6. Flush all ClearType text onto the opaque card interior.
	DrawTexts(c, texts)
	return c, l
}

func popoverFooterSpecs() []footerItemSpec {
	return []footerItemSpec{
		{id: "ftr:dashboard", label: "Open Web Dashboard", icon: IconDashboard, action: Action{Kind: ActionOpenDashboard}},
		{id: "ftr:reload", label: "Reload Config", shortcut: "Ctrl+R", icon: IconReload, action: Action{Kind: ActionReloadConfig}},
		{id: "ftr:logs", label: "Open Logs Folder", icon: IconFolder, action: Action{Kind: ActionOpenLogs}},
		{id: "ftr:config", label: "Open Config File", icon: IconFileCode, action: Action{Kind: ActionOpenConfig}},
		{id: "ftr:copy", label: "Copy Status", shortcut: "Ctrl+C", icon: IconCopy, action: Action{Kind: ActionCopyStatus}},
		{id: "ftr:exit", label: "Exit McpHub", icon: IconPower, action: Action{Kind: ActionExit}, sepBefore: true, danger: true},
	}
}

type inlineActionSpec struct {
	icon    IconID
	action  Action
	danger  bool
	tooltip string
}

func serviceInlineActions(svc ServiceItem) []inlineActionSpec {
	state := normalizedServiceState(svc)
	var out []inlineActionSpec
	switch state {
	case "stopped":
		out = append(out, inlineActionSpec{icon: IconPlay, action: Action{Kind: ActionServiceStart, Service: svc.Name}, tooltip: "Start"})
	case "sleeping":
		out = append(out, inlineActionSpec{icon: IconWake, action: Action{Kind: ActionServiceWake, Service: svc.Name}, tooltip: "Wake"})
	case "starting", "stopping":
		out = append(out, inlineActionSpec{icon: IconStop, action: Action{Kind: ActionServiceStop, Service: svc.Name}, danger: true, tooltip: "Stop"})
	default: // running, degraded
		out = append(out, inlineActionSpec{icon: IconRestart, action: Action{Kind: ActionServiceRestart, Service: svc.Name}, tooltip: "Restart"})
		if svc.HasReload {
			out = append(out, inlineActionSpec{icon: IconReload, action: Action{Kind: ActionServiceReload, Service: svc.Name}, tooltip: "Reload"})
		}
		if svc.CanSleep {
			out = append(out, inlineActionSpec{icon: IconSleep, action: Action{Kind: ActionServiceSleep, Service: svc.Name}, tooltip: "Sleep"})
		}
		out = append(out, inlineActionSpec{icon: IconStop, action: Action{Kind: ActionServiceStop, Service: svc.Name}, danger: true, tooltip: "Stop"})
	}
	out = append(out, inlineActionSpec{icon: IconLog, action: Action{Kind: ActionServiceOpenLog, Service: svc.Name}, tooltip: "Open Log"})
	return out
}

type drawerActionSpec struct {
	label  string
	icon   IconID
	action Action
	danger bool
}

func serviceDrawerActions(svc ServiceItem) []drawerActionSpec {
	state := normalizedServiceState(svc)
	var out []drawerActionSpec
	if strings.TrimSpace(svc.ClientURL) != "" {
		out = append(out, drawerActionSpec{
			label:  "Copy URL",
			icon:   IconCopy,
			action: Action{Kind: ActionServiceCopyURL, Service: svc.Name, Payload: svc.ClientURL},
		})
	}
	out = append(out, drawerActionSpec{
		label:  "Log",
		icon:   IconLog,
		action: Action{Kind: ActionServiceOpenLog, Service: svc.Name},
	})
	switch state {
	case "stopped":
		out = append(out, drawerActionSpec{label: "Start", icon: IconPlay, action: Action{Kind: ActionServiceStart, Service: svc.Name}})
	case "sleeping":
		out = append(out, drawerActionSpec{label: "Wake", icon: IconWake, action: Action{Kind: ActionServiceWake, Service: svc.Name}})
	case "starting", "stopping":
		out = append(out, drawerActionSpec{label: "Stop", icon: IconStop, action: Action{Kind: ActionServiceStop, Service: svc.Name}, danger: true})
	default:
		out = append(out, drawerActionSpec{label: "Restart", icon: IconRestart, action: Action{Kind: ActionServiceRestart, Service: svc.Name}})
		if svc.CanSleep {
			out = append(out, drawerActionSpec{label: "Sleep", icon: IconSleep, action: Action{Kind: ActionServiceSleep, Service: svc.Name}})
		} else {
			out = append(out, drawerActionSpec{label: "Stop", icon: IconStop, action: Action{Kind: ActionServiceStop, Service: svc.Name}, danger: true})
		}
	}
	return out
}

func normalizedServiceState(svc ServiceItem) string {
	st := strings.ToLower(strings.TrimSpace(svc.State))
	if st != "" {
		return st
	}
	if svc.Running {
		return "running"
	}
	return "stopped"
}

func drawServiceStatusDot(c *Canvas, svc ServiceItem, cx, cy, scale float64, theme Theme) {
	r := 3.6 * scale
	switch normalizedServiceState(svc) {
	case "running":
		if svc.Healthy || svc.LastError == "" {
			c.FillCircle(cx, cy, r, theme.Success)
		} else {
			c.FillCircle(cx, cy, r, theme.Warning)
		}
	case "sleeping":
		c.StrokeCircle(cx, cy, r-0.4*scale, math.Max(1.4, 1.65*scale), theme.Sleeping)
	case "starting", "stopping":
		c.StrokeArc(cx, cy, r-0.3*scale, -90, 270, math.Max(1.4, 1.65*scale), theme.Accent)
	case "degraded":
		c.FillCircle(cx, cy, r, theme.Warning)
	case "stopped":
		if strings.TrimSpace(svc.LastError) != "" {
			c.FillCircle(cx, cy, r, theme.Error)
		} else {
			c.FillCircle(cx, cy, r, theme.IconDim)
		}
	default:
		c.FillCircle(cx, cy, r, theme.Error)
	}
}

func formatServiceSubtitle(svc ServiceItem, theme Theme) (string, RGBA) {
	mode := strings.ToLower(strings.TrimSpace(svc.Mode))
	if mode == "stdio-bridge" {
		mode = "stdio"
	} else if mode == "" {
		mode = "http"
	}
	prefix := mode
	if svc.Port > 0 && mode == "http" {
		prefix = fmt.Sprintf("http :%d", svc.Port)
	}

	st := normalizedServiceState(svc)
	switch st {
	case "running":
		if svc.PrivateMB > 0 {
			return fmt.Sprintf("%s · %s", prefix, formatMB(svc.PrivateMB)), theme.Muted
		}
		if svc.ActiveSSE > 0 {
			return fmt.Sprintf("%s · %d SSE", prefix, svc.ActiveSSE), theme.Muted
		}
		return prefix + " · ready", theme.Muted
	case "sleeping":
		if svc.WakeCount > 0 {
			return fmt.Sprintf("%s · sleep · %d wakes", prefix, svc.WakeCount), theme.Sleeping
		}
		return prefix + " · sleep", theme.Sleeping
	case "starting":
		return prefix + " · starting", theme.AccentText
	case "stopping":
		return prefix + " · stopping", theme.Muted
	case "degraded":
		crashes := svc.CrashRestarts
		if crashes <= 0 {
			crashes = svc.Restarts
		}
		if crashes == 1 {
			return "degraded · 1 crash", theme.WarningText
		}
		if crashes > 1 {
			return fmt.Sprintf("degraded · %d crashes", crashes), theme.WarningText
		}
		return prefix + " · degraded", theme.WarningText
	case "stopped":
		if strings.TrimSpace(svc.LastError) != "" {
			return prefix + " · error", theme.ErrorText
		}
		return prefix + " · stopped", theme.Muted
	default:
		return prefix + " · " + st, theme.Muted
	}
}

func serviceRightBadge(svc ServiceItem, theme Theme) (text string, bg RGBA, fg RGBA) {
	switch normalizedServiceState(svc) {
	case "running":
		if svc.PrivateMB > 0 {
			return formatMB(svc.PrivateMB), theme.Surface, theme.Muted
		}
		return "UP", theme.SuccessSoft, theme.SuccessText
	case "sleeping":
		return "SLEEP", theme.SleepingSoft, theme.Sleeping
	case "starting":
		return "START", theme.AccentSoft, theme.AccentText
	case "stopping":
		return "STOP", theme.Surface, theme.Muted
	case "degraded":
		return "DEGRADED", theme.WarningSoft, theme.WarningText
	case "stopped":
		if strings.TrimSpace(svc.LastError) != "" {
			return "ERROR", theme.ErrorSoft, theme.ErrorText
		}
		return "OFF", theme.Surface, theme.Muted
	default:
		return "ERR", theme.ErrorSoft, theme.ErrorText
	}
}

func formatServiceMetrics(svc ServiceItem) string {
	var parts []string
	if svc.PID > 0 {
		parts = append(parts, fmt.Sprintf("PID %d", svc.PID))
	}
	if svc.Port > 0 {
		parts = append(parts, fmt.Sprintf(":%d", svc.Port))
	} else if svc.Mode != "" {
		m := svc.Mode
		if m == "stdio-bridge" {
			m = "stdio"
		}
		parts = append(parts, m)
	}
	parts = append(parts, fmt.Sprintf("%d reqs", svc.TotalReqs))
	if svc.ActiveSSE > 0 {
		parts = append(parts, fmt.Sprintf("%d SSE", svc.ActiveSSE))
	}
	if svc.CrashRestarts > 0 {
		parts = append(parts, fmt.Sprintf("%d crashes", svc.CrashRestarts))
	} else if svc.WakeCount > 0 {
		parts = append(parts, fmt.Sprintf("%d wakes", svc.WakeCount))
	} else if svc.Restarts > 0 {
		parts = append(parts, fmt.Sprintf("%d restarts", svc.Restarts))
	}
	return strings.Join(parts, " · ")
}

func formatHeaderSummary(snap Snapshot) string {
	total := len(snap.Services)
	up := snap.RunningCount
	if up == 0 && total > 0 {
		for _, svc := range snap.Services {
			if normalizedServiceState(svc) == "running" {
				up++
			}
		}
	}
	mem := snap.TotalPrivateMB
	if mem <= 0 {
		for _, svc := range snap.Services {
			mem += svc.PrivateMB
		}
	}
	if total == 0 {
		return "idle"
	}
	if mem >= 1.0 {
		return fmt.Sprintf("%d/%d up · %s", up, total, formatMB(mem))
	}
	return fmt.Sprintf("%d/%d up", up, total)
}

func headerSummaryPalette(state string, theme Theme) (bg, dot, text RGBA) {
	switch state {
	case "ok", "":
		return theme.Surface, theme.Success, theme.Muted
	case "sleeping":
		return theme.SleepingSoft, theme.Sleeping, theme.Sleeping
	case "starting":
		return theme.AccentSoft, theme.Accent, theme.AccentText
	case "degraded":
		return theme.WarningSoft, theme.Warning, theme.WarningText
	case "error":
		return theme.ErrorSoft, theme.Error, theme.ErrorText
	default:
		return theme.Surface, theme.Muted, theme.Muted
	}
}

func formatMB(mb float64) string {
	if mb >= 1024 {
		return fmt.Sprintf("%.1f GB", mb/1024.0)
	}
	if mb >= 10 {
		return fmt.Sprintf("%.0f MB", mb)
	}
	return fmt.Sprintf("%.1f MB", mb)
}

// FormatStatusSummary produces a clean, secret-free plain-text summary for "Copy Status" (Ctrl+C).
func FormatStatusSummary(snap Snapshot) string {
	var b strings.Builder
	ver := strings.TrimSpace(snap.Version)
	if ver == "" {
		ver = "0.3.0"
	}
	if !strings.HasPrefix(ver, "v") {
		ver = "v" + ver
	}
	fmt.Fprintf(&b, "McpHub %s (%s)", ver, formatHeaderSummary(snap))
	if snap.UptimeSec > 0 {
		fmt.Fprintf(&b, " · uptime %s", formatUptime(snap.UptimeSec))
	}
	if snap.ControlAddr != "" {
		fmt.Fprintf(&b, " · control %s", snap.ControlAddr)
	}
	b.WriteByte('\n')
	for _, svc := range snap.Services {
		st := normalizedServiceState(svc)
		fmt.Fprintf(&b, "- %s: %s", svc.Name, st)
		if svc.Port > 0 {
			fmt.Fprintf(&b, " :%d", svc.Port)
		}
		if svc.PrivateMB > 0 {
			fmt.Fprintf(&b, " (%s)", formatMB(svc.PrivateMB))
		}
		if svc.CrashRestarts > 0 {
			fmt.Fprintf(&b, " [%d crashes]", svc.CrashRestarts)
		}
		if svc.LastError != "" {
			fmt.Fprintf(&b, " — %s", strings.TrimSpace(svc.LastError))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatUptime(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	if sec < 3600 {
		return fmt.Sprintf("%dm%ds", sec/60, sec%60)
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	if h < 24 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dd%dh", h/24, h%24)
}

func ptInRect(x, y int, r Rect) bool {
	return x >= int(r.Left) && x < int(r.Right) && y >= int(r.Top) && y < int(r.Bottom)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
