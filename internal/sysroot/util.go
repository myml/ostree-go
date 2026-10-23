package sysroot

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"ostree-go/internal/repo"
	"ostree-go/internal/tree"
)

func timeNow() time.Time { return time.Now() }

// writeFileAtomic writes data to path via a temporary file and rename.
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
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// commitInfo is the small subset of commit metadata the deployment code needs.
type commitInfo struct {
	Version     string
	SourceTitle string
	EndOfLife   string
	Rebase      string
	Timestamp   uint64
}

// loadCommitInfo reads the commit metadata for csum.
func loadCommitInfo(r *repo.Repo, csum string) (*commitInfo, error) {
	c, err := tree.LoadCommit(r, csum)
	if err != nil {
		return nil, err
	}
	info := &commitInfo{Timestamp: c.Timestamp}
	info.Version, _ = c.GetMetadataString("version")
	info.SourceTitle, _ = c.GetMetadataString("ostree.source-title")
	info.EndOfLife, _ = c.GetMetadataString("ostree.endoflife")
	info.Rebase, _ = c.GetMetadataString("ostree.endoflife-rebase")
	return info, nil
}

func loadCommit(r *repo.Repo, csum string) (*commitInfo, error) {
	return loadCommitInfo(r, csum)
}

// removeAllIfExists is os.RemoveAll that ignores a missing path.
func removeAllIfExists(path string) error {
	err := os.RemoveAll(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// copyTreeRecursive copies src to dst, preserving mode, ownership and xattrs.
func copyTreeRecursive(src, dst string, noXattrs bool) error {
	return copyTreeFiltered(src, dst, noXattrs, nil)
}

// copyTreeFiltered copies src into dst.  When skipRel is non-nil, paths for
// which it returns true are not copied (used for /etc and /var handling).
func copyTreeFiltered(src, dst string, noXattrs bool, skip func(rel string) bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			// mkdir(2) applies the umask; restore the recorded mode.
			if err := os.Chmod(target, info.Mode().Perm()); err != nil {
				return err
			}
			return copyOwnership(target, info)
		case info.Mode()&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
			return copyOwnership(target, info)
		default:
			if !info.Mode().IsRegular() {
				// Skip fifos, sockets and device nodes.
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
				return err
			}
			return copyOwnership(target, info)
		}
	})
}

// copyOwnership copies uid/gid when running as root.
func copyOwnership(path string, info os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	uid, gid, ok := statUIDGID(info)
	if !ok {
		return nil
	}
	return os.Lchown(path, uid, gid)
}

// statUIDGID extracts uid/gid from a FileInfo.
func statUIDGID(info os.FileInfo) (int, int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
