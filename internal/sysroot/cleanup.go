package sysroot

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"ostree-go/internal/prune"
	"ostree-go/internal/repo"
)

// RemoveDeployment deletes a deployment directory, its origin and backing dir.
func (s *Sysroot) RemoveDeployment(d *Deployment) error {
	backing := filepath.Join(s.Path, d.BackingRelPath())
	origin := d.OriginPath(s.Path)
	deployPath := d.DirPath(s.Path)
	if err := os.RemoveAll(backing); err != nil {
		return err
	}
	if err := os.RemoveAll(origin); err != nil {
		return err
	}
	return os.RemoveAll(deployPath)
}

// listDeploymentDirsForOS lists the deployment directories of one stateroot.
func (s *Sysroot) listDeploymentDirsForOS(osname string) ([]*Deployment, error) {
	dir := filepath.Join(s.Path, "ostree/deploy", osname, "deploy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Deployment
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		csum, serial, err := parseDeployName(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, &Deployment{OsName: osname, Checksum: csum, DeploySerial: serial})
	}
	return out, nil
}

// listAllDeploymentDirs lists every deployment directory in the sysroot.
func (s *Sysroot) listAllDeploymentDirs() ([]*Deployment, error) {
	deployRoot := filepath.Join(s.Path, "ostree/deploy")
	entries, err := os.ReadDir(deployRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Deployment
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		list, err := s.listDeploymentDirsForOS(e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, list...)
	}
	return out, nil
}

// parseBootdirName splits "<osname>-<checksum>".
func parseBootdirName(name string) (osname, csum string, ok bool) {
	dash := strings.LastIndexByte(name, '-')
	if dash <= 0 {
		return "", "", false
	}
	csum = name[dash+1:]
	if !validChecksumString(csum) {
		return "", "", false
	}
	return name[:dash], csum, true
}

// cleanupOtherBootversions removes the inactive boot versions.
func (s *Sysroot) cleanupOtherBootversions() error {
	cleanupBoot := 1
	if s.BootVersion == 0 {
		cleanupBoot = 1
	} else {
		cleanupBoot = 0
	}
	cleanupSub := 0
	if s.SubBootVersion == 0 {
		cleanupSub = 1
	}
	paths := []string{
		fmt.Sprintf("boot/loader.%d", cleanupBoot),
		fmt.Sprintf("ostree/boot.%d", cleanupBoot),
		fmt.Sprintf("ostree/boot.%d.0", cleanupBoot),
		fmt.Sprintf("ostree/boot.%d.1", cleanupBoot),
		fmt.Sprintf("ostree/boot.%d.%d", s.BootVersion, cleanupSub),
	}
	for _, rel := range paths {
		if err := os.RemoveAll(filepath.Join(s.Path, rel)); err != nil {
			return err
		}
	}
	return nil
}

// cleanupOldDeployments removes deployment directories no longer referenced.
func (s *Sysroot) cleanupOldDeployments() error {
	active := make(map[string]bool, len(s.Deployments))
	for _, d := range s.Deployments {
		active[d.DirRelPath()] = true
	}
	all, err := s.listAllDeploymentDirs()
	if err != nil {
		return err
	}
	for _, d := range all {
		if active[d.DirRelPath()] {
			continue
		}
		if err := s.RemoveDeployment(d); err != nil {
			return err
		}
	}
	return nil
}

// cleanupBootfs deletes /boot/ostree directories not referenced anymore.
func (s *Sysroot) cleanupBootfs() error {
	active := make(map[string]bool, len(s.Deployments))
	activeOverlays := make(map[string]bool)
	for _, d := range s.Deployments {
		active[d.BootCsum] = true
		if d.BootConfig != nil {
			for _, initrd := range d.BootConfig.OverlayInitrds {
				activeOverlays[filepath.Base(initrd)] = true
			}
		}
		for _, csum := range d.OverlayInitrds {
			activeOverlays[csum+".img"] = true
		}
	}
	bootOstree := filepath.Join(s.Path, "boot/ostree")
	entries, err := os.ReadDir(bootOstree)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		_, csum, ok := parseBootdirName(e.Name())
		if !ok {
			continue
		}
		if active[csum] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(bootOstree, e.Name())); err != nil {
			return err
		}
	}
	// Overlay initrds.
	overlays := filepath.Join(s.Path, "boot", BootInitramfsOverlays)
	if entries, err := os.ReadDir(overlays); err == nil {
		for _, e := range entries {
			if e.IsDir() || activeOverlays[e.Name()] {
				continue
			}
			if err := os.Remove(filepath.Join(overlays, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupRefPrefix deletes deployment refs under ostree/<boot>/<sub>.
func (s *Sysroot) cleanupRefPrefix(bootVersion, subBootVersion int) error {
	prefix := fmt.Sprintf("ostree/%d/%d", bootVersion, subBootVersion)
	refs, err := s.Repo.ListRefs(prefix)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if ref.Kind != repo.RefLocal {
			continue
		}
		if err := s.Repo.DeleteRef(ref.Name); err != nil {
			return err
		}
	}
	return nil
}

// generateDeploymentRefs points ostree/<boot>/<sub>/<i> at each deployment.
func (s *Sysroot) generateDeploymentRefs(bootVersion, subBootVersion int) error {
	cleanupBoot := 0
	if bootVersion == 0 {
		cleanupBoot = 1
	}
	cleanupSub := 0
	if subBootVersion == 0 {
		cleanupSub = 1
	}
	for _, p := range [][2]int{{cleanupBoot, 0}, {cleanupBoot, 1}, {bootVersion, cleanupSub}} {
		if err := s.cleanupRefPrefix(p[0], p[1]); err != nil {
			return err
		}
	}
	for i, d := range s.Deployments {
		name := fmt.Sprintf("ostree/%d/%d/%d", bootVersion, subBootVersion, i)
		if err := s.Repo.WriteRef(name, d.Checksum); err != nil {
			return err
		}
	}
	return nil
}

// Cleanup implements ostree_sysroot_cleanup: drop stale deployments and
// optionally prune the repository.
func (s *Sysroot) Cleanup(pruneRepo bool) error {
	if err := s.cleanupOtherBootversions(); err != nil {
		return fmt.Errorf("Cleaning bootversions: %w", err)
	}
	if err := s.cleanupOldDeployments(); err != nil {
		return fmt.Errorf("Cleaning deployments: %w", err)
	}
	if err := s.cleanupBootfs(); err != nil {
		return fmt.Errorf("Cleaning bootfs: %w", err)
	}
	if err := s.generateDeploymentRefs(s.BootVersion, s.SubBootVersion); err != nil {
		return fmt.Errorf("Generating deployment refs: %w", err)
	}
	if pruneRepo {
		if err := s.Prune(); err != nil {
			return err
		}
	}
	return nil
}

// Prune removes unreachable objects from the sysroot repository.  Deployment
// refs (written by generateDeploymentRefs) anchor every deployed commit.
//
// Like ostree_sysroot_cleanup_prune_repo(), the operation is bracketed by a
// repository transaction.  The Go repository writes objects directly, so the
// transaction is emulated here: a staging directory is created for the current
// boot id before pruning (as ostree_repo_prepare_transaction() does), and after
// a successful prune the staging directories are garbage collected.  A failed
// transaction leaves its staging directory behind, which a later run cleans up.
func (s *Sysroot) Prune() error {
	if hasRepoTestError("pre-commit") {
		stagingDir, err := s.createRepoStagingDir()
		if err != nil {
			return err
		}
		_ = stagingDir
		return fmt.Errorf("Pruning system repository: OSTREE_REPO_TEST_ERROR_PRE_COMMIT specified")
	}
	opts := prune.Options{RefsOnly: true, Depth: 0, Stdout: io.Discard}
	if err := prune.Run(s.Repo, opts); err != nil {
		return fmt.Errorf("Pruning system repository: %w", err)
	}
	if err := s.cleanupRepoStagingDirs(); err != nil {
		return fmt.Errorf("Cleaning up repository tmp: %w", err)
	}
	return nil
}

// repoBootID mirrors libostree's staging boot id selection.
func repoBootID() string {
	if id := os.Getenv("OSTREE_BOOTID"); id != "" {
		return id
	}
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		if id := strings.TrimSpace(string(data)); id != "" {
			return id
		}
	}
	return "unknown"
}

// hasRepoTestError reports whether OSTREE_REPO_TEST_ERROR contains flag.
func hasRepoTestError(flag string) bool {
	for _, item := range strings.Split(os.Getenv("OSTREE_REPO_TEST_ERROR"), ":,") {
		if item == flag {
			return true
		}
	}
	return false
}

// createRepoStagingDir creates a transaction staging directory under the
// repository's tmp/, like ostree_repo_prepare_transaction().
func (s *Sysroot) createRepoStagingDir() (string, error) {
	dir := filepath.Join(s.Repo.Path(), "tmp")
	if err := os.MkdirAll(dir, 0o775); err != nil {
		return "", err
	}
	name := fmt.Sprintf("staging-%s-%s", repoBootID(), randomStagingSuffix())
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(path, 0o775); err != nil {
		return "", err
	}
	return path, nil
}

// randomStagingSuffix returns a short random hex string for staging paths.
func randomStagingSuffix() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return strconv.FormatInt(timeNow().UnixNano(), 16)
	}
	return hex.EncodeToString(buf[:])
}

// cleanupRepoStagingDirs implements cleanup_tmpdir() for staging directories:
// directories left behind by previous (possibly failed) transactions are
// removed.  Regular files are left alone since they may belong to a
// concurrent writer.
func (s *Sysroot) cleanupRepoStagingDirs() error {
	tmpDir := filepath.Join(s.Repo.Path(), "tmp")
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "staging-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tmpDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// LoadDeploymentOrigin (re)loads a deployment's origin file.
func (s *Sysroot) LoadDeploymentOrigin(d *Deployment) error {
	d.Origin = nil
	return s.loadOrigin(d)
}

// SetPinned changes a deployment's pinned state on disk.
func (s *Sysroot) SetPinned(d *Deployment, pinned bool) error {
	if d.Staged {
		return fmt.Errorf("Cannot pin staged deployment")
	}
	if d.IsPinned() == pinned {
		return nil
	}
	cfg := repo.NewConfig()
	if d.Origin != nil {
		for _, group := range d.Origin.Groups() {
			if group == OriginTransientGroup {
				continue
			}
			keys, err := d.Origin.Keys(group)
			if err != nil {
				return err
			}
			for _, key := range keys {
				v, err := d.Origin.GetRawString(group, key)
				if err != nil {
					return err
				}
				cfg.SetValue(group, key, v)
			}
		}
	}
	if pinned {
		cfg.Set(OriginTransientGroup, "pinned", "true")
	}
	if err := s.WriteOriginFileAndBump(d, cfg); err != nil {
		return err
	}
	d.Origin = cfg
	return nil
}

var _ = strconv.Itoa
