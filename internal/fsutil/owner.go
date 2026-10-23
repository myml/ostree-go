// Package fsutil holds small filesystem helpers shared across packages.
package fsutil

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// Owner returns the uid and gid of a file. os.FileInfo.Sys() is a
// *syscall.Stat_t on Linux while x/sys/unix uses its own Stat_t, so both are
// accepted.
func Owner(fi os.FileInfo) (uint32, uint32) {
	switch st := fi.Sys().(type) {
	case *syscall.Stat_t:
		return st.Uid, st.Gid
	case *unix.Stat_t:
		return st.Uid, st.Gid
	default:
		return 0, 0
	}
}

// UnixMode converts an os.FileMode into the raw Unix mode bits used by OSTree
// (including setuid/setgid/sticky).
func UnixMode(m os.FileMode) uint32 {
	bits := uint32(m.Perm())
	if m&os.ModeSetuid != 0 {
		bits |= 04000
	}
	if m&os.ModeSetgid != 0 {
		bits |= 02000
	}
	if m&os.ModeSticky != 0 {
		bits |= 01000
	}
	return bits
}

// FileMode converts raw Unix mode bits into an os.FileMode that os.Chmod
// understands (which needs ModeSetuid/ModeSetgid/ModeSticky, not the raw
// bits).
func FileMode(bits uint32) os.FileMode {
	fm := os.FileMode(bits & 0777)
	if bits&04000 != 0 {
		fm |= os.ModeSetuid
	}
	if bits&02000 != 0 {
		fm |= os.ModeSetgid
	}
	if bits&01000 != 0 {
		fm |= os.ModeSticky
	}
	return fm
}
