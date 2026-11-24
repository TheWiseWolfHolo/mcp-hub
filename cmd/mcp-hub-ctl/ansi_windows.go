//go:build windows

package main

import (
	"io"
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32DLL                    = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32DLL.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32DLL.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32DLL.NewProc("GetConsoleScreenBufferInfo")
)

const enableVirtualTerminalProcessing = 0x0004

type winCoord struct {
	X int16
	Y int16
}

type winSmallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type winConsoleScreenBufferInfo struct {
	Size              winCoord
	CursorPosition    winCoord
	Attributes        uint16
	Window            winSmallRect
	MaximumWindowSize winCoord
}

func consoleHandle(w io.Writer) (syscall.Handle, bool) {
	f, ok := w.(*os.File)
	if !ok {
		return 0, false
	}
	h := syscall.Handle(f.Fd())
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return 0, false
	}
	return h, true
}

func isTerminalWriter(w io.Writer) bool {
	_, ok := consoleHandle(w)
	return ok
}

func enableVirtualTerminal(w io.Writer) bool {
	h, ok := consoleHandle(w)
	if !ok {
		return false
	}
	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ = procSetConsoleMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}

func terminalWidth(w io.Writer) int {
	h, ok := consoleHandle(w)
	if !ok {
		return 0
	}
	var info winConsoleScreenBufferInfo
	r, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		return 0
	}
	width := int(info.Window.Right-info.Window.Left) + 1
	if width <= 0 {
		return 0
	}
	return width
}
