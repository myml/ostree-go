//go:build linux

package main

import "syscall"

// prSetPdeathsig is PR_SET_PDEATHSIG from <linux/prctl.h>.
const prSetPdeathsig = 1

// setParentDeathSignal makes the non-daemonized server exit when its parent
// does, matching the C helper's prctl(PR_SET_PDEATHSIG, SIGTERM) behavior.
func setParentDeathSignal() {
	_, _, _ = syscall.RawSyscall(syscall.SYS_PRCTL, prSetPdeathsig, uintptr(syscall.SIGTERM), 0)
}

func newSession() error {
	_, err := syscall.Setsid()
	return err
}

func redirectStdioToDevNull(fd int) {
	for _, target := range []int{0, 1, 2} {
		_ = syscall.Dup2(fd, target)
	}
}
