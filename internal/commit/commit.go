// Package commit builds OSTree commit objects from a filesystem tree.
package commit

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/fsutil"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/tree"
)

// Options controls Create.
type Options struct {
	Repo         *repo.Repo
	TreeDir      string
	Branch       string
	Parent       string
	Subject      string
	Body         string
	Timestamp    uint64
	TimestampSet bool
	OwnerUID     *uint32
	OwnerGID     *uint32
	NoXattrs     bool
	Orphan       bool
	NoBindings   bool
	BindRefs     []string
	Metadata     map[string]string
	// RawMetadata holds values given in GVariant text form.
	RawMetadata map[string]gvariant.Variant
	// KeepMetadata lists keys copied from the parent commit's metadata.
	KeepMetadata []string
	// DetachedMetadata is written as the commitmeta object.
	DetachedMetadata map[string]string
	// LinkCheckoutSpeedup reuses metadata for files hardlinked into the repo.
	LinkCheckoutSpeedup bool
	// DevinoCanonical forces repository metadata to win over owner overrides.
	DevinoCanonical bool
	// BaseTree and BaseMeta, when set, reuse an existing tree instead of
	// walking TreeDir.
	BaseTree string
	BaseMeta string
	// SkipList lists tree-relative paths to omit.
	SkipList []string
	// StatOverride lists "MODE PATH" or "=MODE PATH" overrides.
	StatOverride []string
	// Consume removes the source tree after a successful commit.
	Consume bool
	// CanonicalPermissions masks modes the way bare-user does.
	CanonicalPermissions bool
	// ModeROExecutables clears write bits on executable files.
	ModeROExecutables bool
	// Bootable requires the tree to contain a kernel.
	Bootable bool
	// Stats, when set, receives object statistics like --table-output.
	Stats *Stats
	// GenerateSizes records the ostree.sizes metadata entry (archive repos).
	GenerateSizes bool
}

// Stats mirrors OstreeRepoCommitStats.
type Stats struct {
	MetadataTotal       uint64
	MetadataWritten     uint64
	ContentTotal        uint64
	ContentWritten      uint64
	DevinoCacheHits     uint64
	ContentBytesWritten uint64
}

// Create writes all objects for the tree and returns the commit checksum.
// When Branch is set and Orphan is false the branch ref is updated.
func Create(opts Options) (string, error) {
	if opts.Repo == nil {
		return "", fmt.Errorf("no repository")
	}
	if opts.TreeDir == "" && opts.BaseTree == "" {
		return "", fmt.Errorf("a tree is required")
	}
	if os.Getenv("OSTREE_REPO_TEST_ERROR") == "pre-commit" {
		bootID := os.Getenv("OSTREE_BOOTID")
		if bootID == "" {
			bootID = "test"
		}
		dir := filepath.Join(opts.Repo.Path(), "tmp", "staging-"+bootID+"-"+"error")
		if err := os.MkdirAll(dir, 0775); err == nil {
			_ = dir
		}
		return "", fmt.Errorf("OSTREE_REPO_TEST_ERROR_PRE_COMMIT")
	}
	b := &builder{repo: opts.Repo, opts: opts, skip: map[string]bool{}, overrides: map[string]statOverride{}, seenMeta: map[string]bool{}, seenFiles: map[string]bool{}, sizes: map[string]sizeEntry{}}
	if err := b.parseModifiers(); err != nil {
		return "", err
	}
	var treeCsum, metaCsum string
	if opts.BaseTree != "" {
		treeCsum, metaCsum = opts.BaseTree, opts.BaseMeta
	} else {
		var err error
		treeCsum, metaCsum, err = b.commitDir(opts.TreeDir)
		if err != nil {
			return "", err
		}
	}

	// --bootable metadata is derived from the written tree below, matching
	// ostree_commit_metadata_for_bootable().

	parent := ""
	switch {
	case opts.Parent == "none":
		// Explicitly requested an orphan commit.
	case opts.Parent != "":
		p, err := resolveRev(opts.Repo, opts.Parent)
		if err != nil {
			return "", err
		}
		parent = p
	case opts.Branch != "" && !opts.Orphan:
		// Like ostree_repo_resolve_rev() with allow_noent, the current
		// branch is the parent when it exists. A refspec such as
		// "origin:branch" resolves through refs/remotes.
		p, ok, err := opts.Repo.ResolveRef(opts.Branch, true, true)
		switch {
		case err == nil && ok:
			parent = p
		case err != nil && !errors.Is(err, repo.ErrRefIsDirectory):
			return "", err
		}
	}

	c := &object.Commit{
		Metadata:  gvariant.Dict{},
		Parent:    parent,
		Subject:   opts.Subject,
		Body:      opts.Body,
		Timestamp: opts.Timestamp,
		RootTree:  treeCsum,
		RootMeta:  metaCsum,
	}
	if !opts.TimestampSet {
		if env := os.Getenv("SOURCE_DATE_EPOCH"); env != "" {
			n, err := strconv.ParseInt(env, 10, 64)
			if err != nil {
				if errors.Is(err, strconv.ErrRange) {
					return "", fmt.Errorf("Parsing SOURCE_DATE_EPOCH: Numerical result out of range")
				}
				return "", fmt.Errorf("Failed to convert SOURCE_DATE_EPOCH")
			}
			c.Timestamp = uint64(n)
		} else {
			c.Timestamp = uint64(time.Now().Unix())
		}
	}
	bindings := opts.BindRefs
	if opts.Branch != "" && !opts.NoBindings {
		bindings = append([]string{opts.Branch}, bindings...)
	}
	if len(bindings) > 0 {
		sort.Strings(bindings)
		uniq := bindings[:0]
		for i, b := range bindings {
			if i == 0 || b != bindings[i-1] {
				uniq = append(uniq, b)
			}
		}
		bindings = uniq
		arr := make([]gvariant.Value, len(bindings))
		for i, r := range bindings {
			arr[i] = r
		}
		c.Metadata.Set(object.MetaRefBinding, gvariant.Variant{Type: gvariant.MustParseType("as"), Value: arr})
	}
	for k, v := range opts.Metadata {
		if err := c.SetMetadata(k, v); err != nil {
			return "", err
		}
	}
	for k, v := range opts.RawMetadata {
		c.Metadata.Set(k, v)
	}
	if len(opts.KeepMetadata) > 0 && parent != "" {
		parentCommit, err := tree.LoadCommit(opts.Repo, parent)
		if err != nil {
			return "", err
		}
		for _, key := range opts.KeepMetadata {
			if v, ok := parentCommit.Metadata.Get(key); ok {
				c.Metadata.Set(key, v)
			}
		}
	}
	if opts.Bootable {
		linuxRelease, err := bootableLinuxRelease(opts.Repo, treeCsum)
		if err != nil {
			return "", err
		}
		if err := c.SetMetadata("ostree.bootable", true); err != nil {
			return "", err
		}
		if err := c.SetMetadata("ostree.linux", linuxRelease); err != nil {
			return "", err
		}
	}
	if collectionID, err := opts.Repo.Config().Get("core", "collection-id"); err == nil && collectionID != "" {
		if _, ok := c.Metadata.Get(object.MetaCollectionBinding); !ok {
			if err := c.SetMetadata(object.MetaCollectionBinding, collectionID); err != nil {
				return "", err
			}
		}
	}
	// The ostree.sizes metadata records the compressed and unpacked size of
	// every object in the commit's tree; libostree only generates it for
	// archive repositories.
	if opts.GenerateSizes && len(b.sizes) > 0 {
		sizes, err := encodeSizesMetadata(b.sizes)
		if err != nil {
			return "", err
		}
		c.Metadata.Set(object.MetaSizes, gvariant.Variant{Type: gvariant.MustParseType("aay"), Value: sizes})
	}
	data, err := c.Encode()
	if err != nil {
		return "", err
	}
	sum, err := b.writeMeta(repo.ObjectCommit, data)
	if err != nil {
		return "", err
	}
	if opts.Branch != "" && !opts.Orphan {
		// --branch accepts a refspec; "origin:remote1" updates
		// refs/remotes/origin/remote1 rather than a local ref.
		if err := opts.Repo.WriteRefSpec(opts.Branch, sum); err != nil {
			return "", err
		}
	}
	if len(opts.DetachedMetadata) > 0 {
		dict := gvariant.Dict{}
		for k, v := range opts.DetachedMetadata {
			dict.Set(k, gvariant.Variant{Type: gvariant.MustParseType("s"), Value: v})
		}
		data, err := gvariant.Encode(gvariant.MustParseType("a{sv}"), dict)
		if err != nil {
			return "", err
		}
		if err := opts.Repo.WriteRawObject(sum, repo.ObjectCommitmeta, data); err != nil {
			return "", err
		}
	}
	if opts.Consume && opts.TreeDir != "" {
		if err := os.RemoveAll(opts.TreeDir); err != nil {
			return "", err
		}
	}
	return sum, nil
}

type builder struct {
	repo      *repo.Repo
	opts      Options
	skip      map[string]bool
	overrides map[string]statOverride
	seenMeta  map[string]bool
	seenFiles map[string]bool
	devino    map[devIno]string
	devinoOK  bool
	sizes     map[string]sizeEntry
}

// sizeEntry mirrors OstreeContentSizeCacheEntry: the compressed ("archived")
// and uncompressed size of one object, keyed by checksum in the ostree.sizes
// metadata.
type sizeEntry struct {
	objtype  repo.ObjectType
	archived uint64
	unpacked uint64
}

type devIno struct {
	dev uint64
	ino uint64
}

type statOverride struct {
	Mode   uint32
	Op     byte // '=', '+', '-' or 0 for replace
	Exact  bool
	HasUID bool
	HasGID bool
	UID    uint32
	GID    uint32
}

func (b *builder) parseModifiers() error {
	for _, p := range b.opts.SkipList {
		if p != "" {
			b.skip[p] = true
		}
	}
	for _, rule := range b.opts.StatOverride {
		exact := false
		op := byte(0)
		rule = strings.TrimSpace(rule)
		if len(rule) > 0 {
			switch rule[0] {
			case '=':
				exact = true
				op = '='
				rule = rule[1:]
			case '+', '-':
				op = rule[0]
				rule = rule[1:]
			}
		}
		fields := strings.Fields(rule)
		if len(fields) < 2 {
			return fmt.Errorf("Invalid statoverride %q", rule)
		}
		base := 10
		if !strings.ContainsAny(fields[0], "89") {
			base = 8
		}
		mode, err := strconv.ParseUint(fields[0], base, 32)
		if err != nil {
			return fmt.Errorf("Invalid statoverride mode %q", fields[0])
		}
		path := strings.Join(fields[1:], " ")
		b.overrides[strings.TrimPrefix(path, "/")] = statOverride{Mode: uint32(mode), Op: op, Exact: exact}
	}
	return nil
}

// applyModifiers adjusts path metadata according to the commit options.
func (b *builder) applyModifiers(rel string, mode uint32) uint32 {
	if o, ok := b.overrides[strings.TrimPrefix(rel, "/")]; ok {
		switch o.Op {
		case '+':
			mode |= o.Mode & 07777
		case '-':
			mode &^= o.Mode & 07777
		case '=':
			mode = (mode &^ 07777) | o.Mode&07777
		default:
			mode = (mode &^ 07777) | o.Mode&07777
		}
	}
	if (b.opts.CanonicalPermissions || b.repo.Mode() == repo.ModeBareUserOnly) && mode&0170000 != 0120000 {
		// _ostree_repo_commit_modifier_apply(): canonical permissions keep
		// only the 0755 bits of regular files and directories.  Symlink
		// modes are taken verbatim from lstat (0777; the mode of a symlink
		// cannot be changed on disk).
		mode = (mode &^ 07777) | (mode & 0755)
	}
	// The commit_filter() --mode-ro-executables rule only applies to
	// regular files; symlinks keep their 0777 mode.
	if b.opts.ModeROExecutables && mode&0170000 == 0100000 && mode&0111 != 0 {
		mode &^= 0222
	}
	return mode
}

// commitDir writes dirmeta, dirtree and all child objects for dir, returning
// the dirtree and dirmeta checksums.
func (b *builder) commitDir(dir string) (treeCsum, metaCsum string, err error) {
	return b.commitDirRel(dir, "")
}

func (b *builder) commitDirRel(dir, rel string) (treeCsum, metaCsum string, err error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return "", "", err
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("not a directory: %s", dir)
	}
	xattrs, err := b.readXattrs(dir)
	if err != nil {
		return "", "", err
	}
	uid, gid := b.ownership(info)
	dirMode := b.applyModifiers(rel, fsutil.UnixMode(info.Mode())|0040000)
	dm := &object.Dirmeta{UID: uid, GID: gid, Mode: dirMode, Xattrs: xattrs}
	dmData, err := dm.Encode()
	if err != nil {
		return "", "", err
	}
	metaCsum, err = b.writeMeta(repo.ObjectDirmeta, dmData)
	if err != nil {
		return "", "", err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	tree := &object.Dirtree{}
	for _, name := range names {
		if !utf8.ValidString(name) {
			return "", "", fmt.Errorf("Invalid UTF-8 in filename %q", name)
		}
		full := filepath.Join(dir, name)
		childRel := path.Join(rel, name)
		if b.skip[childRel] || b.skip["/"+childRel] {
			continue
		}
		st, err := os.Lstat(full)
		if err != nil {
			return "", "", err
		}
		switch {
		case st.IsDir():
			subTree, subMeta, err := b.commitDirRel(full, childRel)
			if err != nil {
				return "", "", err
			}
			tree.Dirs = append(tree.Dirs, object.DirEntry{Name: name, Tree: subTree, Meta: subMeta})
		case st.Mode().IsRegular(), st.Mode()&os.ModeSymlink != 0:
			sum, err := b.writeFile(full, name, childRel, st)
			if err != nil {
				return "", "", err
			}
			tree.Files = append(tree.Files, object.FileEntry{Name: name, Checksum: sum})
		default:
			return "", "", fmt.Errorf("unsupported file type: %s", full)
		}
	}
	treeData, err := tree.Encode()
	if err != nil {
		return "", "", err
	}
	treeCsum, err = b.writeMeta(repo.ObjectDirtree, treeData)
	if err != nil {
		return "", "", err
	}
	return treeCsum, metaCsum, nil
}

func (b *builder) ownership(info os.FileInfo) (uint32, uint32) {
	uid, gid := fsutil.Owner(info)
	if b.opts.OwnerUID != nil {
		uid = *b.opts.OwnerUID
	}
	if b.opts.OwnerGID != nil {
		gid = *b.opts.OwnerGID
	}
	// --canonical-permissions (and committing into a bare-user-only repo,
	// which always canonicalizes) also squashes ownership to root, matching
	// _ostree_repo_commit_modifier_apply().
	if b.opts.CanonicalPermissions || b.repo.Mode() == repo.ModeBareUserOnly {
		uid, gid = 0, 0
	}
	return uid, gid
}

func (b *builder) readXattrs(path string) ([]object.XAttr, error) {
	if b.opts.NoXattrs {
		return nil, nil
	}
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
	for _, name := range bytes.Split(buf[:n], []byte{0}) {
		if len(name) == 0 {
			continue
		}
		value, err := readXattr(path, string(name))
		if err != nil {
			if err == unix.ENODATA {
				continue
			}
			return nil, err
		}
		out = append(out, object.XAttr{Name: string(name), Value: value})
	}
	return object.Canonicalize(out)
}

func readXattr(path, name string) ([]byte, error) {
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

func (b *builder) writeFile(filePath, name, rel string, info os.FileInfo) (string, error) {
	if info.Mode().IsRegular() {
		ownerOverrides := b.opts.OwnerUID != nil || b.opts.OwnerGID != nil
		if b.opts.DevinoCanonical || !ownerOverrides {
			if sum, ok := b.devinoLookup(info); ok {
				return b.reuseContent(sum, rel, info)
			}
		}
	}
	uid, gid := b.ownership(info)
	xattrs, err := b.readXattrs(filePath)
	if err != nil {
		return "", err
	}
	xattrMap := map[string][]byte{}
	for _, x := range xattrs {
		xattrMap[x.Name] = x.Value
	}
	mode := fsutil.UnixMode(info.Mode())
	hdr := &object.FileHeader{UID: uid, GID: gid, Xattrs: xattrs}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(filePath)
		if err != nil {
			return "", err
		}
		hdr.Mode = b.applyModifiers(rel, mode|0120000)
		hdr.SymlinkTarget = target
		sum, err := hdr.Checksum(nil)
		if err != nil {
			return "", err
		}
		return b.writeSymlink(sum, hdr, target, xattrMap)
	}

	hdr.Mode = b.applyModifiers(rel, mode|0100000)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	sum, err := hdr.Checksum(content)
	if err != nil {
		return "", err
	}
	return b.writeRegular(sum, hdr, content, xattrMap, filePath)
}

func (b *builder) writeSymlink(sum string, hdr *object.FileHeader, target string, xattrs map[string][]byte) (string, error) {
	b.noteContent(sum, 0)
	switch b.repo.Mode() {
	case repo.ModeArchive:
		payload, err := hdr.EncodeArchive()
		if err != nil {
			return "", err
		}
		b.noteSize(repo.ObjectFile, sum, uint64(len(target)), uint64(len(payload)))
		if err := b.repo.WriteRawObject(sum, repo.ObjectFile, payload); err != nil {
			return "", err
		}
		return sum, nil
	case repo.ModeBare:
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Xattrs: xattrs}
		if err := b.repo.WriteSymlinkObject(sum, repo.ObjectFile, target, meta); err != nil {
			return "", err
		}
		return sum, nil
	case repo.ModeBareUser:
		ostreemeta, err := object.EncodeFileMeta(hdr.UID, hdr.GID, hdr.Mode, hdr.Xattrs)
		if err != nil {
			return "", err
		}
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Ostreemeta: ostreemeta}
		if err := b.repo.WriteFileRawObject(sum, append([]byte(target), 0), meta); err != nil {
			return "", err
		}
		return sum, nil
	default:
		// bare-user-only stores real symlinks; the metadata comes from lstat.
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode}
		if err := b.repo.WriteSymlinkObject(sum, repo.ObjectFile, target, meta); err != nil {
			return "", err
		}
		return sum, nil
	}
}

func (b *builder) writeRegular(sum string, hdr *object.FileHeader, content []byte, xattrs map[string][]byte, filePath string) (string, error) {
	b.noteContent(sum, len(content))
	// Consume+link-checkout-speedup can adopt a single-link file into the
	// object store, preserving hardlinks held by the caller.
	if b.opts.Consume && b.opts.LinkCheckoutSpeedup && (b.repo.Mode() == repo.ModeBare || b.repo.Mode() == repo.ModeBareUserOnly) {
		if err := b.repo.AdoptFile(sum, repo.ObjectFile, filePath); err == nil {
			return sum, nil
		}
	}
	var payload []byte
	switch b.repo.Mode() {
	case repo.ModeArchive:
		hdr.Size = uint64(len(content))
		header, err := hdr.EncodeArchive()
		if err != nil {
			return "", err
		}
		compressed, err := object.Deflate(content, 6)
		if err != nil {
			return "", err
		}
		payload = append(header, compressed...)
	default:
		payload = content
	}
	b.noteSize(repo.ObjectFile, sum, uint64(len(content)), uint64(len(payload)))

	meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode}
	switch b.repo.Mode() {
	case repo.ModeBare:
		meta.Xattrs = xattrs
	case repo.ModeBareUser:
		ostreemeta, err := object.EncodeFileMeta(hdr.UID, hdr.GID, hdr.Mode, hdr.Xattrs)
		if err != nil {
			return "", err
		}
		meta.Ostreemeta = ostreemeta
	}
	if err := b.repo.WriteFileRawObject(sum, payload, meta); err != nil {
		return "", err
	}
	return sum, nil
}

func resolveRev(r *repo.Repo, rev string) (string, error) {
	return tree.ResolveRef(r, rev)
}

// lookupDirtreeChild returns the checksum of the named subdirectory of t, or
// "" when it does not exist.
func lookupDirtreeChild(t *object.Dirtree, name string) string {
	for _, d := range t.Dirs {
		if d.Name == name {
			return d.Tree
		}
	}
	return ""
}

// bootableLinuxRelease implements ostree_commit_metadata_for_bootable(): it
// scans usr/lib/modules/*/vmlinuz for the (single) kernel release.
func bootableLinuxRelease(r *repo.Repo, rootTree string) (string, error) {
	t, err := tree.LoadDirtree(r, rootTree)
	if err != nil {
		return "", err
	}
	for _, name := range []string{"usr", "lib", "modules"} {
		treeCsum := lookupDirtreeChild(t, name)
		if treeCsum == "" {
			return "", fmt.Errorf("Opening usr/lib/modules: No such file or directory")
		}
		t, err = tree.LoadDirtree(r, treeCsum)
		if err != nil {
			return "", err
		}
	}

	linuxRelease := ""
	for _, d := range t.Dirs {
		child, err := tree.LoadDirtree(r, d.Tree)
		if err != nil {
			return "", err
		}
		hasKernel := false
		for _, f := range child.Files {
			if f.Name == "vmlinuz" {
				hasKernel = true
				break
			}
		}
		if !hasKernel {
			continue
		}
		if linuxRelease != "" {
			return "", fmt.Errorf("Multiple kernels found in /usr/lib/modules")
		}
		linuxRelease = d.Name
	}
	if linuxRelease == "" {
		return "", fmt.Errorf("No kernel found in /usr/lib/modules")
	}
	return linuxRelease, nil
}

func (b *builder) writeMeta(typ repo.ObjectType, data []byte) (string, error) {
	sum := checksum.Hex(checksum.Sum(data))
	// Only file, dirtree and dirmeta objects are recorded in ostree.sizes.
	if typ == repo.ObjectDirtree || typ == repo.ObjectDirmeta {
		b.noteSize(typ, sum, uint64(len(data)), uint64(len(data)))
	}
	if b.seenMeta[sum] {
		return sum, nil
	}
	b.seenMeta[sum] = true
	if st := b.opts.Stats; st != nil {
		st.MetadataTotal++
	}
	existed, err := b.repo.HasObject(sum, typ)
	if err != nil {
		return "", err
	}
	if existed {
		return sum, nil
	}
	if st := b.opts.Stats; st != nil {
		st.MetadataWritten++
	}
	if _, err := b.repo.WriteObject(typ, data); err != nil {
		return "", err
	}
	return sum, nil
}

// noteSize records the size of an object for the ostree.sizes metadata.
// libostree only generates size information for archive repositories.
func (b *builder) noteSize(typ repo.ObjectType, sum string, unpacked, archived uint64) {
	if !b.opts.GenerateSizes || b.repo.Mode() != repo.ModeArchive {
		return
	}
	if typ != repo.ObjectFile && typ != repo.ObjectDirtree && typ != repo.ObjectDirmeta {
		return
	}
	if b.sizes == nil {
		b.sizes = map[string]sizeEntry{}
	}
	if _, ok := b.sizes[sum]; ok {
		return
	}
	b.sizes[sum] = sizeEntry{objtype: typ, archived: archived, unpacked: unpacked}
}

// encodeSizesMetadata serializes the ostree.sizes value: an array of byte
// arrays, each holding a binary checksum, varint compressed size, varint
// unpacked size and the object type, sorted by checksum like libostree.
func encodeSizesMetadata(sizes map[string]sizeEntry) ([]gvariant.Value, error) {
	keys := make([]string, 0, len(sizes))
	for k := range sizes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]gvariant.Value, 0, len(keys))
	for _, k := range keys {
		e := sizes[k]
		csum, err := checksum.FromHex(k)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		buf.Write(csum[:])
		writeVaruint64(&buf, e.archived)
		writeVaruint64(&buf, e.unpacked)
		buf.WriteByte(byte(e.objtype))
		out = append(out, buf.Bytes())
	}
	return out, nil
}

// writeVaruint64 appends libostree's varint encoding of n (_ostree_write_varuint64).
func writeVaruint64(buf *bytes.Buffer, n uint64) {
	for n >= 0x80 {
		buf.WriteByte(byte(n) | 0x80)
		n >>= 7
	}
	buf.WriteByte(byte(n))
}

// noteContent records stats for a content object that is about to be written.
func (b *builder) noteContent(sum string, size int) {
	if b.seenFiles[sum] {
		return
	}
	b.seenFiles[sum] = true
	st := b.opts.Stats
	if st == nil {
		return
	}
	st.ContentTotal++
	if existed, err := b.repo.HasObject(sum, repo.ObjectFile); err == nil && !existed {
		st.ContentWritten++
		st.ContentBytesWritten += uint64(size)
	}
}

// devinoLookup maps a hardlinked file back to an existing content object.
func (b *builder) devinoLookup(info os.FileInfo) (string, bool) {
	if !b.opts.LinkCheckoutSpeedup && !b.opts.DevinoCanonical {
		return "", false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return "", false
	}
	if !b.devinoOK {
		b.devinoOK = true
		b.devino = map[devIno]string{}
		objectsDir := filepath.Join(b.repo.Path(), "objects")
		filepath.Walk(objectsDir, func(p string, fi os.FileInfo, err error) error {
			if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
				return nil
			}
			name := fi.Name()
			if !strings.HasSuffix(name, ".file") {
				return nil
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				return nil
			}
			dir := filepath.Base(filepath.Dir(p))
			csum := dir + strings.TrimSuffix(name, ".file")
			b.devino[devIno{uint64(st.Dev), uint64(st.Ino)}] = csum
			return nil
		})
	}
	sum, ok := b.devino[devIno{uint64(st.Dev), uint64(st.Ino)}]
	return sum, ok
}

// reuseContent records a devino cache hit and reuses an existing object.
func (b *builder) reuseContent(sum, rel string, info os.FileInfo) (string, error) {
	if st := b.opts.Stats; st != nil {
		st.ContentTotal++
		if !b.seenFiles[sum] {
			st.DevinoCacheHits++
		}
	}
	b.seenFiles[sum] = true
	return sum, nil
}

func statNlink(path string) (uint64, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), true
	}
	return 0, false
}
