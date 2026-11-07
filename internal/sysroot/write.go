package sysroot

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/myml/ostree-go/internal/bootconfig"
	"github.com/myml/ostree-go/internal/repo"
)

// SimpleWriteDeploymentFlags mirror OstreeSysrootSimpleWriteDeploymentFlags.
type SimpleWriteDeploymentFlags uint

const (
	// SimpleWriteRetain keeps every existing deployment.
	SimpleWriteRetain SimpleWriteDeploymentFlags = 1 << iota
	// SimpleWriteRetainPending keeps pending deployments.
	SimpleWriteRetainPending
	// SimpleWriteRetainRollback keeps rollback deployments.
	SimpleWriteRetainRollback
	// SimpleWriteNotDefault appends after the booted/merge deployment
	// instead of prepending.
	SimpleWriteNotDefault
	// SimpleWriteNoClean skips the post-deployment cleanup.
	SimpleWriteNoClean
)

// Clone returns a copy of d with an independent bootconfig and origin.
func (d *Deployment) Clone() *Deployment {
	out := *d
	if d.BootConfig != nil {
		out.BootConfig = d.BootConfig.Clone()
	}
	if d.Origin != nil {
		out.Origin = d.Origin.Clone()
	}
	out.OverlayInitrds = append([]string(nil), d.OverlayInitrds...)
	return &out
}

// SimpleWriteDeployment prepends newDeployment to the deployment list, writes
// it out and (unless SimpleWriteNoClean) cleans up.
func (s *Sysroot) SimpleWriteDeployment(osname string, newDeployment, mergeDeployment *Deployment,
	flags SimpleWriteDeploymentFlags) error {
	postclean := flags&SimpleWriteNoClean == 0
	makeDefault := flags&SimpleWriteNotDefault == 0
	retainPending := flags&SimpleWriteRetainPending != 0
	retainRollback := flags&SimpleWriteRetainRollback != 0
	retain := flags&SimpleWriteRetain != 0

	if osname == "" && s.Booted != nil {
		osname = s.Booted.OsName
	}

	addedNew := false
	newDeployments := make([]*Deployment, 0, len(s.Deployments)+1)
	if makeDefault {
		newDeployments = append(newDeployments, newDeployment)
		addedNew = true
	}

	// Without a booted and a merge deployment, retain_pending/rollback
	// become meaningless; just retain all deployments in that case.
	if s.Booted == nil && mergeDeployment == nil && (retainPending || retainRollback) {
		retain = true
	}

	beforeBooted := true
	beforeMerge := true
	for _, d := range s.Deployments {
		osnameMatches := osname == "" || d.OsName == osname
		isBooted := d.Equal(s.Booted)
		isMerge := d.Equal(mergeDeployment)
		if isBooted {
			beforeBooted = false
		}
		if isMerge {
			beforeMerge = false
		}
		passedCrossover := !beforeMerge
		if s.Booted != nil {
			passedCrossover = !beforeBooted
		}
		if retain || d.IsPinned() || !osnameMatches ||
			(retainPending && !passedCrossover) || isBooted || isMerge ||
			(retainRollback && passedCrossover) {
			newDeployments = append(newDeployments, d)
		}
		if !addedNew && passedCrossover {
			newDeployments = append(newDeployments, newDeployment)
			addedNew = true
		}
	}
	if !addedNew {
		newDeployments = append(newDeployments, newDeployment)
	}

	return s.WriteDeployments(newDeployments, WriteDeploymentsOptions{DoPostclean: postclean})
}

// PrepareCleanup removes stale deployments, boot versions and bootfs data
// without pruning the repository.
func (s *Sysroot) PrepareCleanup() error {
	return s.Cleanup(false)
}

// bootloaderEntryFilename mirrors bootloader_entry_filename().
func (s *Sysroot) bootloaderEntryFilename(nDeployments int, d *Deployment) string {
	return bootconfig.EntryFilename("ostree", nDeployments, d.Index,
		s.bootCountingTries(), d.BootConfig)
}

// SetDeploymentKargs replaces a deployment's kernel arguments by rewriting its
// bootloader entry in place (ostree_sysroot_deployment_set_kargs_in_place).
func (s *Sysroot) SetDeploymentKargs(d *Deployment, kargsStr string) error {
	if d.BootConfig == nil {
		d.BootConfig = bootconfig.New()
	}
	d.BootConfig.Set("options", kargsStr)
	name := s.bootloaderEntryFilename(len(s.Deployments), d)
	entriesDir := filepath.Join(s.Path, fmt.Sprintf("boot/loader.%d/entries", s.BootVersion))
	d.BootConfig.Filename = name
	if err := d.BootConfig.WriteFile(filepath.Join(entriesDir, name)); err != nil {
		return err
	}
	return s.BumpMtime()
}

// RewriteDeploymentKargs clones d with new kernel arguments and writes the
// deployment list (ostree_sysroot_deployment_set_kargs).
func (s *Sysroot) RewriteDeploymentKargs(d *Deployment, newKargs []string) error {
	newDeployment := d.Clone()
	if newDeployment.BootConfig == nil {
		newDeployment.BootConfig = bootconfig.New()
	}
	kargs := bootconfig.NewKernelArgs()
	kargs.AppendArgv(newKargs)
	newDeployment.BootConfig.Set("options", kargs.String())

	newDeployments := make([]*Deployment, 0, len(s.Deployments))
	for _, cur := range s.Deployments {
		if cur.Equal(d) {
			newDeployments = append(newDeployments, newDeployment)
		} else {
			newDeployments = append(newDeployments, cur)
		}
	}
	return s.WriteDeployments(newDeployments, WriteDeploymentsOptions{DoPostclean: true})
}

// StageOverlayInitrd copies an overlay initramfs into /run/ostree/staged-initrds
// and returns its sha256.
func (s *Sysroot) StageOverlayInitrd(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	dir := "/run/ostree/staged-initrds"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-overlay-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	csum := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmpName, filepath.Join(dir, csum)); err != nil {
		return "", err
	}
	return csum, nil
}

// installOverlayInitrds copies staged overlay initrds into
// /boot/ostree/initramfs-overlays.
func (s *Sysroot) installOverlayInitrds(d *Deployment) error {
	if len(d.OverlayInitrds) == 0 {
		return nil
	}
	dir := filepath.Join(s.Path, "boot", BootInitramfsOverlays)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, csum := range d.OverlayInitrds {
		dest := filepath.Join(dir, csum+".img")
		if _, err := os.Lstat(dest); err == nil {
			continue
		}
		src := filepath.Join("/run/ostree/staged-initrds", csum)
		if _, err := os.Lstat(src); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if err := installIntoBoot(src, dest); err != nil {
			return err
		}
	}
	return nil
}

// stagedDeploymentRunstatePath is the runstate dir of d.
func stagedDeploymentRunstatePath(d *Deployment) string {
	return filepath.Join(DeploymentRunstateDir, fmt.Sprintf("%s.%d", d.Checksum, d.DeploySerial))
}

// GetStagedDeployment returns the staged deployment, or nil.  The staged
// deployment state lives in /run/ostree/staged-deployment and is only
// meaningful for a booted sysroot.
func (s *Sysroot) GetStagedDeployment() *Deployment {
	return nil
}

// ChangeFinalization flips the finalization lock of the staged deployment.
func (s *Sysroot) ChangeFinalization(staged *Deployment) error {
	if staged == nil {
		return fmt.Errorf("No staged deployment")
	}
	return fmt.Errorf("Changing the finalization state is not supported yet")
}

// UnlockedStateFromString maps a state name to the enum value.
func UnlockedStateFromString(s string) (string, error) {
	switch s {
	case "none", "hotfix", "development", "transient":
		return s, nil
	}
	return "", fmt.Errorf("Invalid unlocked state: %s", s)
}

// Unlock makes d writable in the given unlocked state.
func (s *Sysroot) Unlock(d *Deployment, state string) error {
	if state == UnlockedNone {
		return fmt.Errorf("Invalid unlocked state")
	}
	if d.UnlockedState != "" && d.UnlockedState != UnlockedNone {
		return fmt.Errorf("Deployment is already in unlocked state: %s", d.UnlockedState)
	}
	merge := s.GetMergeDeployment(d.OsName)
	if merge == nil {
		return fmt.Errorf("No previous deployment to duplicate")
	}
	deployDir := d.DirPath(s.Path)
	usrMode := os.FileMode(0o755)
	if info, err := os.Stat(filepath.Join(deployDir, "usr")); err == nil {
		usrMode = info.Mode().Perm()
	}

	switch state {
	case "hotfix":
		// Push a rollback target: deploy a copy of the deployment.
		origin := d.Origin
		kargs := ""
		if merge.BootConfig != nil {
			kargs = merge.BootConfig.Get("options")
		}
		keys := bootconfig.FromString(kargs).Strv()
		newDeployment, err := s.DeployTree(d.OsName, d.Checksum, origin, merge,
			&DeployOptions{OverrideKernelArgs: keys, OverrideKernelArgsSet: true})
		if err != nil {
			return err
		}
		if err := s.SimpleWriteDeployment(d.OsName, newDeployment, merge, SimpleWriteNotDefault); err != nil {
			return err
		}
		// Create the overlayfs directories in the deployment root.
		for _, name := range []string{".usr-ovl-upper", ".usr-ovl-work"} {
			path := filepath.Join(deployDir, name)
			if err := os.MkdirAll(path, usrMode); err != nil {
				return err
			}
			if err := os.Chmod(path, usrMode); err != nil {
				return err
			}
		}
		cfg := d.Origin
		if cfg == nil {
			cfg = repo.NewConfig()
		}
		cfg.Set("origin", "unlocked", "hotfix")
		if err := s.WriteOriginFileAndBump(d, cfg); err != nil {
			return err
		}
		d.Origin = cfg
		d.UnlockedState = "hotfix"
		return nil

	case "development", "transient":
		flag := RunstateFlagDevelopment
		if state == "transient" {
			flag = RunstateFlagTransient
		}
		dir := stagedDeploymentRunstatePath(d)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, flag), nil, 0o644); err != nil {
			return err
		}
		backing := filepath.Join(s.Path, d.BackingRelPath(), "usr-transient")
		_ = os.RemoveAll(backing)
		for _, sub := range []string{"", "upper", "work"} {
			path := filepath.Join(backing, sub)
			if err := os.MkdirAll(path, usrMode); err != nil {
				return err
			}
			if err := os.Chmod(path, usrMode); err != nil {
				return err
			}
		}
		d.UnlockedState = state
		return nil
	}
	return fmt.Errorf("Invalid unlocked state: %s", state)
}

// LockedStateStrings maps unlocked states for status output.
func UnlockedStateString(state string) string {
	switch state {
	case "hotfix":
		return "hotfix"
	case "development":
		return "development"
	case "transient":
		return "transient"
	}
	return "none"
}
