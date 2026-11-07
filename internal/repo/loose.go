package repo

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/fsutil"
)

// LoosePath returns the loose object path relative to objects/, e.g.
//
//	ab/cdef...<type>z
//
// The "z" suffix is appended only for non-metadata objects in archive
// repositories, exactly like _ostree_loose_path(). mode must be a valid
// repository mode and checksum a valid 64 character lowercase sha256.
func LoosePath(checksum string, typ ObjectType, mode Mode) (string, error) {
	if err := ValidateChecksum(checksum); err != nil {
		return "", err
	}
	if !typ.Valid() {
		return "", fmt.Errorf("%w: %d", ErrInvalidObjectType, int(typ))
	}
	if !mode.Valid() {
		return "", fmt.Errorf("%w: %d", ErrInvalidMode, int(mode))
	}
	suffix := typ.String()
	if !typ.IsMeta() && mode == ModeArchive {
		suffix += "z"
	}
	return checksum[:2] + "/" + checksum[2:] + "." + suffix, nil
}

// ObjectRelPath returns the object path including the "objects/" prefix,
// relative to the repository root.
func (r *Repo) ObjectRelPath(checksum string, typ ObjectType) (string, error) {
	rel, err := LoosePath(checksum, typ, r.mode)
	if err != nil {
		return "", err
	}
	return "objects/" + rel, nil
}

// ObjectPath returns the absolute path of a loose object.
func (r *Repo) ObjectPath(checksum string, typ ObjectType) (string, error) {
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.path, filepath.FromSlash(rel)), nil
}

// HasObject reports whether the loose object exists in this repository's
// current mode.
func (r *Repo) HasObject(checksum string, typ ObjectType) (bool, error) {
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return false, err
	}
	if r.root == nil {
		return false, errors.New("repository is closed")
	}
	if _, err := r.root.Lstat(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if p := r.Parent(); p != nil {
				return p.HasObject(checksum, typ)
			}
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ReadObject returns the raw bytes of a loose object. For archive mode, the
// on-disk ".filez" (and other non-metadata "z") files are returned verbatim;
// decompression belongs to the object layer.
func (r *Repo) ReadObject(checksum string, typ ObjectType) ([]byte, error) {
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return nil, err
	}
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	if st, err := r.root.Lstat(rel); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		target, err := r.root.Readlink(rel)
		if err != nil {
			return nil, fmt.Errorf("read object %s: %w", rel, err)
		}
		return []byte(target), nil
	}
	data, err := r.readRootFile(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if p := r.Parent(); p != nil {
				if data, perr := p.ReadObject(checksum, typ); perr == nil {
					return data, nil
				}
			}
			return nil, fmt.Errorf("%w: %s.%s: %w", ErrObjectNotFound, checksum, typ, err)
		}
		return nil, fmt.Errorf("read object %s: %w", rel, err)
	}
	return data, nil
}

// WriteObject stores an object whose serialized bytes are data and returns its
// sha256 checksum.
//
// The object layer is responsible for assembling file headers and content; the
// checksum is always computed over data as passed in. In archive mode, file
// objects are additionally compressed with raw deflate (no zlib header) at the
// level configured by [archive] zlib-level before being written to ".filez".
// Metadata is written verbatim. An existing object is left untouched and its
// checksum returned.
func (r *Repo) WriteObject(typ ObjectType, data []byte) (string, error) {
	if !typ.Valid() {
		return "", fmt.Errorf("%w: %d", ErrInvalidObjectType, int(typ))
	}
	if !r.writable {
		if r.writableErr != nil {
			return "", r.writableErr
		}
		return "", ErrReadOnly
	}
	if r.root == nil {
		return "", errors.New("repository is closed")
	}
	sum := sha256.Sum256(data)
	checksum := hex.EncodeToString(sum[:])
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return "", err
	}

	if _, err := r.root.Lstat(rel); err == nil {
		return checksum, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	payload := data
	if r.mode == ModeArchive && typ == ObjectFile {
		var compressed bytes.Buffer
		zw, err := flate.NewWriter(&compressed, r.zlibLevel)
		if err != nil {
			return "", err
		}
		if _, err := zw.Write(data); err != nil {
			zw.Close()
			return "", err
		}
		if err := zw.Close(); err != nil {
			return "", err
		}
		payload = compressed.Bytes()
	}

	if err := r.mkdirAll(path.Dir(rel), objDirMode); err != nil {
		return "", err
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return "", err
	}

	f, tmp, err := r.createStagingFile()
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		f.Close()
		r.root.Remove(tmp)
		return "", err
	}
	if _, err := f.Write(payload); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		r.root.Remove(tmp)
		return "", err
	}
	if err := r.renameRootPath(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return "", fmt.Errorf("install object %s: %w", rel, err)
	}
	return checksum, nil
}

// createStagingFile creates a uniquely named file under tmp/ with the same
// staging prefix libostree uses.
func (r *Repo) createStagingFile() (*os.File, string, error) {
	for i := 0; i < 100; i++ {
		rel := path.Join("tmp", r.stagedirPrefix+randomHex(8))
		f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			return f, rel, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", errors.New("could not create a staging file in tmp/")
}

// WriteRawObject stores payload for an object with a caller-computed checksum.
//
// File objects do not use WriteObject: their checksum covers the uncompressed
// header and content, while the on-disk payload differs per mode (raw content
// for bare, a length-prefixed header plus raw deflate data for archive). The
// commit engine therefore assembles the payload itself and names it here.
func (r *Repo) WriteRawObject(checksum string, typ ObjectType, payload []byte) error {
	sum, err := FromHexChecksum(checksum)
	if err != nil {
		return err
	}
	_ = sum
	if !typ.Valid() {
		return fmt.Errorf("%w: %d", ErrInvalidObjectType, int(typ))
	}
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return err
	}
	if _, err := r.root.Lstat(rel); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.mkdirAll(path.Dir(rel), objDirMode); err != nil {
		return err
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return err
	}
	f, tmp, err := r.createStagingFile()
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		r.root.Remove(tmp)
		return err
	}
	if _, err := f.Write(payload); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		r.root.Remove(tmp)
		return err
	}
	if err := r.renameRootPath(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("install object %s: %w", rel, err)
	}
	return nil
}

// WriteSymlinkObject stores a symlink object whose target is target.
func (r *Repo) WriteSymlinkObject(checksum string, typ ObjectType, target string, meta FileObjectMeta) error {
	if _, err := FromHexChecksum(checksum); err != nil {
		return err
	}
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return err
	}
	if _, err := r.root.Lstat(rel); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.mkdirAll(path.Dir(rel), objDirMode); err != nil {
		return err
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return err
	}
	tmp := path.Join("tmp", r.stagedirPrefix+randomHex(8))
	if err := r.root.Symlink(target, tmp); err != nil {
		return err
	}
	if r.mode == ModeBare {
		full := filepath.Join(r.path, filepath.FromSlash(tmp))
		if err := os.Lchown(full, int(meta.UID), int(meta.GID)); err != nil && err != syscall.EPERM {
			r.root.Remove(tmp)
			return err
		}
		for name, value := range meta.Xattrs {
			if err := unix.Lsetxattr(full, name, value, 0); err != nil && err != syscall.EPERM && err != syscall.ENOTSUP && err != syscall.EOPNOTSUPP {
				r.root.Remove(tmp)
				return err
			}
		}
		ts := unix.NsecToTimespec(0)
		_ = unix.UtimesNanoAt(unix.AT_FDCWD, full, []unix.Timespec{ts, ts}, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err := r.renameRootPath(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("install symlink object %s: %w", rel, err)
	}
	return nil
}

// FromHexChecksum validates a checksum string.
func FromHexChecksum(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, fmt.Errorf("%w: %q", ErrInvalidChecksum, s)
	}
	for i := 0; i < 32; i++ {
		hi, err := hexNibble(s[2*i])
		if err != nil {
			return out, fmt.Errorf("%w: %q", ErrInvalidChecksum, s)
		}
		lo, err := hexNibble(s[2*i+1])
		if err != nil {
			return out, fmt.Errorf("%w: %q", ErrInvalidChecksum, s)
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	default:
		return 0, fmt.Errorf("invalid hex digit %q", c)
	}
}

// FileObjectMeta carries the metadata needed to store a file object the way
// libostree does for each repository mode.
type FileObjectMeta struct {
	// UID, GID and Mode are the values from the object header.
	UID  uint32
	GID  uint32
	Mode uint32
	// Xattrs are applied to the object file itself in bare mode.
	Xattrs map[string][]byte
	// Ostreemeta, when non-nil, is written as the user.ostreemeta xattr
	// (bare-user mode).
	Ostreemeta []byte
}

// WriteFileRawObject stores a file object with a caller-computed checksum and
// applies the metadata libostree would apply for the repository mode.
func (r *Repo) WriteFileRawObject(checksum string, payload []byte, meta FileObjectMeta) error {
	if _, err := FromHexChecksum(checksum); err != nil {
		return err
	}
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	rel, err := r.ObjectRelPath(checksum, ObjectFile)
	if err != nil {
		return err
	}
	if _, err := r.root.Lstat(rel); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.mkdirAll(path.Dir(rel), objDirMode); err != nil {
		return err
	}
	if err := r.mkdirAll("tmp", dirMode); err != nil {
		return err
	}
	f, tmp, err := r.createStagingFile()
	if err != nil {
		return err
	}
	fail := func(err error) error {
		f.Close()
		r.root.Remove(tmp)
		return err
	}
	if _, err := f.Write(payload); err != nil {
		return fail(err)
	}
	if err := applyFileObjectMeta(f, r.mode, meta); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		r.root.Remove(tmp)
		return err
	}
	if err := r.renameRootPath(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("install object %s: %w", rel, err)
	}
	return nil
}

func applyFileObjectMeta(f *os.File, mode Mode, meta FileObjectMeta) error {
	switch mode {
	case ModeBare:
		if err := unix.Fchown(int(f.Fd()), int(meta.UID), int(meta.GID)); err != nil && err != unix.EPERM {
			return err
		}
		if err := f.Chmod(fsutil.FileMode(meta.Mode)); err != nil {
			return err
		}
		for name, value := range meta.Xattrs {
			if err := unix.Fsetxattr(int(f.Fd()), name, value, 0); err != nil && err != unix.EPERM && err != unix.ENOTSUP && err != unix.EOPNOTSUPP {
				return err
			}
		}
	case ModeBareUser:
		if meta.Ostreemeta != nil {
			if err := unix.Fsetxattr(int(f.Fd()), "user.ostreemeta", meta.Ostreemeta, 0); err != nil {
				return err
			}
		}
		if meta.Mode&0170000 == 0100000 {
			contentMode := (meta.Mode & (0100000 | 0775)) | 0400
			if err := f.Chmod(fsutil.FileMode(contentMode)); err != nil {
				return err
			}
		}
	case ModeBareUserOnly:
		if err := f.Chmod(fsutil.FileMode(meta.Mode)); err != nil {
			return err
		}
	default:
		if err := f.Chmod(0644); err != nil {
			return err
		}
	}
	epoch := time.Unix(0, 0)
	return os.Chtimes(f.Name(), epoch, epoch)
}

// WriteUncompressedCache stores an uncompressed copy of a content object in
// the legacy uncompressed-objects-cache used by archive checkouts.
func (r *Repo) WriteUncompressedCache(checksum string, content []byte) error {
	if len(checksum) < 3 {
		return nil
	}
	dir := path.Join(r.path, "uncompressed-objects-cache", checksum[:2])
	if err := os.MkdirAll(dir, 0775); err != nil {
		return err
	}
	return os.WriteFile(path.Join(dir, checksum[2:]+".file"), content, 0664)
}

// AdoptFile hardlinks an existing file into the object store. It is used by
// `commit --consume --link-checkout-speedup` so that the caller keeps a
// hardlink to the object.
func (r *Repo) AdoptFile(checksum string, typ ObjectType, srcPath string) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return err
	}
	if _, err := r.root.Lstat(rel); err == nil {
		return nil
	}
	if err := r.mkdirAll(path.Dir(rel), objDirMode); err != nil {
		return err
	}
	abs := path.Join(r.path, rel)
	return os.Link(srcPath, abs)
}

// RemoveTombstone deletes the tombstone commit for checksum if present, which
// happens when the real commit is pulled again.
func (r *Repo) RemoveTombstone(checksum string) error {
	p, err := r.ObjectPath(checksum, ObjectTombstoneCommit)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// MarkPartial records that a commit was fetched without its tree.
func (r *Repo) MarkPartial(checksum string) error {
	if err := r.mkdirAll("state", dirMode); err != nil {
		return err
	}
	f, err := r.root.OpenFile(path.Join("state", checksum+".commitpartial"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return f.Close()
}

// CommitIsPartial reports whether checksum has a commitpartial marker, i.e.
// whether it was fetched without its full tree.
func (r *Repo) CommitIsPartial(checksum string) (bool, error) {
	if err := ValidateChecksum(checksum); err != nil {
		return false, err
	}
	if r.root == nil {
		return false, errors.New("repository is closed")
	}
	rel := path.Join("state", checksum+".commitpartial")
	if _, err := r.root.Lstat(rel); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if p := r.Parent(); p != nil {
		return p.CommitIsPartial(checksum)
	}
	return false, nil
}

// ClearPartial removes a partial-commit marker.
func (r *Repo) ClearPartial(checksum string) error {
	err := r.root.Remove(path.Join("state", checksum+".commitpartial"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// ReplaceRawObject is like WriteRawObject but replaces an existing object.
// Detached metadata (commitmeta) is mutable: signing a commit again rewrites
// it, unlike content objects which are immutable.  The C implementation
// unlinks the previous file in ostree_repo_write_commit_detached_metadata().
func (r *Repo) ReplaceRawObject(checksum string, typ ObjectType, payload []byte) error {
	rel, err := r.ObjectRelPath(checksum, typ)
	if err != nil {
		return err
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if _, err := r.root.Lstat(rel); err == nil {
		if err := r.root.Remove(rel); err != nil {
			return fmt.Errorf("remove existing object %s: %w", rel, err)
		}
	}
	return r.WriteRawObject(checksum, typ, payload)
}
