package sysroot

import (
	"fmt"
	"os"
	"path/filepath"
)

// InitFsOptions mirror ot-admin-builtin-init-fs.c.
type InitFsOptions struct {
	// Modern is --modern (equivalent to --epoch=1).
	Modern bool
	// Epoch selects the initial state layout: 0 (traditional), 1 (just
	// /boot and /ostree) or 2 (/ostree is 0700).
	Epoch int
}

// InitFs implements `ostree admin init-fs`: it creates the physical sysroot
// skeleton and the empty repository.
func InitFs(path string, opts InitFsOptions) error {
	if opts.Epoch < 0 {
		return fmt.Errorf("Invalid epoch: %d", opts.Epoch)
	}
	if opts.Modern && opts.Epoch == 0 {
		opts.Epoch = 1
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	mkdir := func(rel string, mode os.FileMode) error {
		if err := os.MkdirAll(filepath.Join(abs, rel), mode); err != nil {
			return err
		}
		return nil
	}
	// It's common to want to mount this outside of a deployment as well.
	if err := mkdir("boot", 0o755); err != nil {
		return err
	}
	switch opts.Epoch {
	case 0:
		for _, name := range []string{"boot", "dev", "home", "proc", "run", "sys"} {
			if err := mkdir(name, 0o755); err != nil {
				return err
			}
		}
		if err := mkdir("root", 0o700); err != nil {
			return err
		}
		if err := mkdir("tmp", 0o1777); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(abs, "tmp"), 0o1777); err != nil {
			return fmt.Errorf("chmod: tmp: %w", err)
		}
	case 2:
		// In epoch 2, ostree is 0700 - no access from unprivileged code.
		if err := mkdir("ostree", 0o700); err != nil {
			return err
		}
		if err := os.Chmod(filepath.Join(abs, "ostree"), 0o700); err != nil {
			return fmt.Errorf("chmod: ostree: %w", err)
		}
	}

	s := &Sysroot{Path: abs}
	return s.EnsureInitialized()
}
