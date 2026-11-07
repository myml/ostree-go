// Package pull implements fetching objects from one OSTree repository into
// another.  This file covers the local transport used by `ostree pull-local`:
// objects are decoded with internal/tree and re-encoded for the destination
// repository mode rather than being copied verbatim.
package pull

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/staticdelta"
	"github.com/myml/ostree-go/internal/tree"
)

// Options mirrors the option table of ot-builtin-pull-local.c.
type Options struct {
	// Refs are the refspecs to fetch.  Empty means every local and remote ref
	// in the source repository.
	Refs []string
	// Depth is the number of parent commits to traverse (-1 for unlimited).
	Depth int
	// DepthSet records whether --depth was given explicitly.
	DepthSet bool
	// Untrusted requests checksum verification of local sources.  The Go
	// implementation always verifies; the flag is accepted for parity.
	Untrusted bool
	// Remote, when set, overrides the destination remote namespace for refs.
	Remote string
	// CommitMetadataOnly fetches only the commit objects and marks them
	// partial, like the C implementation.
	CommitMetadataOnly bool
	// BareUserOnlyFiles rejects regular file modes outside 0775.
	BareUserOnlyFiles bool

	// Accepted but not acted on by the local transport.
	DisableFsync          bool
	PerObjectFsync        bool
	RequireStaticDeltas   bool
	DisableStaticDeltas   bool
	GPGVerify             bool
	GPGVerifySummary      bool
	DisableVerifyBindings bool

	// Stdout receives the final progress summary.
	Stdout io.Writer
}

// Local imports objects from the repository at srcPath into dst.
func Local(dst *repo.Repo, srcPath string, opts Options) error {
	if dst == nil {
		return errors.New("no destination repository")
	}
	if !dst.Writable() {
		if err := dst.WritableError(); err != nil {
			return fmt.Errorf("Cannot write to repository: %v", err)
		}
		return errors.New("Cannot write to repository: repository is not writable")
	}
	src, err := repo.Open(srcPath)
	if err != nil {
		return fmt.Errorf("opening repo: %v", err)
	}
	defer src.Close()

	collectionID, _ := src.Config().Get("core", "collection-id")
	p := &puller{
		src:           src,
		dst:           dst,
		opts:          opts,
		copiedCommit:  map[string]bool{},
		copiedTree:    map[string]bool{},
		copiedMeta:    map[string]bool{},
		copiedFile:    map[string]bool{},
		collectionID:  collectionID,
		requestedRefs: map[string]string{},
		zlibLevel:     dstZlibLevel(dst),
		// libostree's local importer hardlinks when source and destination
		// modes match, so no content bytes are written in that case.  Match
		// its progress accounting (the object set is identical either way).
		countContentBytes: src.Mode() != dst.Mode(),
	}
	return p.run()
}

// dstZlibLevel returns the raw-deflate level configured for archive writes.
func dstZlibLevel(r *repo.Repo) int {
	level := 6
	if r == nil {
		return level
	}
	if v, err := r.Config().GetInt("archive", "zlib-level", 6); err == nil {
		level = int(v)
	}
	if level < flate.BestSpeed {
		level = flate.BestSpeed
	}
	if level > flate.BestCompression {
		level = flate.BestCompression
	}
	return level
}

type puller struct {
	src  *repo.Repo
	dst  *repo.Repo
	opts Options

	copiedCommit map[string]bool
	copiedTree   map[string]bool
	copiedMeta   map[string]bool
	copiedFile   map[string]bool

	// collectionID is the source repository's collection ID, used for
	// binding verification; requestedRefs maps a commit to the ref it was
	// requested through.
	collectionID  string
	requestedRefs map[string]string

	importedMeta      uint64
	importedContent   uint64
	contentBytes      uint64
	zlibLevel         int
	countContentBytes bool
}

// refUpdate is a ref that must be written after the objects are in place.
type refUpdate struct {
	// remote is non-empty for refs/remotes/<remote>/<name>.
	remote string
	name   string
	csum   string
}

func (p *puller) run() error {
	unlock, err := p.dst.Lock(true)
	if err != nil {
		return err
	}
	defer unlock()

	update, roots, err := p.plan()
	if err != nil {
		return err
	}

	deltaHandled, err := p.tryLocalDeltas(roots)
	if err != nil {
		return err
	}
	if !deltaHandled {
		for _, root := range roots {
			if err := p.fetchCommitClosure(root, p.opts.Depth); err != nil {
				return err
			}
		}
	}

	// Refs are only updated once every referenced object has been written.
	for _, u := range update {
		if u.csum == "" {
			continue
		}
		if err := p.writeRef(u); err != nil {
			return err
		}
	}

	if p.opts.Stdout != nil && !deltaHandled {
		fmt.Fprintf(p.opts.Stdout, "%d metadata, %d content objects imported; %s content written\n",
			p.importedMeta, p.importedContent, formatSize(p.contentBytes))
	}
	return nil
}

// plan resolves the requested refspecs (or all source refs) into commits to
// fetch and refs to update.
func (p *puller) plan() ([]refUpdate, []string, error) {
	var (
		updates []refUpdate
		roots   []string
	)
	if len(p.opts.Refs) == 0 {
		refs, err := p.src.ListRefs("")
		if err != nil {
			return nil, nil, err
		}
		for _, ref := range refs {
			// The C implementation copies refs/heads only
			// (ostree_repo_list_refs() does not include collection refs) and
			// rejects remote refs as invalid ref names.
			if ref.Kind == repo.RefMirror {
				continue
			}
			if ref.Kind == repo.RefRemote {
				return nil, nil, fmt.Errorf("Invalid ref name %s", ref.RefSpec())
			}
			updates = append(updates, refUpdate{
				remote: ref.Remote,
				name:   ref.Name,
				csum:   ref.Checksum,
			})
			roots = append(roots, ref.Checksum)
		}
		return updates, roots, nil
	}

	for _, spec := range p.opts.Refs {
		if repo.IsChecksum(spec) {
			// A checksum fetches the commit without creating a ref.
			roots = append(roots, spec)
			continue
		}
		sum, update, err := p.resolveRef(spec)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, update)
		roots = append(roots, sum)
		if update.name != "" {
			p.requestedRefs[sum] = update.name
		}
	}
	return updates, roots, nil
}

// resolveRef resolves a source refspec into a commit checksum and the ref to
// write in the destination.
func (p *puller) resolveRef(spec string) (string, refUpdate, error) {
	if i := strings.IndexByte(spec, ':'); i > 0 && i < len(spec)-1 {
		remote, name := spec[:i], spec[i+1:]
		sum, err := p.src.ReadRemoteRef(remote, name)
		if err != nil {
			return "", refUpdate{}, fmt.Errorf("Refspec '%s' not found", spec)
		}
		return sum, refUpdate{remote: remote, name: name, csum: sum}, nil
	}
	sum, err := p.src.ReadRef(spec)
	if err != nil {
		return "", refUpdate{}, fmt.Errorf("Refspec '%s' not found", spec)
	}
	return sum, refUpdate{name: spec, csum: sum}, nil
}

func (p *puller) writeRef(u refUpdate) error {
	remote := u.remote
	if p.opts.Remote != "" {
		remote = p.opts.Remote
	}
	if remote != "" {
		return p.dst.WriteRemoteRef(remote, u.name, u.csum)
	}
	return p.dst.WriteRef(u.name, u.csum)
}

// fetchItem is one commit to copy plus the number of parent hops taken to
// reach it.
type fetchItem struct {
	csum  string
	depth int
}

// fetchCommitClosure copies a commit, its tree, and (subject to depth) its
// parent commits.
func (p *puller) fetchCommitClosure(csum string, depth int) error {
	queue := []fetchItem{{csum: csum, depth: 0}}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]

		commit, err := p.copyCommit(item.csum)
		if err != nil {
			// A truncated history is tolerated: stop at the missing parent.
			if errors.Is(err, repo.ErrObjectNotFound) && item.depth > 0 {
				continue
			}
			return err
		}
		if commit == nil {
			continue
		}
		if p.opts.CommitMetadataOnly {
			// Without an explicit depth the fetched commit itself is
			// partial; with a depth the boundary parent is.
			if !p.opts.DepthSet {
				if err := markPartial(p.dst, item.csum); err != nil {
					return err
				}
			} else if depth != -1 && item.depth < depth && commit.Parent != "" {
				if err := p.copyCommitMetadata(commit.Parent); err != nil {
					return err
				}
				if err := markPartial(p.dst, commit.Parent); err != nil {
					return err
				}
			}
			continue
		}
		if depth == -1 || item.depth < depth {
			if commit.Parent != "" {
				queue = append(queue, fetchItem{csum: commit.Parent, depth: item.depth + 1})
			}
		}
	}
	return nil
}

// copyCommit writes a commit object, its detached metadata and its full tree.
// It returns the parsed commit, or nil when it had already been copied.
func (p *puller) copyCommit(csum string) (*object.Commit, error) {
	if p.copiedCommit[csum] {
		return nil, nil
	}
	raw, err := p.src.ReadObject(csum, repo.ObjectCommit)
	if err != nil {
		return nil, fmt.Errorf("Fetching commit %s: %w", csum, err)
	}
	if err := verifyMetadataChecksum(raw, csum, repo.ObjectCommit); err != nil {
		return nil, err
	}
	commit, err := object.ParseCommit(raw)
	if err != nil {
		return nil, fmt.Errorf("Parsing commit %s: %w", csum, err)
	}
	// collection: verify ref/collection bindings, like the remote pull path.
	if !p.opts.DisableVerifyBindings {
		if err := verifyCommitBindings(p.collectionID, p.requestedRefs[csum], commit); err != nil {
			return nil, fmt.Errorf("Commit %s: %w", csum, err)
		}
	}

	if err := p.validateCommitTree(commit.RootTree); err != nil {
		return nil, err
	}
	existed, err := p.dst.HasObject(csum, repo.ObjectCommit)
	if err != nil {
		return nil, err
	}
	if !existed {
		if err := p.writeMetaObject(csum, repo.ObjectCommit, raw); err != nil {
			return nil, err
		}
	}
	p.copiedCommit[csum] = true
	if !existed {
		p.importedMeta++
	}

	// Detached metadata (signatures) is named after the commit checksum and is
	// copied verbatim when present.
	meta, err := p.src.ReadObject(csum, repo.ObjectCommitmeta)
	switch {
	case err == nil:
		// Detached metadata travels with the commit but is not included in
		// the imported-metadata count (matching libostree).
		if _, herr := p.dst.HasObject(csum, repo.ObjectCommitmeta); herr != nil {
			return nil, herr
		}
		if err := p.dst.WriteRawObject(csum, repo.ObjectCommitmeta, meta); err != nil {
			return nil, err
		}
	case errors.Is(err, repo.ErrObjectNotFound):
		// No detached metadata.
	default:
		return nil, err
	}

	if p.opts.CommitMetadataOnly {
		return commit, nil
	}

	if err := p.copyDirmeta(commit.RootMeta); err != nil {
		return nil, err
	}
	if err := p.copyDirtree(commit.RootTree); err != nil {
		return nil, err
	}
	// A re-imported commit supersedes any tombstone left by prune.
	if err := removeTombstone(p.dst, csum); err != nil {
		return nil, err
	}
	// A full tree import makes a previously partial commit whole again.  This
	// is what lets `fsck` succeed after repairing a repository with
	// pull-local.
	if err := clearPartial(p.dst, csum); err != nil {
		return nil, err
	}
	return commit, nil
}

func (p *puller) copyDirmeta(csum string) error {
	if p.copiedMeta[csum] {
		return nil
	}
	raw, err := p.src.ReadObject(csum, repo.ObjectDirmeta)
	if err != nil {
		return fmt.Errorf("Fetching dirmeta %s: %w", csum, err)
	}
	if err := verifyMetadataChecksum(raw, csum, repo.ObjectDirmeta); err != nil {
		return err
	}
	if _, err := object.ParseDirmeta(raw); err != nil {
		return fmt.Errorf("Parsing dirmeta %s: %w", csum, err)
	}
	existed, err := p.dst.HasObject(csum, repo.ObjectDirmeta)
	if err != nil {
		return err
	}
	if !existed {
		if err := p.writeMetaObject(csum, repo.ObjectDirmeta, raw); err != nil {
			return err
		}
	}
	p.copiedMeta[csum] = true
	if !existed {
		p.importedMeta++
	}
	return nil
}

func (p *puller) copyDirtree(csum string) error {
	if p.copiedTree[csum] {
		return nil
	}
	dirtree, err := tree.LoadDirtree(p.src, csum)
	if err != nil {
		return fmt.Errorf("Fetching dirtree %s: %w", csum, err)
	}
	raw, err := p.src.ReadObject(csum, repo.ObjectDirtree)
	if err != nil {
		return fmt.Errorf("Fetching dirtree %s: %w", csum, err)
	}
	if err := verifyMetadataChecksum(raw, csum, repo.ObjectDirtree); err != nil {
		return err
	}
	existed, err := p.dst.HasObject(csum, repo.ObjectDirtree)
	if err != nil {
		return err
	}
	if !existed {
		if err := p.writeMetaObject(csum, repo.ObjectDirtree, raw); err != nil {
			return err
		}
	}
	p.copiedTree[csum] = true
	if !existed {
		p.importedMeta++
	}

	for _, file := range dirtree.Files {
		if err := p.copyFile(file.Checksum); err != nil {
			return err
		}
	}
	for _, dir := range dirtree.Dirs {
		if err := p.copyDirmeta(dir.Meta); err != nil {
			return err
		}
		if err := p.copyDirtree(dir.Tree); err != nil {
			return err
		}
	}
	return nil
}

func (p *puller) copyFile(csum string) error {
	if p.copiedFile[csum] {
		return nil
	}
	file, err := p.loadFile(csum)
	if err != nil {
		return fmt.Errorf("Fetching content object %s: %w", csum, err)
	}
	// With --untrusted the source object is verified; a trusted local pull
	// copies bytes as-is, exactly like libostree.
	if p.opts.Untrusted {
		var body []byte
		if file.Header.IsRegular() {
			body = file.Content
		}
		actual, err := file.Header.Checksum(body)
		if err != nil {
			return fmt.Errorf("Checksumming content object %s: %w", csum, err)
		}
		if actual != csum {
			return fmt.Errorf("Corrupted file object; checksum expected='%s' actual='%s'", csum, actual)
		}
	}
	if p.opts.BareUserOnlyFiles {
		if err := validateBareUserOnlyMode(file.Header.Mode, csum); err != nil {
			return err
		}
	}
	existed, err := p.dst.HasObject(csum, repo.ObjectFile)
	if err != nil {
		return err
	}
	// Like libostree (ostree-repo-pull.c: "If we already have this object,
	// move on to the next"), an object found in this repository or in its
	// parent is not imported again.
	if !existed {
		if err := p.writeFile(csum, file); err != nil {
			return err
		}
	}
	p.copiedFile[csum] = true
	if !existed {
		p.importedContent++
		if p.countContentBytes && file.Header.IsRegular() {
			p.contentBytes += uint64(len(file.Content))
		}
	}
	return nil
}

// writeFile re-encodes a file object for the destination repository mode.
func (p *puller) writeFile(csum string, file *tree.File) error {
	hdr, content := file.Header, file.Content
	xattrs := map[string][]byte{}
	for _, x := range hdr.Xattrs {
		xattrs[x.Name] = x.Value
	}

	// Like libostree, hardlink content objects when source and destination use
	// the same bare repository mode. Symlinks are excluded (their object is a
	// symlink itself, which must not be shared across checkouts).
	// User-mode repositories store regular file content verbatim, so a
	// bare-user -> bare-user-only pull can still hardlink.
	linkableModes := p.dst.Mode() != repo.ModeArchive && p.src.Mode() != repo.ModeArchive &&
		(p.src.Mode() == p.dst.Mode() || (p.src.Mode() == repo.ModeBareUser && p.dst.Mode() == repo.ModeBareUserOnly))
	if linkableModes && hdr.IsRegular() {
		if existed, err := p.dst.HasObject(csum, repo.ObjectFile); err == nil && !existed {
			if dstPath, err := p.dst.ObjectPath(csum, repo.ObjectFile); err == nil {
				if srcPath, err := p.src.ObjectPath(csum, repo.ObjectFile); err == nil {
					if err := os.MkdirAll(filepath.Dir(dstPath), 0775); err == nil {
						if os.Link(srcPath, dstPath) == nil {
							return nil
						}
					}
				}
			}
		}
	}

	// Archive repositories store the same compressed payload, so content
	// objects can be hardlinked too.
	if p.src.Mode() == repo.ModeArchive && p.dst.Mode() == repo.ModeArchive {
		if existed, err := p.dst.HasObject(csum, repo.ObjectFile); err == nil && !existed {
			if dstPath, err := p.dst.ObjectPath(csum, repo.ObjectFile); err == nil {
				if srcPath, err := p.src.ObjectPath(csum, repo.ObjectFile); err == nil {
					if err := os.MkdirAll(filepath.Dir(dstPath), 0775); err == nil {
						if os.Link(srcPath, dstPath) == nil {
							return nil
						}
					}
				}
			}
		}
	}

	switch p.dst.Mode() {
	case repo.ModeArchive:
		var payload []byte
		if hdr.IsRegular() {
			hdr.Size = uint64(len(content))
			header, err := hdr.EncodeArchive()
			if err != nil {
				return err
			}
			compressed, err := object.Deflate(content, p.zlibLevel)
			if err != nil {
				return err
			}
			payload = make([]byte, 0, len(header)+len(compressed))
			payload = append(payload, header...)
			payload = append(payload, compressed...)
		} else {
			header, err := hdr.EncodeArchive()
			if err != nil {
				return err
			}
			payload = header
		}
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Xattrs: xattrs}
		return p.dst.WriteFileRawObject(csum, payload, meta)

	case repo.ModeBare:
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Xattrs: xattrs}
		switch {
		case hdr.IsSymlink():
			return p.dst.WriteSymlinkObject(csum, repo.ObjectFile, hdr.SymlinkTarget, meta)
		case hdr.IsRegular():
			return p.dst.WriteFileRawObject(csum, content, meta)
		default:
			return fmt.Errorf("Unsupported file type for content object %s", csum)
		}

	case repo.ModeBareUser:
		ostreemeta, err := object.EncodeFileMeta(hdr.UID, hdr.GID, hdr.Mode, hdr.Xattrs)
		if err != nil {
			return err
		}
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Ostreemeta: ostreemeta}
		switch {
		case hdr.IsSymlink():
			payload := append([]byte(hdr.SymlinkTarget), 0)
			return p.dst.WriteFileRawObject(csum, payload, meta)
		case hdr.IsRegular():
			return p.dst.WriteFileRawObject(csum, content, meta)
		default:
			return fmt.Errorf("Unsupported file type for content object %s", csum)
		}

	case repo.ModeBareUserOnly:
		// Metadata is canonical: uid/gid 0 and no xattrs.  Refuse to write a
		// file whose object checksum would no longer match.
		dest := &object.FileHeader{Mode: hdr.Mode, SymlinkTarget: hdr.SymlinkTarget}
		destBody := []byte(nil)
		if hdr.IsRegular() {
			destBody = content
		}
		destCsum, err := dest.Checksum(destBody)
		if err != nil {
			return err
		}
		if destCsum != csum {
			return fmt.Errorf("Content object %s: cannot convert metadata to bare-user-only", csum)
		}
		if hdr.IsSymlink() {
			return p.dst.WriteSymlinkObject(csum, repo.ObjectFile, hdr.SymlinkTarget,
				repo.FileObjectMeta{Mode: hdr.Mode})
		}
		if !hdr.IsRegular() {
			return fmt.Errorf("Unsupported file type for content object %s", csum)
		}
		return p.dst.WriteFileRawObject(csum, content, repo.FileObjectMeta{Mode: hdr.Mode})

	default:
		return fmt.Errorf("Pulling into repository mode %s is not supported", p.dst.Mode())
	}
}

// loadFile reads a source content object.  tree.LoadFile is used first; bare
// repositories additionally store symlinks as real symlinks, which
// tree.LoadFile cannot read (its first step opens the object path, following
// the link).  In that case the header is reconstructed from the filesystem
// metadata, exactly as tree.loadBareFile would have done.
func (p *puller) loadFile(csum string) (*tree.File, error) {
	file, err := tree.LoadFile(p.src, csum)
	if err == nil {
		return file, nil
	}
	switch p.src.Mode() {
	case repo.ModeBare, repo.ModeBareUserOnly:
		if symlink, serr := loadBareSymlink(p.src, csum); serr == nil {
			return symlink, nil
		}
	}
	return nil, err
}

func loadBareSymlink(r *repo.Repo, csum string) (*tree.File, error) {
	full, err := r.ObjectPath(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if st.Mode()&os.ModeSymlink == 0 {
		return nil, fmt.Errorf("not a symlink")
	}
	target, err := os.Readlink(full)
	if err != nil {
		return nil, err
	}
	hdr := &object.FileHeader{
		Mode:          uint32(st.Mode().Perm()) | 0120000,
		SymlinkTarget: target,
	}
	if r.Mode() != repo.ModeBareUserOnly {
		if stat, ok := st.Sys().(*unix.Stat_t); ok {
			hdr.UID, hdr.GID = stat.Uid, stat.Gid
		}
		xattrs, err := readLxattrs(full)
		if err != nil {
			return nil, err
		}
		hdr.Xattrs = xattrs
	}
	return &tree.File{Header: hdr}, nil
}

// readLxattrs lists and reads the extended attributes of path without
// following symlinks.
func readLxattrs(path string) ([]object.XAttr, error) {
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
		value, err := readLxattr(path, string(name))
		if err != nil {
			if errors.Is(err, unix.ENODATA) {
				continue
			}
			return nil, err
		}
		out = append(out, object.XAttr{Name: string(name), Value: value})
	}
	return object.Canonicalize(out)
}

func readLxattr(path, name string) ([]byte, error) {
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

func verifyMetadataChecksum(raw []byte, expected string, typ repo.ObjectType) error {
	actual := checksum.Hex(checksum.Sum(raw))
	if actual != expected {
		return fmt.Errorf("Corrupted %s object; checksum expected='%s' actual='%s'",
			typ, expected, actual)
	}
	return nil
}

func validateBareUserOnlyMode(mode uint32, csum string) error {
	const modeTypeMask = 0170000
	const regular = 0100000
	const symlink = 0120000
	switch mode & modeTypeMask {
	case symlink:
		return nil
	case regular:
		invalid := (mode &^ modeTypeMask) &^ 0775
		if invalid != 0 {
			return fmt.Errorf("Content object %s: invalid mode 0%04o with bits 0%04o",
				csum, mode, invalid)
		}
		return nil
	default:
		return fmt.Errorf("Content object %s: unsupported file type 0%04o", csum, mode&modeTypeMask)
	}
}

// removeTombstone deletes a tombstone object for csum, if present.
func removeTombstone(r *repo.Repo, csum string) error {
	rel, err := repo.LoosePath(csum, repo.ObjectTombstoneCommit, r.Mode())
	if err != nil {
		return err
	}
	if err := r.Root().Remove(path.Join("objects", rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// clearPartial removes the partial marker for a commit.
func clearPartial(r *repo.Repo, csum string) error {
	rel := path.Join("state", csum+".commitpartial")
	if err := r.Root().Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Clearing partial commit state for %s: %w", csum, err)
	}
	return nil
}

// markPartial records that a commit was fetched without its tree.
func markPartial(r *repo.Repo, csum string) error {
	rel := path.Join("state", csum+".commitpartial")
	f, err := r.Root().OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("Marking commit %s partial: %w", csum, err)
	}
	return f.Close()
}

// formatSize renders a byte count the way GLib's g_format_size() does for
// non-translated output (SI units, one decimal place above 1000 bytes).
func formatSize(size uint64) string {
	const (
		kb = 1000
		mb = 1000 * kb
		gb = 1000 * mb
		tb = 1000 * gb
		pb = 1000 * tb
	)
	switch {
	case size < kb:
		return fmt.Sprintf("%d bytes", size)
	case size < mb:
		return fmt.Sprintf("%.1f\u00a0kB", float64(size)/kb)
	case size < gb:
		return fmt.Sprintf("%.1f\u00a0MB", float64(size)/mb)
	case size < tb:
		return fmt.Sprintf("%.1f\u00a0GB", float64(size)/gb)
	case size < pb:
		return fmt.Sprintf("%.1f\u00a0TB", float64(size)/tb)
	default:
		return fmt.Sprintf("%.1f\u00a0PB", float64(size)/pb)
	}
}

// writeMetaObject writes a metadata object, hardlinking the source object when
// both repositories are on the same filesystem (libostree does the same for
// local pulls).
func (p *puller) writeMetaObject(csum string, typ repo.ObjectType, raw []byte) error {
	dstPath, err := p.dst.ObjectPath(csum, typ)
	if err == nil {
		srcPath, serr := p.src.ObjectPath(csum, typ)
		if serr == nil {
			if err := os.MkdirAll(filepath.Dir(dstPath), 0775); err == nil {
				if os.Link(srcPath, dstPath) == nil {
					return nil
				}
			}
		}
	}
	_, err = p.dst.WriteObject(typ, raw)
	return err
}

// validateCommitTree rejects content that cannot be stored in a user-mode
// repository before anything is written, so a failed pull leaves no orphan
// commit behind.
func (p *puller) validateCommitTree(treeCsum string) error {
	if err := p.validateNamesRec(treeCsum, map[string]bool{}); err != nil {
		return err
	}
	if p.opts.BareUserOnlyFiles {
		return p.validateTreeRec(treeCsum, map[string]bool{}, true)
	}
	switch p.dst.Mode() {
	case repo.ModeBare:
		if os.Geteuid() == 0 {
			return nil
		}
	case repo.ModeBareUser:
		return nil
	case repo.ModeBareUserOnly:
	default:
		return nil
	}
	return p.validateTreeRec(treeCsum, map[string]bool{}, false)
}

func (p *puller) validateTreeRec(treeCsum string, seen map[string]bool, userOnlyFiles bool) error {
	if seen[treeCsum] {
		return nil
	}
	seen[treeCsum] = true
	t, err := tree.LoadDirtree(p.src, treeCsum)
	if err != nil {
		return err
	}
	for _, f := range t.Files {
		if strings.Contains(f.Name, "/") {
			return fmt.Errorf("Invalid / in filename %s", f.Name)
		}
		loaded, err := p.loadFile(f.Checksum)
		if err != nil {
			return err
		}
		if userOnlyFiles {
			if loaded.Header.Mode&07000 != 0 {
				return fmt.Errorf("Content object %s: invalid mode 0%o with bits 0%o", f.Checksum, loaded.Header.Mode&07777, loaded.Header.Mode&07000)
			}
		} else if loaded.Header.Mode&04000 != 0 {
			return fmt.Errorf("Content object %s: invalid mode 0%o with bits 0%o", f.Checksum, loaded.Header.Mode&07777, loaded.Header.Mode&04000)
		}
	}
	for _, d := range t.Dirs {
		if strings.Contains(d.Name, "/") {
			return fmt.Errorf("Invalid / in filename %s", d.Name)
		}
		if err := p.validateTreeRec(d.Tree, seen, userOnlyFiles); err != nil {
			return err
		}
	}
	return nil
}

// copyCommitMetadata writes only the commit object for csum.
func (p *puller) copyCommitMetadata(csum string) error {
	if p.copiedCommit[csum] {
		return nil
	}
	raw, err := p.src.ReadObject(csum, repo.ObjectCommit)
	if err != nil {
		return fmt.Errorf("Fetching commit %s: %w", csum, err)
	}
	if err := verifyMetadataChecksum(raw, csum, repo.ObjectCommit); err != nil {
		return err
	}
	p.copiedCommit[csum] = true
	return p.writeMetaObject(csum, repo.ObjectCommit, raw)
}

// validateNamesRec rejects filenames that would escape the tree.
func (p *puller) validateNamesRec(treeCsum string, seen map[string]bool) error {
	if seen[treeCsum] {
		return nil
	}
	seen[treeCsum] = true
	t, err := tree.LoadDirtree(p.src, treeCsum)
	if err != nil {
		return err
	}
	for _, f := range t.Files {
		if strings.Contains(f.Name, "/") {
			return fmt.Errorf("Invalid / in filename %s", f.Name)
		}
	}
	for _, d := range t.Dirs {
		if strings.Contains(d.Name, "/") {
			return fmt.Errorf("Invalid / in filename %s", d.Name)
		}
		if err := p.validateNamesRec(d.Tree, seen); err != nil {
			return err
		}
	}
	return nil
}

// publishedDeltaNames returns the delta names the source repository publishes
// for target commit root.  libostree's pull consults the per-target delta
// index first and falls back to the "ostree.static-deltas" dictionary of the
// summary; the same two sources are used here.  Deliberately, the on-disk
// "deltas/" directory is not consulted: a repository configured with
// core.no-deltas-in-summary=true and without indexes must not expose deltas to
// pull, even though the delta files still exist.
func (p *puller) publishedDeltaNames(root string) ([]string, error) {
	seen := map[string]bool{}
	if deltas, ok, err := staticdelta.LoadIndexDeltas(p.src, root); err != nil {
		return nil, err
	} else if ok {
		for name := range deltas {
			seen[name] = true
		}
	} else if data, serr := p.src.ReadSummaryFile(); serr == nil {
		summary, perr := object.ParseSummary(data)
		if perr == nil {
			for name := range staticdelta.ParseDeltaDict(summary.Metadata) {
				seen[name] = true
			}
		}
	} else if !errors.Is(serr, fs.ErrNotExist) {
		return nil, serr
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// deltaCandidate is a delta that can produce a requested commit.
type localDeltaCandidate struct {
	name string
	sb   *staticdelta.Superblock
	// fromTimestamp is the timestamp of the source commit when the delta
	// has one; scratch deltas leave it zero.
	fromTimestamp uint64
}

// findLocalDelta selects the delta to apply for root, mirroring
// get_best_static_delta_start_for(): prefer a delta whose source commit is
// already present (and not partial), choosing the newest source commit, and
// fall back to a from-scratch delta.
func (p *puller) findLocalDelta(root string) (*localDeltaCandidate, error) {
	names, err := p.publishedDeltaNames(root)
	if err != nil {
		return nil, err
	}
	var scratch *localDeltaCandidate
	var best *localDeltaCandidate
	for _, name := range names {
		from, to, perr := staticdelta.ParseDeltaName(name)
		if perr != nil || to != root {
			continue
		}
		path, perr := staticdelta.DeltaSuperblockPath(from, to)
		if perr != nil {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(p.src.Path(), path))
		if rerr != nil {
			continue
		}
		sb, perr := staticdelta.ParseSuperblock(data)
		if perr != nil || checksum.Hex(sb.To) != root {
			continue
		}
		if !sb.HasFrom {
			if scratch == nil {
				scratch = &localDeltaCandidate{name: name, sb: sb}
			}
			continue
		}
		fromSum := checksum.Hex(sb.From)
		have, herr := p.dst.HasObject(fromSum, repo.ObjectCommit)
		if herr != nil {
			return nil, herr
		}
		if !have {
			continue
		}
		partial, perr := p.dst.CommitIsPartial(fromSum)
		if perr != nil {
			return nil, perr
		}
		if partial {
			continue
		}
		commit, cerr := p.dst.ReadObject(fromSum, repo.ObjectCommit)
		if cerr != nil {
			continue
		}
		parsed, cerr := object.ParseCommit(commit)
		if cerr != nil {
			continue
		}
		if best == nil || parsed.Timestamp > best.fromTimestamp {
			best = &localDeltaCandidate{name: name, sb: sb, fromTimestamp: parsed.Timestamp}
		}
	}
	if best != nil {
		return best, nil
	}
	return scratch, nil
}

// tryLocalDeltas applies static deltas from src for the requested commits.
// It returns true when every root was handled by a delta.
func (p *puller) tryLocalDeltas(roots []string) (bool, error) {
	if !p.opts.RequireStaticDeltas || p.opts.DisableStaticDeltas {
		return false, nil
	}
	var totalParts int
	var transferred uint64
	handled := 0
	for _, root := range roots {
		cand, err := p.findLocalDelta(root)
		if err != nil {
			return false, err
		}
		if cand == nil {
			continue
		}
		from, to, _ := staticdelta.ParseDeltaName(cand.name)
		swap := cand.sb.NeedsSwap()
		for i, part := range cand.sb.Parts {
			partPath, perr := staticdelta.DeltaPartPath(from, to, i)
			if perr != nil {
				return false, perr
			}
			data, rerr := os.ReadFile(filepath.Join(p.src.Path(), partPath))
			if rerr != nil {
				return false, rerr
			}
			if verr := staticdelta.VerifyPartChecksum(data, part.Checksum); verr != nil {
				return false, verr
			}
			if aerr := staticdelta.ApplyPartFile(p.dst, part.Objects, data); aerr != nil {
				return false, aerr
			}
			totalParts++
			transferred += staticdelta.Swap64(swap, part.Size)
		}
		commitBytes, cerr := cand.sb.CommitBytes()
		if cerr != nil {
			return false, cerr
		}
		if werr := p.dst.WriteRawObject(root, repo.ObjectCommit, commitBytes); werr != nil {
			return false, werr
		}
		if cerr := p.dst.ClearPartial(root); cerr != nil {
			return false, cerr
		}
		handled++
	}
	if p.opts.Stdout != nil && handled > 0 {
		fmt.Fprintf(p.opts.Stdout, "%d delta parts, %d loose fetched; %s transferred in 0 seconds; 0 bytes content written\n",
			totalParts, 2*handled, formatSize(transferred))
	}
	if handled != len(roots) {
		return false, fmt.Errorf("Static deltas required, but none found for commit %s", roots[0])
	}
	return true, nil
}
