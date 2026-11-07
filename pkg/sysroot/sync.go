package sysroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// FIFREEZE/FITHAW ioctl numbers (linux/fs.h); x/sys/unix does not export them
// on every architecture.
const (
	ioctlFIFREEZE = 0xC0045877
	ioctlFITHAW   = 0xC0045878
)

func ioctlFreeze(fd int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlFIFREEZE, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlThaw(fd int) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlFITHAW, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// syncfs flushes the filesystem containing fd.
func syncfs(fd int) error {
	return unix.Syncfs(fd)
}

// fsfreezeThawCycle freezes and thaws the filesystem of dir, so that the
// bootloader (which cannot replay e.g. the XFS journal) sees consistent data.
func (s *Sysroot) fsfreezeThawCycle(dir string) error {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("During fsfreeze-thaw: %w", err)
	}
	defer unix.Close(fd)

	if s.Debug.TestFifreeze {
		w := s.Stderr
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintln(w, "fifreeze watchdog was run")
		return fmt.Errorf("During fsfreeze-thaw: aborting due to test-fifreeze")
	}
	if err := ioctlFreeze(fd); err != nil {
		if errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.ENOSYS) ||
			errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EBUSY) {
			if err := syncfs(fd); err != nil {
				return fmt.Errorf("During fsfreeze-thaw: syncfs: %w", err)
			}
			return nil
		}
		return fmt.Errorf("During fsfreeze-thaw: ioctl(FIFREEZE): %w", err)
	}
	if err := ioctlThaw(fd); err != nil && !errors.Is(err, syscall.EINVAL) {
		return fmt.Errorf("During fsfreeze-thaw: ioctl(FITHAW): %w", err)
	}
	return nil
}

// fullSystemSync syncs /ostree and /boot, which may be separate filesystems.
func (s *Sysroot) fullSystemSync() error {
	bootDir := filepath.Join(s.Path, "boot")
	if info, err := os.Stat(bootDir); err != nil || !info.IsDir() {
		return fmt.Errorf("Full sync: opening boot/: %w", err)
	}
	if !s.Opts.SkipSync {
		if repoRoot := s.Repo.Root(); repoRoot != nil {
			if f, err := repoRoot.Open("."); err == nil {
				_ = syncfs(int(f.Fd()))
				f.Close()
			}
		}
	}
	if err := s.fsfreezeThawCycle(bootDir); err != nil {
		return fmt.Errorf("Full sync: %w", err)
	}
	return nil
}
