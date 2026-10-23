// Package sysroot implements the OSTree "system root" layout: the on-disk
// deployment database in /ostree/deploy, the bootloader entries under /boot
// and the operations that create, prune and query deployments.
package sysroot

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ostree-go/internal/bootconfig"
	"ostree-go/internal/repo"
)

// Deployment is one entry of the sysroot's deployment list.
type Deployment struct {
	// OsName is the stateroot ("osname") this deployment belongs to.
	OsName string
	// Checksum is the commit checksum backing the deployment.
	Checksum string
	// DeploySerial is the second component of the on-disk deployment
	// directory name (CHECKSUM.DEPLOYSERIAL).
	DeploySerial int
	// BootCsum is the kernel+initramfs checksum used for /boot/ostree.
	BootCsum string
	// BootSerial is the last component of the ostree= bootlink.
	BootSerial int
	// Index is the position in the loaded deployment list.
	Index int
	// Staged marks deployments that are finalized at shutdown.
	Staged bool
	// Origin is the parsed ".origin" file, or nil.
	Origin *repo.Config
	// BootConfig is the BLS entry this deployment was read from, or the
	// one generated for it during a deploy.
	BootConfig *bootconfig.Config
	// OverlayInitrds are checksums of overlay initramfs files.
	OverlayInitrds []string
	// UnlockedState is "none", "hotfix", "development" or "transient".
	UnlockedState string
}

// DirRelPath is the sysroot-relative deployment directory.
func (d *Deployment) DirRelPath() string {
	return fmt.Sprintf("ostree/deploy/%s/deploy/%s.%d", d.OsName, d.Checksum, d.DeploySerial)
}

// DirPath is the absolute deployment directory for sysroot path root.
func (d *Deployment) DirPath(root string) string {
	return filepath.Join(root, d.DirRelPath())
}

// OriginRelPath is the sysroot-relative origin file path.
func (d *Deployment) OriginRelPath() string {
	return fmt.Sprintf("ostree/deploy/%s/deploy/%s.%d.origin", d.OsName, d.Checksum, d.DeploySerial)
}

// OriginPath is the absolute origin file path for sysroot path root.
func (d *Deployment) OriginPath(root string) string {
	return filepath.Join(root, d.OriginRelPath())
}

// BackingRelPath is the sysroot-relative "backing" directory.
func (d *Deployment) BackingRelPath() string {
	return fmt.Sprintf("ostree/deploy/%s/backing/%s.%d", d.OsName, d.Checksum, d.DeploySerial)
}

// String renders "osname checksum.serial".
func (d *Deployment) String() string {
	return fmt.Sprintf("%s %s.%d", d.OsName, d.Checksum, d.DeploySerial)
}

// Equal reports whether two deployments refer to the same on-disk entry.
func (d *Deployment) Equal(other *Deployment) bool {
	if d == nil || other == nil {
		return d == other
	}
	return d.OsName == other.OsName && d.Checksum == other.Checksum &&
		d.DeploySerial == other.DeploySerial
}

// IsPinned reports whether the deployment is pinned against GC.
func (d *Deployment) IsPinned() bool {
	if d.Origin == nil {
		return false
	}
	v, err := d.Origin.GetBool(OriginTransientGroup, "pinned", false)
	if err != nil {
		return false
	}
	return v
}

// SetPinned mutates the in-memory origin file.
func (d *Deployment) SetPinned(pinned bool) {
	if d.Origin == nil {
		d.Origin = repo.NewConfig()
	}
	if pinned {
		d.Origin.Set(OriginTransientGroup, "pinned", "true")
	} else {
		_ = d.Origin.Unset(OriginTransientGroup, "pinned")
	}
}

// OriginTransientGroup mirrors OSTREE_ORIGIN_TRANSIENT_GROUP.
const OriginTransientGroup = "libostree-transient"

// UnlockedNone is the default unlocked state.
const UnlockedNone = "none"

// DebugFlags mirror OSTREE_SYSROOT_DEBUG.
type DebugFlags struct {
	NoXattrs     bool
	NoDtb        bool
	Mutable      bool
	TestFifreeze bool
}

// Options mirror OSTREE_SYSROOT_OPTS.
type Options struct {
	SkipSync          bool
	NoEarlyPrune      bool
	BootloaderNaming1 bool
}

// Sysroot is a loaded OSTree system root.
type Sysroot struct {
	// Path is the absolute sysroot path.
	Path string
	// Repo is the repository at <Path>/ostree/repo.
	Repo *repo.Repo
	// BootVersion is the active /boot/loader.N version.
	BootVersion int
	// SubBootVersion is the active /ostree/boot.N.M subversion.
	SubBootVersion int
	// Deployments is the loaded deployment list, highest version first.
	Deployments []*Deployment
	// Booted is the booted deployment, or nil when not booted via OSTree.
	Booted *Deployment
	// Physical is true when the sysroot is a physical root (no /sysroot).
	Physical bool
	// Debug holds the OSTREE_SYSROOT_DEBUG flags.
	Debug DebugFlags
	// Opts holds the OSTREE_SYSROOT_OPTS flags.
	Opts Options
	// Err receives human readable progress/journal messages, like the C
	// CLI's journal-msg handler does on stdout.
	Err io.Writer
	// Stderr receives diagnostics such as the fifreeze watchdog message,
	// which libostree prints with g_printerr().
	Stderr io.Writer
}

// EmitMessage prints a journal-style message to the sysroot's message writer.
func (s *Sysroot) EmitMessage(msg string) {
	if s.Err != nil {
		fmt.Fprintln(s.Err, msg)
	}
}

// defaultPath implements _ostree_get_default_sysroot_path().
func DefaultPath() string {
	if p := os.Getenv("OSTREE_SYSROOT"); p != "" {
		return p
	}
	return "/"
}

func parseDebugFlags(value string) DebugFlags {
	var f DebugFlags
	for _, item := range strings.Split(value, ",") {
		switch item {
		case "mutable-deployments":
			f.Mutable = true
		case "test-fifreeze":
			f.TestFifreeze = true
		case "no-xattrs":
			f.NoXattrs = true
		case "no-dtb":
			f.NoDtb = true
		}
	}
	return f
}

func parseOpts(value string) Options {
	var o Options
	for _, item := range strings.Split(value, ",") {
		switch item {
		case "skip-sync":
			o.SkipSync = true
		case "no-early-prune":
			o.NoEarlyPrune = true
		case "bootloader-naming-1":
			o.BootloaderNaming1 = true
		}
	}
	return o
}

// Open loads the sysroot at path (which must already be initialized).
func Open(path string) (*Sysroot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s := &Sysroot{
		Path:           abs,
		Debug:          parseDebugFlags(os.Getenv("OSTREE_SYSROOT_DEBUG")),
		Opts:           parseOpts(os.Getenv("OSTREE_SYSROOT_OPTS")),
		BootVersion:    0,
		SubBootVersion: 0,
	}
	r, err := repo.Open(filepath.Join(abs, "ostree/repo"))
	if err != nil {
		return nil, fmt.Errorf("Opening sysroot repo: %w", err)
	}
	s.Repo = r
	if err := s.Load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Load (re)reads bootversion and the deployment list from disk.
func (s *Sysroot) Load() error {
	bv, err := s.readBootVersion()
	if err != nil {
		return err
	}
	sv, err := s.readSubBootVersion(bv)
	if err != nil {
		return err
	}
	s.BootVersion = bv
	s.SubBootVersion = sv

	configs, err := s.ReadLoaderConfigs(bv)
	if err != nil {
		return err
	}
	deployments := make([]*Deployment, 0, len(configs))
	for _, config := range configs {
		d, err := s.parseDeployment(config)
		if err != nil {
			return err
		}
		deployments = append(deployments, d)
	}
	sort.SliceStable(deployments, func(i, j int) bool {
		return compareBootConfigs(deployments[i].BootConfig, deployments[j].BootConfig) < 0
	})
	for i, d := range deployments {
		d.Index = i
	}
	s.Deployments = deployments
	s.Booted = nil

	// Determine whether this is a physical root: a deployment root has a
	// "sysroot" symlink/mount, the physical root does not.
	if _, err := os.Lstat(filepath.Join(s.Path, "sysroot")); err != nil {
		s.Physical = true
	} else {
		s.Physical = false
	}
	return nil
}

// readBootVersion reads the boot/loader symlink target.
func (s *Sysroot) readBootVersion() (int, error) {
	link := filepath.Join(s.Path, "boot/loader")
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	switch target {
	case "loader.0":
		return 0, nil
	case "loader.1":
		return 1, nil
	}
	return 0, fmt.Errorf("Invalid target '%s' in boot/loader", target)
}

// readSubBootVersion reads the ostree/boot.N symlink target.
func (s *Sysroot) readSubBootVersion(bootVersion int) (int, error) {
	link := filepath.Join(s.Path, fmt.Sprintf("ostree/boot.%d", bootVersion))
	target, err := os.Readlink(link)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	switch {
	case strings.HasSuffix(target, ".0"):
		return 0, nil
	case strings.HasSuffix(target, ".1"):
		return 1, nil
	}
	return 0, fmt.Errorf("Invalid target '%s' in ostree/boot.%d", target, bootVersion)
}

// ReadLoaderConfigs reads the BLS entries of bootversion, sorted the way the
// deployment list expects.
func (s *Sysroot) ReadLoaderConfigs(bootVersion int) ([]*bootconfig.Config, error) {
	dir := filepath.Join(s.Path, fmt.Sprintf("boot/loader.%d/entries", bootVersion))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var configs []*bootconfig.Config
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "ostree-") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if e.IsDir() {
			continue
		}
		config, err := bootconfig.ParseFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("Parsing %s: %w", name, err)
		}
		configs = append(configs, config)
	}
	sort.SliceStable(configs, func(i, j int) bool {
		return compareBootConfigs(configs[i], configs[j]) < 0
	})
	return configs, nil
}

// strverscmp approximates GLib's g_strverscmp for the simple decimal version
// strings OSTree writes, comparing digit runs numerically.
func strverscmp(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ca, cb := a[i], b[j]
		if isDigit(ca) && isDigit(cb) {
			si, sj := i, j
			for i < len(a) && isDigit(a[i]) {
				i++
			}
			for j < len(b) && isDigit(b[j]) {
				j++
			}
			na := strings.TrimLeft(a[si:i], "0")
			nb := strings.TrimLeft(b[sj:j], "0")
			if len(na) != len(nb) {
				if len(na) < len(nb) {
					return -1
				}
				return 1
			}
			if c := strings.Compare(na, nb); c != 0 {
				return c
			}
			continue
		}
		if ca != cb {
			if ca < cb {
				return -1
			}
			return 1
		}
		i++
		j++
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return 0
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// compareBootConfigs orders BLS entries by version, descending.
func compareBootConfigs(a, b *bootconfig.Config) int {
	av, bv := a.Get("version"), b.Get("version")
	switch {
	case av != "" && bv != "":
		return -strverscmp(av, bv)
	case av != "":
		return -1
	default:
		return 1
	}
}

var bootlinkRe = mustCompileBootlink()

// parseBootlink parses an ostree= kernel argument.
func parseBootlink(link string) (bootVersion int, osname, bootcsum string, treebootserial int, err error) {
	m := bootlinkRe.FindStringSubmatch(link)
	if m == nil {
		return 0, "", "", 0, fmt.Errorf(
			"Invalid ostree= argument '%s', expected ostree=/ostree/boot.BOOTVERSION/OSNAME/BOOTSUM/TREESERIAL", link)
	}
	bv, _ := strconv.Atoi(m[1])
	serial, _ := strconv.Atoi(m[4])
	return bv, m[2], m[3], serial, nil
}

// parseDeployName parses CHECKSUM.TREESERIAL.
func parseDeployName(name string) (csum string, serial int, err error) {
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return "", 0, fmt.Errorf("Invalid deploy name '%s', expected CHECKSUM.TREESERIAL", name)
	}
	csum = name[:dot]
	serialStr := name[dot+1:]
	if csum == "" || serialStr == "" {
		return "", 0, fmt.Errorf("Invalid deploy name '%s', expected CHECKSUM.TREESERIAL", name)
	}
	for i := 0; i < len(csum); i++ {
		if !isLowerHex(csum[i]) {
			return "", 0, fmt.Errorf("Invalid deploy name '%s', expected CHECKSUM.TREESERIAL", name)
		}
	}
	serial, err = strconv.Atoi(serialStr)
	if err != nil {
		return "", 0, fmt.Errorf("Invalid deploy name '%s', expected CHECKSUM.TREESERIAL", name)
	}
	return csum, serial, nil
}

func isLowerHex(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

var _ = sort.Strings
