//go:build windows

package trayui

import (
	"math"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	gdi32DLL                 = syscall.NewLazyDLL("gdi32.dll")
	procCreateCompatibleDC   = gdi32DLL.NewProc("CreateCompatibleDC")
	procDeleteDC             = gdi32DLL.NewProc("DeleteDC")
	procSelectObject         = gdi32DLL.NewProc("SelectObject")
	procDeleteObject         = gdi32DLL.NewProc("DeleteObject")
	procCreateDIBSection     = gdi32DLL.NewProc("CreateDIBSection")
	procCreateFontIndirectW  = gdi32DLL.NewProc("CreateFontIndirectW")
	procGetTextExtentPoint32 = gdi32DLL.NewProc("GetTextExtentPoint32W")
	procGetTextMetricsW      = gdi32DLL.NewProc("GetTextMetricsW")
	procGetTextFaceW         = gdi32DLL.NewProc("GetTextFaceW")
	procSetBkMode            = gdi32DLL.NewProc("SetBkMode")
	procSetTextColor         = gdi32DLL.NewProc("SetTextColor")
	procExtTextOutW          = gdi32DLL.NewProc("ExtTextOutW")
	procGdiFlush             = gdi32DLL.NewProc("GdiFlush")
)

const (
	biRGB             = 0
	dibRGBColors      = 0
	transparentBkMode = 1
	etoClipped        = 0x0004
	cleartypeQuality  = 5
	defaultCharset    = 1
	outTTPrecis       = 4
	clipDefaultPrecis = 0
)

type gdiSize struct {
	CX int32
	CY int32
}

type gdiRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type bitmapInfoHeader struct {
	BiSize          uint32
	BiWidth         int32
	BiHeight        int32
	BiPlanes        uint16
	BiBitCount      uint16
	BiCompression   uint32
	BiSizeImage     uint32
	BiXPelsPerMeter int32
	BiYPelsPerMeter int32
	BiClrUsed       uint32
	BiClrImportant  uint32
}

type bitmapInfo struct {
	BmiHeader bitmapInfoHeader
	BmiColors [1]uint32
}

type logFontW struct {
	LfHeight         int32
	LfWidth          int32
	LfEscapement     int32
	LfOrientation    int32
	LfWeight         int32
	LfItalic         byte
	LfUnderline      byte
	LfStrikeOut      byte
	LfCharSet        byte
	LfOutPrecision   byte
	LfClipPrecision  byte
	LfQuality        byte
	LfPitchAndFamily byte
	LfFaceName       [32]uint16
}

type textMetricW struct {
	TmHeight           int32
	TmAscent           int32
	TmDescent          int32
	TmInternalLeading  int32
	TmExternalLeading  int32
	TmAveCharWidth     int32
	TmMaxCharWidth     int32
	TmWeight           int32
	TmOverhang         int32
	TmDigitizedAspectX int32
	TmDigitizedAspectY int32
	TmFirstChar        uint16
	TmLastChar         uint16
	TmDefaultChar      uint16
	TmBreakChar        uint16
	TmItalic           byte
	TmUnderlined       byte
	TmStruckOut        byte
	TmPitchAndFamily   byte
	TmCharSet          byte
	_                  [3]byte
}

type fontKey struct {
	face   string
	height int32
	weight int32
}

type cachedFont struct {
	hFont  uintptr
	height int
	ascent int
}

var (
	fontResolveOnce sync.Once
	resolvedSans    = "Segoe UI"
	resolvedMono    = "Consolas"
	resolvedCJK     = "Microsoft YaHei UI"

	fontCacheMu sync.Mutex
	fontCache   = map[fontKey]cachedFont{}
)

func resolveFonts() {
	fontResolveOnce.Do(func() {
		hdc, _, _ := procCreateCompatibleDC.Call(0)
		if hdc == 0 {
			return
		}
		defer procDeleteDC.Call(hdc)

		if gdiFontExists(hdc, "Segoe UI Variable Text") {
			resolvedSans = "Segoe UI Variable Text"
		} else {
			resolvedSans = "Segoe UI"
		}
		if gdiFontExists(hdc, "Cascadia Mono") {
			resolvedMono = "Cascadia Mono"
		} else {
			resolvedMono = "Consolas"
		}
		if gdiFontExists(hdc, "Microsoft YaHei UI") {
			resolvedCJK = "Microsoft YaHei UI"
		} else if gdiFontExists(hdc, "Microsoft YaHei") {
			resolvedCJK = "Microsoft YaHei"
		} else {
			resolvedCJK = resolvedSans
		}
	})
}

func gdiFontExists(hdc uintptr, face string) bool {
	hFont := createGDIFont(face, -14, 400)
	if hFont == 0 {
		return false
	}
	defer procDeleteObject.Call(hFont)
	old, _, _ := procSelectObject.Call(hdc, hFont)
	defer procSelectObject.Call(hdc, old)
	var buf [64]uint16
	n, _, _ := procGetTextFaceW.Call(hdc, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	if n == 0 {
		return false
	}
	got := syscall.UTF16ToString(buf[:n])
	return strings.EqualFold(got, face)
}

func createGDIFont(face string, height, weight int32) uintptr {
	var lf logFontW
	lf.LfHeight = height
	lf.LfWeight = weight
	lf.LfCharSet = defaultCharset
	lf.LfOutPrecision = outTTPrecis
	lf.LfClipPrecision = clipDefaultPrecis
	lf.LfQuality = cleartypeQuality
	u16 := utf16.Encode([]rune(face))
	if len(u16) >= len(lf.LfFaceName) {
		u16 = u16[:len(lf.LfFaceName)-1]
	}
	copy(lf.LfFaceName[:], u16)
	hFont, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&lf)))
	return hFont
}

func getCachedFont(hdc uintptr, face string, sizePx float64, weight int) cachedFont {
	h := -int32(math.Round(sizePx))
	if h == 0 {
		h = -12
	}
	w := int32(weight)
	if w <= 0 {
		w = 400
	}
	key := fontKey{face: face, height: h, weight: w}

	fontCacheMu.Lock()
	if cf, ok := fontCache[key]; ok {
		fontCacheMu.Unlock()
		return cf
	}
	fontCacheMu.Unlock()

	hFont := createGDIFont(face, h, w)
	var tm textMetricW
	if hFont != 0 && hdc != 0 {
		old, _, _ := procSelectObject.Call(hdc, hFont)
		_, _, _ = procGetTextMetricsW.Call(hdc, uintptr(unsafe.Pointer(&tm)))
		_, _, _ = procSelectObject.Call(hdc, old)
	}
	cf := cachedFont{
		hFont:  hFont,
		height: int(tm.TmHeight),
		ascent: int(tm.TmAscent),
	}
	if cf.height <= 0 {
		cf.height = int(-h)
	}
	if cf.ascent <= 0 {
		cf.ascent = int(float64(cf.height) * 0.8)
	}

	fontCacheMu.Lock()
	if existing, ok := fontCache[key]; ok {
		fontCacheMu.Unlock()
		if hFont != 0 {
			_, _, _ = procDeleteObject.Call(hFont)
		}
		return existing
	}
	fontCache[key] = cf
	fontCacheMu.Unlock()
	return cf
}

func primaryFace(spec FontSpec) string {
	resolveFonts()
	if spec.Family == FontMono {
		return resolvedMono
	}
	return resolvedSans
}

// MeasureText returns the exact ClearType GDI pixel size of s under spec (with CJK run-splitting).
func MeasureText(s string, spec FontSpec) TextSize {
	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return TextSize{W: len(s) * int(math.Ceil(spec.SizePx*0.55)), H: int(math.Ceil(spec.SizePx * 1.25))}
	}
	defer procDeleteDC.Call(hdc)

	pFont := getCachedFont(hdc, primaryFace(spec), spec.SizePx, spec.Weight)
	if s == "" {
		return TextSize{W: 0, H: pFont.height, Ascent: pFont.ascent}
	}

	runs := splitTextRuns(s)
	totalW := 0
	maxH := pFont.height
	ascent := pFont.ascent
	for _, run := range runs {
		face := primaryFace(spec)
		if run.isCJK {
			face = resolvedCJK
		}
		cf := getCachedFont(hdc, face, spec.SizePx, spec.Weight)
		if cf.hFont == 0 {
			continue
		}
		old, _, _ := procSelectObject.Call(hdc, cf.hFont)
		u16 := utf16.Encode([]rune(run.text))
		if len(u16) > 0 {
			var sz gdiSize
			_, _, _ = procGetTextExtentPoint32.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)), uintptr(unsafe.Pointer(&sz)))
			totalW += int(sz.CX)
		}
		_, _, _ = procSelectObject.Call(hdc, old)
		if cf.height > maxH {
			maxH = cf.height
		}
	}
	return TextSize{W: totalW, H: maxH, Ascent: ascent}
}

// DrawTexts renders all queued TextDraw operations onto c using Windows ClearType GDI
// with CJK run-splitting and opaque-interior alpha=255 fixup for UpdateLayeredWindow.
func DrawTexts(c *Canvas, draws []TextDraw) {
	if c == nil || len(draws) == 0 {
		return
	}
	resolveFonts()

	hdc, _, _ := procCreateCompatibleDC.Call(0)
	if hdc == 0 {
		return
	}
	defer procDeleteDC.Call(hdc)

	var bmi bitmapInfo
	bmi.BmiHeader = bitmapInfoHeader{
		BiSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		BiWidth:       int32(c.Width),
		BiHeight:      -int32(c.Height), // top-down DIB
		BiPlanes:      1,
		BiBitCount:    32,
		BiCompression: biRGB,
	}

	var bitsPtr unsafe.Pointer
	hBmp, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bmi)),
		dibRGBColors,
		uintptr(unsafe.Pointer(&bitsPtr)),
		0,
		0,
	)
	if hBmp == 0 || bitsPtr == nil {
		return
	}
	defer procDeleteObject.Call(hBmp)

	oldBmp, _, _ := procSelectObject.Call(hdc, hBmp)
	defer procSelectObject.Call(hdc, oldBmp)

	dib := unsafe.Slice((*byte)(bitsPtr), len(c.Pix))
	copy(dib, c.Pix)

	_, _, _ = procSetBkMode.Call(hdc, transparentBkMode)

	// Track bounding box of touched pixels for fast opaque-interior alpha=255 fixup.
	dirtyMinX, dirtyMinY := c.Width, c.Height
	dirtyMaxX, dirtyMaxY := 0, 0

	for _, d := range draws {
		if d.Text == "" || d.Color.A == 0 {
			continue
		}
		pFont := getCachedFont(hdc, primaryFace(d.Spec), d.Spec.SizePx, d.Spec.Weight)
		text := d.Text
		if d.MaxW > 0 {
			text = TruncateText(text, d.Spec, d.MaxW)
		}
		if text == "" {
			continue
		}

		// Compute total width if right-aligned.
		startX := d.X
		if d.AlignR {
			w := measureOnDC(hdc, text, d.Spec)
			startX = d.X - w
		}

		colorRef := uintptr(d.Color.R) | (uintptr(d.Color.G) << 8) | (uintptr(d.Color.B) << 16)
		_, _, _ = procSetTextColor.Call(hdc, colorRef)

		clipRight := int32(c.Width)
		if d.MaxW > 0 {
			if d.AlignR {
				clipRight = int32(d.X)
			} else if startX+d.MaxW < c.Width {
				clipRight = int32(startX + d.MaxW)
			}
		}
		clipLeft := int32(startX)
		if d.AlignR && d.MaxW > 0 {
			if int32(d.X-d.MaxW) > clipLeft {
				clipLeft = int32(d.X - d.MaxW)
			}
		}
		if clipLeft < 0 {
			clipLeft = 0
		}

		curX := startX
		runs := splitTextRuns(text)
		for _, run := range runs {
			face := primaryFace(d.Spec)
			if run.isCJK {
				face = resolvedCJK
			}
			cf := getCachedFont(hdc, face, d.Spec.SizePx, d.Spec.Weight)
			if cf.hFont == 0 {
				continue
			}
			u16 := utf16.Encode([]rune(run.text))
			if len(u16) == 0 {
				continue
			}

			oldFont, _, _ := procSelectObject.Call(hdc, cf.hFont)
			var sz gdiSize
			_, _, _ = procGetTextExtentPoint32.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)), uintptr(unsafe.Pointer(&sz)))

			// Baseline-align CJK runs to the primary Latin font baseline.
			runY := d.Y + (pFont.ascent - cf.ascent)
			rc := gdiRect{
				Left:   clipLeft,
				Top:    int32(maxInt(0, runY-2)),
				Right:  clipRight,
				Bottom: int32(minInt(c.Height, runY+int(sz.CY)+4)),
			}
			if d.Clip != nil {
				if d.Clip.Left > rc.Left {
					rc.Left = d.Clip.Left
				}
				if d.Clip.Top > rc.Top {
					rc.Top = d.Clip.Top
				}
				if d.Clip.Right < rc.Right {
					rc.Right = d.Clip.Right
				}
				if d.Clip.Bottom < rc.Bottom {
					rc.Bottom = d.Clip.Bottom
				}
				if rc.Left >= rc.Right || rc.Top >= rc.Bottom {
					_, _, _ = procSelectObject.Call(hdc, oldFont)
					curX += int(sz.CX)
					continue
				}
			}

			_, _, _ = procExtTextOutW.Call(
				hdc,
				uintptr(int32(curX)),
				uintptr(int32(runY)),
				etoClipped,
				uintptr(unsafe.Pointer(&rc)),
				uintptr(unsafe.Pointer(&u16[0])),
				uintptr(len(u16)),
				0,
			)
			_, _, _ = procSelectObject.Call(hdc, oldFont)

			// Update dirty box.
			rx0 := maxInt(int(rc.Left), curX-2)
			ry0 := maxInt(0, int(rc.Top))
			rx1 := minInt(int(rc.Right), curX+int(sz.CX)+2)
			ry1 := minInt(c.Height, int(rc.Bottom))
			if rx0 < dirtyMinX {
				dirtyMinX = rx0
			}
			if ry0 < dirtyMinY {
				dirtyMinY = ry0
			}
			if rx1 > dirtyMaxX {
				dirtyMaxX = rx1
			}
			if ry1 > dirtyMaxY {
				dirtyMaxY = ry1
			}

			curX += int(sz.CX)
		}
	}

	_, _, _ = procGdiFlush.Call()

	// Opaque-interior alpha=255 fixup: GDI clears alpha to 0 on pixels it touches.
	// Every pixel inside the opaque card originally had alpha == 255, so restoring
	// A = 255 preserves exact ClearType subpixel antialiasing for UpdateLayeredWindow.
	if dirtyMinX < dirtyMaxX && dirtyMinY < dirtyMaxY {
		x0 := maxInt(0, dirtyMinX)
		y0 := maxInt(0, dirtyMinY)
		x1 := minInt(c.Width, dirtyMaxX)
		y1 := minInt(c.Height, dirtyMaxY)
		for py := y0; py < y1; py++ {
			rowOff := py * c.Width * 4
			for px := x0; px < x1; px++ {
				i := rowOff + px*4
				if c.Pix[i+3] == 255 {
					c.Pix[i+0] = dib[i+0]
					c.Pix[i+1] = dib[i+1]
					c.Pix[i+2] = dib[i+2]
					c.Pix[i+3] = 255
				}
			}
		}
	}
}

func measureOnDC(hdc uintptr, s string, spec FontSpec) int {
	totalW := 0
	for _, run := range splitTextRuns(s) {
		face := primaryFace(spec)
		if run.isCJK {
			face = resolvedCJK
		}
		cf := getCachedFont(hdc, face, spec.SizePx, spec.Weight)
		if cf.hFont == 0 {
			continue
		}
		u16 := utf16.Encode([]rune(run.text))
		if len(u16) == 0 {
			continue
		}
		oldFont, _, _ := procSelectObject.Call(hdc, cf.hFont)
		var sz gdiSize
		_, _, _ = procGetTextExtentPoint32.Call(hdc, uintptr(unsafe.Pointer(&u16[0])), uintptr(len(u16)), uintptr(unsafe.Pointer(&sz)))
		_, _, _ = procSelectObject.Call(hdc, oldFont)
		totalW += int(sz.CX)
	}
	return totalW
}
