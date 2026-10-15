//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

var kernel32GetConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleMode")

// stdinIsTerminal reports whether stdin is a console handle. Pipes and NUL
// are not consoles, so non-interactive callers (agents, CI, redirections)
// correctly report false (issue #5).
func stdinIsTerminal() bool {
	var mode uint32
	ret, _, _ := kernel32GetConsoleMode.Call(os.Stdin.Fd(), uintptr(unsafe.Pointer(&mode)))
	return ret != 0
}
