// Package prune implements `ostree prune`: computing the set of objects
// reachable from refs (or from every commit) and deleting the rest.
package prune

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"ostree-go/internal/dateparse"
	"ostree-go/internal/gvariant"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
	"ostree-go/internal/staticdelta"
	"ostree-go/internal/tree"
)

// Options mirrors the option table of ot-builtin-prune.c.
type Options struct {
	// NoPrune only reports unreachable objects; nothing is deleted.
	NoPrune bool
	// RefsOnly computes reachability from refs instead of from every commit.
	RefsOnly bool
	// Depth limits parent traversal (-1 for unlimited); 0 keeps only each
	// tip commit plus its tree.
	Depth int
	// DeleteCommit deletes a specific commit before the normal prune pass.
	DeleteCommit string
	// KeepYoungerThan keeps the history of each ref down to commits at or
	// after the given date.
	KeepYoungerThan string
	// KeepUnreferencedYoungerThan keeps unreferenced commits at or after the
	// given date (and their trees).
	KeepUnreferencedYoungerThan string
	// StaticDeltasOnly restricts --delete-commit to static deltas (not
	// implemented by the Go build).
	StaticDeltasOnly bool
	// RetainBranchDepth holds BRANCH=DEPTH overrides.
	RetainBranchDepth []string
	// OnlyBranch limits pruning to the named branches.
	OnlyBranch []string
	// CommitOnly traverses and deletes commit objects only.
	CommitOnly bool

	// Stdout receives the summary lines.
	Stdout io.Writer
}

// objectKey identifies one object by checksum and type.
type objectKey struct {
	checksum string
	typ      repo.ObjectType
}

// objectEntry is one loose object found in objects/.
type objectEntry struct {
	key  objectKey
	rel  string // path relative to the repository root
	size uint64
}

// Run performs the prune operation and prints the C-compatible summary.
func Run(r *repo.Repo, opts Options) error {
	if opts.DeleteCommit != "" && opts.NoPrune {
		return errors.New("Cannot specify both --delete-commit and --no-prune")
	}
	if opts.StaticDeltasOnly && opts.DeleteCommit == "" {
		return errors.New("--static-deltas-only requires --delete-commit; see https://github.com/ostreedev/ostree/issues/1479")
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if !opts.NoPrune && !r.Writable() {
		if err := r.WritableError(); err != nil {
			return fmt.Errorf("Cannot write to repository: %v", err)
		}
		return errors.New("Cannot write to repository: repository is not writable")
	}

	unlock, err := r.Lock(true)
	if err != nil {
		return err
	}
	defer unlock()

	if !opts.NoPrune {
		if err := pruneSummaryCache(r); err != nil {
			return err
		}
	}

	if opts.DeleteCommit != "" {
		if opts.StaticDeltasOnly {
			if err := pruneStaticDeltas(r, opts.DeleteCommit); err != nil {
				return err
			}
		} else if err := deleteCommit(r, opts.DeleteCommit); err != nil {
			return err
		}
	}

	objects, err := listObjects(r, opts.CommitOnly)
	if err != nil {
		return err
	}
	reach, err := computeReachable(r, opts, objects)
	if err != nil {
		return err
	}

	result, err := pruneObjects(r, objects, reach, opts)
	if err != nil {
		return err
	}
	writeSummary(opts, result)
	return nil
}

// pruneResult carries the numbers printed at the end.
type pruneResult struct {
	total  int
	pruned int
	freed  uint64
}

func writeSummary(opts Options, res pruneResult) {
	if opts.CommitOnly {
		fmt.Fprintf(opts.Stdout, "Total (commit only) objects: %d\n", res.total)
	} else {
		fmt.Fprintf(opts.Stdout, "Total objects: %d\n", res.total)
	}
	switch {
	case res.pruned == 0:
		fmt.Fprintln(opts.Stdout, "No unreachable objects")
	case opts.NoPrune:
		fmt.Fprintf(opts.Stdout, "Would delete: %d objects, freeing %s\n", res.pruned, formatSize(res.freed))
	default:
		fmt.Fprintf(opts.Stdout, "Deleted %d objects, %s freed\n", res.pruned, formatSize(res.freed))
	}
}

// computeReachable builds the reachable object set.  The manual path mirrors
// the complicated branch of ostree_builtin_prune(), where options such as
// --retain-branch-depth force --refs-only semantics.
func computeReachable(r *repo.Repo, opts Options, objects []objectEntry) (map[objectKey]bool, error) {
	manual := opts.RetainBranchDepth != nil || opts.KeepYoungerThan != "" ||
		opts.KeepUnreferencedYoungerThan != "" || len(opts.OnlyBranch) > 0
	reach := newReachability(r, opts.CommitOnly)

	if !manual {
		if opts.RefsOnly {
			return reach.set, reach.refs(opts.Depth)
		}
		for _, obj := range objects {
			if obj.key.typ != repo.ObjectCommit {
				continue
			}
			if err := reach.commit(obj.key.checksum, opts.Depth); err != nil {
				return nil, err
			}
		}
		return reach.set, nil
	}

	retain, err := parseRetainBranchDepth(opts.RetainBranchDepth)
	if err != nil {
		return nil, err
	}
	var keepYounger, keepUnreferenced int64
	if opts.KeepYoungerThan != "" {
		keepYounger, err = dateparse.Parse(opts.KeepYoungerThan)
		if err != nil {
			return nil, fmt.Errorf("Could not parse '%s'", opts.KeepYoungerThan)
		}
	}
	if opts.KeepUnreferencedYoungerThan != "" {
		keepUnreferenced, err = dateparse.Parse(opts.KeepUnreferencedYoungerThan)
		if err != nil {
			return nil, fmt.Errorf("Could not parse '%s'", opts.KeepUnreferencedYoungerThan)
		}
	}

	allRefs, err := r.ListRefs("")
	if err != nil {
		return nil, err
	}
	var refs []repo.Ref
	for _, ref := range allRefs {
		// Like ostree_repo_list_refs(), collection refs are handled
		// separately below.
		if ref.Kind == repo.RefMirror {
			continue
		}
		refs = append(refs, ref)
	}

	if len(opts.OnlyBranch) > 0 {
		only := map[string]bool{}
		for _, name := range opts.OnlyBranch {
			if _, err := resolveRef(r, name); err != nil {
				return nil, err
			}
			only[name] = true
		}
		for _, ref := range refs {
			spec := ref.RefSpec()
			if !only[spec] {
				if _, ok := retain[spec]; !ok {
					retain[spec] = -1
				}
			}
		}
	}

	for _, ref := range refs {
		spec := ref.RefSpec()
		depth, haveDepth := retain[spec]
		// libostree stores the depth as a pointer; a depth of 0 is
		// indistinguishable from "no override" (GINT_TO_POINTER(0) == NULL),
		// so it falls through to the global --depth setting.  Preserve that
		// behavior for compatibility.
		if haveDepth && depth != 0 {
			if err := reach.commit(ref.Checksum, depth); err != nil {
				return nil, err
			}
			continue
		}
		if opts.KeepYoungerThan != "" {
			if err := reach.keepYoungerThan(ref.Checksum, keepYounger); err != nil {
				return nil, err
			}
			continue
		}
		if err := reach.commit(ref.Checksum, opts.Depth); err != nil {
			return nil, err
		}
	}

	if opts.RetainBranchDepth == nil && opts.KeepYoungerThan == "" && len(opts.OnlyBranch) == 0 {
		// No per-branch overrides: collection refs participate in the global
		// depth-based traversal as well.
		for _, ref := range allRefs {
			if ref.Kind != repo.RefMirror {
				continue
			}
			if err := reach.commit(ref.Checksum, opts.Depth); err != nil {
				return nil, err
			}
		}
	}

	if opts.KeepUnreferencedYoungerThan != "" {
		for _, obj := range objects {
			if obj.key.typ != repo.ObjectCommit {
				continue
			}
			if reach.set[obj.key] {
				continue
			}
			commit, err := tree.LoadCommit(r, obj.key.checksum)
			if err != nil {
				if errors.Is(err, repo.ErrObjectNotFound) {
					continue
				}
				return nil, err
			}
			if int64(commit.Timestamp) >= keepUnreferenced {
				if _, err := reach.commitTree(obj.key.checksum); err != nil {
					return nil, err
				}
			}
		}
	}
	return reach.set, nil
}

// reachability accumulates objects reachable from a set of roots.
type reachability struct {
	r            *repo.Repo
	set          map[objectKey]bool
	visitedTrees map[string]bool
	commitOnly   bool
}

func newReachability(r *repo.Repo, commitOnly bool) *reachability {
	return &reachability{
		r:            r,
		set:          map[objectKey]bool{},
		visitedTrees: map[string]bool{},
		commitOnly:   commitOnly,
	}
}

func (v *reachability) refs(depth int) error {
	all, err := v.r.ListRefs("")
	if err != nil {
		return err
	}
	for _, ref := range all {
		if err := v.commit(ref.Checksum, depth); err != nil {
			return err
		}
	}
	return nil
}

// commit marks a commit, its tree, and (subject to depth) its parents as
// reachable.
func (v *reachability) commit(csum string, depth int) error {
	if csum == "" {
		return nil
	}
	if v.set[objectKey{csum, repo.ObjectCommit}] {
		// Matches ostree_repo_traverse_commit_with_flags(), which returns
		// immediately when the commit is already in the reachable set.
		return nil
	}
	commit, err := tree.LoadCommit(v.r, csum)
	if err != nil {
		// A missing commit (for example a partial repository) simply ends
		// traversal, like load_variant_if_exists() in the C implementation.
		if errors.Is(err, repo.ErrObjectNotFound) {
			return nil
		}
		return fmt.Errorf("Loading commit %s: %w", csum, err)
	}
	v.set[objectKey{csum, repo.ObjectCommit}] = true
	if !v.commitOnly {
		if err := v.tree(commit.RootMeta, commit.RootTree, v.isPartial(csum)); err != nil {
			return err
		}
	}
	if depth == -1 || depth > 0 {
		next := depth
		if next > 0 {
			next--
		}
		return v.commit(commit.Parent, next)
	}
	return nil
}

// commitTree marks one commit and its tree, without following parents.  It
// returns the parsed commit so callers can decide about the parent chain.
func (v *reachability) commitTree(csum string) (*object.Commit, error) {
	commit, err := tree.LoadCommit(v.r, csum)
	if err != nil {
		if errors.Is(err, repo.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("Loading commit %s: %w", csum, err)
	}
	v.set[objectKey{csum, repo.ObjectCommit}] = true
	if !v.commitOnly {
		if err := v.tree(commit.RootMeta, commit.RootTree, v.isPartial(csum)); err != nil {
			return nil, err
		}
	}
	return commit, nil
}

// tree marks the dirmeta and dirtree of one directory, recursively.  When
// ignoreMissing is set (partial commits), missing dirtrees are skipped instead
// of aborting, matching traverse_iter(ignore_missing_dirs=TRUE).
func (v *reachability) tree(metaCsum, treeCsum string, ignoreMissing bool) error {
	v.set[objectKey{metaCsum, repo.ObjectDirmeta}] = true
	if v.visitedTrees[treeCsum] {
		return nil
	}
	dirtree, err := tree.LoadDirtree(v.r, treeCsum)
	if err != nil {
		if ignoreMissing && errors.Is(err, repo.ErrObjectNotFound) {
			return nil
		}
		return fmt.Errorf("Loading dirtree %s: %w", treeCsum, err)
	}
	v.visitedTrees[treeCsum] = true
	v.set[objectKey{treeCsum, repo.ObjectDirtree}] = true
	for _, file := range dirtree.Files {
		v.set[objectKey{file.Checksum, repo.ObjectFile}] = true
	}
	for _, dir := range dirtree.Dirs {
		if err := v.tree(dir.Meta, dir.Tree, ignoreMissing); err != nil {
			return err
		}
	}
	return nil
}

// isPartial reports whether a commit has a partial marker in state/.
func (v *reachability) isPartial(csum string) bool {
	_, err := v.r.Root().Lstat(path.Join("state", csum+".commitpartial"))
	return err == nil
}

// keepYoungerThan keeps the tip commit of a ref and every ancestor at or
// after cutoff, with their trees.
func (v *reachability) keepYoungerThan(csum string, cutoff int64) error {
	commit, err := v.commitTree(csum)
	if err != nil {
		return err
	}
	if commit == nil {
		return nil
	}
	for commit.Parent != "" {
		parent, err := tree.LoadCommit(v.r, commit.Parent)
		if err != nil {
			if errors.Is(err, repo.ErrObjectNotFound) {
				return nil
			}
			return fmt.Errorf("Loading commit %s: %w", commit.Parent, err)
		}
		if int64(parent.Timestamp) < cutoff {
			return nil
		}
		if _, err := v.commitTree(commit.Parent); err != nil {
			return err
		}
		commit = parent
	}
	return nil
}

// listObjects enumerates the loose objects in the repository.
func listObjects(r *repo.Repo, commitOnly bool) ([]objectEntry, error) {
	dir, err := r.Root().Open("objects")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	shards, err := dir.ReadDir(-1)
	closeErr := dir.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}

	var out []objectEntry
	for _, shard := range shards {
		if !shard.IsDir() || len(shard.Name()) != 2 {
			continue
		}
		sub, err := r.Root().Open(path.Join("objects", shard.Name()))
		if err != nil {
			return nil, err
		}
		entries, err := sub.ReadDir(-1)
		sub.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			key, rel, ok, err := parseLooseName(shard.Name(), entry.Name(), r.Mode())
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if commitOnly && key.typ != repo.ObjectCommit {
				continue
			}
			info, err := r.Root().Lstat(path.Join("objects", rel))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return nil, err
			}
			size := uint64(0)
			if info.Size() > 0 {
				size = uint64(info.Size())
			}
			out = append(out, objectEntry{key: key, rel: "objects/" + rel, size: size})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].key.typ != out[j].key.typ {
			return out[i].key.typ < out[j].key.typ
		}
		return out[i].key.checksum < out[j].key.checksum
	})
	return out, nil
}

// parseLooseName decodes a loose object filename such as
// "3dc57....dirtree" (shard = first two checksum characters).  The set of
// recognized suffixes matches list_loose_objects_at(): file objects depend on
// the repository mode, and detached metadata/tombstones/file-xattrs are never
// enumerated (and therefore never pruned).
func parseLooseName(shard, name string, mode repo.Mode) (objectKey, string, bool, error) {
	dot := strings.LastIndexByte(name, '.')
	if dot < 0 {
		return objectKey{}, "", false, nil
	}
	csum := shard + name[:dot]
	if err := repo.ValidateChecksum(csum); err != nil {
		return objectKey{}, "", false, nil
	}
	typ, ok := objectTypeFromSuffix(name[dot:], mode)
	if !ok {
		return objectKey{}, "", false, nil
	}
	return objectKey{checksum: csum, typ: typ}, path.Join(shard, name), true, nil
}

// objectTypeFromSuffix maps loose object suffixes to object types.
func objectTypeFromSuffix(suffix string, mode repo.Mode) (repo.ObjectType, bool) {
	switch suffix {
	case ".file":
		if !mode.IsArchive() {
			return repo.ObjectFile, true
		}
	case ".filez":
		if mode.IsArchive() {
			return repo.ObjectFile, true
		}
	case ".dirtree":
		return repo.ObjectDirtree, true
	case ".dirmeta":
		return repo.ObjectDirmeta, true
	case ".commit":
		return repo.ObjectCommit, true
	case ".payload-link":
		return repo.ObjectPayloadLink, true
	}
	return 0, false
}

// pruneObjects deletes every unreachable object, or only counts them when
// --no-prune was given.
func pruneObjects(r *repo.Repo, objects []objectEntry, reach map[objectKey]bool, opts Options) (pruneResult, error) {
	res := pruneResult{total: len(objects)}
	for _, obj := range objects {
		if reach[obj.key] {
			continue
		}
		res.pruned++
		res.freed += obj.size
		if opts.NoPrune {
			continue
		}
		if err := deleteObject(r, obj); err != nil {
			return res, err
		}
	}
	return res, nil
}

// deleteObject removes one loose object, along with companion files.
func deleteObject(r *repo.Repo, obj objectEntry) error {
	if obj.key.typ == repo.ObjectCommit {
		if err := removeCompanion(r, obj.key.checksum, repo.ObjectCommitmeta); err != nil {
			return err
		}
		rel := path.Join("state", obj.key.checksum+".commitpartial")
		if err := r.Root().Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.Root().Remove(obj.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("Deleting object %s.commit: %w", obj.key.checksum, err)
		}
		if tombstoneCommits(r) {
			return writeTombstone(r, obj.key.checksum)
		}
		return nil
	}
	if err := r.Root().Remove(obj.rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("Deleting object %s.%s: %w", obj.key.checksum, obj.key.typ, err)
	}
	return nil
}

func removeCompanion(r *repo.Repo, csum string, typ repo.ObjectType) error {
	rel, err := r.ObjectRelPath(csum, typ)
	if err != nil {
		return err
	}
	if err := r.Root().Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// pruneStaticDeltas implements ostree_repo_prune_static_deltas(): remove
// every static delta whose target is commit.
func pruneStaticDeltas(r *repo.Repo, commit string) error {
	names, err := staticdelta.List(r)
	if err != nil {
		return err
	}
	for _, name := range names {
		to := name
		if dash := strings.IndexByte(name, '-'); dash >= 0 {
			to = name[dash+1:]
		}
		if to != commit {
			continue
		}
		if err := staticdelta.Delete(r, name); err != nil {
			return err
		}
	}
	return nil
}

// deleteCommit implements --delete-commit without static deltas.
func deleteCommit(r *repo.Repo, commit string) error {
	if err := repo.ValidateChecksum(commit); err != nil {
		return fmt.Errorf("Invalid checksum %s", commit)
	}
	allRefs, err := r.ListRefs("")
	if err != nil {
		return err
	}
	for _, ref := range allRefs {
		if ref.Checksum != commit {
			continue
		}
		if ref.Kind == repo.RefMirror {
			return fmt.Errorf("Commit '%s' is referenced by (%s, %s)", commit, ref.Collection, ref.Name)
		}
		return fmt.Errorf("Commit '%s' is referenced by '%s'", commit, ref.RefSpec())
	}

	if err := enableTombstones(r); err != nil {
		return err
	}
	rel, err := r.ObjectRelPath(commit, repo.ObjectCommit)
	if err != nil {
		return err
	}
	if err := removeCompanion(r, commit, repo.ObjectCommitmeta); err != nil {
		return err
	}
	if err := r.Root().Remove(path.Join("state", commit+".commitpartial")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.Root().Remove(rel); err != nil {
		return fmt.Errorf("Deleting object %s.commit: %w", commit, err)
	}
	if err := writeTombstone(r, commit); err != nil {
		return err
	}
	return nil
}

func tombstoneCommits(r *repo.Repo) bool {
	on, err := r.Config().GetBool("core", "tombstone-commits", false)
	return err == nil && on
}

// enableTombstones turns on core.tombstone-commits, like
// ot_enable_tombstone_commits() does for --delete-commit.
func enableTombstones(r *repo.Repo) error {
	if tombstoneCommits(r) {
		return nil
	}
	cfg := r.CloneConfig()
	cfg.Set("core", "tombstone-commits", "true")
	return r.WriteConfig(cfg)
}

// writeTombstone writes a tombstone commit object named after csum.  Its
// payload is the a{sv} dictionary libostree uses: {"commit": <csum>}.
func writeTombstone(r *repo.Repo, csum string) error {
	payload := gvariant.Dict{
		{Key: "commit", Value: gvariant.Variant{
			Type:  gvariant.MustParseType("ay"),
			Value: append([]byte(csum), 0),
		}},
	}
	data, err := gvariant.Encode(gvariant.MustParseType("a{sv}"), payload)
	if err != nil {
		return err
	}
	return r.WriteRawObject(csum, repo.ObjectTombstoneCommit, data)
}

// resolveRef resolves a local or "remote:branch" ref to a commit checksum.
func resolveRef(r *repo.Repo, spec string) (string, error) {
	if i := strings.IndexByte(spec, ':'); i > 0 && i < len(spec)-1 {
		remote, name := spec[:i], spec[i+1:]
		sum, err := r.ReadRemoteRef(remote, name)
		if err != nil {
			return "", fmt.Errorf("Refspec '%s' not found", spec)
		}
		return sum, nil
	}
	sum, err := r.ReadRef(spec)
	if err != nil {
		return "", fmt.Errorf("Refspec '%s' not found", spec)
	}
	return sum, nil
}

// parseRetainBranchDepth parses --retain-branch-depth values.
func parseRetainBranchDepth(values []string) (map[string]int, error) {
	out := map[string]int{}
	for _, value := range values {
		branch, depthStr, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("Invalid value %s, must specify BRANCH=DEPTH", value)
		}
		depth, err := strconv.ParseInt(depthStr, 10, 32)
		if err != nil {
			if errors.Is(err, strconv.ErrRange) {
				return nil, fmt.Errorf("Out of range depth %s", depthStr)
			}
			return nil, fmt.Errorf("Invalid depth %s", depthStr)
		}
		out[branch] = int(depth)
	}
	return out, nil
}

// formatSize renders bytes like g_format_size() (SI units, U+00A0 before the
// unit).
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

// pruneSummaryCache removes cached remote summaries whose remote no longer
// exists, mirroring _ostree_repo_prune_tmpdirs()'s summary cache cleanup.
func pruneSummaryCache(r *repo.Repo) error {
	dir := filepath.Join(r.Path(), "tmp", "cache", "summaries")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	remotes := map[string]bool{}
	for _, name := range r.Config().RemoteNames() {
		remotes[name] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		base := strings.TrimSuffix(name, ".sig")
		if remotes[base] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}
