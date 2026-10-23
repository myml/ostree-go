//go:build linux

package cli

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f refers to a terminal, using the same
// isatty(3) semantics as glnx_stdout_is_tty().  It is implemented with
// a raw ioctl so that the binary stays cgo-free.
func isTerminal(f *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TCGETS),
		uintptr(unsafe.Pointer(&termios)),
		0, 0, 0,
	)
	return errno == 0
}
