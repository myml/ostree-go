package repo

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// CreateOptions are optional parameters for Create. Libostree only accepts a
// collection ID at repository creation time; everything else is written to the
// config later.
type CreateOptions struct {
	CollectionID string
}

// Repo is an open OSTree repository.
//
// All file access goes through an *os.Root, so paths constructed from
// repository data cannot escape the repository directory. The repository
// layout itself follows libostree: objects/ is the marker for an existing
// repository, and the lock lives in <repo>/.lock.
type Repo struct {
	path string
	root *os.Root

	mode        Mode
	writable    bool
	writableErr error
	ownerUID    int

	config     *Config
	parent     *Repo
	parentOnce sync.Once
	// parentErr records why the parent repository could not be opened.
	parentErr error

	zlibLevel int

	stagedirPrefix string

	lockMu        sync.Mutex
	lockFile      *os.File
	lockShared    int
	lockExclusive int
	// lockMode mirrors libostree's lock_timeout_seconds:
	// -2 disabled, -1 blocking, >= 0 seconds to poll.
	lockMode int

	closed bool
}

const (
	lockDisabled = -2
	lockBlocking = -1
)

// Repository layout creation order, from repo_create_at_internal(). objects/
// is deliberately last: its existence is the "repository already exists"
// marker.
var createDirs = []string{
	"tmp",
	"extensions",
	"state",
	"refs",
	"refs/heads",
	"refs/mirrors",
	"refs/remotes",
	"objects",
}

const (
	// accessWOK is W_OK from <unistd.h>; Go does not export it.
	accessWOK = 2

	dirMode    os.FileMode = 0o775
	fileMode   os.FileMode = 0o644
	objDirMode os.FileMode = 0o777
)

// Open opens an existing repository. The only mandatory layout element is
// objects/; if config is missing, an implicit bare/repo_version=1 config is
// used. A present config is parsed strictly (invalid modes or repo versions
// are errors).
func Open(p string) (*Repo, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}
	objectsPath := filepath.Join(abs, "objects")
	st, err := os.Stat(objectsPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s has no objects directory", ErrNotARepo, abs)
		}
		return nil, fmt.Errorf("stat %s: %w", objectsPath, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNotARepo, objectsPath)
	}

	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	r := &Repo{
		path:           abs,
		root:           root,
		mode:           ModeBare,
		ownerUID:       statOwnerUID(st),
		stagedirPrefix: stagingPrefix(),
	}
	if err := syscall.Access(objectsPath, accessWOK); err != nil {
		r.writable = false
		r.writableErr = fmt.Errorf("%w: %v", ErrReadOnly, err)
	} else {
		r.writable = true
	}

	cfg, err := r.readConfig()
	if err != nil {
		root.Close()
		return nil, err
	}
	r.config = cfg
	mode, err := modeFromConfig(cfg)
	if err != nil {
		root.Close()
		return nil, err
	}
	r.mode = mode
	if err := r.applyConfig(cfg); err != nil {
		root.Close()
		return nil, err
	}

	if r.writable {
		// libostree recreates tmp/ on every open to be friendly to people
		// reclaiming space.
		if err := r.mkdirAll("tmp", dirMode); err != nil {
			root.Close()
			return nil, err
		}
	}
	return r, nil
}

// readConfig reads config, synthesizing a bare/version 1 config if the file is
// absent. Other read errors are returned.
func (r *Repo) readConfig() (*Config, error) {
	data, err := r.readRootFile("config")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c := NewConfig()
			c.Set("core", "repo_version", "1")
			c.Set("core", "mode", ModeBare.ConfigString())
			return c, nil
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func modeFromConfig(c *Config) (Mode, error) {
	if version, err := c.GetValue("core", "repo_version"); err == nil {
		if version != "1" {
			return 0, fmt.Errorf("%w: invalid repository version %q", ErrInvalidConfig, version)
		}
	} else if !errors.Is(err, ErrKeyNotFound) && !errors.Is(err, ErrGroupNotFound) {
		return 0, err
	}
	legacyArchive, err := c.GetBool("core", "archive", false)
	if err != nil {
		return 0, err
	}
	if legacyArchive {
		return 0, fmt.Errorf("%w: this version of OSTree no longer supports %q repositories; use archive-z2 instead", ErrInvalidMode, "archive")
	}
	modeStr := ModeBare.ConfigString()
	if v, err := c.GetValue("core", "mode"); err == nil {
		modeStr = trimConfigValue(v)
	} else if !errors.Is(err, ErrKeyNotFound) && !errors.Is(err, ErrGroupNotFound) {
		return 0, err
	}
	return ParseMode(modeStr)
}

// applyConfig refreshes cached settings derived from the config file.
func (r *Repo) applyConfig(c *Config) error {
	r.zlibLevel = 6
	if raw, err := c.GetValue("archive", "zlib-level"); err == nil {
		if level, convErr := strconv.Atoi(trimConfigValue(raw)); convErr == nil {
			if level < 1 {
				level = 1
			} else if level > 9 {
				level = 9
			}
			r.zlibLevel = level
		}
	} else if !errors.Is(err, ErrKeyNotFound) && !errors.Is(err, ErrGroupNotFound) {
		return err
	}

	locking, err := c.GetBool("core", "locking", true)
	if err != nil {
		return err
	}
	if !locking {
		r.lockMode = lockDisabled
		return nil
	}
	timeout := int64(300)
	if raw, err := c.GetValue("core", "lock-timeout-secs"); err == nil {
		// libostree uses g_ascii_strtoll(), which yields 0 for garbage.
		timeout, _ = strconv.ParseInt(trimConfigValue(raw), 10, 64)
	} else if !errors.Is(err, ErrKeyNotFound) && !errors.Is(err, ErrGroupNotFound) {
		return err
	}
	switch {
	case timeout <= lockDisabled:
		r.lockMode = lockDisabled
	case timeout == lockBlocking:
		r.lockMode = lockBlocking
	default:
		if timeout > int64(^uint(0)>>1) {
			timeout = int64(^uint(0) >> 1)
		}
		r.lockMode = int(timeout)
	}
	return nil
}

// Create creates a repository at path. If objects/ already exists, the
// existing repository is opened and returned unchanged (libostree's early
// return). Directories are created in libostree order with mode 0775 and
// config is written with mode 0660; both are subject to umask.
func Create(p string, mode Mode, opts *CreateOptions) (*Repo, error) {
	if !mode.Valid() {
		return nil, fmt.Errorf("%w: %d", ErrInvalidMode, int(mode))
	}
	if opts != nil && opts.CollectionID != "" {
		if err := ValidateCollectionID(opts.CollectionID); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return nil, err
	}

	objectsPath := filepath.Join(abs, "objects")
	if st, err := os.Stat(objectsPath); err == nil {
		if !st.IsDir() {
			return nil, fmt.Errorf("%w: %s is not a directory", ErrNotARepo, objectsPath)
		}
		return Open(abs)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat %s: %w", objectsPath, err)
	}

	if err := os.Mkdir(abs, dirMode); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrNotARepo, abs)
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	r := &Repo{
		path:           abs,
		root:           root,
		mode:           mode,
		writable:       true,
		ownerUID:       statOwnerUID(st),
		config:         NewConfig(),
		zlibLevel:      6,
		stagedirPrefix: stagingPrefix(),
	}
	defer func() {
		if r.root != nil {
			r.root.Close()
			r.root = nil
		}
	}()

	if _, err := root.Stat("config"); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("stat config: %w", err)
		}
		if err := r.atomicWrite("config", createConfigBytes(mode, opts), fileMode); err != nil {
			return nil, fmt.Errorf("write config: %w", err)
		}
	}

	for _, dir := range createDirs {
		if err := r.ensureDir(dir, dirMode); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}

	r.root.Close()
	r.root = nil
	return Open(abs)
}

func createConfigBytes(mode Mode, opts *CreateOptions) []byte {
	var b strings.Builder
	b.WriteString("[core]\nrepo_version=1\n")
	b.WriteString("mode=")
	b.WriteString(mode.ConfigString())
	b.WriteByte('\n')
	if opts != nil && opts.CollectionID != "" {
		b.WriteString("collection-id=")
		b.WriteString(opts.CollectionID)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Path returns the absolute repository path.
func (r *Repo) Path() string { return r.path }

// Root returns the repository's os.Root. The root is owned by the Repo and is
// closed by Close; callers must not close it.
func (r *Repo) Root() *os.Root { return r.root }

// Mode returns the repository mode read from config.
func (r *Repo) Mode() Mode { return r.mode }

// Writable reports whether objects/ is writable.
func (r *Repo) Writable() bool { return r.writable }

// WritableError returns the error that made the repository read-only, if any.
func (r *Repo) WritableError() error { return r.writableErr }

// OwnerUID returns the uid owning objects/, used by bare-user handling in
// higher layers.
func (r *Repo) OwnerUID() int { return r.ownerUID }

// Parent returns the repository named by core.parent, or nil.
func (r *Repo) Parent() *Repo {
	r.parentOnce.Do(func() {
		parentPath, err := r.config.Get("core", "parent")
		if err != nil || parentPath == "" {
			return
		}
		p, err := Open(parentPath)
		if err != nil {
			r.parentErr = err
			return
		}
		r.parent = p
	})
	return r.parent
}

// Config returns the cached configuration. Callers may mutate it and then call
// WriteConfig to persist it (write_config_and_reload semantics); use Clone to
// avoid mutating the cached document.
func (r *Repo) Config() *Config { return r.config }

// CloneConfig returns a deep copy of the cached configuration.
func (r *Repo) CloneConfig() *Config { return r.config.Clone() }

// ReloadConfig re-reads config from disk and refreshes derived settings.
func (r *Repo) ReloadConfig() error {
	cfg, err := r.readConfig()
	if err != nil {
		return err
	}
	mode, err := modeFromConfig(cfg)
	if err != nil {
		return err
	}
	if err := r.applyConfig(cfg); err != nil {
		return err
	}
	r.config = cfg
	r.mode = mode
	return nil
}

// WriteConfig atomically writes cfg as the repository config, validates it and
// updates the cached config and mode, like
// ostree_repo_write_config_and_reload().
func (r *Repo) WriteConfig(cfg *Config) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	mode, err := modeFromConfig(cfg)
	if err != nil {
		return err
	}
	// Validate derived settings before replacing the file, mirroring
	// ostree_repo_write_config_and_reload()'s test reload.
	if err := r.applyConfig(cfg); err != nil {
		return err
	}
	if err := r.atomicWrite("config", cfg.Bytes(), fileMode); err != nil {
		return err
	}
	r.config = cfg.Clone()
	r.mode = mode
	return nil
}

// Close releases the lock file and the repository root. It is safe to call
// more than once.
func (r *Repo) Close() error {
	r.lockMu.Lock()
	if r.lockFile != nil {
		_ = unlockFD(r.lockFile.Fd())
		_ = r.lockFile.Close()
		r.lockFile = nil
		r.lockShared = 0
		r.lockExclusive = 0
	}
	closed := r.closed
	r.closed = true
	var root *os.Root
	if !closed {
		root = r.root
		r.root = nil
	}
	r.lockMu.Unlock()
	if root != nil {
		return root.Close()
	}
	return nil
}

// mkdirAll creates a directory relative to the repository root.
func (r *Repo) mkdirAll(rel string, perm os.FileMode) error {
	if rel == "" || rel == "." {
		return nil
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if st, err := r.root.Stat(rel); err == nil {
		if st.IsDir() {
			return nil
		}
		return fmt.Errorf("%s: not a directory", rel)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := path.Dir(rel)
	if parent != "." && parent != "/" {
		if err := r.mkdirAll(parent, perm); err != nil {
			return err
		}
	}
	if err := r.root.Mkdir(rel, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return nil
}

// ensureDir creates one directory (not its parents) and verifies that an
// existing entry is a directory.
func (r *Repo) ensureDir(rel string, perm os.FileMode) error {
	if st, err := r.root.Stat(rel); err == nil {
		if !st.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", rel)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.root.Mkdir(rel, perm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			st, statErr := r.root.Stat(rel)
			if statErr != nil {
				return statErr
			}
			if !st.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", rel)
			}
			return nil
		}
		return err
	}
	return nil
}

// atomicWrite creates a temporary file next to rel, writes data, fsyncs it and
// renames it over rel. The file is created with perm (subject to umask).
func (r *Repo) atomicWrite(rel string, data []byte, perm os.FileMode) error {
	if r.root == nil {
		return errors.New("repository is closed")
	}
	dir := path.Dir(rel)
	base := path.Base(rel)
	if dir != "." {
		if err := r.mkdirAll(dir, dirMode); err != nil {
			return err
		}
	}
	var (
		f   *os.File
		tmp string
		err error
	)
	for i := 0; i < 100; i++ {
		tmp = path.Join(dir, ".tmp-"+base+"-"+randomHex(8))
		f, err = r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	if f == nil {
		return fmt.Errorf("could not create a temporary file next to %s", rel)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		r.root.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		r.root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		r.root.Remove(tmp)
		return err
	}
	if err := r.renameRootPath(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return err
	}
	return nil
}

// readRootFile reads a file relative to the repository root.
//
// os.Root.ReadFile was added after Go 1.24, so use Open + io.ReadAll to keep
// the package buildable (and vet-clean) with the module's go directive while
// still getting os.Root's traversal protection.
func (r *Repo) readRootFile(rel string) ([]byte, error) {
	f, err := r.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// rootAbsPath maps a validated repository-relative path to an absolute path.
func (r *Repo) rootAbsPath(rel string) string {
	return filepath.Join(r.path, filepath.FromSlash(rel))
}

// renameRootPath atomically renames two paths relative to the repository
// root. Go 1.24's os.Root has no Rename method; all callers pass paths built
// from validated checksums, ref names or fixed layout names, so no traversal
// is possible.
func (r *Repo) renameRootPath(oldRel, newRel string) error {
	return os.Rename(r.rootAbsPath(oldRel), r.rootAbsPath(newRel))
}

// removeAllRootPath removes a repository-relative path recursively. As with
// renameRootPath, callers pass validated paths.
func (r *Repo) removeAllRootPath(rel string) error {
	return os.RemoveAll(r.rootAbsPath(rel))
}

// stagingPrefix returns the prefix used for files in tmp/, mirroring
// OSTREE_REPO_TMPDIR_STAGING + boot id.
func stagingPrefix() string {
	id := os.Getenv("OSTREE_BOOTID")
	if id == "" {
		if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
			id = strings.TrimSpace(string(data))
		}
	}
	if id == "" {
		id = "unknown"
	}
	return "staging-" + id + "-"
}

func statOwnerUID(fi os.FileInfo) int {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid)
	}
	return 0
}
