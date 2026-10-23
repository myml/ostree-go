package sysroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"ostree-go/internal/repo"
)

// Linux F_OFD_SETLK/F_OFD_SETLKW.  syscall does not export them on every
// architecture; the values are stable across Linux architectures.
const (
	fOFDSetLK  = 37
	fOFDSetLKW = 38
)

// Lock takes the exclusive multi-process sysroot lock (<sysroot>/ostree/lock),
// mirroring ostree_sysroot_lock().  The returned function releases it.
func (s *Sysroot) Lock() (func(), error) {
	if err := os.MkdirAll(filepath.Join(s.Path, "ostree"), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(s.Path, "ostree", "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o660)
	if err != nil {
		return nil, err
	}
	if err := sysrootLockFD(f.Fd()); err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		_ = sysrootUnlockFD(f.Fd())
		f.Close()
	}, nil
}

// sysrootLockFD takes a blocking exclusive OFD lock, falling back to flock(2)
// when the filesystem does not support OFD locks (like libglnx).
func sysrootLockFD(fd uintptr) error {
	fl := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0, Start: 0, Len: 0}
	err := fcntlRetry(fd, fOFDSetLKW, &fl)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("Locking sysroot: %w", err)
	}
	for {
		err = syscall.Flock(int(fd), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("Locking sysroot: %w", err)
	}
	return nil
}

// sysrootUnlockFD releases the lock taken by sysrootLockFD.
func sysrootUnlockFD(fd uintptr) error {
	fl := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	err := fcntlRetry(fd, fOFDSetLK, &fl)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EINVAL) {
		return err
	}
	for {
		err = syscall.Flock(int(fd), syscall.LOCK_UN)
		if err != syscall.EINTR {
			break
		}
	}
	return err
}

func fcntlRetry(fd uintptr, cmd int, fl *syscall.Flock_t) error {
	for {
		err := syscall.FcntlFlock(fd, cmd, fl)
		if err != syscall.EINTR {
			return err
		}
	}
}

// OpenLocked opens the sysroot at path and acquires its write lock *before*
// loading the deployment list, like the C admin builtins do.  Concurrent admin
// commands are then serialized over read-modify-write sequences.
func OpenLocked(path string) (*Sysroot, func(), error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	s := &Sysroot{
		Path:           abs,
		Debug:          parseDebugFlags(os.Getenv("OSTREE_SYSROOT_DEBUG")),
		Opts:           parseOpts(os.Getenv("OSTREE_SYSROOT_OPTS")),
		BootVersion:    0,
		SubBootVersion: 0,
	}
	unlock, err := s.Lock()
	if err != nil {
		return nil, nil, err
	}
	r, err := repo.Open(filepath.Join(abs, "ostree/repo"))
	if err != nil {
		unlock()
		return nil, nil, fmt.Errorf("Opening sysroot repo: %w", err)
	}
	s.Repo = r
	err = s.Load()
	if err != nil {
		unlock()
		return nil, nil, err
	}
	return s, unlock, nil
}
