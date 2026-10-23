//go:build !linux

package cli

import "os"

// isTerminal is a stub for non-Linux platforms.  The rewrite targets
// Linux only; this keeps cross-compilation of the package working.
func isTerminal(f *os.File) bool { return false }
