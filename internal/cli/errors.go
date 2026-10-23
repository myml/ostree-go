package cli

import (
	"errors"
	"fmt"
)

// ErrNotImplemented is returned by commands that are registered (so that
// help and dispatch work) but whose implementation lands in a later
// layer.
var ErrNotImplemented = errors.New("not implemented")

// printError writes an "error: MESSAGE" line to stderr.  When stdout is
// a terminal the "error:" marker is colored red and bold, matching the C
// CLI (the tty check is done on *stdout*, not stderr).
func (a *App) printError(message string) {
	if a.colorEnabled() {
		fmt.Fprintf(a.Stderr, "\x1b[31m\x1b[1merror:\x1b[0m\x1b[22m %s\n", message)
		return
	}
	fmt.Fprintf(a.Stderr, "error: %s\n", message)
}

// colorEnabled mirrors glnx_stdout_is_tty() based color output.
func (a *App) colorEnabled() bool {
	if a.StdoutIsTTY == nil {
		return false
	}
	return a.StdoutIsTTY(a.Stdout)
}
