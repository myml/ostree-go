package repo

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// Linux fcntl commands for open file description locks. Go's syscall package
// does not export these on every architecture, but the values are part of the
// kernel UAPI (F_OFD_GETLK=36, F_OFD_SETLK=37, F_OFD_SETLKW=38).
const (
	fOFDGetLK  = 36
	fOFDSetLK  = 37
	fOFDSetLKW = 38
)

const (
	lockStateUnlocked = iota
	lockStateShared
	lockStateExclusive
)

// Lock takes a shared or exclusive repository lock. It honours
// core.locking=false (no-op), core.lock-timeout-secs (-1 blocking, >=0 poll
// once per second, default 300) and read-only repositories (no-op), matching
// ostree_repo_lock_push().
//
// The returned function releases the lock and is safe to call more than once.
// Locks are recursive within a Repo: taking a shared lock twice and releasing
// it twice is fine, and an exclusive lock upgrades a held shared lock.
//
// The on-disk strategy is the same as libostree's: F_OFD_SETLK/F_OFD_SETLKW,
// falling back to flock(2) only when the kernel returns EINVAL. Using just
// flock(2) would not be mutually exclusive with the C implementation.
func (r *Repo) Lock(exclusive bool) (func(), error) {
	return r.lock(exclusive, false)
}

// TryLock is like Lock but never waits: it returns ErrLockWouldBlock if an
// incompatible lock is held.
func (r *Repo) TryLock(exclusive bool) (func(), error) {
	return r.lock(exclusive, true)
}

func (r *Repo) lock(exclusive, nonblocking bool) (func(), error) {
	if !r.writable || r.lockMode == lockDisabled {
		return func() {}, nil
	}
	if nonblocking || r.lockMode == lockBlocking {
		r.lockMu.Lock()
		defer r.lockMu.Unlock()
		if err := r.pushLockLocked(exclusive, !nonblocking); err != nil {
			return nil, err
		}
		return r.unlockFunc(exclusive), nil
	}

	// Poll once a second up to lock-timeout-secs, like libostree. The mutex is
	// dropped while sleeping so unlock calls from other goroutines can
	// proceed.
	deadline := time.Now().Add(time.Duration(r.lockMode) * time.Second)
	for {
		r.lockMu.Lock()
		err := r.pushLockLocked(exclusive, false)
		r.lockMu.Unlock()
		if err == nil {
			return r.unlockFunc(exclusive), nil
		}
		if !errors.Is(err, ErrLockWouldBlock) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %w: %s", ErrLockTimeout, ErrLockWouldBlock, r.path)
		}
		time.Sleep(time.Second)
	}
}

func (r *Repo) unlockFunc(exclusive bool) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			r.lockMu.Lock()
			defer r.lockMu.Unlock()
			if r.lockFile == nil {
				return
			}
			// The unlock signature cannot return an error. A failed unlock is
			// recovered when the fd is closed by Close.
			_ = r.popLockLocked(exclusive)
		})
	}
}

func (r *Repo) lockStateLocked() int {
	switch {
	case r.lockShared+r.lockExclusive == 0:
		return lockStateUnlocked
	case r.lockExclusive > 0:
		return lockStateExclusive
	default:
		return lockStateShared
	}
}

func (r *Repo) ensureLockFileLocked() (*os.File, error) {
	if r.lockFile != nil {
		return r.lockFile, nil
	}
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	f, err := r.root.OpenFile(".lock", os.O_CREATE|os.O_RDWR, fileMode)
	if err != nil {
		return nil, fmt.Errorf("opening lock file %s/.lock failed: %w", r.path, err)
	}
	r.lockFile = f
	return f, nil
}

func (r *Repo) pushLockLocked(exclusive, blocking bool) error {
	f, err := r.ensureLockFileLocked()
	if err != nil {
		return err
	}
	want := lockStateShared
	if exclusive {
		want = lockStateExclusive
	}
	state := r.lockStateLocked()
	if state == lockStateExclusive || state == want {
		// Already at least as strong as requested; only the counters change.
	} else {
		if err := lockFD(f.Fd(), exclusive, blocking); err != nil {
			return err
		}
	}
	if exclusive {
		r.lockExclusive++
	} else {
		r.lockShared++
	}
	return nil
}

// popLockLocked mirrors pop_repo_lock() in libostree: it downgrades exclusive
// to shared when the last exclusive lock is released while shared locks remain.
func (r *Repo) popLockLocked(exclusive bool) error {
	depth := r.lockShared + r.lockExclusive
	if depth == 0 {
		return errors.New("cannot pop an unlocked repository lock")
	}
	var counter *int
	if exclusive {
		counter = &r.lockExclusive
	} else {
		counter = &r.lockShared
	}
	if *counter == 0 {
		return fmt.Errorf("repository %s lock pop requested, but none have been taken", r.path)
	}

	state := r.lockStateLocked()
	next := lockStateUnlocked
	if depth > 1 {
		if exclusive {
			if r.lockExclusive > 1 {
				next = lockStateExclusive
			} else {
				next = lockStateShared
			}
		} else if r.lockExclusive > 0 {
			next = lockStateExclusive
		} else {
			next = lockStateShared
		}
	}

	f := r.lockFile
	if f == nil {
		return errors.New("repository lock file is not open")
	}
	switch {
	case next == lockStateUnlocked:
		if err := unlockFD(f.Fd()); err != nil {
			return fmt.Errorf("unlocking repo failed: %w", err)
		}
	case state == next:
		// Maintain the current kernel lock state.
	default:
		// Exclusive -> shared. libostree never downgrades in any other
		// direction.
		if err := lockFD(f.Fd(), false, true); err != nil {
			return fmt.Errorf("setting repo lock to shared failed: %w", err)
		}
	}
	*counter--
	return nil
}

func retryFcntl(fd uintptr, cmd int, fl *syscall.Flock_t) error {
	for {
		err := syscall.FcntlFlock(fd, cmd, fl)
		if err != syscall.EINTR {
			return err
		}
	}
}

func lockFD(fd uintptr, exclusive, blocking bool) error {
	typ := int16(syscall.F_RDLCK)
	if exclusive {
		typ = syscall.F_WRLCK
	}
	cmd := fOFDSetLK
	if blocking {
		cmd = fOFDSetLKW
	}
	fl := syscall.Flock_t{Type: typ, Whence: 0, Start: 0, Len: 0}
	err := retryFcntl(fd, cmd, &fl)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EINVAL) {
		// Fallback for kernels/filesystems without OFD locks, exactly as
		// libostree does.
		how := syscall.LOCK_SH
		if exclusive {
			how = syscall.LOCK_EX
		}
		if !blocking {
			how |= syscall.LOCK_NB
		}
		for {
			err = syscall.Flock(int(fd), how)
			if err != syscall.EINTR {
				break
			}
		}
		if err == nil {
			return nil
		}
	}
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EWOULDBLOCK) {
		return fmt.Errorf("%w: %v", ErrLockWouldBlock, err)
	}
	return fmt.Errorf("locking repo failed: %w", err)
}

func unlockFD(fd uintptr) error {
	fl := syscall.Flock_t{Type: syscall.F_UNLCK, Whence: 0, Start: 0, Len: 0}
	err := retryFcntl(fd, fOFDSetLK, &fl)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EINVAL) {
		for {
			err = syscall.Flock(int(fd), syscall.LOCK_UN)
			if err != syscall.EINTR {
				break
			}
		}
		return err
	}
	return err
}
