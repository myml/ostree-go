package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/fsutil"
	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
)

func lsOptions() []cli.Option {
	return []cli.Option{
		{LongName: "dironly", ShortName: 'd', ArgType: cli.ArgNone, Description: "Do not recurse into directory arguments"},
		{LongName: "recursive", ShortName: 'R', ArgType: cli.ArgNone, Description: "Print directories recursively"},
		{LongName: "checksum", ShortName: 'C', ArgType: cli.ArgNone, Description: "Print checksum"},
		{LongName: "xattrs", ShortName: 'X', ArgType: cli.ArgNone, Description: "Print extended attributes"},
		{LongName: "nul-filenames-only", ArgType: cli.ArgNone, Description: "Print only filenames, NUL separated"},
	}
}

type lsFlags struct {
	dironly   bool
	recursive bool
	checksum  bool
	xattrs    bool
	nul       bool
}

// lsNode is one entry in a commit tree. Directory paths are stored without a
// trailing slash; displayPath adds it.
type lsNode struct {
	path     string
	isDir    bool
	tree     string // dirtree checksum (directories)
	meta     string // dirmeta checksum (directories)
	fileCsum string // file object checksum (files)
}

func (n lsNode) displayPath() string {
	return n.path
}

func runLs(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("An COMMIT argument is required")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	flags := lsFlags{
		dironly:   ctx.Options.Has("dironly"),
		recursive: ctx.Options.Has("recursive"),
		checksum:  ctx.Options.Has("checksum"),
		xattrs:    ctx.Options.Has("xattrs"),
		nul:       ctx.Options.Has("nul-filenames-only"),
	}

	checksum, err := resolveRevision(r, ctx.Args[0])
	if err != nil {
		return err
	}
	_, commit, err := readCommitObject(r, checksum)
	if err != nil {
		return err
	}
	root := lsNode{path: "/", isDir: true, tree: commit.RootTree, meta: commit.RootMeta}

	if len(ctx.Args) > 1 {
		for _, arg := range ctx.Args[1:] {
			node, err := resolveTreePath(r, root, arg)
			if err != nil {
				return fmt.Errorf("Inspecting path '%s': %w", arg, err)
			}
			remaining := 1
			if flags.recursive {
				remaining = -1
			} else if flags.dironly {
				remaining = 0
			}
			if err := printLsNode(ctx.Stdout, r, node, flags, remaining); err != nil {
				return fmt.Errorf("Inspecting path '%s': %w", arg, err)
			}
		}
		return nil
	}
	remaining := 1
	if flags.recursive {
		remaining = -1
	} else if flags.dironly {
		remaining = 0
	}
	return printLsNode(ctx.Stdout, r, root, flags, remaining)
}

// resolveTreePath resolves a user supplied path the way
// g_file_resolve_relative_path() + the OstreeRepoFile tree do: literal
// components, no "." or ".." handling, and empty components are an error.
func resolveTreePath(r *repo.Repo, root lsNode, arg string) (lsNode, error) {
	if arg == "" {
		return lsNode{}, noSuchFile("/")
	}
	// A path made only of slashes is the root itself.
	if strings.Trim(arg, "/") == "" {
		return root, nil
	}

	parts := strings.Split(arg, "/")
	if parts[0] == "" {
		parts = parts[1:]
	}

	cur := root
	for i, part := range parts {
		if part == "" {
			return lsNode{}, noSuchFile(attemptedPath(cur.path, part))
		}
		if !cur.isDir {
			return lsNode{}, errNotDirectory
		}
		data, err := r.ReadObject(cur.tree, repo.ObjectDirtree)
		if err != nil {
			return lsNode{}, err
		}
		tree, err := object.ParseDirtree(data)
		if err != nil {
			return lsNode{}, err
		}
		childPath := joinTreePath(cur.path, part)
		last := i == len(parts)-1
		for _, f := range tree.Files {
			if f.Name == part {
				node := lsNode{path: childPath, fileCsum: f.Checksum}
				if !last {
					return lsNode{}, errNotDirectory
				}
				return node, nil
			}
		}
		found := false
		for _, d := range tree.Dirs {
			if d.Name == part {
				cur = lsNode{path: childPath, isDir: true, tree: d.Tree, meta: d.Meta}
				found = true
				break
			}
		}
		if !found {
			return lsNode{}, noSuchFile(childPath)
		}
	}
	return cur, nil
}

var errNotDirectory = errors.New("Not a directory")

func noSuchFile(path string) error {
	return fmt.Errorf("No such file or directory: %s", path)
}

func joinTreePath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func attemptedPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func printLsNode(w io.Writer, r *repo.Repo, node lsNode, flags lsFlags, remaining int) error {
	if flags.nul {
		if _, err := io.WriteString(w, node.displayPath()+"\x00"); err != nil {
			return err
		}
	} else {
		info, err := loadLsInfo(r, node)
		if err != nil {
			return err
		}
		if err := printLsLine(w, node, info, flags); err != nil {
			return err
		}
	}

	if !node.isDir || remaining == 0 {
		return nil
	}
	data, err := r.ReadObject(node.tree, repo.ObjectDirtree)
	if err != nil {
		return err
	}
	tree, err := object.ParseDirtree(data)
	if err != nil {
		return err
	}
	next := remaining
	if next > 0 {
		next--
	}
	for _, f := range tree.Files {
		child := lsNode{path: joinTreePath(node.path, f.Name), fileCsum: f.Checksum}
		if err := printLsNode(w, r, child, flags, next); err != nil {
			return err
		}
	}
	for _, d := range tree.Dirs {
		child := lsNode{path: joinTreePath(node.path, d.Name), isDir: true, tree: d.Tree, meta: d.Meta}
		if err := printLsNode(w, r, child, flags, next); err != nil {
			return err
		}
	}
	return nil
}

// lsInfo is the metadata needed to format one ls line.
type lsInfo struct {
	mode   uint32
	uid    uint32
	gid    uint32
	size   uint64
	target string
	xattrs []object.XAttr
}

func loadLsInfo(r *repo.Repo, node lsNode) (*lsInfo, error) {
	if node.isDir {
		data, err := r.ReadObject(node.meta, repo.ObjectDirmeta)
		if err != nil {
			return nil, err
		}
		dm, err := object.ParseDirmeta(data)
		if err != nil {
			return nil, err
		}
		return &lsInfo{mode: dm.Mode, uid: dm.UID, gid: dm.GID, xattrs: dm.Xattrs}, nil
	}
	fm, err := readFileMetadata(r, node.fileCsum)
	if err != nil {
		return nil, err
	}
	return &lsInfo{
		mode:   fm.Mode,
		uid:    fm.UID,
		gid:    fm.GID,
		size:   fm.Size,
		target: fm.SymlinkTarget,
		xattrs: fm.Xattrs,
	}, nil
}

// userVisibleModeMask is S_IFMT: the file type bits.
const userVisibleModeMask = 0170000

func fileTypeChar(mode uint32) byte {
	switch mode & userVisibleModeMask {
	case 0100000:
		return '-'
	case 0040000:
		return 'd'
	case 0120000:
		return 'l'
	case 0020000:
		return 'c'
	case 0060000:
		return 'b'
	default:
		return '?'
	}
}

func printLsLine(w io.Writer, node lsNode, info *lsInfo, flags lsFlags) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%c0%04o %d %d %6d ", fileTypeChar(info.mode), info.mode&07777, info.uid, info.gid, info.size)
	if flags.checksum {
		if node.isDir {
			fmt.Fprintf(&b, "%s %s ", node.tree, node.meta)
		} else {
			fmt.Fprintf(&b, "%s ", node.fileCsum)
		}
	}
	if flags.xattrs {
		formatted, err := formatXattrs(info.xattrs)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "{ %s } ", formatted)
	}
	b.WriteString(node.displayPath())
	if info.mode&userVisibleModeMask == 0120000 {
		fmt.Fprintf(&b, " -> %s", info.target)
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

var xattrsType = gvariant.MustParseType("a(ayay)")

// formatXattrs renders an xattr array exactly like g_variant_print() on the
// a(ayay) value returned by ostree_repo_file_get_xattrs().
func formatXattrs(xattrs []object.XAttr) (string, error) {
	vals := make([]gvariant.Value, len(xattrs))
	for i, x := range xattrs {
		name := append([]byte(nil), x.Name...)
		name = append(name, 0)
		vals[i] = []gvariant.Value{name, x.Value}
	}
	return gvFormat(xattrsType, vals, true, false)
}

// fileMetadata is the mode independent metadata of a file object.
type fileMetadata struct {
	UID           uint32
	GID           uint32
	Mode          uint32
	Size          uint64
	SymlinkTarget string
	Xattrs        []object.XAttr
}

// readFileMetadata reads the metadata of a file object in the repository's
// current mode (archive headers, bare filesystem metadata or bare-user
// user.ostreemeta).
func readFileMetadata(r *repo.Repo, checksum string) (*fileMetadata, error) {
	mode := r.Mode()
	if mode.IsArchive() {
		data, err := r.ReadObject(checksum, repo.ObjectFile)
		if err != nil {
			return nil, fileObjectError(err, checksum)
		}
		h, _, err := object.ParseArchiveHeader(data)
		if err != nil {
			return nil, err
		}
		return &fileMetadata{
			UID:           h.UID,
			GID:           h.GID,
			Mode:          h.Mode,
			Size:          h.Size,
			SymlinkTarget: h.SymlinkTarget,
			Xattrs:        h.Xattrs,
		}, nil
	}

	path, err := r.ObjectPath(checksum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fileObjectError(err, checksum)
	}

	fm := &fileMetadata{
		UID:  func() uint32 { u, _ := fsutil.Owner(fi); return u }(),
		GID:  func() uint32 { _, g := fsutil.Owner(fi); return g }(),
		Mode: uint32(fi.Mode().Perm()),
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return nil, err
		}
		fm.Mode |= 0120000
		fm.SymlinkTarget = target
	} else {
		fm.Mode |= 0100000
		fm.Size = uint64(fi.Size())
	}

	switch mode {
	case repo.ModeBare:
		xattrs, err := readFilesystemXattrs(path)
		if err != nil {
			return nil, err
		}
		fm.Xattrs = xattrs
	case repo.ModeBareUser:
		// bare-user stores the real metadata (including symlink mode)
		// in the user.ostreemeta xattr.
		if data, err := lgetxattr(path, "user.ostreemeta"); err == nil {
			meta, err := object.ParseFileMeta(data)
			if err != nil {
				return nil, err
			}
			fm.UID, fm.GID, fm.Mode = meta.UID, meta.GID, meta.Mode
			fm.Xattrs = meta.Xattrs
			if fm.Mode&userVisibleModeMask == 0120000 {
				content, err := r.ReadObject(checksum, repo.ObjectFile)
				if err != nil {
					return nil, fileObjectError(err, checksum)
				}
				fm.SymlinkTarget = strings.TrimSuffix(string(content), "\x00")
				fm.Size = 0
			}
		} else if !errors.Is(err, unix.ENODATA) {
			return nil, err
		}
	case repo.ModeBareSplitXattrs:
		// In split-xattrs mode the xattr array lives in a separate
		// object with the same checksum.
		if data, err := r.ReadObject(checksum, repo.ObjectFileXattrs); err == nil {
			v, err := gvariant.Decode(xattrsType, data)
			if err != nil {
				return nil, err
			}
			xattrs, err := parseXattrValues(v)
			if err != nil {
				return nil, err
			}
			fm.Xattrs = xattrs
		} else if !errors.Is(err, repo.ErrObjectNotFound) {
			return nil, err
		}
	}
	return fm, nil
}

func fileObjectError(err error, checksum string) error {
	if errors.Is(err, repo.ErrObjectNotFound) || errors.Is(err, fs.ErrNotExist) {
		// Matches ostree_repo_load_file()'s error text.
		return fmt.Errorf("Couldn't find file object '%s'", checksum)
	}
	return err
}

func parseXattrValues(v gvariant.Value) ([]object.XAttr, error) {
	vals, ok := v.([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("xattrs: malformed value")
	}
	out := make([]object.XAttr, 0, len(vals))
	for _, e := range vals {
		pair, ok := e.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("xattrs: malformed entry")
		}
		name, ok1 := pair[0].([]byte)
		value, ok2 := pair[1].([]byte)
		if !ok1 || !ok2 || len(name) == 0 || name[len(name)-1] != 0 {
			return nil, fmt.Errorf("xattrs: malformed entry")
		}
		out = append(out, object.XAttr{Name: string(name[:len(name)-1]), Value: value})
	}
	return object.Canonicalize(out)
}

// readFilesystemXattrs reads all extended attributes of path without
// following symlinks.
func readFilesystemXattrs(path string) ([]object.XAttr, error) {
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
	for _, raw := range bytes.Split(buf[:n], []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		name := string(raw)
		value, err := lgetxattr(path, name)
		if err != nil {
			if err == unix.ENODATA {
				continue
			}
			return nil, err
		}
		out = append(out, object.XAttr{Name: name, Value: value})
	}
	return object.Canonicalize(out)
}

func lgetxattr(path, name string) ([]byte, error) {
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
