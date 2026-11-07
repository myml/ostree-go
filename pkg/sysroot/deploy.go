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
	"sort"
	"strconv"
	"strings"

	"github.com/myml/ostree-go/pkg/bootconfig"
	"github.com/myml/ostree-go/pkg/bootloader"
	"github.com/myml/ostree-go/pkg/checkout"
	"github.com/myml/ostree-go/pkg/repo"
)

// kernelLayout describes where a deployment's kernel lives and what it is
// called in /boot/ostree.
type kernelLayout struct {
	// bootDir is the absolute directory containing the kernel in the tree.
	bootDir string
	// kernelSrc is the source filename of the kernel inside bootDir.
	kernelSrc string
	// kernelName is the destination filename (e.g. vmlinuz-3.6.0).
	kernelName string
	// initramfsSrc/Name are the initramfs, when present.
	initramfsSrc  string
	initramfsName string
	// devicetreeSrc/Name are the devicetree, when present.  A nil name
	// means devicetreeSrc is a whole directory of device trees.
	devicetreeSrc  string
	devicetreeName string
	hasDevicetree  bool
	// hmacSrc/Name are the optional FIPS hmac file.
	hmacSrc  string
	hmacName string
	// bootCsum is the kernel+initramfs+devicetree checksum.
	bootCsum string
}

// checksumFile updates h with the contents of path.
func checksumFile(h io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(h, f)
	return err
}

// checksumDirRecurse mirrors checksum_dir_recurse(): entries are processed in
// sorted order, directories recursively, regular files by content.
func checksumDirRecurse(h io.Writer, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		if info.IsDir() {
			if err := checksumDirRecurse(h, path); err != nil {
				return err
			}
			continue
		}
		if err := checksumFile(h, path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("checksum %s: %w", path, err)
		}
	}
	return nil
}

func validChecksumString(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isLowerHex(s[i]) {
			return false
		}
	}
	return true
}

// getKernelFromTreeUSRLibModules looks in /usr/lib/modules/$kver.
func getKernelFromTreeUSRLibModules(s *Sysroot, deployDir string) (*kernelLayout, error) {
	modDir := filepath.Join(deployDir, "usr/lib/modules")
	entries, err := os.ReadDir(modDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var kver, modPath string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		candidate := filepath.Join(modDir, e.Name(), "vmlinuz")
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		kver = e.Name()
		modPath = filepath.Join(modDir, e.Name())
		break
	}
	if kver == "" {
		return nil, nil
	}
	layout := &kernelLayout{
		bootDir:    modPath,
		kernelSrc:  "vmlinuz",
		kernelName: "vmlinuz-" + kver,
	}
	h := sha256.New()
	if err := checksumFile(h, filepath.Join(modPath, "vmlinuz")); err != nil {
		return nil, err
	}
	for _, name := range []string{"initramfs.img", "initramfs"} {
		if _, err := os.Stat(filepath.Join(modPath, name)); err == nil {
			layout.initramfsSrc = name
			layout.initramfsName = "initramfs-" + kver + ".img"
			if err := checksumFile(h, filepath.Join(modPath, name)); err != nil {
				return nil, err
			}
			break
		}
	}
	noDtb := s.Debug.NoDtb
	if !noDtb {
		if _, err := os.Stat(filepath.Join(modPath, "devicetree")); err == nil {
			layout.devicetreeSrc = "devicetree"
			layout.devicetreeName = "devicetree-" + kver
			layout.hasDevicetree = true
			if err := checksumFile(h, filepath.Join(modPath, "devicetree")); err != nil {
				return nil, err
			}
		} else if info, err := os.Stat(filepath.Join(modPath, "dtb")); err == nil && info.IsDir() {
			layout.devicetreeSrc = "dtb"
			// A nil devicetreeName means "a whole directory".
			layout.devicetreeName = ""
			layout.hasDevicetree = true
			if err := checksumDirRecurse(h, filepath.Join(modPath, "dtb")); err != nil {
				return nil, err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(modPath, ".vmlinuz.hmac")); err == nil {
		layout.hmacSrc = ".vmlinuz.hmac"
		layout.hmacName = "." + layout.kernelName + ".hmac"
	}
	layout.bootCsum = hex.EncodeToString(h.Sum(nil))
	return layout, nil
}

// getKernelFromTreeLegacyLayouts looks in /usr/lib/ostree-boot and /boot.
func getKernelFromTreeLegacyLayouts(deployDir string) (*kernelLayout, error) {
	var bootDir string
	for _, rel := range []string{"usr/lib/ostree-boot", "boot"} {
		candidate := filepath.Join(deployDir, rel)
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			bootDir = candidate
			break
		}
	}
	if bootDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(bootDir)
	if err != nil {
		return nil, err
	}
	layout := &kernelLayout{bootDir: bootDir}
	var kernelCsum, initramfsCsum, devicetreeCsum string
	for _, e := range entries {
		name := e.Name()
		dash := strings.LastIndexByte(name, '-')
		if dash < 0 {
			continue
		}
		suffix := name[dash+1:]
		if !validChecksumString(suffix) {
			continue
		}
		switch {
		case layout.kernelSrc == "" && strings.HasPrefix(name, "vmlinuz-"):
			kernelCsum = suffix
			layout.kernelSrc = name
			layout.kernelName = name[:dash]
		case layout.initramfsSrc == "" && strings.HasPrefix(name, "initramfs-"):
			initramfsCsum = suffix
			layout.initramfsSrc = name
			layout.initramfsName = name[:dash]
		case layout.devicetreeSrc == "" && strings.HasPrefix(name, "devicetree-"):
			devicetreeCsum = suffix
			layout.devicetreeSrc = name
			layout.devicetreeName = name[:dash]
			layout.hasDevicetree = true
		}
		if layout.kernelSrc != "" && layout.initramfsSrc != "" && layout.devicetreeSrc != "" {
			break
		}
	}
	if layout.kernelSrc == "" {
		return nil, nil
	}
	if layout.initramfsSrc != "" && kernelCsum != initramfsCsum {
		return nil, fmt.Errorf("Mismatched kernel checksum vs initrd")
	}
	if layout.devicetreeSrc != "" && kernelCsum != devicetreeCsum {
		return nil, fmt.Errorf("Mismatched kernel checksum vs device tree in tree")
	}
	layout.bootCsum = kernelCsum
	return layout, nil
}

// getKernelFromTree locates the kernel for a deployment.
func getKernelFromTree(s *Sysroot, deployDir string) (*kernelLayout, error) {
	modules, err := getKernelFromTreeUSRLibModules(s, deployDir)
	if err != nil {
		return nil, err
	}
	legacy, err := getKernelFromTreeLegacyLayouts(deployDir)
	if err != nil {
		return nil, err
	}
	switch {
	case legacy == nil && modules == nil:
		return nil, fmt.Errorf("Failed to find kernel in /usr/lib/modules, /usr/lib/ostree-boot or /boot")
	case legacy == nil:
		return modules, nil
	case modules != nil && modules.initramfsSrc == "" && legacy.initramfsSrc != "":
		return legacy, nil
	case modules != nil:
		return modules, nil
	default:
		return legacy, nil
	}
}

// installIntoBoot hardlinks (or copies) a file into /boot.
func installIntoBoot(srcPath, destPath string) error {
	if err := os.Link(srcPath, destPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrExist) {
		// EMLINK/EXDEV and friends: fall back to a copy below.
	}
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destPath); err == nil {
		return nil
	}
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-"+filepath.Base(destPath))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if uid, gid, ok := statUIDGID(info); ok {
		// Best effort; running unprivileged cannot chown.
		_ = tmp.Chown(uid, gid)
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpName, destPath); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		if err := os.Rename(tmpName, destPath); err != nil {
			return err
		}
	}
	return nil
}

// prepareDeploymentEtc normalizes /etc and populates it from /usr/etc.
func (s *Sysroot) prepareDeploymentEtc(d *Deployment, deployDir string) error {
	etcPath := filepath.Join(deployDir, "etc")
	usrEtcPath := filepath.Join(deployDir, "usr/etc")

	etcExists := false
	etcPopulated := false
	if entries, err := os.ReadDir(etcPath); err == nil {
		etcExists = true
		etcPopulated = len(entries) > 0
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	usrEtcExists := false
	if _, err := os.Lstat(usrEtcPath); err == nil {
		usrEtcExists = true
	}

	switch {
	case !etcExists:
		// nothing to do
	case !etcPopulated:
		if usrEtcExists {
			if err := os.Remove(etcPath); err != nil {
				return fmt.Errorf("Failed to remove empty etc: %w", err)
			}
			etcExists = false
		}
	default:
		if usrEtcExists {
			return fmt.Errorf("Tree contains both /etc and /usr/etc")
		}
		if err := os.Rename(etcPath, usrEtcPath); err != nil {
			return err
		}
		etcExists = false
		usrEtcExists = true
	}

	if usrEtcExists && !etcExists {
		opts := checkout.Options{
			Repo:        s.Repo,
			Commit:      d.Checksum,
			Destination: etcPath,
			Subpath:     "/usr/etc",
			ForceCopy:   true,
		}
		if err := checkout.Checkout(opts); err != nil {
			return fmt.Errorf("Preparing /etc: %w", err)
		}
	}
	return nil
}

// prepareDeploymentVar initializes the stateroot's /var from the tree.
func (s *Sysroot) prepareDeploymentVar(d *Deployment, deployDir string) error {
	varPath := filepath.Join(deployDir, "var")
	if _, err := os.Lstat(varPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	staterootVar := filepath.Join(s.Path, "ostree/deploy", d.OsName, "var")
	entries, err := os.ReadDir(staterootVar)
	if err != nil {
		return fmt.Errorf("Preparing deployment /var: opening stateroot: %w", err)
	}
	if len(entries) > 0 {
		return nil
	}
	opts := checkout.Options{
		Repo:        s.Repo,
		Commit:      d.Checksum,
		Destination: staterootVar,
		Subpath:     "/var",
		ForceCopy:   true,
		Union:       true,
		UnionMode:   1,
	}
	if err := checkout.Checkout(opts); err != nil {
		return fmt.Errorf("Preparing deployment /var: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(deployDir, TmpfilesDir)); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := staterootLegacyVarInit(staterootVar); err != nil {
			return fmt.Errorf("Legacy mode stateroot var initialization: %w", err)
		}
	}
	return nil
}

// staterootLegacyVarInit creates the core /var subdirectories.
func staterootLegacyVarInit(dir string) error {
	for _, item := range []struct {
		name string
		mode os.FileMode
	}{{"tmp", 0o777}, {"lib", 0o777}, {"log", 0o755}} {
		path := filepath.Join(dir, item.name)
		if err := os.MkdirAll(path, item.mode); err != nil {
			return err
		}
		if err := os.Chmod(path, item.mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(filepath.Join(dir, "tmp"), 0o1777); err != nil {
		return err
	}
	for _, link := range []struct{ name, target string }{
		{"run", "../run"},
		{"lock", "../run/lock"},
	} {
		path := filepath.Join(dir, link.name)
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		if err := os.Symlink(link.target, path); err != nil {
			return err
		}
	}
	return nil
}

// etcEntry is one path of an /etc tree.
type etcEntry struct {
	isDir  bool
	isLink bool
	mode   os.FileMode
	sum    string
	target string
}

func etcTree(root string) (map[string]etcEntry, error) {
	out := make(map[string]etcEntry)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		entry := etcEntry{mode: info.Mode().Perm()}
		switch {
		case info.IsDir():
			entry.isDir = true
		case info.Mode()&os.ModeSymlink != 0:
			entry.isLink = true
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry.target = target
			entry.sum = "l:" + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			entry.sum = hex.EncodeToString(sum[:])
		default:
			// Special files (fifos, sockets, device nodes) are ignored, like
			// ostree_diff_dirs() does.
			return nil
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func parentRel(rel string) string {
	dir := filepath.Dir(rel)
	if dir == "." {
		return ""
	}
	return dir
}

// copySinglePath copies one non-directory (file or symlink) to dst.
func copySinglePath(src, dst string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
	default:
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return copyOwnership(dst, info)
}

// copyEntry copies one path (recursively for directories) from src to dst.
func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if info.IsDir() {
		return copyTreeRecursive(src, dst, false)
	}
	return copySinglePath(src, dst, info)
}

// mergeConfigurationFrom applies /etc changes from merge into d.
func (s *Sysroot) mergeConfigurationFrom(merge, d *Deployment) error {
	mergeDir := merge.DirPath(s.Path)
	newDir := d.DirPath(s.Path)
	origRoot := filepath.Join(mergeDir, "usr/etc")
	modRoot := filepath.Join(mergeDir, "etc")
	newRoot := filepath.Join(newDir, "etc")

	orig, err := etcTree(origRoot)
	if err != nil {
		return fmt.Errorf("During /etc merge: %w", err)
	}
	modified, err := etcTree(modRoot)
	if err != nil {
		return fmt.Errorf("During /etc merge: %w", err)
	}

	var removed, mods, added []string
	for rel, oe := range orig {
		me, ok := modified[rel]
		if !ok {
			removed = append(removed, rel)
			continue
		}
		if oe.isDir != me.isDir || oe.isLink != me.isLink ||
			(!oe.isDir && (oe.sum != me.sum || oe.mode != me.mode)) ||
			(oe.isDir && oe.mode != me.mode) {
			mods = append(mods, rel)
		}
	}
	// Only copy a modified directory if it has no modified parent; the
	// recursive copy below handles whole subtrees.
	for rel := range modified {
		if _, ok := orig[rel]; !ok {
			added = append(added, rel)
		}
	}
	sort.Strings(removed)
	sort.Strings(mods)
	sort.Strings(added)

	for _, rel := range removed {
		if err := os.RemoveAll(filepath.Join(newRoot, rel)); err != nil {
			return fmt.Errorf("During /etc merge: %w", err)
		}
	}
	copiedDirs := make(map[string]bool)
	copyOne := func(rel string) error {
		src := filepath.Join(modRoot, rel)
		dst := filepath.Join(newRoot, rel)
		if info, err := os.Lstat(src); err != nil {
			return err
		} else if info.IsDir() {
			// Create the directory itself if missing; a directory that
			// exists in the target keeps its contents.
			if _, err := os.Lstat(dst); err == nil {
				return os.Chmod(dst, info.Mode().Perm())
			}
			if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
				return err
			}
			// mkdir(2) applies the umask; restore the recorded mode.
			if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
				return err
			}
			copiedDirs[rel] = true
			return copyOwnership(dst, info)
		}
		if info, err := os.Lstat(dst); err == nil && info.IsDir() {
			return fmt.Errorf("Modified config file newly defaults to directory '%s', cannot merge", rel)
		}
		return copyEntry(src, dst)
	}
	for _, rel := range mods {
		if err := copyOne(rel); err != nil {
			return fmt.Errorf("During /etc merge: %w", err)
		}
	}
	for _, rel := range added {
		if err := copyOne(rel); err != nil {
			return fmt.Errorf("During /etc merge: %w", err)
		}
	}
	return nil
}

// allocateDeploySerial finds the next deployment serial for csum.
func (s *Sysroot) allocateDeploySerial(osname, csum string) (int, error) {
	next := 0
	dir := filepath.Join(s.Path, "ostree/deploy", osname, "deploy")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		c, serial, err := parseDeployName(e.Name())
		if err != nil {
			continue
		}
		if c != csum {
			continue
		}
		if serial+1 > next {
			next = serial + 1
		}
	}
	return next, nil
}

// checkoutDeploymentTree checks the commit out to its deployment directory.
func (s *Sysroot) checkoutDeploymentTree(osname, csum string, deploySerial int) (string, error) {
	osDeploy := filepath.Join(s.Path, "ostree/deploy", osname, "deploy")
	if err := os.MkdirAll(osDeploy, 0o775); err != nil {
		return "", fmt.Errorf("Checking out deployment tree: %w", err)
	}
	targetName := fmt.Sprintf("%s.%d", csum, deploySerial)
	target := filepath.Join(osDeploy, targetName)
	if err := os.RemoveAll(target); err != nil {
		return "", err
	}
	opts := checkout.Options{
		Repo:                        s.Repo,
		Commit:                      csum,
		Destination:                 target,
		ProcessPassthroughWhiteouts: true,
	}
	if err := checkout.Checkout(opts); err != nil {
		return "", fmt.Errorf("Checking out deployment tree: %w", err)
	}
	return target, nil
}

// DeployOptions configures DeployTree.
type DeployOptions struct {
	// OverrideKernelArgs replaces the inherited kernel arguments.
	OverrideKernelArgs []string
	// OverrideKernelArgsSet records whether the above is meaningful.
	OverrideKernelArgsSet bool
	// OverlayInitrds are checksums of overlay initramfs files.
	OverlayInitrds []string
}

// InitializeDeployment performs the first half of a deploy: checkout plus the
// initial /etc and /var state.
func (s *Sysroot) InitializeDeployment(osname, revision string, origin *repo.Config,
	opts *DeployOptions) (*Deployment, error) {
	if err := s.RequireStateroot(osname); err != nil {
		return nil, fmt.Errorf("Initializing deployment: %w", err)
	}
	backing := filepath.Join(s.Path, "ostree/deploy", osname, "backing")
	if err := os.MkdirAll(backing, 0o700); err != nil {
		return nil, fmt.Errorf("Initializing deployment: Creating backing directory: %w", err)
	}
	deploySerial, err := s.allocateDeploySerial(osname, revision)
	if err != nil {
		return nil, err
	}
	deployDir, err := s.checkoutDeploymentTree(osname, revision, deploySerial)
	if err != nil {
		return nil, err
	}
	d := &Deployment{
		OsName:        osname,
		Checksum:      revision,
		DeploySerial:  deploySerial,
		Index:         0,
		Origin:        origin,
		UnlockedState: UnlockedNone,
	}
	layout, err := getKernelFromTree(s, deployDir)
	if err != nil {
		return nil, fmt.Errorf("Initializing deployment: %w", err)
	}
	d.BootCsum = layout.bootCsum
	d.BootConfig = bootconfig.New()
	if opts != nil {
		d.OverlayInitrds = append([]string(nil), opts.OverlayInitrds...)
	}
	if opts != nil && opts.OverrideKernelArgsSet {
		kargs := bootconfig.NewKernelArgs()
		kargs.AppendArgv(opts.OverrideKernelArgs)
		d.BootConfig.Set("options", kargs.String())
	}
	if err := s.prepareDeploymentEtc(d, deployDir); err != nil {
		return nil, err
	}
	if err := s.prepareDeploymentVar(d, deployDir); err != nil {
		return nil, err
	}
	return d, nil
}

// FinalizeDeployment performs the second half of a deploy: configuration
// merge, /var fixups and origin writing.
func (s *Sysroot) FinalizeDeployment(d, merge *Deployment) error {
	deployDir := d.DirPath(s.Path)
	if merge != nil && !d.BootConfig.Has("options") {
		if merge.BootConfig != nil {
			if v := merge.BootConfig.Get("options"); v != "" {
				d.BootConfig.Set("options", v)
			}
		}
	}
	if merge != nil {
		if err := s.mergeConfigurationFrom(merge, d); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(deployDir, "etc/.updated")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	varDir := filepath.Join(s.Path, "ostree/deploy", d.OsName, "var")
	if err := os.Remove(filepath.Join(varDir, ".updated")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	backing := filepath.Join(s.Path, d.BackingRelPath())
	for _, dir := range []string{backing, filepath.Join(backing, "root-transient"),
		filepath.Join(backing, "root-transient/work"), filepath.Join(backing, "root-transient/upper")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("Finalizing deployment: Preparing deployment backing dir: %w", err)
		}
	}
	if err := s.WriteOriginFile(d, d.Origin); err != nil {
		return fmt.Errorf("Finalizing deployment: %w", err)
	}
	return nil
}

// DeployTree checks out revision and prepares a new deployment.
func (s *Sysroot) DeployTree(osname, revision string, origin *repo.Config, merge *Deployment,
	opts *DeployOptions) (*Deployment, error) {
	d, err := s.InitializeDeployment(osname, revision, origin, opts)
	if err != nil {
		return nil, err
	}
	if err := s.FinalizeDeployment(d, merge); err != nil {
		return nil, fmt.Errorf("Deploying tree: %w", err)
	}
	return d, nil
}

// symlinkAtReplace atomically replaces a symlink.
func symlinkAtReplace(target, path string) error {
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// createNewBootlinks writes the ostree/boot.N.M symlink farm.
func (s *Sysroot) createNewBootlinks(bootVersion int, newDeployments []*Deployment) error {
	oldSub, err := s.subBootVersionFor(bootVersion)
	if err != nil {
		return err
	}
	newSub := 0
	if oldSub == 0 {
		newSub = 1
	}
	subName := fmt.Sprintf("boot.%d.%d", bootVersion, newSub)
	subPath := filepath.Join(s.Path, "ostree", subName)
	if err := os.RemoveAll(subPath); err != nil {
		return err
	}
	if err := os.MkdirAll(subPath, 0o755); err != nil {
		return err
	}
	for _, d := range newDeployments {
		parentRel := filepath.Join(d.OsName, d.BootCsum)
		linkRel := filepath.Join(parentRel, strconv.Itoa(d.BootSerial))
		if err := os.MkdirAll(filepath.Join(subPath, parentRel), 0o755); err != nil {
			return err
		}
		target := fmt.Sprintf("../../../deploy/%s/deploy/%s.%d", d.OsName, d.Checksum, d.DeploySerial)
		if err := symlinkAtReplace(target, filepath.Join(subPath, linkRel)); err != nil {
			return err
		}
	}
	return nil
}

// subBootVersionFor returns the current subbootversion for bootVersion.
func (s *Sysroot) subBootVersionFor(bootVersion int) (int, error) {
	if bootVersion != s.BootVersion {
		return s.readSubBootVersion(bootVersion)
	}
	return s.SubBootVersion, nil
}

// swapBootlinks makes the freshly created subbootversion active.
func (s *Sysroot) swapBootlinks(bootVersion int) (string, error) {
	oldSub, err := s.subBootVersionFor(bootVersion)
	if err != nil {
		return "", err
	}
	newSub := 0
	if oldSub == 0 {
		newSub = 1
	}
	subName := fmt.Sprintf("boot.%d.%d", bootVersion, newSub)
	bootName := fmt.Sprintf("boot.%d", bootVersion)
	if err := symlinkAtReplace(subName, filepath.Join(s.Path, "ostree", bootName)); err != nil {
		return "", err
	}
	return subName, nil
}

// assignBootSerials numbers deployments per boot checksum.
func assignBootSerials(deployments []*Deployment) {
	counts := make(map[string]int)
	for _, d := range deployments {
		n := counts[d.BootCsum]
		d.BootSerial = n
		counts[d.BootCsum] = n + 1
	}
}

// nonOstreeKargs returns the kernel arguments with ostree= removed.
func nonOstreeKargs(config *bootconfig.Config) string {
	kargs := bootconfig.FromString(config.Get("options"))
	_ = kargs.DeleteIfPresent("ostree")
	return kargs.String()
}

// deploymentBootConfigsEqual reports whether two deployments are compatible
// from the bootloader's point of view.
func (s *Sysroot) deploymentBootConfigsEqual(a, b *Deployment) bool {
	if a.BootCsum != b.BootCsum {
		return false
	}
	if strings.Join(a.OverlayInitrds, ",") != strings.Join(b.OverlayInitrds, ",") {
		return false
	}
	if nonOstreeKargs(a.BootConfig) != nonOstreeKargs(b.BootConfig) {
		return false
	}
	if s.CommitVersion(a.Checksum) != s.CommitVersion(b.Checksum) {
		return false
	}
	return a.OsName == b.OsName
}

// WriteDeploymentsOptions mirror OstreeSysrootWriteDeploymentsOpts.
type WriteDeploymentsOptions struct {
	DoPostclean bool
}

// WriteDeployments writes the new deployment list and updates the bootloader.
func (s *Sysroot) WriteDeployments(newDeployments []*Deployment, opts WriteDeploymentsOptions) error {
	curLen := len(s.Deployments)
	assignBootSerials(newDeployments)

	requiresNewBootVersion := false
	if len(newDeployments) != curLen {
		requiresNewBootVersion = true
	} else {
		isNoop := true
		for i := range newDeployments {
			cur := s.Deployments[i]
			nw := newDeployments[i]
			if !s.deploymentBootConfigsEqual(cur, nw) {
				requiresNewBootVersion = true
				isNoop = false
				break
			}
			if cur != nw {
				isNoop = false
			}
		}
		if isNoop {
			return nil
		}
	}

	foundBootedDeployment := false
	for i, d := range newDeployments {
		if d.Equal(s.Booted) {
			foundBootedDeployment = true
		}
		dir := d.DirPath(s.Path)
		if _, err := os.Lstat(dir); err != nil {
			return fmt.Errorf("Unable to find expected deployment root: %s", dir)
		}
		d.Index = i
	}
	if s.Booted != nil && !foundBootedDeployment {
		return fmt.Errorf("Attempting to remove booted deployment")
	}

	bootloaderIsAtomic := false
	var bl bootloader.Bootloader
	newSubBootDir := ""
	if !requiresNewBootVersion {
		if err := s.createNewBootlinks(s.BootVersion, newDeployments); err != nil {
			return fmt.Errorf("Creating new current bootlinks: %w", err)
		}
		if err := s.fullSystemSync(); err != nil {
			return err
		}
		sub, err := s.swapBootlinks(s.BootVersion)
		if err != nil {
			return fmt.Errorf("Swapping new version bootlinks: %w", err)
		}
		newSubBootDir = sub
		bootloaderIsAtomic = true
	} else {
		queryCtx := &bootloader.Context{SysrootPath: s.Path, Repo: s.Repo}
		var err error
		bl, err = bootloader.Query(queryCtx)
		if err != nil {
			return err
		}
		bootloaderIsAtomic = bl != nil && bl.IsAtomic()
		sub, err := s.writeDeploymentsBootswap(newDeployments, bl)
		if err != nil {
			return err
		}
		newSubBootDir = sub
	}

	kind := "Bootloader updated"
	if bootloaderIsAtomic {
		kind = "Transaction complete"
	}
	swap := "no"
	if requiresNewBootVersion {
		swap = "yes"
	}
	blName := "none"
	if bl != nil {
		blName = bl.Name()
	}
	_ = blName
	msg := fmt.Sprintf("%s; bootconfig swap: %s; bootversion: %s, deployment count change: %d",
		kind, swap, newSubBootDir, len(newDeployments)-curLen)
	s.EmitMessage(msg)

	if err := s.writeDeploymentsFinish(); err != nil {
		return err
	}
	if opts.DoPostclean {
		if err := s.Cleanup(true); err != nil {
			return fmt.Errorf("Performing final cleanup: %w", err)
		}
	}
	return nil
}

// writeDeploymentsBootswap writes a fresh bootversion with all deployments.
func (s *Sysroot) writeDeploymentsBootswap(newDeployments []*Deployment,
	bl bootloader.Bootloader) (string, error) {
	newBootVersion := 0
	if s.BootVersion == 0 {
		newBootVersion = 1
	}
	entriesDir := filepath.Join(s.Path, fmt.Sprintf("boot/loader.%d/entries", newBootVersion))
	if err := os.RemoveAll(entriesDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(entriesDir, 0o755); err != nil {
		return "", err
	}
	showOsname := false
	for i := 1; i < len(newDeployments); i++ {
		if newDeployments[0].OsName != newDeployments[i].OsName {
			showOsname = true
			break
		}
	}
	for _, d := range newDeployments {
		if err := s.installDeploymentKernel(newBootVersion, d, len(newDeployments), showOsname); err != nil {
			return "", err
		}
	}
	if err := s.createNewBootlinks(newBootVersion, newDeployments); err != nil {
		return "", fmt.Errorf("Creating new current bootlinks: %w", err)
	}
	subName, err := s.swapBootlinks(newBootVersion)
	if err != nil {
		return "", fmt.Errorf("Swapping new version bootlinks: %w", err)
	}
	if bl != nil {
		configs, err := s.ReadLoaderConfigs(newBootVersion)
		if err != nil {
			return "", err
		}
		ctx := &bootloader.Context{
			SysrootPath:   s.Path,
			Repo:          s.Repo,
			BootVersion:   newBootVersion,
			LoaderConfigs: configs,
		}
		if err := bl.WriteConfig(ctx); err != nil {
			return "", fmt.Errorf("Bootloader write config: %w", err)
		}
	}
	if err := s.prepareNewBootloaderLink(s.BootVersion, newBootVersion); err != nil {
		return "", err
	}
	if err := s.fullSystemSync(); err != nil {
		return "", err
	}
	if err := s.swapBootloader(newBootVersion); err != nil {
		return "", err
	}
	return subName, nil
}

// prepareNewBootloaderLink stages the new boot/loader symlink.
func (s *Sysroot) prepareNewBootloaderLink(current, newBootVersion int) error {
	bootBoot := filepath.Join(s.Path, "boot/boot")
	if _, err := os.Lstat(bootBoot); errors.Is(err, fs.ErrNotExist) {
		if err := os.Symlink(".", bootBoot); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("Preparing final bootloader swap: %w", err)
		}
	}
	target := fmt.Sprintf("loader.%d", newBootVersion)
	if err := symlinkAtReplace(target, filepath.Join(s.Path, "boot/loader.tmp")); err != nil {
		return fmt.Errorf("Preparing final bootloader swap: %w", err)
	}
	return nil
}

// swapBootloader renames boot/loader.tmp into place.
func (s *Sysroot) swapBootloader(newBootVersion int) error {
	if err := os.Rename(filepath.Join(s.Path, "boot/loader.tmp"),
		filepath.Join(s.Path, "boot/loader")); err != nil {
		return fmt.Errorf("Final bootloader swap: %w", err)
	}
	return nil
}

// writeDeploymentsFinish bumps the deploy mtime and reloads the deployment
// list from disk (write_deployments_finish).
func (s *Sysroot) writeDeploymentsFinish() error {
	if err := s.BumpMtime(); err != nil {
		return err
	}
	if err := s.Load(); err != nil {
		return fmt.Errorf("Reloading deployments after commit: %w", err)
	}
	return s.cleanupLegacyCurrentSymlinks()
}

// cleanupLegacyCurrentSymlinks removes the obsolete ostree/deploy/OS/current
// symlinks.
func (s *Sysroot) cleanupLegacyCurrentSymlinks() error {
	for _, d := range s.Deployments {
		path := filepath.Join(s.Path, "ostree/deploy", d.OsName, "current")
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// installDeploymentKernel installs the kernel and writes the BLS entry.
func (s *Sysroot) installDeploymentKernel(newBootVersion int, d *Deployment, nDeployments int,
	showOsname bool) error {
	deployDir := d.DirPath(s.Path)
	layout, err := getKernelFromTree(s, deployDir)
	if err != nil {
		return fmt.Errorf("Installing kernel: %w", err)
	}
	if err := s.installOverlayInitrds(d); err != nil {
		return fmt.Errorf("Installing overlay initramfs: %w", err)
	}
	bootDir := filepath.Join(s.Path, "boot")
	bootCsumDir := filepath.Join(bootDir, "ostree", fmt.Sprintf("%s-%s", d.OsName, d.BootCsum))
	if err := os.MkdirAll(bootCsumDir, 0o775); err != nil {
		return fmt.Errorf("Installing kernel: %w", err)
	}
	installOne := func(srcName, destName string) error {
		if _, err := os.Lstat(filepath.Join(bootCsumDir, destName)); err == nil {
			return nil
		}
		return installIntoBoot(filepath.Join(layout.bootDir, srcName),
			filepath.Join(bootCsumDir, destName))
	}
	if err := installOne(layout.kernelSrc, layout.kernelName); err != nil {
		return fmt.Errorf("Installing kernel: %w", err)
	}
	if layout.initramfsSrc != "" {
		if err := installOne(layout.initramfsSrc, layout.initramfsName); err != nil {
			return err
		}
	}
	if layout.hasDevicetree {
		if layout.devicetreeName != "" {
			if err := installOne(layout.devicetreeSrc, layout.devicetreeName); err != nil {
				return err
			}
		} else {
			dst := filepath.Join(bootCsumDir, layout.devicetreeSrc)
			if err := copyTreeRecursive(filepath.Join(layout.bootDir, layout.devicetreeSrc), dst, true); err != nil {
				return err
			}
		}
	}
	if layout.hmacSrc != "" {
		if err := installOne(layout.hmacSrc, layout.hmacName); err != nil {
			return err
		}
	}

	// os-release title
	osReleasePath := filepath.Join(deployDir, "usr/lib/os-release")
	if _, err := os.Stat(osReleasePath); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		osReleasePath = filepath.Join(deployDir, "etc/os-release")
	}
	osRelease, err := readOsRelease(osReleasePath)
	if err != nil {
		return fmt.Errorf("Installing kernel: %w", err)
	}
	title := osRelease["PRETTY_NAME"]
	if title == "" {
		title = osRelease["ID"]
	}
	if title == "" {
		return fmt.Errorf("Installing kernel: No PRETTY_NAME or ID in /etc/os-release")
	}
	deploymentVersion := s.CommitVersion(d.Checksum)
	if deploymentVersion != "" && !strings.Contains(title, deploymentVersion) {
		title += " " + deploymentVersion
	}
	title += " (ostree"
	if showOsname {
		title += ":" + d.OsName
	}
	title += fmt.Sprintf(":%d)", d.Index)

	config := d.BootConfig
	if config == nil {
		config = bootconfig.New()
		d.BootConfig = config
	}
	config.Set("title", title)
	config.Set("version", strconv.Itoa(nDeployments-d.Index))

	bootprefix := "/"
	if bootloader.Bootprefix(s.Repo) {
		bootprefix = "/boot/"
	}
	bootCsumRel := fmt.Sprintf("ostree/%s-%s", d.OsName, d.BootCsum)
	config.Set("linux", bootprefix+bootCsumRel+"/"+layout.kernelName)

	kargs := bootconfig.FromString(config.Get("options"))
	if layout.initramfsName != "" {
		config.Set("initrd", bootprefix+bootCsumRel+"/"+layout.initramfsName)
		if len(d.OverlayInitrds) > 0 {
			overlays := make([]string, 0, len(d.OverlayInitrds))
			for _, csum := range d.OverlayInitrds {
				overlays = append(overlays, fmt.Sprintf("%s%s/%s.img", bootprefix,
					BootInitramfsOverlays, csum))
			}
			config.OverlayInitrds = overlays
		}
	} else {
		config.Delete("initrd")
		config.OverlayInitrds = nil
		kargs.Replace(fmt.Sprintf(
			"init=/ostree/boot.%d/%s/%s/%d/usr/lib/ostree/ostree-prepare-root",
			newBootVersion, d.OsName, d.BootCsum, d.BootSerial))
	}
	if layout.hasDevicetree {
		if layout.devicetreeName != "" {
			config.Set("devicetree", bootprefix+bootCsumRel+"/"+layout.devicetreeName)
		} else {
			config.Set("fdtdir", bootprefix+bootCsumRel+"/"+layout.devicetreeSrc)
		}
	}
	kargs.Replace(fmt.Sprintf("ostree=/ostree/boot.%d/%s/%s/%d",
		newBootVersion, d.OsName, d.BootCsum, d.BootSerial))
	config.Set("options", kargs.String())

	// Only append extension keys to non-default deployments.
	if d.Index != 0 {
		for _, kv := range s.blsAppendValues() {
			if i := strings.IndexByte(kv, '='); i > 0 {
				config.Set(kv[:i], kv[i+1:])
			}
		}
	}

	bootCounting := s.bootCountingTries()
	name := bootconfig.EntryFilename("ostree", nDeployments, d.Index, bootCounting, config)
	entriesDir := filepath.Join(s.Path, fmt.Sprintf("boot/loader.%d/entries", newBootVersion))
	if err := config.WriteFile(filepath.Join(entriesDir, name)); err != nil {
		return err
	}
	return nil
}

// blsAppendValues parses sysroot.bls-append-except-default.
func (s *Sysroot) blsAppendValues() []string {
	v, err := s.Repo.Config().Get("sysroot", "bls-append-except-default")
	if err != nil || v == "" {
		return nil
	}
	var out []string
	for _, item := range strings.Split(v, ";") {
		if item == "" {
			continue
		}
		if !strings.Contains(item, "=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

// bootCountingTries returns sysroot.boot-counting-tries.
func (s *Sysroot) bootCountingTries() uint {
	v, err := s.Repo.Config().Get("sysroot", "boot-counting-tries")
	if err != nil || v == "" {
		return 0
	}
	n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
	if err != nil || n > 5 {
		return 0
	}
	return uint(n)
}

// readOsRelease parses an os-release file.
func readOsRelease(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("Reading /etc/os-release: %w", err)
	}
	out := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		out[line[:eq]] = shellUnquote(line[eq+1:])
	}
	return out, nil
}

// shellUnquote removes shell quoting from a value.
func shellUnquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			quote := s[0]
			body := s[1 : len(s)-1]
			var b strings.Builder
			for i := 0; i < len(body); i++ {
				c := body[i]
				if c == '\\' && quote == '"' && i+1 < len(body) {
					switch body[i+1] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '"', '\\', '$', '`':
						b.WriteByte(body[i+1])
					default:
						b.WriteByte(c)
						b.WriteByte(body[i+1])
					}
					i++
					continue
				}
				b.WriteByte(c)
			}
			return b.String()
		}
	}
	return s
}

// AssignIndexes numbers the deployments in list order.
func AssignIndexes(deployments []*Deployment) {
	for i, d := range deployments {
		d.Index = i
	}
}

var _ = repo.ModeBare
