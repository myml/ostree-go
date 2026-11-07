package repo

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func lockPathFor(r *Repo) string { return filepath.Join(r.Path(), ".lock") }

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestLockFileIsLazyAndHasMode0644(t *testing.T) {
	withUmask(t, 0)
	r := newTestRepo(t, ModeBare)
	if _, err := os.Stat(lockPathFor(r)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".lock should not exist after Open, stat err = %v", err)
	}
	unlock, err := r.Lock(false)
	if err != nil {
		t.Fatalf("Lock(shared): %v", err)
	}
	st, err := os.Stat(lockPathFor(r))
	if err != nil {
		t.Fatalf(".lock was not created: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf(".lock perm = %o, want 0644 (umask 0)", perm)
	}
	unlock()
	// The lock file is kept for reuse after unlocking.
	if _, err := os.Stat(lockPathFor(r)); err != nil {
		t.Errorf(".lock disappeared after unlock: %v", err)
	}
	unlock() // idempotent
}

func TestLockMutualExclusionBetweenRepoInstances(t *testing.T) {
	dir := newTestRepo(t, ModeBare).Path()
	r1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	un1, err := r1.Lock(true)
	if err != nil {
		t.Fatalf("exclusive lock: %v", err)
	}
	if _, err := r2.TryLock(true); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("TryLock(exclusive) while exclusive held = %v, want ErrLockWouldBlock", err)
	}
	if _, err := r2.TryLock(false); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("TryLock(shared) while exclusive held = %v, want ErrLockWouldBlock", err)
	}
	un1()

	// Shared locks are compatible with each other but block exclusive ones.
	shared1, err := r1.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	shared2, err := r2.TryLock(false)
	if err != nil {
		t.Fatalf("second shared lock: %v", err)
	}
	if _, err := r2.TryLock(true); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("TryLock(exclusive) while shared held = %v, want ErrLockWouldBlock", err)
	}
	shared2()
	shared1()

	un2, err := r2.TryLock(true)
	if err != nil {
		t.Fatalf("exclusive lock after releases: %v", err)
	}
	un2()
}

func TestLockRecursiveAndDowngrade(t *testing.T) {
	dir := newTestRepo(t, ModeBare).Path()
	r1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	shared1, err := r1.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	shared2, err := r1.Lock(false)
	if err != nil {
		t.Fatal(err)
	}
	compatible, err := r2.TryLock(false)
	if err != nil {
		t.Fatalf("shared lock should be compatible while shared is held: %v", err)
	}
	compatible()
	exclusive, err := r1.Lock(true) // upgrade shared -> exclusive
	if err != nil {
		t.Fatalf("upgrade to exclusive: %v", err)
	}
	if _, err := r2.TryLock(false); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("shared lock while exclusive held = %v, want ErrLockWouldBlock", err)
	}
	// Releasing the exclusive lock downgrades to the still-held shared lock.
	exclusive()
	if _, err := r2.TryLock(true); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("exclusive lock after downgrade = %v, want ErrLockWouldBlock", err)
	}
	sharedOther, err := r2.TryLock(false)
	if err != nil {
		t.Fatalf("shared lock after downgrade: %v", err)
	}
	sharedOther()
	shared2()
	shared1()

	unlock, err := r2.TryLock(true)
	if err != nil {
		t.Fatalf("exclusive after all shared released: %v", err)
	}
	unlock()
}

func TestLockDisabledByConfig(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	cfg := r.CloneConfig()
	cfg.Set("core", "locking", "false")
	if err := r.WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	unlock, err := r.Lock(true)
	if err != nil {
		t.Fatalf("Lock with locking=false: %v", err)
	}
	unlock()
	if _, err := r.TryLock(true); err != nil {
		t.Errorf("TryLock with locking=false: %v", err)
	}
	if _, err := os.Stat(lockPathFor(r)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("locking=false must not create .lock, stat err = %v", err)
	}
}

func TestLockTimeout(t *testing.T) {
	dir := newTestRepo(t, ModeBare).Path()
	r1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	cfg := r1.CloneConfig()
	cfg.Set("core", "lock-timeout-secs", "0")
	if err := r1.WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()

	unlock, err := r1.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start := time.Now()
	_, err = r2.Lock(true)
	if !errors.Is(err, ErrLockTimeout) || !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("Lock with timeout 0 error = %v, want ErrLockTimeout and ErrLockWouldBlock", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v, expected an immediate failure", elapsed)
	}
}

// TestLockHelperProcess is not a real test: it is the child half of
// TestLockAcrossProcesses.
func TestLockHelperProcess(t *testing.T) {
	repoPath := os.Getenv("OSTREE_GO_LOCK_HELPER_REPO")
	if repoPath == "" {
		t.Skip("helper process only")
	}
	r, err := Open(repoPath)
	if err != nil {
		t.Fatalf("helper Open: %v", err)
	}
	defer r.Close()
	unlock, err := r.Lock(true)
	if err != nil {
		t.Fatalf("helper Lock: %v", err)
	}
	defer unlock()
	if marker := os.Getenv("OSTREE_GO_LOCK_HELPER_MARKER"); marker != "" {
		if err := os.WriteFile(marker, []byte("locked"), 0o600); err != nil {
			t.Fatalf("helper marker: %v", err)
		}
	}
	time.Sleep(500 * time.Millisecond)
}

func TestLockAcrossProcesses(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	marker := filepath.Join(t.TempDir(), "locked")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHelperProcess$", "-test.v=false")
	cmd.Env = append(os.Environ(),
		"OSTREE_GO_LOCK_HELPER_REPO="+r.Path(),
		"OSTREE_GO_LOCK_HELPER_MARKER="+marker,
	)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waitForFile(t, marker, 10*time.Second)
	if _, err := r.TryLock(true); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("TryLock while another process holds exclusive = %v, want ErrLockWouldBlock", err)
	}
	if _, err := r.TryLock(false); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("shared TryLock while another process holds exclusive = %v, want ErrLockWouldBlock", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr.String())
	}
	unlock, err := r.TryLock(true)
	if err != nil {
		t.Fatalf("lock after helper exited: %v", err)
	}
	unlock()
}

const cLockHelperSource = `#define _GNU_SOURCE
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <errno.h>
#include <unistd.h>

int main(int argc, char **argv) {
    if (argc < 5) return 2;
    int fd = open(argv[1], O_CREAT | O_RDWR, 0644);
    if (fd < 0) { perror("open"); return 1; }
    struct flock fl;
    memset(&fl, 0, sizeof(fl));
    fl.l_type = F_WRLCK;
    fl.l_whence = SEEK_SET;
    int cmd = atoi(argv[2]) == 0 ? F_OFD_SETLK : F_OFD_SETLKW;
    if (fcntl(fd, cmd, &fl) < 0) {
        fprintf(stderr, "fcntl: %s\n", strerror(errno));
        return 1;
    }
    FILE *marker = fopen(argv[4], "w");
    if (marker) { fputs("locked", marker); fclose(marker); }
    int hold = atoi(argv[3]);
    if (hold > 0) sleep(hold);
    printf("LOCKED\n");
    fflush(stdout);
    return 0;
}
`

func buildCLockHelper(t *testing.T) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		cc, err = exec.LookPath("gcc")
		if err != nil {
			t.Skip("no C compiler available for OFD lock interop test")
		}
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "lockhelper.c")
	if err := os.WriteFile(src, []byte(cLockHelperSource), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "lockhelper")
	cmd := exec.Command(cc, "-O0", "-o", exe, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("C lock helper did not compile (%v): %s", err, out)
	}
	return exe
}

// TestLockInteropWithCOFD checks that a C program using F_OFD_SETLK and the Go
// implementation are mutually exclusive. This is the property that a naive
// flock(2)-only implementation would violate.
func TestLockInteropWithCOFD(t *testing.T) {
	exe := buildCLockHelper(t)
	r := newTestRepo(t, ModeBare)
	lockPath := lockPathFor(r)

	// C holds an exclusive OFD lock; Go must not be able to acquire either
	// kind of lock.
	marker := filepath.Join(t.TempDir(), "c-locked")
	cmd := exec.Command(exe, lockPath, "1", "2", marker)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start C helper: %v", err)
	}
	waitForFile(t, marker, 10*time.Second)
	if _, err := r.TryLock(true); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("Go exclusive lock while C holds OFD lock = %v, want ErrLockWouldBlock", err)
	}
	if _, err := r.TryLock(false); !errors.Is(err, ErrLockWouldBlock) {
		t.Errorf("Go shared lock while C holds OFD lock = %v, want ErrLockWouldBlock", err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	// Go holds an exclusive lock; C's non-blocking OFD lock must fail.
	unlock, err := r.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	nonblocking := exec.Command(exe, lockPath, "0", "0", filepath.Join(t.TempDir(), "never"))
	out, err := nonblocking.CombinedOutput()
	if err == nil {
		t.Fatalf("C acquired an OFD lock while Go held one; output=%q", out)
	}
	if !strings.Contains(string(out), "fcntl") {
		t.Logf("C helper output: %q", out)
	}
	unlock()

	// Once Go releases, C can take the lock.
	after := exec.Command(exe, lockPath, "0", "0", filepath.Join(t.TempDir(), "c-after"))
	if out, err := after.CombinedOutput(); err != nil {
		t.Errorf("C could not lock after Go released: %v (%q)", err, out)
	}
}

func TestLockConcurrentGoroutines(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := r.Lock(false)
			if err != nil {
				t.Errorf("concurrent shared lock: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
			unlock()
		}()
	}
	wg.Wait()
	if _, err := r.TryLock(true); err != nil {
		t.Errorf("exclusive lock after concurrent shared locks: %v", err)
	}
}
