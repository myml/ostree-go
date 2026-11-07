package sysroot

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/myml/ostree-go/internal/bootconfig"
	"github.com/myml/ostree-go/internal/repo"
)

const (
	// RunstateStaged is the staged deployment variant path.
	RunstateStaged = "/run/ostree/staged-deployment"
	// RunstateStagedLocked disables automatic finalization.
	RunstateStagedLocked = "/run/ostree/staged-deployment-locked"
	// DeploymentRunstateDir holds per-deployment runtime state.
	DeploymentRunstateDir = "/run/ostree/deployment-state/"
	// RunstateFlagDevelopment marks an unlocked deployment.
	RunstateFlagDevelopment = "unlocked-development"
	// RunstateFlagTransient marks a transient-unlocked deployment.
	RunstateFlagTransient = "unlocked-transient"
	// BootInitramfsOverlays is the /boot directory for overlay initrds.
	BootInitramfsOverlays = "ostree/initramfs-overlays"
	// TmpfilesDir marks a systemd-based OS.
	TmpfilesDir = "usr/lib/tmpfiles.d"
)

func mustCompileBootlink() *regexp.Regexp {
	return regexp.MustCompile(`^/ostree/boot\.([01])/([^/]+)/([^/]+)/([0-9]+)$`)
}

// ostreeArgFromConfig extracts the value of the ostree= kernel argument.
func ostreeArgFromConfig(config *bootconfig.Config) string {
	options := config.Get("options")
	if options == "" {
		return ""
	}
	for _, opt := range strings.Split(options, " ") {
		if strings.HasPrefix(opt, "ostree=") {
			return opt[len("ostree="):]
		}
	}
	return ""
}

// loadOrigin reads a deployment's origin file into d.Origin.
func (s *Sysroot) loadOrigin(d *Deployment) error {
	data, err := os.ReadFile(d.OriginPath(s.Path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	cfg, err := repo.ParseConfig(data)
	if err != nil {
		return fmt.Errorf("Parsing %s: %w", d.OriginRelPath(), err)
	}
	d.Origin = cfg
	return nil
}

// OriginRefspec returns origin.refspec, or "".
func (d *Deployment) OriginRefspec() string {
	if d.Origin == nil {
		return ""
	}
	v, err := d.Origin.Get("origin", "refspec")
	if err != nil {
		return ""
	}
	return v
}

// parseDeployment builds a Deployment from one BLS entry.
func (s *Sysroot) parseDeployment(config *bootconfig.Config) (*Deployment, error) {
	ostreeArg := ostreeArgFromConfig(config)
	if ostreeArg == "" {
		return nil, fmt.Errorf("No ostree= kernel argument found")
	}
	_, osname, bootcsum, bootserial, err := parseBootlink(ostreeArg)
	if err != nil {
		return nil, err
	}
	rel := strings.TrimPrefix(ostreeArg, "/")
	target, err := os.Readlink(filepath.Join(s.Path, rel))
	if err != nil {
		return nil, fmt.Errorf("Parsing deployment %s in stateroot '%s': %w", ostreeArg, osname, err)
	}
	csum, deployserial, err := parseDeployName(filepath.Base(target))
	if err != nil {
		return nil, err
	}
	d := &Deployment{
		OsName:        osname,
		Checksum:      csum,
		DeploySerial:  deployserial,
		BootCsum:      bootcsum,
		BootSerial:    bootserial,
		BootConfig:    config,
		UnlockedState: UnlockedNone,
	}
	if err := s.loadOrigin(d); err != nil {
		return nil, err
	}
	devPath := filepath.Join(DeploymentRunstateDir,
		fmt.Sprintf("%s.%d", d.Checksum, d.DeploySerial))
	if _, err := os.Lstat(filepath.Join(devPath, RunstateFlagDevelopment)); err == nil {
		d.UnlockedState = "development"
	} else if _, err := os.Lstat(filepath.Join(devPath, RunstateFlagTransient)); err == nil {
		d.UnlockedState = "transient"
	} else if d.Origin != nil {
		if unlocked, err := d.Origin.Get("origin", "unlocked"); err == nil && unlocked == "hotfix" {
			d.UnlockedState = "hotfix"
		}
	}
	return d, nil
}

// EnsureInitialized creates /ostree/repo etc. (ostree_sysroot_ensure_initialized).
func (s *Sysroot) EnsureInitialized() error {
	for _, dir := range []string{"ostree/repo", "ostree/deploy"} {
		if err := os.MkdirAll(filepath.Join(s.Path, dir), 0o755); err != nil {
			return err
		}
	}
	repoPath := filepath.Join(s.Path, "ostree/repo")
	if _, err := repo.Create(repoPath, repo.ModeBare, nil); err != nil {
		return err
	}
	return nil
}

// InitOsName creates the stateroot directories and its /var.
func (s *Sysroot) InitOsName(osname string) error {
	deploydir := filepath.Join(s.Path, "ostree/deploy", osname)
	if err := os.Mkdir(deploydir, 0o777); err != nil {
		return fmt.Errorf("Creating ostree/deploy/%s: %w", osname, err)
	}
	if err := os.Mkdir(filepath.Join(deploydir, "var"), 0o777); err != nil {
		return fmt.Errorf("Creating %s: %w", "var", err)
	}
	return s.BumpMtime()
}

// BumpMtime updates ostree/deploy's timestamps so monitors notice changes.
func (s *Sysroot) BumpMtime() error {
	now := timeNow()
	return os.Chtimes(filepath.Join(s.Path, "ostree/deploy"), now, now)
}

// RequireStateroot errors when the stateroot does not exist.
func (s *Sysroot) RequireStateroot(osname string) error {
	path := filepath.Join(s.Path, "ostree/deploy", osname)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("No such stateroot: %s", osname)
		}
		return err
	}
	return nil
}

// OriginNewFromRefspec creates a fresh origin keyfile.
func (s *Sysroot) OriginNewFromRefspec(refspec string) *repo.Config {
	cfg := repo.NewConfig()
	cfg.Set("origin", "refspec", refspec)
	return cfg
}

// WriteOriginFile writes (or updates) a deployment's origin file.
func (s *Sysroot) WriteOriginFile(d *Deployment, cfg *repo.Config) error {
	if cfg == nil {
		cfg = d.Origin
	}
	if cfg == nil {
		return nil
	}
	if err := writeFileAtomic(d.OriginPath(s.Path), cfg.Bytes(), 0o644); err != nil {
		return fmt.Errorf("Writing out origin file: %w", err)
	}
	return nil
}

// WriteOriginFileAndBump is WriteOriginFile plus the deploy mtime bump.
func (s *Sysroot) WriteOriginFileAndBump(d *Deployment, cfg *repo.Config) error {
	if err := s.WriteOriginFile(d, cfg); err != nil {
		return err
	}
	return s.BumpMtime()
}

// QueryDeploymentsFor finds the pending and rollback deployments for osname.
func (s *Sysroot) QueryDeploymentsFor(osname string) (pending, rollback *Deployment) {
	if osname == "" && s.Booted != nil {
		osname = s.Booted.OsName
	}
	foundBooted := false
	for _, d := range s.Deployments {
		if d.OsName != osname {
			continue
		}
		if s.Booted != nil && d.Equal(s.Booted) {
			foundBooted = true
			continue
		}
		if !foundBooted && pending == nil {
			pending = d
		} else if foundBooted && rollback == nil {
			rollback = d
		}
	}
	return pending, rollback
}

// GetMergeDeployment picks the deployment whose /etc is merged on deploy.
func (s *Sysroot) GetMergeDeployment(osname string) *Deployment {
	if osname == "" && s.Booted != nil {
		osname = s.Booted.OsName
	}
	if s.Booted != nil && s.Booted.OsName == osname {
		return s.Booted
	}
	pending, _ := s.QueryDeploymentsFor(osname)
	return pending
}

// OsnameOrDefault returns osname, falling back to the booted deployment's.
func (s *Sysroot) OsnameOrDefault(osname string) (string, error) {
	if osname != "" {
		return osname, nil
	}
	if s.Booted == nil {
		return "", fmt.Errorf("Not currently booted into an OSTree system and no --os= argument given")
	}
	return s.Booted.OsName, nil
}

// RequireBootedDeployment returns the booted deployment or an error.
func (s *Sysroot) RequireBootedDeployment() (*Deployment, error) {
	if s.Booted == nil {
		return nil, fmt.Errorf("Not currently booted into an OSTree system")
	}
	return s.Booted, nil
}

// GetIndexedDeployment returns deployments[index] with C's error messages.
func (s *Sysroot) GetIndexedDeployment(index int) (*Deployment, error) {
	if index < 0 {
		return nil, fmt.Errorf("Invalid index %d", index)
	}
	if index >= len(s.Deployments) {
		return nil, fmt.Errorf("Out of range deployment index %d, expected < %d", index, len(s.Deployments))
	}
	return s.Deployments[index], nil
}

// CommitVersion returns the ostree commit "version" metadata for a checksum.
func (s *Sysroot) CommitVersion(csum string) string {
	c, err := loadCommit(s.Repo, csum)
	if err != nil {
		return ""
	}
	return c.Version
}

// CommitSourceTitle returns the ostree.source-title metadata.
func (s *Sysroot) CommitSourceTitle(csum string) string {
	c, err := loadCommit(s.Repo, csum)
	if err != nil {
		return ""
	}
	return c.SourceTitle
}
