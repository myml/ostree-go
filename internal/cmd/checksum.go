package cmd

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/fsutil"
	"github.com/myml/ostree-go/internal/object"
)

func checksumOptions() []cli.Option {
	return []cli.Option{
		{LongName: "ignore-xattrs", ArgType: cli.ArgNone, Description: "Don't include xattrs in checksum"},
	}
}

func runChecksum(ctx *cli.Context) error {
	if len(ctx.Args) != 1 {
		return fmt.Errorf("Exactly one argument must be specified")
	}
	sum, err := checksumPath(ctx.Args[0], ctx.Options.Has("ignore-xattrs"))
	if err != nil {
		return err
	}
	fmt.Fprintln(ctx.Stdout, sum)
	return nil
}

func checksumPath(path string, ignoreXattrs bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	xattrs, err := xattrsOf(path, ignoreXattrs)
	if err != nil {
		return "", err
	}
	uid, gid := fsutil.Owner(info)
	switch {
	case info.IsDir():
		dm := &object.Dirmeta{UID: uid, GID: gid, Mode: fsutil.UnixMode(info.Mode()) | 0040000, Xattrs: xattrs}
		data, err := dm.Encode()
		if err != nil {
			return "", err
		}
		return checksum.Hex(checksum.Sum(data)), nil
	case info.Mode().IsRegular():
		h := &object.FileHeader{UID: uid, GID: gid, Mode: fsutil.UnixMode(info.Mode()) | 0100000, Xattrs: xattrs}
		content, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return h.Checksum(content)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		h := &object.FileHeader{UID: uid, GID: gid, Mode: fsutil.UnixMode(info.Mode()) | 0120000, SymlinkTarget: target, Xattrs: xattrs}
		return h.Checksum(nil)
	default:
		return "", fmt.Errorf("Unsupported file type: %s", path)
	}
}

func xattrsOf(path string, ignore bool) ([]object.XAttr, error) {
	if ignore {
		return nil, nil
	}
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
	for _, raw := range splitNul(buf[:n]) {
		if raw == "" {
			continue
		}
		vsize, err := unix.Lgetxattr(path, raw, nil)
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return nil, err
		}
		value := []byte{}
		if vsize > 0 {
			value = make([]byte, vsize)
			if _, err := unix.Lgetxattr(path, raw, value); err != nil {
				return nil, err
			}
		}
		out = append(out, object.XAttr{Name: raw, Value: value})
	}
	return object.Canonicalize(out)
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
