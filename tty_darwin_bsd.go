//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// stdinIsTerminal reports whether stdin is an interactive terminal, via the
// TIOCGETA ioctl.
//
// Deliberately not the os.ModeCharDevice heuristic: /dev/null is a character
// device too, so that check mis-classifies the exact non-interactive cases
// this guard exists for (agent shells, CI, `< /dev/null`) as a TTY — only the
// ioctl distinguishes them (issue #5).
func stdinIsTerminal() bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TIOCGETA,
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}
