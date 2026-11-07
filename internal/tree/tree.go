// Package tree loads OSTree objects for the mode they are stored in.
package tree

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/fsutil"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
)

// LoadCommit reads a commit object by checksum.
func LoadCommit(r *repo.Repo, csum string) (*object.Commit, error) {
	data, err := r.ReadObject(csum, repo.ObjectCommit)
	if err != nil {
		return nil, err
	}
	return object.ParseCommit(data)
}

// LoadDirtree reads a dirtree object by checksum.
func LoadDirtree(r *repo.Repo, csum string) (*object.Dirtree, error) {
	data, err := r.ReadObject(csum, repo.ObjectDirtree)
	if err != nil {
		return nil, err
	}
	return object.ParseDirtree(data)
}

// LoadDirmeta reads a dirmeta object by checksum.
func LoadDirmeta(r *repo.Repo, csum string) (*object.Dirmeta, error) {
	data, err := r.ReadObject(csum, repo.ObjectDirmeta)
	if err != nil {
		return nil, err
	}
	return object.ParseDirmeta(data)
}

// File is a loaded file object.
type File struct {
	Header  *object.FileHeader
	Content []byte
}

// LoadFile reads a file object and reconstructs its header from the
// repository mode.
func LoadFile(r *repo.Repo, csum string) (*File, error) {
	if rel, err := r.ObjectRelPath(csum, repo.ObjectFile); err == nil {
		if _, err := r.Root().Lstat(rel); err != nil {
			if p := r.Parent(); p != nil {
				return LoadFile(p, csum)
			}
		}
	}
	raw, err := r.ReadObject(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	switch r.Mode() {
	case repo.ModeArchive:
		return loadArchiveFile(raw)
	case repo.ModeBare:
		return loadBareFile(r, csum, raw)
	case repo.ModeBareUser:
		return loadBareUserFile(r, csum, raw)
	case repo.ModeBareUserOnly:
		return loadBareUserOnlyFile(r, csum, raw)
	default:
		return nil, fmt.Errorf("file objects in mode %s are not supported", r.Mode())
	}
}

func loadArchiveFile(raw []byte) (*File, error) {
	h, rest, err := object.ParseArchiveHeader(raw)
	if err != nil {
		return nil, err
	}
	if h.IsSymlink() {
		return &File{Header: h}, nil
	}
	if len(rest) == 0 {
		return &File{Header: h}, nil
	}
	fr := flate.NewReader(bytes.NewReader(rest))
	defer fr.Close()
	content, err := io.ReadAll(fr)
	if err != nil {
		return nil, err
	}
	if uint64(len(content)) != h.Size {
		return nil, fmt.Errorf("file object size mismatch: header %d, content %d", h.Size, len(content))
	}
	return &File{Header: h, Content: content}, nil
}

func loadBareFile(r *repo.Repo, csum string, raw []byte) (*File, error) {
	full, err := r.ObjectPath(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	xattrs, err := listXattrs(full)
	if err != nil {
		return nil, err
	}
	var header *object.FileHeader
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return nil, err
		}
		header = &object.FileHeader{
			UID:           statUID(st),
			GID:           statGID(st),
			Mode:          fsutil.UnixMode(st.Mode()) | 0120000,
			SymlinkTarget: target,
			Xattrs:        xattrs,
		}
		return &File{Header: header}, nil
	}
	header = &object.FileHeader{
		UID:    statUID(st),
		GID:    statGID(st),
		Mode:   fsutil.UnixMode(st.Mode()) | 0100000,
		Xattrs: xattrs,
	}
	return &File{Header: header, Content: raw}, nil
}

func loadBareUserFile(r *repo.Repo, csum string, raw []byte) (*File, error) {
	full, err := r.ObjectPath(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	meta, err := readOneXattr(full, "user.ostreemeta")
	if err != nil {
		return nil, fmt.Errorf("bare-user object %s: %w", csum, err)
	}
	header, err := object.ParseFileMeta(meta)
	if err != nil {
		return nil, err
	}
	if header.IsSymlink() {
		if len(raw) == 0 || raw[len(raw)-1] != 0 {
			return nil, fmt.Errorf("bare-user symlink object %s lacks NUL terminator", csum)
		}
		header.SymlinkTarget = string(raw[:len(raw)-1])
		return &File{Header: header}, nil
	}
	return &File{Header: header, Content: raw}, nil
}

func loadBareUserOnlyFile(r *repo.Repo, csum string, raw []byte) (*File, error) {
	full, err := r.ObjectPath(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return nil, err
		}
		return &File{Header: &object.FileHeader{Mode: fsutil.UnixMode(st.Mode()) | 0120000, SymlinkTarget: target}}, nil
	}
	mode := fsutil.UnixMode(st.Mode()) | 0100000
	return &File{Header: &object.FileHeader{Mode: mode}, Content: raw}, nil
}

func readOneXattr(path, name string) ([]byte, error) {
	size, err := unix.Lgetxattr(path, name, nil)
	if err != nil {
		return nil, err
	}
	if size == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, size)
	n, err := unix.Lgetxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func statUID(fi os.FileInfo) uint32 {
	u, _ := fsutil.Owner(fi)
	return u
}

func statGID(fi os.FileInfo) uint32 {
	_, g := fsutil.Owner(fi)
	return g
}

func listXattrs(path string) ([]object.XAttr, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) {
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
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		key := string(name)
		vsize, err := unix.Lgetxattr(path, key, nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return nil, err
		}
		value := []byte{}
		if vsize > 0 {
			value = make([]byte, vsize)
			if _, err := unix.Lgetxattr(path, key, value); err != nil {
				return nil, err
			}
		}
		out = append(out, object.XAttr{Name: key, Value: value})
	}
	return object.Canonicalize(out)
}

// ResolveRef resolves a ref or checksum to a commit checksum. A trailing
// set of "^" characters walks to the parent commit, like git.
func ResolveRef(r *repo.Repo, rev string) (string, error) {
	parents := 0
	for strings.HasSuffix(rev, "^") {
		parents++
		rev = strings.TrimSuffix(rev, "^")
	}
	sum, err := resolveRefBase(r, rev)
	if err != nil {
		return "", err
	}
	for i := 0; i < parents; i++ {
		commit, err := LoadCommit(r, sum)
		if err != nil {
			return "", err
		}
		if commit.Parent == "" {
			return "", fmt.Errorf("Commit %s has no parent", sum)
		}
		sum = commit.Parent
	}
	return sum, nil
}

func resolveRefBase(r *repo.Repo, rev string) (string, error) {
	if len(rev) >= 4 && len(rev) < 64 && isLowerHex(rev) {
		commits, err := ListCommits(r)
		if err != nil {
			return "", err
		}
		var matches []string
		for _, c := range commits {
			if strings.HasPrefix(c, rev) {
				matches = append(matches, c)
			}
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return "", fmt.Errorf("Multiple commits found for %s", rev)
		}
	}
	if checksum.IsHex(rev) {
		ok, err := r.HasObject(rev, repo.ObjectCommit)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("No such commit: %s", rev)
		}
		return rev, nil
	}
	remote, name, hasRemote := splitRefspec(rev)
	var (
		sum string
		err error
	)
	if hasRemote {
		sum, err = r.ReadRemoteRef(remote, name)
	} else {
		sum, err = r.ReadRef(rev)
		if err != nil {
			// libostree also resolves remotes by path: aremote/test2 reads
			// refs/remotes/aremote/test2.
			if i := strings.IndexByte(rev, '/'); i > 0 {
				if v, rerr := r.ReadRemoteRef(rev[:i], rev[i+1:]); rerr == nil {
					sum, err = v, nil
				}
			}
		}
	}
	if err != nil {
		// libostree's resolve_refspec(fallback_remote=TRUE) also searches
		// refs/remotes/<remote>/<name> when a bare local ref is missing.
		if v, ok, ferr := r.ResolveRef(rev, true, true); ferr == nil && ok {
			sum, err = v, nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("Ref %s not found", rev)
	}
	return sum, nil
}

func splitRefspec(rev string) (remote, name string, ok bool) {
	for i := 0; i < len(rev); i++ {
		if rev[i] == ':' {
			return rev[:i], rev[i+1:], true
		}
	}
	return "", rev, false
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return len(s) > 0
}

// ListCommits returns all commit checksums in the repository.
func ListCommits(r *repo.Repo) ([]string, error) {
	objectsDir := filepath.Join(r.Path(), "objects")
	dirs, err := os.ReadDir(objectsDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(objectsDir, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".commit") {
				out = append(out, d.Name()+strings.TrimSuffix(e.Name(), ".commit"))
			}
		}
	}
	return out, nil
}
