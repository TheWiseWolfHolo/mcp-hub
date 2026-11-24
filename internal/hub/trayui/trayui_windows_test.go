//go:build windows

package trayui

import (
	"testing"
	"unsafe"
)

func TestGDI_StructSizes(t *testing.T) {
	if got := unsafe.Sizeof(logFontW{}); got != 92 {
		t.Fatalf("unsafe.Sizeof(logFontW) = %d, want 92", got)
	}
	if got := unsafe.Sizeof(textMetricW{}); got != 60 {
		t.Fatalf("unsafe.Sizeof(textMetricW) = %d, want 60", got)
	}
	if got := unsafe.Sizeof(bitmapInfoHeader{}); got != 40 {
		t.Fatalf("unsafe.Sizeof(bitmapInfoHeader) = %d, want 40", got)
	}
}

func TestText_CJKRunSplittingAndMeasure(t *testing.T) {
	spec := FontSpec{Family: FontSans, SizePx: 14, Weight: 400}
	sz := MeasureText("McpHub 本地服务状态", spec)
	if sz.W < 80 || sz.H < 12 {
		t.Fatalf("MeasureText mixed CJK/Latin returned unexpected size: %+v", sz)
	}
	runs := splitTextRuns("McpHub 本地服务 v0.3.0")
	if len(runs) != 3 || runs[0].isCJK || !runs[1].isCJK || runs[2].isCJK {
		t.Fatalf("unexpected CJK runs: %+v", runs)
	}
}
