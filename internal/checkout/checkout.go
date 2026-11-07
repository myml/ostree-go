// Package checkout writes commit trees to the filesystem.
package checkout

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/fsutil"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/tree"
)

// Options controls Checkout.
type Options struct {
	Repo        *repo.Repo
	Commit      string
	Destination string
	Subpath     string
	UserMode    bool
	ForceCopy   bool
	Overwrite   bool
	Union       bool
	// UnionMode is 0 (off), 1 (overwrite), 2 (add only) or 3 (identical only).
	UnionMode int
	// BareUserOnlyDirs suppresses mode bits outside of 0775 on directories.
	BareUserOnlyDirs bool
	// AllowNoent silently ignores a missing subpath.
	AllowNoent bool
	// DisableCache skips the uncompressed object cache for archive repos.
	DisableCache bool
	// ProcessPassthroughWhiteouts converts .ostree-wh.* files into character
	// devices 0:0, as overlayfs uses.
	ProcessPassthroughWhiteouts bool
	Whiteouts                   bool
	// RequireHardlinks makes a failed hardlink fatal.
	RequireHardlinks bool
}

// Checkout checks out the given commit into Destination.
func Checkout(opts Options) error {
	if opts.Repo == nil {
		return fmt.Errorf("no repository")
	}
	csum, err := tree.ResolveRef(opts.Repo, opts.Commit)
	if err != nil {
		return err
	}
	commit, err := tree.LoadCommit(opts.Repo, csum)
	if err != nil {
		return err
	}
	treeCsum := commit.RootTree
	var subFile *object.FileEntry
	if opts.Subpath != "" {
		sub, meta, f, err := resolveSubpath(opts.Repo, treeCsum, opts.Subpath)
		if err != nil {
			if opts.AllowNoent && errors.Is(err, errSubpathNotFound) {
				return nil
			}
			return err
		}
		if f != nil {
			subFile = f
		} else {
			treeCsum = sub
			_ = meta
		}
	}
	dest := opts.Destination
	if dest == "" {
		dest = "."
	}
	if err := os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	if opts.UnionMode == 3 && !opts.RequireHardlinks {
		return fmt.Errorf("--union-identical requires --require-hardlinks")
	}
	if opts.RequireHardlinks {
		switch opts.Repo.Mode() {
		case repo.ModeBare:
			if opts.UserMode {
				return fmt.Errorf("Bare repository mode cannot hardlink in user mode")
			}
		case repo.ModeBareUser:
			if !opts.UserMode {
				return fmt.Errorf("User repository mode requires --user-mode to hardlink")
			}
		}
	}
	c := &checkout{opts: opts, dest: dest}
	metaCsum := commit.RootMeta
	if opts.Subpath != "" && subFile == nil {
		metaCsum, err = findSubtreeMeta(opts.Repo, commit.RootTree, opts.Subpath)
		if err != nil {
			return err
		}
	}
	if subFile != nil {
		return c.writeFile("", *subFile)
	}
	return c.writeDir(treeCsum, metaCsum, "")
}

var errSubpathNotFound = errors.New("subpath not found")

// resolveSubpath walks a subpath and returns either a directory (tree and
// metadata checksums) or a single file entry.
func resolveSubpath(r *repo.Repo, root, subpath string) (string, string, *object.FileEntry, error) {
	parts := splitPath(subpath)
	cur := root
	for i, part := range parts {
		t, err := tree.LoadDirtree(r, cur)
		if err != nil {
			return "", "", nil, err
		}
		last := i == len(parts)-1
		foundDir := ""
		foundMeta := ""
		for _, d := range t.Dirs {
			if d.Name == part {
				foundDir, foundMeta = d.Tree, d.Meta
				break
			}
		}
		if foundDir != "" {
			if last {
				return foundDir, foundMeta, nil, nil
			}
			cur = foundDir
			continue
		}
		if last {
			for fi := range t.Files {
				if t.Files[fi].Name == part {
					f := t.Files[fi]
					return "", "", &f, nil
				}
			}
		}
		return "", "", nil, fmt.Errorf("%w: No such file or directory: %s", errSubpathNotFound, subpath)
	}
	return "", "", nil, fmt.Errorf("%w: No such file or directory: %s", errSubpathNotFound, subpath)
}

func findSubtree(r *repo.Repo, root string, subpath string) (string, error) {
	parts := splitPath(subpath)
	cur := root
	for _, part := range parts {
		t, err := tree.LoadDirtree(r, cur)
		if err != nil {
			return "", err
		}
		found := ""
		for _, d := range t.Dirs {
			if d.Name == part {
				found = d.Tree
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("No such subpath: %s", subpath)
		}
		cur = found
	}
	return cur, nil
}

type checkout struct {
	opts Options
	dest string
}

func (c *checkout) writeDir(treeCsum, metaCsum, rel string) error {
	t, err := tree.LoadDirtree(c.opts.Repo, treeCsum)
	if err != nil {
		return err
	}
	meta, err := tree.LoadDirmeta(c.opts.Repo, metaCsum)
	if err != nil {
		return err
	}
	dir := filepath.Join(c.dest, rel)
	if rel != "" {
		// If a non-directory is in the way (e.g. from an earlier union
		// checkout), replace it.
		if st, err := os.Lstat(dir); err == nil && !st.IsDir() {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
		// Create with owner-only permissions while populating, then apply
		// the real mode below like libostree does.
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}

	for _, f := range t.Files {
		if strings.HasPrefix(f.Name, ".ostree-wh.") && c.opts.ProcessPassthroughWhiteouts {
			if err := c.mknodWhiteout(dir, f); err != nil {
				return err
			}
			continue
		}
		if isWhiteout(f.Name) && c.opts.Whiteouts {
			if err := c.processWhiteout(dir, f.Name); err != nil {
				return err
			}
			continue
		}
		if err := c.writeFile(rel, f); err != nil {
			return err
		}
	}
	for _, d := range t.Dirs {
		if err := c.writeDir(d.Tree, d.Meta, filepath.Join(rel, d.Name)); err != nil {
			return err
		}
	}

	if rel != "" {
		mode := meta.Mode
		if c.opts.Repo.Mode() == repo.ModeBareUserOnly {
			// User-only repositories canonicalize directories and never
			// leave them world-writable.
			mode &^= 0002
			if c.opts.BareUserOnlyDirs {
				mode &^= 0020
			}
		} else if c.opts.BareUserOnlyDirs {
			mode &^= 0002
		}
		if err := c.applyMeta(dir, mode, meta.UID, meta.GID, meta.Xattrs); err != nil {
			return err
		}
	}
	return nil
}

// findSubtreeMeta returns the dirmeta checksum of a subpath.
func findSubtreeMeta(r *repo.Repo, root, subpath string) (string, error) {
	parts := splitPath(subpath)
	cur, curMeta := root, ""
	parentCommitTree := root
	_ = parentCommitTree
	for _, part := range parts {
		t, err := tree.LoadDirtree(r, cur)
		if err != nil {
			return "", err
		}
		found, foundMeta := "", ""
		for _, d := range t.Dirs {
			if d.Name == part {
				found, foundMeta = d.Tree, d.Meta
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("No such subpath: %s", subpath)
		}
		cur, curMeta = found, foundMeta
	}
	return curMeta, nil
}

func (c *checkout) writeFile(rel string, f object.FileEntry) error {
	loaded, err := tree.LoadFile(c.opts.Repo, f.Checksum)
	if err != nil {
		return err
	}
	path := filepath.Join(c.dest, rel, f.Name)
	if c.opts.UnionMode == 2 {
		if _, err := os.Lstat(path); err == nil {
			return nil
		}
	}
	if c.opts.UnionMode == 3 {
		if _, err := os.Lstat(path); err == nil {
			same, err := localFileMatches(c.opts.Repo, path, f.Checksum)
			if err != nil {
				return err
			}
			if same {
				return nil
			}
			return fmt.Errorf("File exists: %s already exists with different content", path)
		}
	}
	if loaded.Header.IsSymlink() {
		os.RemoveAll(path)
		if err := os.Symlink(loaded.Header.SymlinkTarget, path); err != nil {
			return err
		}
		return c.applySymlinkMeta(path, loaded.Header)
	}
	// Replace anything (including directories) already in the way.
	if st, err := os.Lstat(path); err == nil && st.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	fileMode := loaded.Header.Mode
	if c.opts.Repo.Mode() == repo.ModeBareUserOnly {
		fileMode = canonicalUserOnlyFileMode(fileMode)
	}
	// Empty files are intentionally copied rather than hardlinked so that
	// separate zero-length files do not share an inode. Hardlinks are only
	// safe when the checkout mode matches the stored object (chmod would
	// otherwise mutate the repository object in place).
	canLinkMode := (loaded.Header.Mode & 07777) == (fileMode & 07777)
	if c.canHardlink() && canLinkMode && !c.opts.ForceCopy && len(loaded.Content) > 0 && loaded.Header.Mode&0777 != 0 {
		objPath, err := c.opts.Repo.ObjectPath(f.Checksum, repo.ObjectFile)
		if err == nil {
			os.Remove(path)
			if err := os.Link(objPath, path); err == nil {
				return nil
			} else if c.opts.RequireHardlinks {
				return err
			}
		}
	}
	if err := os.WriteFile(path, loaded.Content, 0600); err != nil {
		return err
	}
	if c.opts.Repo.Mode().IsArchive() && !c.opts.DisableCache {
		_ = c.opts.Repo.WriteUncompressedCache(f.Checksum, loaded.Content)
	}
	return c.applyMeta(path, fileMode&07777, loaded.Header.UID, loaded.Header.GID, loaded.Header.Xattrs)
}

// canHardlink reports whether file objects may be hardlinked into the
// checkout, matching libostree's mode rules.
func (c *checkout) canHardlink() bool {
	switch c.opts.Repo.Mode() {
	case repo.ModeBare:
		return !c.opts.UserMode
	case repo.ModeBareUser:
		return c.opts.UserMode
	default:
		return false
	}
}

func (c *checkout) applyMeta(path string, mode, uid, gid uint32, xattrs []object.XAttr) error {
	if c.opts.UserMode && mode&0777 == 0 {
		// A user-mode checkout must remain readable by its owner.
		mode |= 0400
	}
	if !c.opts.UserMode {
		if err := os.Chown(path, int(uid), int(gid)); err != nil && !errors.Is(err, os.ErrPermission) {
			return err
		}
	}
	if err := os.Chmod(path, fsutil.FileMode(mode)); err != nil {
		return err
	}
	if !c.opts.UserMode {
		for _, x := range xattrs {
			if err := unix.Setxattr(path, x.Name, x.Value, 0); err != nil {
				if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
					continue
				}
				return err
			}
		}
	}
	ts := unix.NsecToTimespec(time.Unix(0, 0).UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, 0)
}

func (c *checkout) applySymlinkMeta(path string, h *object.FileHeader) error {
	if !c.opts.UserMode {
		if err := os.Lchown(path, int(h.UID), int(h.GID)); err != nil && !errors.Is(err, os.ErrPermission) {
			return err
		}
		for _, x := range h.Xattrs {
			if err := unix.Lsetxattr(path, x.Name, x.Value, 0); err != nil {
				if errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
					continue
				}
				return err
			}
		}
	}
	ts := unix.NsecToTimespec(time.Unix(0, 0).UnixNano())
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
}

func splitPath(p string) []string {
	var out []string
	for _, part := range filepath.SplitList(p) {
		_ = part
	}
	cur := p
	for cur != "" && cur != "." && cur != "/" {
		out = append([]string{filepath.Base(cur)}, out...)
		cur = filepath.Dir(cur)
	}
	return out
}

func isWhiteout(name string) bool {
	return len(name) > 4 && name[:4] == ".wh."
}

// localFileMatches reports whether path already has the same content and
// metadata as the repository file object.
func localFileMatches(r *repo.Repo, path, csum string) (bool, error) {
	loaded, err := tree.LoadFile(r, csum)
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	if loaded.Header.IsSymlink() {
		target, err := os.Readlink(path)
		if err != nil {
			return false, err
		}
		return target == loaded.Header.SymlinkTarget, nil
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	got, err := loaded.Header.Checksum(content)
	if err != nil {
		return false, err
	}
	return got == csum, nil
}

// processWhiteout applies an overlayfs whiteout entry.
func (c *checkout) processWhiteout(dir, name string) error {
	if name == ".wh..wh..opq" {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	target := filepath.Join(dir, strings.TrimPrefix(name, ".wh."))
	if err := os.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// mknodWhiteout converts an .ostree-wh.NAME file into a 0:0 character device
// named NAME.
func (c *checkout) mknodWhiteout(dir string, f object.FileEntry) error {
	name := strings.TrimPrefix(f.Name, ".ostree-wh.")
	target := filepath.Join(dir, name)
	if err := os.RemoveAll(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The mode of the quoted whiteout file is preserved: only the file type
	// bits are replaced with S_IFCHR (see _checkout_overlayfs_whiteout_at).
	perm := uint32(0o600)
	if loaded, err := tree.LoadFile(c.opts.Repo, f.Checksum); err == nil {
		perm = loaded.Header.Mode &^ 0o170000
	}
	dev := int(unix.Mkdev(0, 0))
	if err := unix.Mknod(target, unix.S_IFCHR|perm, dev); err != nil {
		return err
	}
	// mknod(2) applies the umask; restore the recorded permission bits like
	// _checkout_overlayfs_whiteout_at_no_overwrite() does with fchmodat().
	if err := os.Chmod(target, fs.FileMode(perm)); err != nil {
		return err
	}
	return nil
}

// canonicalUserOnlyFileMode mirrors the mode normalization applied when
// checking out into a bare-user-only repository.
func canonicalUserOnlyFileMode(mode uint32) uint32 {
	// Files that are world-writable are also stripped of group write.
	if mode&0002 != 0 {
		mode &^= 0022
	}
	return mode
}
