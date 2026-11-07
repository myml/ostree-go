// Package diff compares two commits or a commit and a directory.
package diff

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/fsutil"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/tree"
)

// Entry is one path in a comparison.
type Entry struct {
	Path string // display path
	Kind byte   // 'f' regular, 'l' symlink, 'd' directory
	Sum  string // content checksum for files
}

// Options controls Diff.
type Options struct {
	Repo     *repo.Repo
	NoXattrs bool
	// OwnerUID/OwnerGID override the ownership used for local files.
	OwnerUID *uint32
	OwnerGID *uint32
}

// Diff compares src and dst (revision names or directory paths) and returns
// rendered lines.
func Diff(opts Options, src, dst string) ([]string, error) {
	a, err := loadSide(opts, src)
	if err != nil {
		return nil, err
	}
	b, err := loadSide(opts, dst)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for p := range a {
		paths[p] = true
	}
	for p := range b {
		paths[p] = true
	}
	var modified, deleted, added []string
	for p := range paths {
		ae, aok := a[p]
		be, bok := b[p]
		switch {
		case aok && bok:
			if ae.Kind != be.Kind || ((ae.Kind == 'f' || ae.Kind == 'd') && ae.Sum != be.Sum) {
				modified = append(modified, render('M', ae.Path))
			}
		case aok:
			deleted = append(deleted, render('D', ae.Path))
		default:
			added = append(added, render('A', be.Path))
		}
	}
	sort.Strings(modified)
	sort.Strings(deleted)
	sort.Strings(added)
	return append(append(modified, deleted...), added...), nil
}

func render(kind byte, path string) string {
	return fmt.Sprintf("%c    %s", kind, path)
}

func loadSide(opts Options, rev string) (map[string]Entry, error) {
	if isDirArg(rev) {
		out := map[string]Entry{}
		if err := walkDir(out, rev, "", false, opts); err != nil {
			return nil, err
		}
		return out, nil
	}
	csum, err := tree.ResolveRef(opts.Repo, rev)
	if err != nil {
		return nil, err
	}
	commit, err := tree.LoadCommit(opts.Repo, csum)
	if err != nil {
		return nil, err
	}
	out := map[string]Entry{}
	if err := walkTree(opts.Repo, out, commit.RootTree, "", true); err != nil {
		return nil, err
	}
	return out, nil
}

// isDirArg decides whether an argument names a directory rather than a
// revision. libostree treats bare words as revisions; only arguments with a
// path separator (or . / ..) are directories.
func isDirArg(p string) bool {
	if p != "." && p != ".." && !strings.ContainsRune(p, '/') {
		return false
	}
	st, err := os.Lstat(p)
	return err == nil && st.IsDir()
}

func joinDisplay(dir, name string, absolute bool) string {
	rel := filepath.ToSlash(filepath.Join(dir, name))
	if absolute {
		return "/" + rel
	}
	return rel
}

func walkDir(out map[string]Entry, root, rel string, absolute bool, opts Options) error {
	entries, err := os.ReadDir(filepath.Join(root, rel))
	if err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(root, rel, e.Name())
		childRel := filepath.Join(rel, e.Name())
		display := joinDisplay(rel, e.Name(), absolute)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			sum, err := dirChecksum(full, info, opts)
			if err != nil {
				return err
			}
			out[childRel] = Entry{Path: display, Kind: 'd', Sum: sum}
			if err := walkDir(out, root, childRel, absolute, opts); err != nil {
				return err
			}
		case info.Mode().IsRegular(), info.Mode()&os.ModeSymlink != 0:
			sum, err := fileChecksum(full, info, opts)
			if err != nil {
				return err
			}
			kind := byte('f')
			if info.Mode()&os.ModeSymlink != 0 {
				kind = 'l'
			}
			out[childRel] = Entry{Path: display, Kind: kind, Sum: sum}
		}
	}
	return nil
}

func fileChecksum(path string, info os.FileInfo, opts Options) (string, error) {
	xattrs, err := readXattrs(path)
	if err != nil {
		return "", err
	}
	uid, gid := ownerOf(info)
	if opts.OwnerUID != nil {
		uid = *opts.OwnerUID
	}
	if opts.OwnerGID != nil {
		gid = *opts.OwnerGID
	}
	h := &object.FileHeader{UID: uid, GID: gid, Xattrs: xattrs}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		h.Mode = fsutil.UnixMode(info.Mode()) | 0120000
		h.SymlinkTarget = target
		return h.Checksum(nil)
	}
	h.Mode = fsutil.UnixMode(info.Mode()) | 0100000
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return h.Checksum(content)
}

func walkTree(r *repo.Repo, out map[string]Entry, treeCsum, rel string, absolute bool) error {
	t, err := tree.LoadDirtree(r, treeCsum)
	if err != nil {
		return err
	}
	for _, f := range t.Files {
		display := joinDisplay(rel, f.Name, absolute)
		kind := byte('f')
		if loaded, err := tree.LoadFile(r, f.Checksum); err == nil && loaded.Header.IsSymlink() {
			kind = 'l'
		}
		joined := filepath.ToSlash(filepath.Join(rel, f.Name))
		out[joined] = Entry{Path: display, Kind: kind, Sum: f.Checksum}
	}
	for _, d := range t.Dirs {
		display := joinDisplay(rel, d.Name, absolute)
		joined := filepath.ToSlash(filepath.Join(rel, d.Name))
		out[joined] = Entry{Path: display, Kind: 'd', Sum: d.Meta}
		if err := walkTree(r, out, d.Tree, joined, absolute); err != nil {
			return err
		}
	}
	return nil
}

var _ = checksum.Size

func readXattrs(path string) ([]object.XAttr, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP {
			return nil, nil
		}
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	n, err := unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}
	var out []object.XAttr
	for _, name := range splitNul(buf[:n]) {
		if name == "" {
			continue
		}
		vsize, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			if err == unix.ENODATA {
				continue
			}
			return nil, err
		}
		value := []byte{}
		if vsize > 0 {
			value = make([]byte, vsize)
			if _, err := unix.Lgetxattr(path, name, value); err != nil {
				return nil, err
			}
		}
		out = append(out, object.XAttr{Name: name, Value: value})
	}
	return object.Canonicalize(out)
}

func ownerOf(info os.FileInfo) (uint32, uint32) {
	return fsutil.Owner(info)
}

func splitNul(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			out = append(out, string(b[start:i]))
			start = i + 1
		}
	}
	return out
}

// dirChecksum computes the dirmeta checksum for a local directory.
func dirChecksum(path string, info os.FileInfo, opts Options) (string, error) {
	var xattrs []object.XAttr
	if !opts.NoXattrs {
		var err error
		xattrs, err = readXattrs(path)
		if err != nil {
			return "", err
		}
	}
	uid, gid := ownerOf(info)
	if opts.OwnerUID != nil {
		uid = *opts.OwnerUID
	}
	if opts.OwnerGID != nil {
		gid = *opts.OwnerGID
	}
	dm := &object.Dirmeta{UID: uid, GID: gid, Mode: fsutil.UnixMode(info.Mode()) | 0040000, Xattrs: xattrs}
	data, err := dm.Encode()
	if err != nil {
		return "", err
	}
	return checksum.Hex(checksum.Sum(data)), nil
}
