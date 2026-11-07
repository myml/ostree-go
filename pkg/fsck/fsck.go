// Package fsck verifies repository integrity.
package fsck

import (
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/tree"
)

// Options controls Run.
type Options struct {
	All            bool
	Delete         bool
	Quiet          bool
	AddTombstones  bool
	VerifyBindings bool
	VerifyBackRefs bool
	Out            io.Writer
	Err            io.Writer
}

// Run performs the fsck and returns the number of detected errors.
func Run(r *repo.Repo, opts Options) (int, error) {
	if opts.AddTombstones {
		if err := AddTombstones(r); err != nil {
			return 0, err
		}
	}
	out := opts.Out
	errOut := opts.Err
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	report := func(err bool, format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		if opts.Delete || !err {
			fmt.Fprintln(errOut, msg)
		} else {
			fmt.Fprintf(errOut, "error: %s\n", msg)
		}
	}
	if !opts.Quiet {
		fmt.Fprintln(out, "Validating refs...")
		fmt.Fprintln(out, "Validating refs in collections...")
	}

	corruption := false
	refs, err := r.ListRefs("")
	if err != nil {
		return 0, err
	}
	for _, ref := range refs {
		if !checksum.IsHex(ref.Checksum) {
			report(true, "Invalid checksum for ref %s", ref.Name)
			corruption = true
			continue
		}
		if _, err := r.ReadObject(ref.Checksum, repo.ObjectCommit); err != nil {
			report(true, "Object missing: %s.commit", ref.Checksum)
			corruption = true
			continue
		}
		if opts.VerifyBindings {
			if err := checkBinding(r, ref, report); err != nil {
				corruption = true
			}
		}
	}

	if !opts.Quiet {
		fmt.Fprintln(out, "Enumerating commits...")
	}
	commits, err := listObjects(r)
	if err != nil {
		return 0, err
	}
	var commitSums []string
	for _, o := range commits {
		if o.typ == repo.ObjectCommit {
			commitSums = append(commitSums, o.csum)
		}
	}
	sort.Strings(commitSums)
	partialSums := map[string]bool{}
	pulledPartial := loadPartial(r)
	// Only non-partial commits are traversed for content integrity; the
	// others are reported separately, like ot-builtin-fsck.c.
	nVerified := 0
	for _, csum := range commitSums {
		if !pulledPartial[csum] {
			nVerified++
		}
	}
	if !opts.Quiet {
		fmt.Fprintf(out, "Verifying content integrity of %d commit objects...\n", nVerified)
	}
	nPulledPartial := 0
	nCorruptPartial := 0

	for _, csum := range commitSums {
		if pulledPartial[csum] {
			if countPartial(r) > 0 && partialFileNonEmpty(r, csum) {
				nCorruptPartial++
			} else {
				nPulledPartial++
			}
			continue
		}
		data, err := r.ReadObject(csum, repo.ObjectCommit)
		if err != nil {
			report(true, "Object missing: %s.commit", csum)
			continue
		}
		if got := checksum.Hex(checksum.Sum(data)); got != csum {
			report(true, "Object %s.commit: checksum mismatch", csum)
			continue
		}
		commit, err := object.ParseCommit(data)
		if err != nil {
			report(true, "In commits %s: %v", csum, err)
			continue
		}
		if opts.VerifyBackRefs {
			if !checkBackRefs(r, csum, commit, report) {
				corruption = true
			}
		}
		bad := &collector{repo: r, commit: csum, opts: opts, report: report, errOut: errOut}
		if err := bad.walkTree(commit.RootTree, commit.RootMeta); err != nil {
			return 0, err
		}
		if bad.failed {
			partialSums[csum] = true
			if opts.Delete {
				if err := writePartial(r, csum); err != nil {
					return 0, err
				}
			}
		}
	}

	total := len(commits)
	if total > 0 {
		fmt.Fprintf(out, "fsck objects (%d/%d) 100%%\n", total, total)
	}

	if nCorruptPartial > 0 {
		fmt.Fprintf(errOut, "error: %d partial commits from fsck-detected corruption\n", nCorruptPartial)
		return nCorruptPartial, fmt.Errorf("Repository corruption encountered")
	}
	// Partially pulled commits have no tree to verify.
	if nPulledPartial > 0 && !opts.Quiet {
		fmt.Fprintf(out, "%d partial commits not verified\n", nPulledPartial)
	}
	if len(partialSums) > 0 && !opts.Delete {
		return len(partialSums), fmt.Errorf("Repository corruption encountered")
	}
	if len(partialSums) > 0 && opts.Delete {
		return len(partialSums), fmt.Errorf("Repository corruption encountered")
	}
	if corruption {
		return 1, fmt.Errorf("Repository corruption encountered")
	}
	fmt.Fprintf(out, "object fsck of %d commits completed successfully - no errors found.\n", nVerified)
	return 0, nil
}

type objectRef struct {
	csum string
	typ  repo.ObjectType
}

func listObjects(r *repo.Repo) ([]objectRef, error) {
	objectsDir := path.Join(r.Path(), "objects")
	dirs, err := os.ReadDir(objectsDir)
	if err != nil {
		return nil, err
	}
	var out []objectRef
	for _, d := range dirs {
		if !d.IsDir() || len(d.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(path.Join(objectsDir, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			name := e.Name()
			dot := strings.LastIndexByte(name, '.')
			if dot < 0 {
				continue
			}
			suffix := strings.TrimSuffix(name[dot+1:], "z")
			typ, err := repo.ParseObjectType(suffix)
			if err != nil {
				continue
			}
			csum := d.Name() + name[:dot]
			if !checksum.IsHex(csum) {
				continue
			}
			out = append(out, objectRef{csum: csum, typ: typ})
		}
	}
	return out, nil
}

type collector struct {
	repo    *repo.Repo
	commit  string
	opts    Options
	report  func(bool, string, ...any)
	errOut  io.Writer
	failed  bool
	visited map[string]bool
}

func (c *collector) fail(format string, args ...any) {
	c.failed = true
	c.report(true, "In commits %s: %s", c.commit, fmt.Sprintf(format, args...))
}

func (c *collector) walkTree(treeCsum, metaCsum string) error {
	if c.visited == nil {
		c.visited = map[string]bool{}
	}
	if c.visited["t:"+treeCsum] {
		return nil
	}
	c.visited["t:"+treeCsum] = true

	treeData, err := c.repo.ReadObject(treeCsum, repo.ObjectDirtree)
	if err != nil {
		c.fail("fsck content object %s: Object missing: %s.dirtree", treeCsum, treeCsum)
		return nil
	}
	if got := checksum.Hex(checksum.Sum(treeData)); got != treeCsum {
		c.fail("fsck content object %s: checksum mismatch", treeCsum)
		return nil
	}
	if err := c.checkMeta(metaCsum, repo.ObjectDirmeta); err != nil {
		return err
	}
	t, err := object.ParseDirtree(treeData)
	if err != nil {
		c.fail("fsck content object %s: %v", treeCsum, err)
		return nil
	}
	for _, f := range t.Files {
		c.checkFile(f.Checksum)
	}
	for _, d := range t.Dirs {
		if err := c.checkMeta(d.Meta, repo.ObjectDirmeta); err != nil {
			return err
		}
		if err := c.walkTree(d.Tree, d.Meta); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) checkMeta(csum string, typ repo.ObjectType) error {
	if csum == "" {
		return nil
	}
	if c.visited["m:"+csum] {
		return nil
	}
	c.visited["m:"+csum] = true
	data, err := c.repo.ReadObject(csum, typ)
	if err != nil {
		c.fail("fsck content object %s: Object missing: %s.%s", csum, csum, typ)
		return nil
	}
	if got := checksum.Hex(checksum.Sum(data)); got != csum {
		c.fail("fsck content object %s: checksum mismatch", csum)
	}
	return nil
}

func (c *collector) checkFile(csum string) {
	if c.visited["f:"+csum] {
		return
	}
	c.visited["f:"+csum] = true
	loaded, err := tree.LoadFile(c.repo, csum)
	if err != nil {
		if c.opts.Delete {
			if full, perr := c.repo.ObjectPath(csum, repo.ObjectFile); perr == nil {
				os.Remove(full)
			}
		}
		c.fail("fsck content object %s: %v", csum, err)
		return
	}
	got, err := loaded.Header.Checksum(loaded.Content)
	if err != nil {
		c.fail("fsck content object %s: %v", csum, err)
		return
	}
	if got != csum {
		if c.opts.Delete {
			if full, perr := c.repo.ObjectPath(csum, repo.ObjectFile); perr == nil {
				os.Remove(full)
			}
		}
		c.fail("fsck content object %s: Corrupted file object; checksum expected='%s' actual='%s'", csum, csum, got)
	}
}

func writePartial(r *repo.Repo, csum string) error {
	dir := path.Join(r.Path(), "state")
	if err := os.MkdirAll(dir, 0775); err != nil {
		return err
	}
	return os.WriteFile(path.Join(dir, csum+".commitpartial"), []byte("1"), 0644)
}

func countPartial(r *repo.Repo) int {
	entries, err := os.ReadDir(path.Join(r.Path(), "state"))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".commitpartial") {
			n++
		}
	}
	return n
}

// checkBinding verifies that a ref is listed in the commit's ref-binding
// metadata when --verify-bindings is used.
func checkBinding(r *repo.Repo, ref repo.Ref, report func(bool, string, ...any)) error {
	data, err := r.ReadObject(ref.Checksum, repo.ObjectCommit)
	if err != nil {
		return err
	}
	commit, err := object.ParseCommit(data)
	if err != nil {
		return err
	}
	v, ok := commit.Metadata.Get(object.MetaRefBinding)
	if !ok {
		report(true, "Commit has no requested ref ‘%s’ in ref binding metadata", ref.Name)
		return fmt.Errorf("missing binding")
	}
	variant, ok := v.(gvariant.Variant)
	if !ok {
		return fmt.Errorf("invalid binding")
	}
	arr, ok := variant.Value.([]gvariant.Value)
	if !ok {
		return fmt.Errorf("invalid binding")
	}
	var names []string
	found := false
	for _, item := range arr {
		name, _ := item.(string)
		names = append(names, name)
		if name == ref.Name {
			found = true
		}
	}
	if !found {
		report(true, "Commit has no requested ref ‘%s’ in ref binding metadata (‘%s’)", ref.Name, strings.Join(names, "’, ‘"))
		return fmt.Errorf("missing binding")
	}
	if ref.Collection != "" {
		if v, ok := commit.Metadata.Get(object.MetaCollectionBinding); ok {
			binding := metaString(v)
			if binding != "" && binding != ref.Collection {
				report(true, "Commit has collection ID ‘%s’ in collection binding metadata, while the remote it came from has collection ID ‘%s’", binding, ref.Collection)
				return fmt.Errorf("collection mismatch")
			}
		}
	}
	return nil
}

// metaString unwraps a string metadata value.
func metaString(v gvariant.Value) string {
	if variant, ok := v.(gvariant.Variant); ok {
		s, _ := variant.Value.(string)
		return s
	}
	s, _ := v.(string)
	return s
}

// checkBackRefs verifies that every collection–ref listed in a commit's
// bindings actually exists as a ref.
func checkBackRefs(r *repo.Repo, csum string, commit *object.Commit, report func(bool, string, ...any)) bool {
	v, ok := commit.Metadata.Get(object.MetaRefBinding)
	if !ok {
		return true
	}
	variant, ok := v.(gvariant.Variant)
	if !ok {
		return true
	}
	arr, ok := variant.Value.([]gvariant.Value)
	if !ok {
		return true
	}
	collection := ""
	if cv, ok := commit.Metadata.Get(object.MetaCollectionBinding); ok {
		collection = metaString(cv)
	}
	have := map[string]bool{}
	if collection != "" {
		mirrors, err := r.ListRefs(collection + ":")
		if err != nil {
			return false
		}
		for _, ref := range mirrors {
			if ref.Kind == repo.RefMirror && ref.Collection == collection {
				have[ref.Name] = true
			}
		}
	} else {
		refs, err := r.ListRefs("")
		if err != nil {
			return false
		}
		for _, ref := range refs {
			if ref.Kind != repo.RefMirror {
				have[ref.Name] = true
			}
		}
	}
	ok = true
	for _, item := range arr {
		name, _ := item.(string)
		if have[name] {
			continue
		}
		if collection != "" {
			report(true, "Collection–ref (%s, %s) in bindings for commit %s does not exist", collection, name, csum)
		} else {
			report(true, "Ref ‘%s’ in bindings for commit %s does not exist", name, csum)
		}
		ok = false
	}
	return ok
}

// AddTombstones writes tombstone commits for commits whose parent objects are
// missing, then deletes those commits and enables tombstone-commits.
func AddTombstones(r *repo.Repo) error {
	commits, err := listCommitChecksums(r)
	if err != nil {
		return err
	}
	dictType := gvariant.MustParseType("a{sv}")
	var tombstones []string
	for _, csum := range commits {
		data, err := r.ReadObject(csum, repo.ObjectCommit)
		if err != nil {
			continue
		}
		c, err := object.ParseCommit(data)
		if err != nil || c.Parent == "" {
			continue
		}
		exists, err := r.HasObject(c.Parent, repo.ObjectCommit)
		if err != nil {
			return err
		}
		if !exists {
			tombstones = append(tombstones, csum)
		}
	}
	if len(tombstones) == 0 {
		return nil
	}
	cfg := r.CloneConfig()
	cfg.Set("core", "tombstone-commits", "true")
	if err := r.WriteConfig(cfg); err != nil {
		return err
	}
	for _, csum := range tombstones {
		csumBytes, err := hex.DecodeString(csum)
		if err != nil {
			return err
		}
		dict := gvariant.Dict{}
		dict.Set("commit", gvariant.Variant{Type: gvariant.MustParseType("ay"), Value: csumBytes})
		data, err := gvariant.Encode(dictType, dict)
		if err != nil {
			return err
		}
		if err := r.WriteRawObject(csum, repo.ObjectTombstoneCommit, data); err != nil {
			return err
		}
		if p, err := r.ObjectPath(csum, repo.ObjectCommit); err == nil {
			_ = os.Remove(p)
		}
	}
	return nil
}

// listCommitChecksums enumerates all commit objects in the repository.
func listCommitChecksums(r *repo.Repo) ([]string, error) {
	root := path.Join(r.Path(), "objects")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 {
			continue
		}
		sub, err := os.ReadDir(path.Join(root, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			name := f.Name()
			if !strings.HasSuffix(name, ".commit") {
				continue
			}
			out = append(out, e.Name()+strings.TrimSuffix(name, ".commit"))
		}
	}
	return out, nil
}

// loadPartial returns the set of commits with a commitpartial marker.
func loadPartial(r *repo.Repo) map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(path.Join(r.Path(), "state"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".commitpartial") {
			out[strings.TrimSuffix(e.Name(), ".commitpartial")] = true
		}
	}
	return out
}

// partialFileNonEmpty reports whether a commitpartial marker was written by
// fsck --delete (non-empty) rather than by a metadata-only pull (empty).
func partialFileNonEmpty(r *repo.Repo, csum string) bool {
	fi, err := os.Stat(path.Join(r.Path(), "state", csum+".commitpartial"))
	return err == nil && fi.Size() > 0
}
