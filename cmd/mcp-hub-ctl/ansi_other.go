//go:build !windows

package main

import (
	"io"
	"os"
	"strconv"
	"strings"
)

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func enableVirtualTerminal(w io.Writer) bool {
	return isTerminalWriter(w)
}

func terminalWidth(w io.Writer) int {
	if !isTerminalWriter(w) {
		return 0
	}
	if raw := strings.TrimSpace(os.Getenv("COLUMNS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return 0
}
