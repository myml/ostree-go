package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// RefKind identifies the namespace a ref lives in.
type RefKind int

const (
	// RefLocal is refs/heads/<name>.
	RefLocal RefKind = iota
	// RefRemote is refs/remotes/<remote>/<name>.
	RefRemote
	// RefMirror is refs/mirrors/<collection>/<name>.
	RefMirror
)

// String returns the namespace name.
func (k RefKind) String() string {
	switch k {
	case RefLocal:
		return "local"
	case RefRemote:
		return "remote"
	case RefMirror:
		return "mirror"
	default:
		return fmt.Sprintf("RefKind(%d)", int(k))
	}
}

// Ref is a resolved ref.
type Ref struct {
	Kind       RefKind
	Name       string // ref name inside the namespace (may contain '/')
	Remote     string // set for RefRemote
	Collection string // set for RefMirror
	Checksum   string
}

// RefSpec renders the ref the way libostree lists refspecs: plain names for
// local refs, "remote:name" for remote refs and "collection:name" for mirror
// refs.
func (r Ref) RefSpec() string {
	switch r.Kind {
	case RefRemote:
		return r.Remote + ":" + r.Name
	case RefMirror:
		return r.Collection + ":" + r.Name
	default:
		return r.Name
	}
}

// RelPath returns the ref path relative to the repository root.
func (r Ref) RelPath() string {
	switch r.Kind {
	case RefRemote:
		return "refs/remotes/" + r.Remote + "/" + r.Name
	case RefMirror:
		return "refs/mirrors/" + r.Collection + "/" + r.Name
	default:
		return "refs/heads/" + r.Name
	}
}

// ReadRef reads a local ref (refs/heads/<name>) and validates its checksum.
// Surrounding whitespace is stripped before validation.
func (r *Repo) ReadRef(name string) (string, error) {
	if err := ValidateRefName(name); err != nil {
		return "", err
	}
	return r.readRefFile("refs/heads/" + name)
}

// ReadRemoteRef reads refs/remotes/<remote>/<name>.
func (r *Repo) ReadRemoteRef(remote, name string) (string, error) {
	if err := ValidateRemoteName(remote); err != nil {
		return "", err
	}
	if err := ValidateRefName(name); err != nil {
		return "", err
	}
	return r.readRefFile("refs/remotes/" + remote + "/" + name)
}

// ReadMirrorRef reads refs/mirrors/<collection>/<name>.
func (r *Repo) ReadMirrorRef(collection, name string) (string, error) {
	if err := ValidateCollectionID(collection); err != nil {
		return "", err
	}
	if err := ValidateRefName(name); err != nil {
		return "", err
	}
	return r.readRefFile("refs/mirrors/" + collection + "/" + name)
}

// WriteRef writes a local ref (refs/heads/<name>) as "<checksum>\n" using a
// temporary file and rename. A file/directory conflict is reported as
// ErrRefConflict.
func (r *Repo) WriteRef(name, checksum string) error {
	if err := validateWritableRef(name, checksum); err != nil {
		return err
	}
	return r.writeRefFile("refs/heads/"+name, checksum)
}

// WriteRemoteRef writes refs/remotes/<remote>/<name>.
func (r *Repo) WriteRemoteRef(remote, name, checksum string) error {
	if err := ValidateRemoteName(remote); err != nil {
		return err
	}
	if err := validateWritableRef(name, checksum); err != nil {
		return err
	}
	return r.writeRefFile("refs/remotes/"+remote+"/"+name, checksum)
}

// WriteMirrorRef writes refs/mirrors/<collection>/<name>.
func (r *Repo) WriteMirrorRef(collection, name, checksum string) error {
	if err := ValidateCollectionID(collection); err != nil {
		return err
	}
	if err := validateWritableRef(name, checksum); err != nil {
		return err
	}
	return r.writeRefFile("refs/mirrors/"+collection+"/"+name, checksum)
}

func validateWritableRef(name, checksum string) error {
	if err := ValidateRefName(name); err != nil {
		return err
	}
	// libostree refuses to create a ref that looks like a checksum, to avoid
	// ambiguity with revision arguments.
	if IsChecksum(name) {
		return fmt.Errorf("%w: rev name %q looks like a checksum", ErrInvalidRef, name)
	}
	if err := ValidateChecksum(checksum); err != nil {
		return err
	}
	return nil
}

func (r *Repo) readRefFile(rel string) (string, error) {
	if r.root == nil {
		return "", errors.New("repository is closed")
	}
	// Report directories separately (EISDIR in C, G_IO_ERROR_IS_DIRECTORY)
	// instead of letting io.ReadAll produce a generic error.
	if st, err := r.root.Lstat(rel); err == nil {
		if st.IsDir() {
			return "", fmt.Errorf("ref %s: %w", rel, ErrRefIsDirectory)
		}
	} else if !isMissingPath(err) {
		return "", fmt.Errorf("read ref %s: %w", rel, err)
	}
	data, err := r.readRootFile(rel)
	if err != nil {
		return "", fmt.Errorf("read ref %s: %w", rel, err)
	}
	checksum := strings.TrimSpace(string(data))
	if err := ValidateChecksum(checksum); err != nil {
		return "", fmt.Errorf("ref %s: %w", rel, err)
	}
	return checksum, nil
}

func (r *Repo) writeRefFile(rel, checksum string) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	// C's write_checksum_file_at first creates the file. If a directory is in
	// the way it lists refs below that prefix; if any exist this is a
	// conflict, otherwise the (empty) directory tree is removed.
	if st, err := r.root.Lstat(rel); err == nil {
		if st.IsDir() {
			found, err := r.firstRefUnder(rel)
			if err != nil {
				return err
			}
			if found != "" {
				return fmt.Errorf("%w: Conflict: %s exists under %s when attempting write", ErrRefConflict, found, rel)
			}
			if err := r.removeAllRootPath(rel); err != nil {
				return fmt.Errorf("remove ref directory %s: %w", rel, err)
			}
		}
	} else if !isMissingPath(err) {
		return err
	}
	if err := r.checkRefAncestors(rel); err != nil {
		return err
	}
	if err := r.atomicWrite(rel, []byte(checksum+"\n"), fileMode); err != nil {
		return fmt.Errorf("write ref %s: %w", rel, err)
	}
	return nil
}

// checkRefAncestors rejects writing a ref below a path component that is a
// regular file (the file-vs-directory conflict in the other direction).
func (r *Repo) checkRefAncestors(rel string) error {
	parts := strings.Split(rel, "/")
	cur := ""
	for i := 0; i < len(parts)-1; i++ {
		if i > 0 {
			cur += "/"
		}
		cur += parts[i]
		st, err := r.root.Lstat(cur)
		if err == nil {
			if !st.IsDir() {
				return fmt.Errorf("%w: %s exists and is not a directory", ErrRefConflict, cur)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Nonexistent ancestors will be created by atomicWrite.
		return nil
	}
	return nil
}

// firstRefUnder returns the first valid ref name found below rel, or "" if the
// directory contains no refs.
func (r *Repo) firstRefUnder(rel string) (string, error) {
	var found string
	err := r.walkRefTree(rel, "", func(name, _ string) error {
		if found == "" {
			found = name
		}
		return nil
	})
	return found, err
}

// ListRefs lists refs from refs/heads, refs/remotes and refs/mirrors matching
// prefix. The results are sorted by kind and refspec.
//
// Prefix matching follows libostree's path-based refspec semantics rather than
// plain string prefixing:
//
//   - an empty prefix lists everything (local, remote and mirror refs);
//   - a prefix without ':' selects local refs whose path is prefix or lies
//     below prefix/ (so "te" does not match "test", but "test" matches the
//     "test" ref and "testdir" matches "testdir/child");
//   - a prefix of the form "remote:" or "remote:ref" selects refs in that
//     remote namespace, and likewise for a mirror collection ID.
//
// The returned Ref.Name always contains the full name inside its namespace
// (the equivalent of `ostree refs --list`); trimming the prefix for plain
// `ostree refs` output is left to the CLI layer.
func (r *Repo) ListRefs(prefix string) ([]Ref, error) {
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	var refs []Ref

	if err := r.walkRefTree("refs/heads", "", func(name, checksum string) error {
		refs = append(refs, Ref{Kind: RefLocal, Name: name, Checksum: checksum})
		return nil
	}); err != nil {
		return nil, err
	}

	if _, err := r.root.Lstat("refs/remotes"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("Listing refs: opendir refs/remotes: No such file or directory")
		}
		return nil, err
	}
	remotes, err := r.subdirs("refs/remotes")
	if err != nil {
		return nil, err
	}
	for _, remote := range remotes {
		if ValidateRemoteName(remote) != nil {
			continue
		}
		base := path.Join("refs/remotes", remote)
		if err := r.walkRefTree(base, "", func(name, checksum string) error {
			refs = append(refs, Ref{Kind: RefRemote, Remote: remote, Name: name, Checksum: checksum})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	mirrors, err := r.subdirs("refs/mirrors")
	if err != nil {
		return nil, err
	}
	for _, collection := range mirrors {
		if ValidateCollectionID(collection) != nil {
			continue
		}
		base := path.Join("refs/mirrors", collection)
		if err := r.walkRefTree(base, "", func(name, checksum string) error {
			refs = append(refs, Ref{Kind: RefMirror, Collection: collection, Name: name, Checksum: checksum})
			return nil
		}); err != nil {
			return nil, err
		}
	}

	refs = filterRefsByPrefix(refs, prefix)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].RefSpec() < refs[j].RefSpec()
	})
	return refs, nil
}

func pathPrefixMatch(name, prefix string) bool {
	return prefix == "" || name == prefix || strings.HasPrefix(name, prefix+"/")
}

// filterRefsByPrefix implements libostree's refspec prefix rules.
func filterRefsByPrefix(refs []Ref, prefix string) []Ref {
	if prefix == "" {
		return refs
	}
	if idx := strings.IndexByte(prefix, ':'); idx >= 0 {
		namespace, refPrefix := prefix[:idx], prefix[idx+1:]
		if refPrefix == "." {
			refPrefix = ""
		}
		out := refs[:0]
		for _, ref := range refs {
			switch ref.Kind {
			case RefRemote:
				if ref.Remote == namespace && pathPrefixMatch(ref.Name, refPrefix) {
					out = append(out, ref)
				}
			case RefMirror:
				if ref.Collection == namespace && pathPrefixMatch(ref.Name, refPrefix) {
					out = append(out, ref)
				}
			}
		}
		return out
	}
	out := refs[:0]
	for _, ref := range refs {
		if ref.Kind == RefLocal && pathPrefixMatch(ref.Name, prefix) {
			out = append(out, ref)
		}
	}
	return out
}

// ListLocalRefs is a convenience wrapper returning only refs/heads refs.
func (r *Repo) ListLocalRefs(prefix string) ([]Ref, error) {
	refs, err := r.ListRefs(prefix)
	if err != nil {
		return nil, err
	}
	out := refs[:0]
	for _, ref := range refs {
		if ref.Kind == RefLocal {
			out = append(out, ref)
		}
	}
	return out, nil
}

// walkRefTree walks ref files below base/rel. It mirrors libostree's
// enumerate_refs_recurse(): entries whose names are not valid ref fragments are
// ignored entirely, directories are recursed into, and symlinks are treated as
// ref files and read through.
func (r *Repo) walkRefTree(base, rel string, fn func(name, checksum string) error) error {
	dir := base
	if rel != "" {
		dir = path.Join(base, rel)
	}
	f, err := r.root.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open refs dir %s: %w", dir, err)
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("read refs dir %s: %w", dir, err)
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		name := entry.Name()
		if !validRefFragment(name) {
			continue
		}
		childRel := name
		if rel != "" {
			childRel = rel + "/" + name
		}
		if entry.IsDir() {
			if err := r.walkRefTree(base, childRel, fn); err != nil {
				return err
			}
			continue
		}
		// Match enumerate_refs_recurse(): only regular files and symlinks are
		// refs. Skipping other types also avoids blocking on a stray FIFO.
		if mode := entry.Type(); !mode.IsRegular() && mode&fs.ModeSymlink == 0 {
			continue
		}
		checksum, err := r.readRefFile(path.Join(base, childRel))
		if err != nil {
			return err
		}
		if err := fn(childRel, checksum); err != nil {
			return err
		}
	}
	return nil
}

// subdirs returns the directory names directly below rel, sorted, or nil if
// rel does not exist.
func (r *Repo) subdirs(rel string) ([]string, error) {
	f, err := r.root.Open(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("open %s: %w", rel, err)
	}
	entries, err := f.ReadDir(-1)
	closeErr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// DeleteRef removes a local ref and prunes any now-empty parent directories.
func (r *Repo) DeleteRef(name string) error {
	if err := ValidateRefName(name); err != nil {
		return err
	}
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	rel := "refs/heads/" + name
	if _, err := r.root.Lstat(rel); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("ref %s: %w", name, fs.ErrNotExist)
		}
		return err
	}
	if err := r.root.Remove(rel); err != nil {
		return err
	}
	// Remove empty directories from the leaf upwards, stopping at refs/heads.
	dir := path.Dir(rel)
	for dir != "refs/heads" && dir != "refs" && dir != "." {
		empty, err := r.dirIsEmpty(dir)
		if err != nil || !empty {
			break
		}
		if err := r.root.Remove(dir); err != nil {
			break
		}
		dir = path.Dir(dir)
	}
	return nil
}

// ParseRefSpec splits a refspec into an optional remote name and a ref name,
// following ostree_parse_refspec(): "origin:branch" names the remote ref
// refs/remotes/origin/branch while "branch" names the local ref
// refs/heads/branch. Both parts are syntactically validated.
func ParseRefSpec(refspec string) (remote, name string, err error) {
	if idx := strings.IndexByte(refspec, ':'); idx >= 0 {
		remote, name = refspec[:idx], refspec[idx+1:]
		if ValidateRemoteName(remote) != nil || ValidateRefName(name) != nil {
			return "", "", fmt.Errorf("%w: %q", ErrInvalidRef, refspec)
		}
		return remote, name, nil
	}
	if err := ValidateRefName(refspec); err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrInvalidRef, refspec)
	}
	return "", refspec, nil
}

// readRefSpec reads the checksum of a parsed refspec.
func (r *Repo) readRefSpec(remote, name string) (string, error) {
	if remote != "" {
		return r.ReadRemoteRef(remote, name)
	}
	return r.ReadRef(name)
}

// ResolveRef resolves refspec to a checksum, following
// ostree_repo_resolve_rev() / ostree_repo_resolve_rev_ext().
//
// A refspec whose ref part looks like a checksum resolves to itself. When
// fallbackRemote is true a missing local ref is also looked up in
// refs/remotes: first as "refs/remotes/<name>", then below every remote
// ("origin:<name>"), which mirrors resolve_refspec()'s fallback behavior.
//
// A missing ref either yields ("", false, nil) when allowNoent is set or a
// "Refspec ... not found" error. If the ref path exists but is a directory
// ErrRefIsDirectory is returned so callers can reproduce libostree's
// G_IO_ERROR_IS_DIRECTORY handling.
func (r *Repo) ResolveRef(refspec string, allowNoent, fallbackRemote bool) (string, bool, error) {
	remote, name, err := ParseRefSpec(refspec)
	if err != nil {
		return "", false, err
	}
	if IsChecksum(name) {
		return name, true, nil
	}
	sum, err := r.readRefSpec(remote, name)
	if err == nil {
		return sum, true, nil
	}
	if !isMissingPath(err) {
		return "", false, err
	}
	if remote == "" && fallbackRemote {
		sum, ok, ferr := r.resolveRefRemoteFallback(name)
		if ferr != nil {
			return "", false, ferr
		}
		if ok {
			return sum, true, nil
		}
	}
	if allowNoent {
		return "", false, nil
	}
	return "", false, fmt.Errorf("Refspec %q not found", refspec)
}

// resolveRefRemoteFallback implements find_ref_in_remotes() plus the direct
// "refs/remotes/<name>" lookup of resolve_refspec().
func (r *Repo) resolveRefRemoteFallback(name string) (string, bool, error) {
	if err := ValidateRefName(name); err != nil {
		return "", false, nil
	}
	candidates := []string{"refs/remotes/" + name}
	remotes, err := r.subdirs("refs/remotes")
	if err != nil {
		return "", false, err
	}
	for _, remote := range remotes {
		if ValidateRemoteName(remote) != nil {
			continue
		}
		candidates = append(candidates, "refs/remotes/"+remote+"/"+name)
	}
	for _, rel := range candidates {
		sum, err := r.readRefFile(rel)
		if err == nil {
			return sum, true, nil
		}
		if !isMissingPath(err) {
			return "", false, err
		}
	}
	return "", false, nil
}

// WriteRefSpec writes checksum as the target of refspec, which may name a
// remote ref ("origin:branch").
func (r *Repo) WriteRefSpec(refspec, checksum string) error {
	remote, name, err := ParseRefSpec(refspec)
	if err != nil {
		return err
	}
	if remote != "" {
		return r.WriteRemoteRef(remote, name, checksum)
	}
	return r.WriteRef(name, checksum)
}

// DeleteRefSpec removes the ref named by refspec, which may be a remote ref.
// Missing refs are ignored, matching _ostree_repo_write_ref() with a NULL
// revision.
// DeleteRefObject removes the ref described by ref, which may be a local,
// remote or collection mirror ref.
func (r *Repo) DeleteRefObject(ref Ref) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if err := r.root.Remove(ref.RelPath()); err != nil {
		if isMissingPath(err) {
			return nil
		}
		return err
	}
	return nil
}

func (r *Repo) DeleteRefSpec(refspec string) error {
	remote, name, err := ParseRefSpec(refspec)
	if err != nil {
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
	rel := "refs/heads/" + name
	if remote != "" {
		rel = "refs/remotes/" + remote + "/" + name
	}
	if err := r.root.Remove(rel); err != nil {
		if isMissingPath(err) {
			return nil
		}
		return err
	}
	return nil
}

// RefAlias is an alias ref: a symbolic link inside refs/heads that points at
// another local ref (created by `ostree refs -A`).
type RefAlias struct {
	// Name is the alias ref path below refs/heads.
	Name string
	// Target is the *ref* path the link points at, with the leading
	// "../" components stripped, as libostree lists it.
	Target string
}

// aliasLinkTarget reproduces relative_symlink_to(): one "../" per path
// component of the alias name, then the target ref.
func aliasLinkTarget(name, target string) string {
	return strings.Repeat("../", strings.Count(name, "/")) + target
}

// aliasTarget strips leading "../" components from a symlink target.
func aliasTarget(link string) string {
	for strings.HasPrefix(link, "../") {
		link = link[3:]
	}
	return link
}

// WriteAliasRef atomically creates refs/heads/<name> as a relative symlink to
// the local ref target. The symlink is staged under a temporary (invalid ref)
// name in the same directory and renamed into place.
func (r *Repo) WriteAliasRef(name, target string) error {
	if !r.writable {
		if r.writableErr != nil {
			return r.writableErr
		}
		return ErrReadOnly
	}
	if r.root == nil {
		return errors.New("repository is closed")
	}
	if err := ValidateRefName(name); err != nil {
		return err
	}
	if err := ValidateRefName(target); err != nil {
		return err
	}
	rel := "refs/heads/" + name
	dir := path.Dir(rel)
	if err := r.mkdirAll(dir, dirMode); err != nil {
		return err
	}
	tmp := path.Join(dir, ".tmp-alias-"+randomHex(8))
	if err := r.root.Symlink(aliasLinkTarget(name, target), tmp); err != nil {
		return fmt.Errorf("create alias %s: %w", rel, err)
	}
	if err := r.root.Rename(tmp, rel); err != nil {
		r.root.Remove(tmp)
		return fmt.Errorf("create alias %s: %w", rel, err)
	}
	return nil
}

// ListRefAliases returns the alias refs (symlinks) in refs/heads, sorted by
// name. Remote-namespace symlinks are intentionally not returned: libostree
// cannot create them and lists them without a remote prefix, which would make
// the names ambiguous.
func (r *Repo) ListRefAliases() ([]RefAlias, error) {
	if r.root == nil {
		return nil, errors.New("repository is closed")
	}
	var out []RefAlias
	var walk func(rel string) error
	walk = func(rel string) error {
		dir := "refs/heads"
		if rel != "" {
			dir = path.Join("refs/heads", rel)
		}
		f, err := r.root.Open(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("open refs dir %s: %w", dir, err)
		}
		entries, err := f.ReadDir(-1)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("read refs dir %s: %w", dir, err)
		}
		if closeErr != nil {
			return closeErr
		}
		for _, entry := range entries {
			name := entry.Name()
			if !validRefFragment(name) {
				continue
			}
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			if entry.IsDir() {
				if err := walk(childRel); err != nil {
					return err
				}
				continue
			}
			if entry.Type()&fs.ModeSymlink == 0 {
				continue
			}
			link, err := r.root.Readlink(path.Join("refs/heads", childRel))
			if err != nil {
				return fmt.Errorf("read alias %s: %w", childRel, err)
			}
			out = append(out, RefAlias{Name: childRel, Target: aliasTarget(link)})
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
