// Package bootloader implements the bootloader backends OSTree knows how to
// update: syslinux, U-Boot and GRUB2.  The "none"/"auto" settings are handled
// by Query, which returns a nil Bootloader when no backend is active.
package bootloader

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/myml/ostree-go/pkg/bootconfig"
	"github.com/myml/ostree-go/pkg/repo"
)

// Context is everything a backend needs in order to regenerate its
// configuration for a new bootversion.
type Context struct {
	// SysrootPath is the absolute sysroot path.
	SysrootPath string
	// Repo is the sysroot repository (for sysroot.* configuration).
	Repo *repo.Repo
	// BootVersion is the *new* bootversion being written.
	BootVersion int
	// LoaderConfigs are the BLS entries of the new bootversion, already
	// sorted the way libostree sorts them.
	LoaderConfigs []*bootconfig.Config
}

// Bootloader is one backend.
type Bootloader interface {
	// Name is the human readable name, e.g. "syslinux".
	Name() string
	// IsAtomic reports whether the backend updates its config atomically.
	IsAtomic() bool
	// WriteConfig regenerates the on-disk configuration.
	WriteConfig(ctx *Context) error
}

// Bootdir returns the sysroot's boot directory.
func (c *Context) Bootdir() string { return filepath.Join(c.SysrootPath, "boot") }

// configString returns a sysroot.* config value, or def.
func configString(r *repo.Repo, key string, def string) string {
	if r == nil {
		return def
	}
	v, err := r.Config().Get("sysroot", key)
	if err != nil || v == "" {
		return def
	}
	return v
}

func configBool(r *repo.Repo, key string, def bool) bool {
	if r == nil {
		return def
	}
	v, err := r.Config().GetBool("sysroot", key, def)
	if err != nil {
		return def
	}
	return v
}

// Bootprefix reports whether sysroot.bootprefix is enabled.
func Bootprefix(r *repo.Repo) bool {
	return configBool(r, "bootprefix", false)
}

var bootloaderOpts = []string{"auto", "none", "grub2", "syslinux", "uboot", "zipl", "aboot"}

// Validate checks the sysroot.bootloader configuration value.
func Validate(r *repo.Repo) (string, error) {
	value := configString(r, "bootloader", "auto")
	for _, opt := range bootloaderOpts {
		if value == opt {
			return value, nil
		}
	}
	return "", fmt.Errorf("Invalid bootloader configuration: '%s'", value)
}

// Query resolves the configured bootloader for ctx.  A nil Bootloader with a
// nil error means "no bootloader to update".
func Query(ctx *Context) (Bootloader, error) {
	value, err := Validate(ctx.Repo)
	if err != nil {
		return nil, err
	}
	switch value {
	case "none":
		return nil, nil
	case "auto":
		for _, name := range []string{"syslinux", "grub2", "uboot"} {
			bl := newBackend(name, ctx.SysrootPath)
			active, err := bl.query(ctx)
			if err != nil {
				return nil, err
			}
			if active {
				return bl, nil
			}
		}
		return nil, nil
	default:
		bl := newBackend(value, ctx.SysrootPath)
		if bl == nil {
			return nil, nil
		}
		return bl, nil
	}
}

type backend struct {
	name string
	path string
	// grub2-only state
	efiConfigPath string
	isEFI         bool
}

func newBackend(name, path string) *backend {
	switch name {
	case "syslinux", "grub2", "uboot":
		return &backend{name: name, path: path}
	}
	return nil
}

func (b *backend) Name() string {
	if b.name == "uboot" {
		return "U-Boot"
	}
	return b.name
}

func (b *backend) IsAtomic() bool {
	if b.name == "grub2" {
		return !b.isEFI
	}
	return true
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (b *backend) query(ctx *Context) (bool, error) {
	switch b.name {
	case "syslinux":
		return exists(filepath.Join(b.path, "boot/syslinux/syslinux.cfg")), nil
	case "uboot":
		return exists(filepath.Join(b.path, "boot/loader/uEnv.txt")), nil
	case "grub2":
		return b.queryGrub2()
	}
	return false, nil
}

func (b *backend) queryGrub2() (bool, error) {
	bootupd := filepath.Join(b.path, "boot/bootupd-state.json")
	if data, err := os.ReadFile(bootupd); err == nil {
		contents := string(data)
		if strings.Contains(contents, `"static-configs"`) &&
			!strings.Contains(contents, `"static-configs":null`) {
			return false, nil
		}
	}
	if exists(filepath.Join(b.path, "boot/grub/grub.cfg")) ||
		exists(filepath.Join(b.path, "boot/grub2/grub.cfg")) {
		return true, nil
	}
	efiBase := filepath.Join(b.path, "boot/efi/EFI")
	entries, err := os.ReadDir(efiBase)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if e.Name() == "BOOT" || !e.IsDir() {
			continue
		}
		cfg := filepath.Join(efiBase, e.Name(), "grub.cfg")
		if exists(cfg) {
			b.efiConfigPath = cfg
			b.isEFI = true
			return true, nil
		}
	}
	return false, nil
}

func (b *backend) WriteConfig(ctx *Context) error {
	switch b.name {
	case "syslinux":
		return b.writeSyslinux(ctx)
	case "uboot":
		return b.writeUboot(ctx)
	case "grub2":
		return b.writeGrub2(ctx)
	}
	return fmt.Errorf("unsupported bootloader %q", b.name)
}

// readFileAllowNoent reads path, returning ok=false when it is missing.
func readFileAllowNoent(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(data), true, nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
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
	return os.Rename(name, path)
}

// joinLines mirrors _ostree_sysroot_join_lines(): every line gets a newline,
// and blank lines are collapsed (no leading blank, no doubled blanks).
func joinLines(lines []string) string {
	var b strings.Builder
	prevWasEmpty := false
	for i, line := range lines {
		if line == "" {
			if prevWasEmpty || i == 0 {
				continue
			}
			prevWasEmpty = true
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// --- syslinux ---

func (b *backend) writeSyslinux(ctx *Context) error {
	configPath := filepath.Join(b.path, "boot/syslinux/syslinux.cfg")
	contents, ok, err := readFileAllowNoent(configPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("opening %s: %w", configPath, fs.ErrNotExist)
	}

	var newLines, tmpLines []string
	var kernelArg string
	sawDefault := false
	regenerateDefault := false
	parsingLabel := false

	lines := strings.Split(contents, "\n")
	// Iterate one extra time with an empty terminator so the last LABEL is
	// flushed, exactly like the C loop over a NULL-terminated vector.
	for _, line := range append(lines, "\x00") {
		eol := line == "\x00"
		skip := false

		if parsingLabel && (eol || !strings.HasPrefix(line, "\t")) {
			parsingLabel = false
			if kernelArg == "" {
				return fmt.Errorf("No KERNEL argument found after LABEL")
			}
			if !strings.HasPrefix(kernelArg, "/ostree/") &&
				!strings.HasPrefix(kernelArg, "/boot/ostree/") {
				newLines = append(newLines, tmpLines...)
			}
			tmpLines = nil
			kernelArg = ""
		}
		if eol {
			break
		}
		switch {
		case !parsingLabel && strings.HasPrefix(line, "LABEL "):
			parsingLabel = true
			tmpLines = nil
		case parsingLabel && strings.HasPrefix(line, "\tKERNEL "):
			kernelArg = line[len("\tKERNEL "):]
		case !parsingLabel && strings.HasPrefix(line, "DEFAULT "):
			sawDefault = true
			if strings.HasPrefix(line, "DEFAULT ostree:") || strings.Contains(line, "(ostree") {
				regenerateDefault = true
			}
			skip = true
		}
		if skip {
			continue
		}
		if parsingLabel {
			tmpLines = append(tmpLines, line)
		} else {
			newLines = append(newLines, line)
		}
	}
	if !sawDefault {
		regenerateDefault = true
	}

	for i, config := range ctx.LoaderConfigs {
		title := config.Get("title")
		if title == "" {
			title = "(Untitled)"
		}
		if regenerateDefault && i == 0 {
			newLines = append(newLines, "DEFAULT "+title)
		}
		newLines = append(newLines, "LABEL "+title)
		linux := config.Get("linux")
		if linux == "" {
			return fmt.Errorf(`No "linux" key in bootloader config`)
		}
		prefix := "/boot"
		if Bootprefix(ctx.Repo) {
			prefix = ""
		}
		newLines = append(newLines, "\tKERNEL "+prefix+linux)
		if v := config.Get("initrd"); v != "" {
			newLines = append(newLines, "\tINITRD "+prefix+v)
		}
		if v := config.Get("devicetree"); v != "" {
			newLines = append(newLines, "\tDEVICETREE "+prefix+v)
		}
		if v := config.Get("options"); v != "" {
			newLines = append(newLines, "\tAPPEND "+v)
		}
	}

	newPath := filepath.Join(b.path, fmt.Sprintf("boot/loader.%d/syslinux.cfg", ctx.BootVersion))
	return writeFileAtomic(newPath, []byte(joinLines(newLines)), 0o644)
}

// --- U-Boot ---

func (b *backend) writeUboot(ctx *Context) error {
	configPath := filepath.Join(b.path, "boot/loader/uEnv.txt")
	_, ok, err := readFileAllowNoent(configPath)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("opening %s: %w", configPath, fs.ErrNotExist)
	}

	var newLines []string
	for i, config := range ctx.LoaderConfigs {
		suffix := ""
		if i > 0 {
			suffix = fmt.Sprintf("%d", i+1)
		}
		linux := config.Get("linux")
		if linux == "" {
			return fmt.Errorf(`No "linux" key in bootloader config`)
		}
		newLines = append(newLines, "kernel_image"+suffix+"=/boot"+linux)
		if v := config.Get("initrd"); v != "" {
			newLines = append(newLines, "ramdisk_image"+suffix+"=/boot"+v)
		}
		if v := config.Get("devicetree"); v != "" {
			newLines = append(newLines, "fdt_file"+suffix+"=/boot"+v)
		}
		if v := config.Get("fdtdir"); v != "" {
			newLines = append(newLines, "fdtdir"+suffix+"=/boot"+v)
		}
		if v := config.Get("options"); v != "" {
			newLines = append(newLines, "bootargs"+suffix+"="+v)
			if i == 0 {
				// Append the system's own uEnv.txt from the deployment.
				kargs := bootconfig.FromString(v)
				ostreeArg, _ := kargs.GetLastValue("ostree")
				if ostreeArg == "" {
					return fmt.Errorf("No ostree= kernel argument found in boot loader configuration file")
				}
				uenvPath := filepath.Join(b.path, ostreeArg+"/usr/lib/ostree-boot/uEnv.txt")
				uenv, ok, err := readFileAllowNoent(uenvPath)
				if err != nil {
					return err
				}
				if ok {
					newLines = append(newLines, uenv)
				}
			}
		}
	}

	newPath := filepath.Join(b.path, fmt.Sprintf("boot/loader.%d/uEnv.txt", ctx.BootVersion))
	return writeFileAtomic(newPath, []byte(joinLines(newLines)), 0o644)
}

// --- GRUB2 ---

func (b *backend) writeGrub2(ctx *Context) error {
	grubExec := os.Getenv("OSTREE_GRUB2_EXEC")
	useSystem := true
	if grubExec == "" {
		grubExec = "grub2-mkconfig"
	} else if !strings.HasSuffix(grubExec, "/grub2-mkconfig") {
		useSystem = false
	}
	_ = useSystem

	var newConfigPath string
	if b.isEFI {
		newConfigPath = filepath.Join(filepath.Dir(b.efiConfigPath), "grub.cfg.new")
		_ = os.Remove(newConfigPath)
	} else {
		newConfigPath = filepath.Join(b.path, fmt.Sprintf("boot/loader.%d/grub.cfg", ctx.BootVersion))
	}
	if err := os.MkdirAll(filepath.Dir(newConfigPath), 0o755); err != nil {
		return err
	}

	cmd := exec.Command(grubExec, "-o", newConfigPath)
	cmd.Env = append(os.Environ(), fmt.Sprintf("_OSTREE_GRUB2_BOOTVERSION=%d", ctx.BootVersion))
	if b.isEFI {
		cmd.Env = append(cmd.Env, "_OSTREE_GRUB2_IS_EFI=1")
	}
	if os.Getenv("INVOCATION_ID") == "" && os.Getenv("OSTREE_DEBUG_GRUB2") == "" {
		// Match the C code, which discards output outside of systemd.
		devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err == nil {
			defer devnull.Close()
			cmd.Stdout = devnull
			cmd.Stderr = devnull
		}
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %v", grubExec, err)
	}
	if f, err := os.Open(newConfigPath); err == nil {
		_ = f.Sync()
		f.Close()
	} else {
		return err
	}

	if b.isEFI {
		oldPath := filepath.Join(filepath.Dir(b.efiConfigPath), "grub.cfg.old")
		if data, err := os.ReadFile(b.efiConfigPath); err == nil {
			if err := os.WriteFile(oldPath, data, 0o644); err != nil {
				return err
			}
		}
		_ = os.Remove(b.efiConfigPath)
		if err := os.Rename(newConfigPath, b.efiConfigPath); err != nil {
			return err
		}
	}
	return nil
}
