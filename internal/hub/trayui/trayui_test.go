package trayui

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIcons_AllStayInsideGridAndHaveBalancedInkCoverage(t *testing.T) {
	icons := AllIcons()
	if len(icons) < 12 {
		t.Fatalf("expected comprehensive UI icon set, got %d", len(icons))
	}
	ink := RGBA{0x15, 0x16, 0x19, 0xFF}

	for _, id := range icons {
		c := NewCanvas(24, 24)
		DrawIcon(c, id, 0, 0, 24, ink)

		var alphaSum float64
		for y := 0; y < 24; y++ {
			for x := 0; x < 24; x++ {
				a := c.Pix[(y*24+x)*4+3]
				if a > 0 {
					if x == 0 || x == 23 || y == 0 || y == 23 {
						t.Errorf("icon %q touched canvas edge at (%d,%d) alpha=%d", id.Name(), x, y, a)
					}
					alphaSum += float64(a) / 255.0
				}
			}
		}
		coverage := alphaSum / float64(24*24)
		if coverage < 0.015 || coverage > 0.30 {
			t.Errorf("icon %q coverage = %.2f%%, want 1.5%%..30%%", id.Name(), coverage*100)
		}
	}
}

func TestTrayIcons_AllStatesSizesAndThemes(t *testing.T) {
	states := []string{"ok", "sleeping", "starting", "degraded", "error"}
	sizes := []int{16, 20, 24, 32}

	for _, light := range []bool{false, true} {
		for _, sz := range sizes {
			fingerprints := make(map[string]string, len(states))
			for _, st := range states {
				c := RenderTrayIcon(sz, st, light)
				if c.Width != sz || c.Height != sz {
					t.Fatalf("RenderTrayIcon(%d,%q,%v) size = %dx%d", sz, st, light, c.Width, c.Height)
				}
				nonZero := 0
				for i := 3; i < len(c.Pix); i += 4 {
					if c.Pix[i] > 0 {
						nonZero++
					}
				}
				if nonZero < sz*sz/10 {
					t.Errorf("tray icon (%d,%q,light=%v) has too few lit pixels: %d", sz, st, light, nonZero)
				}
				fp := string(c.Pix)
				if prev, dup := fingerprints[fp]; dup {
					t.Errorf("tray icon (%d,light=%v): state %q identical to %q", sz, light, st, prev)
				}
				fingerprints[fp] = st
			}
		}
	}
}

func TestAppIcon_AllResolutions(t *testing.T) {
	for _, sz := range []int{16, 20, 24, 32, 48, 256} {
		c := RenderAppIcon(sz)
		if c.Width != sz || c.Height != sz {
			t.Fatalf("RenderAppIcon(%d) = %dx%d", sz, c.Width, c.Height)
		}
		// Center pixel should be bright cobalt (#60A5FA -> B > R).
		mid := ((sz/2)*sz + (sz / 2)) * 4
		b, g, r, a := c.Pix[mid+0], c.Pix[mid+1], c.Pix[mid+2], c.Pix[mid+3]
		if a < 200 || b <= r {
			t.Errorf("RenderAppIcon(%d) center pixel BGRA=(%d,%d,%d,%d), expected cobalt core", sz, b, g, r, a)
		}
		// Corner pixel (0,0) on larger icons must be transparent (rounded squircle).
		if sz >= 32 && c.Pix[3] != 0 {
			t.Errorf("RenderAppIcon(%d) corner (0,0) alpha=%d, want 0", sz, c.Pix[3])
		}
	}
}

func TestPlacePopup_MultiMonitorAndAllTaskbarEdges(t *testing.T) {
	// Secondary monitor to the left with negative X coordinates.
	negWork := Rect{Left: -1920, Top: 0, Right: 0, Bottom: 1040}

	// 1. Bottom taskbar on negative monitor.
	wx, wy := PlacePopup(Rect{Left: -120, Top: 1040, Right: -96, Bottom: 1080}, true, -108, 1060, 356, 420, 8, negWork, false)
	cardL, cardT := wx+8, wy+8
	if cardL < negWork.Left+4 || cardL+356 > negWork.Right-4 {
		t.Errorf("bottom taskbar X out of work area: left=%d right=%d work=[%d,%d]", cardL, cardL+356, negWork.Left, negWork.Right)
	}
	if cardT < negWork.Top+4 || cardT+420 > negWork.Bottom-4 {
		t.Errorf("bottom taskbar Y out of work area: top=%d bot=%d work=[%d,%d]", cardT, cardT+420, negWork.Top, negWork.Bottom)
	}

	// 2. Top taskbar.
	topWork := Rect{Left: 0, Top: 48, Right: 1920, Bottom: 1080}
	wx, wy = PlacePopup(Rect{Left: 1800, Top: 0, Right: 1824, Bottom: 48}, true, 1812, 24, 356, 380, 8, topWork, false)
	cardT = wy + 8
	if cardT < topWork.Top+4 || cardT > topWork.Top+16 {
		t.Errorf("top taskbar should open directly below workArea.Top: got cardT=%d topWork.Top=%d", cardT, topWork.Top)
	}

	// 3. Left taskbar.
	leftWork := Rect{Left: 64, Top: 0, Right: 1920, Bottom: 1080}
	wx, wy = PlacePopup(Rect{Left: 0, Top: 960, Right: 64, Bottom: 992}, true, 32, 976, 356, 380, 8, leftWork, false)
	cardL = wx + 8
	if cardL < leftWork.Left+4 || cardL > leftWork.Left+16 {
		t.Errorf("left taskbar should open directly right of workArea.Left: got cardL=%d leftWork.Left=%d", cardL, leftWork.Left)
	}

	// 4. Right taskbar (Quick Menu).
	rightWork := Rect{Left: 0, Top: 0, Right: 1856, Bottom: 1080}
	wx, wy = PlacePopup(Rect{Left: 1856, Top: 960, Right: 1920, Bottom: 992}, true, 1888, 976, 248, 320, 8, rightWork, true)
	cardL, cardT = wx+8, wy+8
	if cardL+248 > rightWork.Right-4 || cardL < rightWork.Left+4 {
		t.Errorf("right taskbar Quick Menu X clamped poorly: left=%d right=%d", cardL, cardL+248)
	}
	if cardT+320 > rightWork.Bottom-4 || cardT < rightWork.Top+4 {
		t.Errorf("right taskbar Quick Menu Y clamped poorly: top=%d bot=%d", cardT, cardT+320)
	}
}

func sampleSnapshot() Snapshot {
	return Snapshot{
		State:          "degraded",
		Tooltip:        "McpHub v0.3.0 — 3/5 running, 1 sleeping, 1 degraded",
		Version:        "0.3.0",
		UptimeSec:      14820,
		TotalPrivateMB: 312.4,
		ControlAddr:    "127.0.0.1:18470",
		ConfigPath:     "C:/Users/dev/AppData/Local/McpHub/config.json",
		LogDir:         "C:/Users/dev/AppData/Local/McpHub/logs",
		RunningCount:   3,
		SleepingCount:  1,
		ErrorCount:     1,
		Services: []ServiceItem{
			{
				Name:       "docs-mcp",
				State:      "running",
				Healthy:    true,
				Running:    true,
				HasReload:  true,
				CanSleep:   true,
				Mode:       "http",
				StartMode:  "eager",
				PID:        14820,
				Port:       18473,
				PrivateMB:  107.2,
				TotalReqs:  428,
				ActiveSSE:  2,
				LogFile:    "docs-mcp.log",
				ClientURL:  "http://127.0.0.1:18471/s/docs/mcp",
			},
			{
				Name:       "search-mcp",
				State:      "running",
				Healthy:    true,
				Running:    true,
				HasReload:  false,
				CanSleep:   true,
				Mode:       "http",
				StartMode:  "eager",
				PID:        15104,
				Port:       18474,
				PrivateMB:  128.6,
				TotalReqs:  912,
				LogFile:    "search-mcp.log",
				ClientURL:  "http://127.0.0.1:18471/s/search/mcp",
			},
			{
				Name:      "filesystem-mcp",
				State:     "sleeping",
				Healthy:   false,
				Running:   false,
				CanSleep:  true,
				Mode:      "stdio-bridge",
				StartMode: "lazy",
				WakeCount: 4,
				TotalReqs: 63,
				LogFile:   "filesystem-mcp.log",
				ClientURL: "http://127.0.0.1:18471/s/fs/mcp",
			},
			{
				Name:          "browser-mcp",
				State:         "degraded",
				Healthy:       false,
				Running:       true,
				HasReload:     true,
				CanSleep:      false,
				Mode:          "http",
				StartMode:     "eager",
				PID:           19302,
				Port:          18476,
				PrivateMB:     76.6,
				Restarts:      3,
				CrashRestarts: 2,
				TotalReqs:     19,
				LogFile:       "browser-mcp.log",
				ClientURL:     "http://127.0.0.1:18471/s/browser/mcp",
				LastError:     "circuit breaker open: upstream health probe timed out on 127.0.0.1:18476/healthz",
			},
			{
				Name:      "notes-mcp",
				State:     "stopped",
				Healthy:   false,
				Running:   false,
				Mode:      "stdio-bridge",
				StartMode: "lazy",
				LogFile:   "notes-mcp.log",
				ClientURL: "http://127.0.0.1:18471/s/notes/mcp",
			},
		},
	}
}

func TestPopoverAndQuickMenu_OffscreenRenderContentAndHitTest(t *testing.T) {
	snap := sampleSnapshot()

	for _, dpi := range []int{96, 192} {
		for _, light := range []bool{true, false} {
			theme := ThemeFor(light)
			state := PopoverState{
				ExpandedService: "browser-mcp",
				HoveredService:  "docs-mcp",
				HoveredTargetID: "svcbtn:docs-mcp:12",
				FocusIndex:      -1,
				CopiedStatus:    true,
			}
			c, l := RenderPopover(snap, state, theme, dpi, 0)
			if c.Width <= 0 || c.Height <= 0 {
				t.Fatalf("empty popover canvas at dpi=%d light=%v", dpi, light)
			}

			// Verify real non-background ink pixels exist in header, services list, expanded drawer, and footer!
			checkRegionHasInk(t, c, l.HeaderRect, theme.Canvas, "header")
			checkRegionHasInk(t, c, l.ListRect, theme.Canvas, "services-list")
			checkRegionHasInk(t, c, l.FooterRect, theme.Canvas, "footer")

			var drawerChecked bool
			for _, sr := range l.Services {
				if sr.expanded {
					checkRegionHasInk(t, c, sr.drawer.boxRect, theme.Surface, "expanded-drawer")
					drawerChecked = true
					// Hit-test first drawer button.
					if len(sr.drawer.buttons) == 0 {
						t.Fatalf("expected drawer buttons for %s", sr.svc.Name)
					}
					b := sr.drawer.buttons[0]
					hit, _ := l.HitTest(int((b.Rect.Left+b.Rect.Right)/2), int((b.Rect.Top+b.Rect.Bottom)/2))
					if hit == nil || hit.Action.Kind != b.Action.Kind {
						t.Fatalf("drawer button HitTest failed: got %+v, want %+v", hit, b.Action)
					}
				}
			}
			if !drawerChecked {
				t.Fatal("expanded drawer was not laid out")
			}

			// Hit-test inline hover button on docs-mcp.
			docRow := l.Services[0]
			if len(docRow.inlineBtns) < 4 {
				t.Fatalf("expected >=4 inline buttons on running reloadable sleepable service, got %d", len(docRow.inlineBtns))
			}
			ib := docRow.inlineBtns[0]
			hit, hovSvc := l.HitTest(int((ib.Rect.Left+ib.Rect.Right)/2), int((ib.Rect.Top+ib.Rect.Bottom)/2))
			if hit == nil || hit.Kind != TargetServiceInlineButton || hovSvc != "docs-mcp" {
				t.Fatalf("inline button HitTest = (%+v, %q), want inline btn for docs-mcp", hit, hovSvc)
			}

			// Quick Menu render & hit test.
			qmCanvas, qmLayout := RenderQuickMenu(snap, QuickMenuState{HoverIndex: len(QuickMenuItems()) - 1, FocusIndex: -1}, theme, dpi)
			checkRegionHasInk(t, qmCanvas, qmLayout.HeaderRect, theme.Canvas, "quickmenu-header")
			lastRow := qmLayout.Rows[len(qmLayout.Rows)-1]
			if idx := qmLayout.HitTest(int((lastRow.rowRect.Left+lastRow.rowRect.Right)/2), int((lastRow.rowRect.Top+lastRow.rowRect.Bottom)/2)); idx != len(qmLayout.Rows)-1 {
				t.Fatalf("quickmenu HitTest = %d, want %d", idx, len(qmLayout.Rows)-1)
			}
		}
	}

	summary := FormatStatusSummary(snap)
	if !strings.Contains(summary, "McpHub v0.3.0") || !strings.Contains(summary, "browser-mcp: degraded") {
		t.Errorf("unexpected FormatStatusSummary output:\n%s", summary)
	}
}

func checkRegionHasInk(t *testing.T, c *Canvas, r Rect, bg RGBA, label string) {
	t.Helper()
	diffCount := 0
	for y := maxInt(0, int(r.Top)+2); y < minInt(c.Height, int(r.Bottom)-2); y++ {
		for x := maxInt(0, int(r.Left)+2); x < minInt(c.Width, int(r.Right)-2); x++ {
			i := (y*c.Width + x) * 4
			if c.Pix[i+3] != 255 {
				continue
			}
			db := absInt(int(c.Pix[i+0]) - int(bg.B))
			dg := absInt(int(c.Pix[i+1]) - int(bg.G))
			dr := absInt(int(c.Pix[i+2]) - int(bg.R))
			if db+dg+dr > 30 {
				diffCount++
			}
		}
	}
	if diffCount < 40 {
		t.Errorf("region %q has too few distinct ink pixels (%d)", label, diffCount)
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestVisualQA generates high-resolution PNG contact sheets when MCP_HUB_RENDER_QA_DIR is set.
func TestVisualQA(t *testing.T) {
	outDir := strings.TrimSpace(os.Getenv("MCP_HUB_RENDER_QA_DIR"))
	if outDir == "" {
		t.Skip("set MCP_HUB_RENDER_QA_DIR to render visual QA PNG contact sheets")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", outDir, err)
	}

	// 1. tray-icons-sheet.png: All 5 states x Light & Dark taskbar strips x 16/20/24/32px + App Icon 16/32/48/128/256px + UI 24x24 Icon Grid.
	renderTrayIconsSheet(t, filepath.Join(outDir, "tray-icons-sheet.png"))

	// 2. popover-light-dark.png: Full Left-Click Control Popover in Light & Dark themes (2x HiDPI 192 DPI for razor-sharp inspection).
	renderPopoverSheet(t, filepath.Join(outDir, "popover-light-dark.png"))

	// 3. quickmenu-light-dark.png: Full Right-Click Quick Menu in Light & Dark themes (resting + danger Exit hover, 2x HiDPI).
	renderQuickMenuSheet(t, filepath.Join(outDir, "quickmenu-light-dark.png"))
}

func saveCanvasPNG(t *testing.T, path string, c *Canvas) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %q: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, c.ToNRGBA()); err != nil {
		t.Fatalf("encode png %q: %v", path, err)
	}
}

func renderTrayIconsSheet(t *testing.T, outPath string) {
	const W, H = 1140, 810
	sheet := NewCanvas(W, H)
	sheet.Fill(RGBA{0x12, 0x13, 0x17, 0xFF})

	var texts []TextDraw
	h1 := FontSpec{Family: FontSans, SizePx: 20, Weight: 600}
	h2 := FontSpec{Family: FontSans, SizePx: 14, Weight: 600}
	mono := FontSpec{Family: FontMono, SizePx: 12, Weight: 400}

	texts = append(texts, TextDraw{
		Text:  "McpHub v0.3.0 — Tri-Node Hub Icon System (Tray States × DPI + App Icon)",
		X:     28,
		Y:     22,
		Color: RGBA{0xF5, 0xF6, 0xF8, 0xFF},
		Spec:  h1,
	})

	states := []string{"ok", "sleeping", "starting", "degraded", "error"}
	sizes := []int{16, 20, 24, 32}

	strips := []struct {
		title     string
		y         int
		bg        RGBA
		tileBg    RGBA
		textCol   RGBA
		lightTask bool
	}{
		{
			title:     "DARK TASKBAR (SystemUsesLightTheme = 0)",
			y:         66,
			bg:        RGBA{0x1C, 0x1E, 0x24, 0xFF},
			tileBg:    RGBA{0x25, 0x28, 0x30, 0xFF},
			textCol:   RGBA{0xE5, 0xE7, 0xEB, 0xFF},
			lightTask: false,
		},
		{
			title:     "LIGHT TASKBAR (SystemUsesLightTheme = 1)",
			y:         248,
			bg:        RGBA{0xEE, 0xEF, 0xF2, 0xFF},
			tileBg:    RGBA{0xFA, 0xF9, 0xF6, 0xFF},
			textCol:   RGBA{0x15, 0x16, 0x19, 0xFF},
			lightTask: true,
		},
	}

	for _, st := range strips {
		sheet.FillRoundedRect(28, float64(st.y), W-56, 166, 12, st.bg)
		texts = append(texts, TextDraw{
			Text:  st.title,
			X:     44,
			Y:     st.y + 12,
			Color: st.textCol,
			Spec:  h2,
		})

		colW := 206
		for si, state := range states {
			colX := 44 + si*colW
			sheet.FillRoundedRect(float64(colX), float64(st.y+40), float64(colW-14), 112, 8, st.tileBg)
			texts = append(texts, TextDraw{
				Text:  strings.ToUpper(state),
				X:     colX + 12,
				Y:     st.y + 50,
				Color: st.textCol,
				Spec:  mono,
			})
			ix := colX + 14
			for _, sz := range sizes {
				icon := RenderTrayIcon(sz, state, st.lightTask)
				iy := st.y + 84 + (32-sz)/2
				sheet.CompositeOver(icon, ix, iy)
				texts = append(texts, TextDraw{
					Text:  string(rune('0'+sz/10)) + string(rune('0'+sz%10)),
					X:     ix + (sz-14)/2,
					Y:     st.y + 124,
					Color: st.textCol.WithAlpha(180),
					Spec:  FontSpec{Family: FontMono, SizePx: 10, Weight: 400},
				})
				ix += sz + 16
			}
		}
	}

	// Bottom-left card: App Icon multi-size preview (16, 32, 48, 128, 256 px).
	sheet.FillRoundedRect(28, 434, 728, 352, 12, RGBA{0x22, 0x25, 0x2D, 0xFF})
	texts = append(texts, TextDraw{
		Text:  "APP .EXE ICON — 16 / 32 / 48 / 128 / 256 px",
		X:     44,
		Y:     448,
		Color: RGBA{0xE5, 0xE7, 0xEB, 0xFF},
		Spec:  h2,
	})

	appSizes := []int{16, 32, 48, 128, 256}
	ax := 50
	for _, sz := range appSizes {
		ic := RenderAppIcon(sz)
		ay := 488 + (256-sz)/2
		if sz <= 128 {
			sheet.FillRoundedRect(float64(ax-8), float64(ay-8), float64(sz+16), float64(sz+16), 10, RGBA{0x2D, 0x31, 0x3C, 0xFF})
		}
		sheet.CompositeOver(ic, ax, ay)
		texts = append(texts, TextDraw{
			Text:  formatPxLabel(sz),
			X:     ax,
			Y:     754,
			Color: RGBA{0x9C, 0xA3, 0xB2, 0xFF},
			Spec:  mono,
		})
		ax += sz + 38
	}

	// Bottom-right card: 24x24 UI glyph showcase grid.
	sheet.FillRoundedRect(772, 434, 340, 352, 12, RGBA{0x22, 0x25, 0x2D, 0xFF})
	texts = append(texts, TextDraw{
		Text:  "24x24 UI GLYPHS (2px stroke)",
		X:     788,
		Y:     448,
		Color: RGBA{0xE5, 0xE7, 0xEB, 0xFF},
		Spec:  h2,
	})
	for i, id := range AllIcons() {
		col := i % 6
		row := i / 6
		gx := 788 + col*52
		gy := 492 + row*54
		sheet.FillRoundedRect(float64(gx), float64(gy), 44, 44, 8, RGBA{0x18, 0x19, 0x1C, 0xFF})
		DrawIcon(sheet, id, float64(gx+10), float64(gy+10), 24, RGBA{0xF5, 0xF6, 0xF8, 0xFF})
	}

	DrawTexts(sheet, texts)
	saveCanvasPNG(t, outPath, sheet)
}

func formatPxLabel(sz int) string {
	switch sz {
	case 16:
		return "16px"
	case 32:
		return "32px"
	case 48:
		return "48px"
	case 128:
		return "128px"
	case 256:
		return "256px"
	default:
		return ""
	}
}

func renderPopoverSheet(t *testing.T, outPath string) {
	snap := sampleSnapshot()
	const dpi = 144 // 150% DPI for crisp inspection

	lightState := PopoverState{
		ExpandedService: "browser-mcp",
		HoveredService:  "docs-mcp",
		HoveredTargetID: "svcbtn:docs-mcp:12",
		FocusIndex:      -1,
		CopiedStatus:    true,
	}
	darkState := PopoverState{
		ExpandedService:  "browser-mcp",
		HoveredService:   "docs-mcp",
		HoveredTargetID:  "svcbtn:docs-mcp:14",
		FocusIndex:       -1,
		CopiedServiceURL: "browser-mcp",
	}

	lightPop, _ := RenderPopover(snap, lightState, LightTheme, dpi, 0)
	darkPop, _ := RenderPopover(snap, darkState, DarkTheme, dpi, 0)

	pad := 44
	topBar := 48
	paneW := maxInt(lightPop.Width, darkPop.Width) + pad*2
	paneH := maxInt(lightPop.Height, darkPop.Height) + pad*2 + topBar
	sheet := NewCanvas(paneW*2, paneH)

	// Left half: soft warm desktop backdrop for Light Popover.
	sheet.FillRect(0, 0, paneW, paneH, RGBA{0xE2, 0xE5, 0xEB, 0xFF})
	// Right half: deep slate desktop backdrop for Dark Popover.
	sheet.FillRect(paneW, 0, paneW, paneH, RGBA{0x0D, 0x0F, 0x14, 0xFF})

	sheet.CompositeOver(lightPop, (paneW-lightPop.Width)/2, topBar+(paneH-topBar-lightPop.Height)/2)
	sheet.CompositeOver(darkPop, paneW+(paneW-darkPop.Width)/2, topBar+(paneH-topBar-darkPop.Height)/2)

	lblSpec := FontSpec{Family: FontSans, SizePx: 15, Weight: 600}
	DrawTexts(sheet, []TextDraw{
		{Text: "Light (Alabaster) · Hover Actions + Drawer + Copied", X: 28, Y: 16, Color: RGBA{0x1F, 0x23, 0x2B, 0xFF}, Spec: lblSpec},
		{Text: "Dark (Obsidian) · Hover Actions + Drawer + Copy URL", X: paneW + 28, Y: 16, Color: RGBA{0xE5, 0xE7, 0xEB, 0xFF}, Spec: lblSpec},
	})

	saveCanvasPNG(t, outPath, sheet)
}

func renderQuickMenuSheet(t *testing.T, outPath string) {
	snap := sampleSnapshot()
	snap.State = "ok"
	snap.RunningCount = 5
	snap.ErrorCount = 0
	const dpi = 144 // 150% DPI

	lightRest, _ := RenderQuickMenu(snap, QuickMenuState{HoverIndex: 1, FocusIndex: -1}, LightTheme, dpi)
	darkDanger, _ := RenderQuickMenu(snap, QuickMenuState{HoverIndex: len(QuickMenuItems()) - 1, FocusIndex: -1}, DarkTheme, dpi)

	pad := 48
	topBar := 48
	paneW := maxInt(lightRest.Width, darkDanger.Width) + pad*2
	paneH := maxInt(lightRest.Height, darkDanger.Height) + pad*2 + topBar
	sheet := NewCanvas(paneW*2, paneH)

	sheet.FillRect(0, 0, paneW, paneH, RGBA{0xE2, 0xE5, 0xEB, 0xFF})
	sheet.FillRect(paneW, 0, paneW, paneH, RGBA{0x0D, 0x0F, 0x14, 0xFF})

	sheet.CompositeOver(lightRest, (paneW-lightRest.Width)/2, topBar+(paneH-topBar-lightRest.Height)/2)
	sheet.CompositeOver(darkDanger, paneW+(paneW-darkDanger.Width)/2, topBar+(paneH-topBar-darkDanger.Height)/2)

	lblSpec := FontSpec{Family: FontSans, SizePx: 15, Weight: 600}
	DrawTexts(sheet, []TextDraw{
		{Text: "Quick Menu — Light (Row Hover)", X: 28, Y: 16, Color: RGBA{0x1F, 0x23, 0x2B, 0xFF}, Spec: lblSpec},
		{Text: "Quick Menu — Dark (Danger Exit Hover)", X: paneW + 28, Y: 16, Color: RGBA{0xE5, 0xE7, 0xEB, 0xFF}, Spec: lblSpec},
	})

	saveCanvasPNG(t, outPath, sheet)
}
