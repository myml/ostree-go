package pull

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/tree"
)

// RemoteOptions configures a remote (file:// or http) pull.
type RemoteOptions struct {
	Repo   *repo.Repo
	URL    string
	Remote string
	Refs   []string
	Depth  int
	Mirror bool
	// RemoteCollection is the remote's collection ID, if any.
	RemoteCollection string
	// CommitMetadataOnly fetches only commit objects, marking them partial.
	CommitMetadataOnly bool
	// DepthSet records whether --depth was given explicitly.
	DepthSet bool
	// RequireStaticDeltas makes pull use static deltas or fail.
	RequireStaticDeltas bool
	DryRun              bool
	Untrusted           bool
	Quiet               bool
	Out                 io.Writer
	Err                 io.Writer

	// Transport options.
	// RemoteConfig, when nil, is loaded from Repo's configuration using
	// Remote as the name.
	RemoteConfig *RemoteConfig
	// URLOverride is pull's --url: it replaces the remote's configured url
	// for this operation.
	URLOverride string
	// ContentURL overrides the remote's contenturl (unused by the CLI).
	ContentURL string
	// HTTPHeaders are additional NAME=VALUE headers (pull's --http-header).
	HTTPHeaders []string
	// AppendUserAgent is appended to the default User-Agent.
	AppendUserAgent string
	// NetworkRetries is the number of retries after the first attempt;
	// NetworkRetriesSet distinguishes 0 from "use the default".
	NetworkRetries    int
	NetworkRetriesSet bool
	// LocalCacheRepos are repositories consulted for objects before the
	// network (pull -L/--localcache-repo).
	LocalCacheRepos []string
	// CacheDir is a local payload cache (pull --cache-dir).
	CacheDir string

	// Subpaths limits the fetch to these subtrees (pull --subpath), matching
	// the "subdir"/"subdirs" pull options of libostree.
	Subpaths []string
	// DisableVerifyBindings skips the ostree.ref-binding and
	// ostree.collection-binding verification (pull --disable-verify-bindings).
	DisableVerifyBindings bool
	// BareUserOnlyFiles rejects regular file modes outside 0775
	// (pull --bareuseronly-files).
	BareUserOnlyFiles bool
	// TrustedHTTP skips content checksum verification when mirroring into an
	// archive repository (pull --http-trusted).
	TrustedHTTP bool
	// RefOverrides maps a requested ref name to an explicit commit
	// (pull's BRANCH@COMMIT syntax).
	RefOverrides map[string]string
	// CollectionRefs requests explicit collection refs, as used by
	// find-remotes/create-usb.
	CollectionRefs []CollectionRef

	// requestedRefs maps a commit checksum to the ref name it was requested
	// through; binding verification uses it.
	requestedRefs map[string]string

	// Verify configures commit and summary signature verification.
	Verify VerifyOptions
	// verify is the resolved verifier, populated by Remote for pullClosure.
	verify *PullVerifier
}

// RemoteResult reports what was fetched.
type RemoteResult struct {
	FetchedCommits int
	FetchedObjects int
	Refs           map[string]string

	// MetadataFetched/ContentFetched count network fetches; the *Local
	// counters count objects imported from --localcache-repo repositories.
	MetadataFetched int
	ContentFetched  int
	MetadataLocal   int
	ContentLocal    int

	// ContentBytesWritten is the on-disk size of newly written content
	// objects, reported as "<size> content written" in the pull status.
	ContentBytesWritten uint64
}

// Remote pulls refs and their commit closures from a remote repository.
func Remote(opts RemoteOptions) (*RemoteResult, error) {
	f, preloadedSummary, err := prepareRemote(&opts)
	if err != nil {
		return nil, err
	}
	verifier, err := NewPullVerifier(opts.Repo, opts.Remote, opts.Verify)
	if err != nil {
		return nil, err
	}
	opts.verify = verifier
	res := &RemoteResult{Refs: map[string]string{}}
	opts.requestedRefs = map[string]string{}

	// The summary is only needed to resolve refs that were requested without
	// an explicit commit, or to enumerate every ref for a mirror pull.  In
	// particular `pull REMOTE BRANCH@COMMIT` must work with a broken summary.
	needSummary := false
	switch {
	case len(opts.CollectionRefs) > 0:
		for _, cr := range opts.CollectionRefs {
			if cr.Checksum == "" {
				needSummary = true
				break
			}
		}
	case opts.Mirror && len(opts.Refs) == 0:
		needSummary = true
	default:
		for _, ref := range opts.Refs {
			if repo.ValidateChecksum(ref) == nil {
				continue
			}
			if opts.RefOverrides[ref] == "" {
				needSummary = true
				break
			}
		}
	}
	if verifier.NeedsSummary() || opts.RequireStaticDeltas {
		needSummary = true
	}
	var summaryData []byte
	if preloadedSummary != nil {
		summaryData = preloadedSummary
	} else if needSummary {
		data, serr := f.get("summary")
		switch {
		case serr == nil:
			summaryData = data
		case errors.Is(serr, fs404):
		default:
			return nil, serr
		}
	}
	// Signature verification: fetch and verify summary.sig, and cache the
	// verified pair, before the summary is used or published.
	var summarySig []byte
	if verifier.NeedsSummary() {
		if summaryData == nil {
			return nil, verifier.MissingSummaryError()
		}
		sig, serr := f.get("summary.sig")
		if serr != nil {
			return nil, verifier.MissingSummarySigError()
		}
		summarySig = sig
		if err := verifier.VerifyAndCacheSummary(summaryData, summarySig); err != nil {
			return nil, err
		}
	}
	// A mirror pull stores the remote summary unless only specific refs were
	// requested (libostree checks !refs_to_fetch && !collection_refs_set).
	if opts.Mirror && !opts.DryRun && summaryData != nil &&
		len(opts.Refs) == 0 && len(opts.CollectionRefs) == 0 {
		if summarySig != nil {
			if err := opts.Repo.WriteSummaryAndSignature(summaryData, summarySig); err != nil {
				return nil, err
			}
		} else if err := opts.Repo.WriteSummaryFile(summaryData); err != nil {
			return nil, err
		}
	}
	var refMap = map[string]string{}
	var parsedSummary *object.Summary
	if summaryData != nil {
		refMap, err = summaryRefs(summaryData)
		if err != nil {
			return nil, err
		}
		parsedSummary, _ = object.ParseSummary(summaryData)
	}
	// collection: an explicit collection-ref pull never enumerates the
	// remote's plain refs.
	if len(opts.Refs) == 0 && len(opts.CollectionRefs) == 0 {
		for name := range refMap {
			opts.Refs = append(opts.Refs, name)
		}
		if base := f.localBase(); base != "" {
			if local, err := listLocalRefs(base); err == nil {
				for name := range local {
					if _, ok := refMap[name]; !ok {
						refMap[name] = ""
						opts.Refs = append(opts.Refs, name)
					}
				}
			}
		}
	}
	sort.Strings(opts.Refs)
	if len(opts.Refs) == 0 && len(opts.CollectionRefs) == 0 {
		return nil, fmt.Errorf("No refs found on remote")
	}
	seen := map[string]bool{}
	for _, ref := range opts.Refs {
		// A checksum argument requests that commit directly rather than a
		// named ref; no ref is written for it (ostree_repo_pull's
		// commits_to_fetch path).
		if repo.ValidateChecksum(ref) == nil {
			usedDelta := false
			if opts.RequireStaticDeltas {
				have, herr := opts.Repo.HasObject(ref, repo.ObjectCommit)
				if herr != nil {
					return nil, herr
				}
				if have {
					usedDelta = true
				}
				handled := false
				var derr error
				if !have {
					handled, derr = pullStaticDelta(f, opts, summaryData, ref)
					if derr != nil {
						return nil, derr
					}
					if !handled && !opts.DryRun {
						return nil, fmt.Errorf("deltas required, but none found")
					}
					usedDelta = handled
				}
			}
			if !usedDelta {
				if err := pullClosure(f, opts, ref, opts.Depth, seen, res); err != nil {
					return nil, err
				}
			}
			continue
		}
		csum := opts.RefOverrides[ref]
		if csum == "" {
			var ok bool
			csum, ok = refMap[ref]
			if !ok || csum == "" {
				data, err := f.get("refs/heads/" + ref)
				if err != nil {
					if errors.Is(err, fs404) {
						if summaryData == nil {
							return nil, fmt.Errorf("No such ref '%s' (404)", ref)
						}
						return nil, fmt.Errorf("No such ref '%s'", ref)
					}
					return nil, fmt.Errorf("Fetching ref '%s': %w", ref, err)
				}
				csum = strings.TrimSpace(strings.NewReplacer("\r", "", "\n", " ").Replace(string(data)))
				if repo.ValidateChecksum(csum) != nil {
					return nil, fmt.Errorf("Fetching checksum for ref ((empty), %s): Invalid rev %s", ref, csum)
				}
			}
		}
		if repo.ValidateChecksum(csum) != nil {
			return nil, fmt.Errorf("Invalid checksum for ref '%s'", ref)
		}
		res.Refs[ref] = csum
		opts.requestedRefs[csum] = ref
		usedDelta := false
		if opts.RequireStaticDeltas {
			have, herr := opts.Repo.HasObject(csum, repo.ObjectCommit)
			if herr != nil {
				return nil, herr
			}
			if have {
				usedDelta = true
			}
			handled := false
			var derr error
			if !have {
				handled, derr = pullStaticDelta(f, opts, summaryData, csum)
				if derr != nil {
					return nil, derr
				}
				if !handled && !opts.DryRun {
					return nil, fmt.Errorf("deltas required, but none found")
				}
				usedDelta = handled
			}
		}
		if !usedDelta {
			if err := pullClosure(f, opts, csum, opts.Depth, seen, res); err != nil {
				return nil, err
			}
		}
		if !opts.DryRun {
			if opts.Mirror {
				if opts.RemoteCollection != "" {
					if err := opts.Repo.WriteMirrorRef(opts.RemoteCollection, ref, csum); err != nil {
						return nil, err
					}
				} else if err := opts.Repo.WriteRef(ref, csum); err != nil {
					return nil, err
				}
			} else if opts.RemoteCollection != "" {
				if err := opts.Repo.WriteMirrorRef(opts.RemoteCollection, ref, csum); err != nil {
					return nil, err
				}
			} else if opts.Remote != "" {
				if err := opts.Repo.WriteRemoteRef(opts.Remote, ref, csum); err != nil {
					return nil, err
				}
			} else {
				if err := opts.Repo.WriteRef(ref, csum); err != nil {
					return nil, err
				}
			}
		}
	}
	// Explicit collection refs (find-remotes/create-usb): resolve through the
	// summary's collection map and write refs/mirrors.
	for _, cr := range opts.CollectionRefs {
		csum := cr.Checksum
		if csum == "" && parsedSummary != nil {
			if v, ok := summaryLookupCollectionRef(parsedSummary, cr.CollectionID, cr.RefName); ok {
				csum = v
			}
		}
		if csum == "" {
			data, serr := f.get("refs/mirrors/" + cr.CollectionID + "/" + cr.RefName)
			if serr != nil {
				return nil, fmt.Errorf("No such branch (%s, %s) in repository summary",
					cr.CollectionID, cr.RefName)
			}
			csum = strings.TrimSpace(string(data))
		}
		if repo.ValidateChecksum(csum) != nil {
			return nil, fmt.Errorf("Invalid checksum for ref '%s'", cr.RefName)
		}
		opts.requestedRefs[csum] = cr.RefName
		if err := pullClosure(f, opts, csum, opts.Depth, seen, res); err != nil {
			return nil, err
		}
		if !opts.DryRun {
			ownCollection, _ := opts.Repo.Config().Get("core", "collection-id")
			if cr.CollectionID == ownCollection {
				if err := opts.Repo.WriteRef(cr.RefName, csum); err != nil {
					return nil, err
				}
			} else if err := opts.Repo.WriteMirrorRef(cr.CollectionID, cr.RefName, csum); err != nil {
				return nil, err
			}
		}
	}
	res.MetadataFetched = f.stats.metadataFetched
	res.ContentFetched = f.stats.contentFetched
	res.MetadataLocal = f.stats.metadataLocal
	res.ContentLocal = f.stats.contentLocal
	if opts.Out != nil && !opts.DryRun {
		writePullStatus(opts.Out, res)
	}
	return res, nil
}

// writePullStatus emits the same summary line as
// ostree_repo_pull_default_console_progress_changed() on a non-TTY.
func writePullStatus(w io.Writer, res *RemoteResult) {
	fmt.Fprintf(w, "%d metadata, %d content objects fetched",
		res.MetadataFetched, res.ContentFetched)
	if res.MetadataLocal != 0 || res.ContentLocal != 0 {
		fmt.Fprintf(w, " (%d meta, %d content local)", res.MetadataLocal, res.ContentLocal)
	}
	fmt.Fprintf(w, "; %s content written", formatSize(res.ContentBytesWritten))
	fmt.Fprintln(w)
}

// FetchSummaryRaw returns the raw summary bytes from a remote URL.
func FetchSummaryRaw(url string) ([]byte, error) {
	f, err := newFetcher(url)
	if err != nil {
		return nil, err
	}
	return f.get("summary")
}

func fetchSummaryRefs(f *fetcher) (map[string]string, error) {
	data, err := f.get("summary")
	out := map[string]string{}
	if err != nil {
		if errors.Is(err, fs404) {
			return out, nil
		}
		return nil, err
	}
	return summaryRefs(data)
}

func summaryRefs(data []byte) (map[string]string, error) {
	out := map[string]string{}
	summary, err := object.ParseSummary(data)
	if err != nil {
		return nil, err
	}
	for _, ref := range summary.Refs {
		out[ref.Name] = ref.Commit
	}
	return out, nil
}

func pullClosure(f *fetcher, opts RemoteOptions, csum string, depth int, seen map[string]bool, res *RemoteResult) error {
	if seen[csum] {
		return nil
	}
	seen[csum] = true
	// pull: skip already-present objects; libostree checks has_object before
	// enqueueing any request, so a repeated pull reports zero fetched objects.
	// Stored commits are still re-scanned when they are partial.
	if !opts.DryRun {
		existed, herr := opts.Repo.HasObject(csum, repo.ObjectCommit)
		if herr != nil {
			return herr
		}
		if existed {
			return pullPresentCommit(f, opts, csum, depth, seen, res)
		}
	}
	raw, fromCache, err := fetchObject(f, csum, repo.ObjectCommit)
	if err != nil {
		return fmt.Errorf("Fetching commit %s: %w", csum, err)
	}
	// checksum: content fetched over HTTP is always verified.
	if !f.local {
		if err := verifyMetadataChecksum(raw, csum, repo.ObjectCommit); err != nil {
			return err
		}
	}
	f.countObject(repo.ObjectCommit, fromCache)
	commit, err := object.ParseCommit(raw)
	if err != nil {
		return fmt.Errorf("Parsing commit %s: %w", csum, err)
	}
	// collection: verify ref/collection bindings before the commit is written,
	// like _verify_unwritten_commit() in ostree-repo-pull.c.
	if !opts.DisableVerifyBindings {
		if err := verifyCommitBindings(opts.RemoteCollection, opts.requestedRefs[csum], commit); err != nil {
			return fmt.Errorf("Commit %s: %w", csum, err)
		}
	}
	// Fetch the detached metadata now so signatures are verified before the
	// commit is written.  Detached metadata is optional: only a real 404 is
	// ignored.
	var detached []byte
	detachedFromCache := false
	if data, cached, derr := fetchObject(f, csum, repo.ObjectCommitmeta); derr == nil {
		detached = data
		detachedFromCache = cached
	} else if !errors.Is(derr, repo.ErrObjectNotFound) {
		return fmt.Errorf("Fetching commitmeta %s: %w", csum, derr)
	}
	if err := opts.verify.VerifyCommit(csum, raw, detached); err != nil {
		return err
	}
	if !opts.DryRun {
		if _, err := opts.Repo.WriteObject(repo.ObjectCommit, raw); err != nil {
			return err
		}
		// pull: mark the commit partial until its whole tree is in place; an
		// interrupted pull then resumes by re-scanning this commit.
		if err := opts.Repo.MarkPartial(csum); err != nil {
			return err
		}
		if err := opts.Repo.RemoveTombstone(csum); err != nil {
			return err
		}
	}
	res.FetchedCommits++
	switch {
	case opts.CommitMetadataOnly:
		if !opts.DepthSet {
			if err := opts.Repo.MarkPartial(csum); err != nil {
				return err
			}
		}
	case len(opts.Subpaths) > 0:
		// subpath: the commit is only partially present, so leave the
		// commitpartial marker behind (a later plain pull clears it).
		if !opts.DryRun {
			if err := opts.Repo.MarkPartial(csum); err != nil {
				return err
			}
		}
		if err := pullTree(f, opts, "/", commit.RootTree, commit.RootMeta, seen, res); err != nil {
			return err
		}
	default:
		if err := pullTree(f, opts, "/", commit.RootTree, commit.RootMeta, seen, res); err != nil {
			return err
		}
		if !opts.DryRun {
			// A full pull removes any commitpartial marker left by an earlier
			// --subpath or --commit-metadata-only fetch.
			if err := opts.Repo.ClearPartial(csum); err != nil {
				return err
			}
		}
	}
	// Detached metadata is optional.  ReplaceRawObject is used because a
	// commit's detached metadata is mutable (signatures may be added or
	// deleted on the remote between pulls).
	if detached != nil && !opts.DryRun {
		if err := opts.Repo.ReplaceRawObject(csum, repo.ObjectCommitmeta, detached); err != nil {
			return err
		}
		f.countObject(repo.ObjectCommitmeta, detachedFromCache)
	}
	if depth != 0 && commit.Parent != "" {
		next := depth
		if depth > 0 {
			next--
		}
		err := pullClosure(f, opts, commit.Parent, next, seen, res)
		if err != nil {
			// Truncated histories are tolerated at the boundary.
			if errors.Is(err, repo.ErrObjectNotFound) {
				return nil
			}
			return err
		}
	}
	return nil
}

// pullPresentCommit handles a commit that is already in the destination
// repository.  Like libostree it re-fetches the (mutable) detached metadata,
// walks the tree again when the commit is partial or a --subpath pull is in
// progress, and fetches nothing else.
func pullPresentCommit(f *fetcher, opts RemoteOptions, csum string, depth int, seen map[string]bool, res *RemoteResult) error {
	raw, err := opts.Repo.ReadObject(csum, repo.ObjectCommit)
	if err != nil {
		return fmt.Errorf("Fetching commit %s: %w", csum, err)
	}
	commit, err := object.ParseCommit(raw)
	if err != nil {
		return fmt.Errorf("Parsing commit %s: %w", csum, err)
	}
	// collection: a stored commit requested through a ref is re-verified, so
	// pulling an already-present commit under a different ref still fails.
	if !opts.DisableVerifyBindings {
		if err := verifyCommitBindings(opts.RemoteCollection, opts.requestedRefs[csum], commit); err != nil {
			return fmt.Errorf("Commit %s: %w", csum, err)
		}
	}
	// Signatures may have been added or removed on the remote, so always
	// re-fetch the detached metadata (rpm-ostree#630).  Verify it before the
	// local copy is replaced so an untrusted signature is never stored.
	var detached []byte
	if data, _, derr := fetchObject(f, csum, repo.ObjectCommitmeta); derr == nil {
		detached = data
	} else if !errors.Is(derr, repo.ErrObjectNotFound) {
		return fmt.Errorf("Fetching commitmeta %s: %w", csum, derr)
	}
	if err := opts.verify.VerifyCommit(csum, raw, detached); err != nil {
		return err
	}
	if detached != nil {
		if err := opts.Repo.ReplaceRawObject(csum, repo.ObjectCommitmeta, detached); err != nil {
			return err
		}
	}
	if !opts.CommitMetadataOnly {
		partial, perr := opts.Repo.CommitIsPartial(csum)
		if perr != nil {
			return perr
		}
		if partial || len(opts.Subpaths) > 0 {
			if err := pullTree(f, opts, "/", commit.RootTree, commit.RootMeta, seen, res); err != nil {
				return err
			}
		}
		if len(opts.Subpaths) == 0 {
			// A full pull removes any commitpartial marker left by an earlier
			// --subpath or --commit-metadata-only fetch.
			if err := opts.Repo.ClearPartial(csum); err != nil {
				return err
			}
		}
	}
	if depth != 0 && commit.Parent != "" {
		next := depth
		if depth > 0 {
			next--
		}
		if err := pullClosure(f, opts, commit.Parent, next, seen, res); err != nil {
			if errors.Is(err, repo.ErrObjectNotFound) {
				return nil
			}
			return err
		}
	}
	return nil
}

// localOrFetchMeta returns the bytes of one metadata object, reading the
// destination repository when the object is already present.  libostree
// always re-scans stored dirtrees (their children may be missing in a partial
// repository) but never re-fetches them.
func localOrFetchMeta(f *fetcher, opts RemoteOptions, csum string, typ repo.ObjectType, res *RemoteResult) ([]byte, error) {
	if !opts.DryRun {
		existed, err := opts.Repo.HasObject(csum, typ)
		if err != nil {
			return nil, err
		}
		if existed {
			return opts.Repo.ReadObject(csum, typ)
		}
	}
	raw, fromCache, err := fetchObject(f, csum, typ)
	if err != nil {
		return nil, err
	}
	// Validate before writing so a malicious object never reaches the repo.
	if typ == repo.ObjectDirtree {
		if err := validateDirtreeNames(raw, csum); err != nil {
			return nil, err
		}
	}
	if !f.local {
		if err := verifyMetadataChecksum(raw, csum, typ); err != nil {
			return nil, err
		}
	}
	f.countObject(typ, fromCache)
	if !opts.DryRun {
		if _, err := opts.Repo.WriteObject(typ, raw); err != nil {
			return nil, err
		}
	}
	res.FetchedObjects++
	return raw, nil
}

// countObject updates the fetcher's fetched/imported counters.  Metadata
// objects (dirtree, dirmeta, commit, commitmeta) are reported separately from
// content (file) objects, exactly like the pull status in libostree.
func (f *fetcher) countObject(typ repo.ObjectType, fromCache bool) {
	if f.stats == nil {
		return
	}
	if typ.IsMeta() {
		if fromCache {
			f.stats.metadataLocal++
		} else {
			f.stats.metadataFetched++
		}
		return
	}
	if fromCache {
		f.stats.contentLocal++
	} else {
		f.stats.contentFetched++
	}
}

// pullTree fetches a dirtree, its dirmeta and (subject to --subpath filtering)
// its children.  dirPath is the tree's path inside the commit with a leading
// and trailing '/', so the root is "/".
func pullTree(f *fetcher, opts RemoteOptions, dirPath string, treeCsum, metaCsum string, seen map[string]bool, res *RemoteResult) error {
	if seen["t:"+treeCsum] {
		return nil
	}
	seen["t:"+treeCsum] = true
	raw, err := localOrFetchMeta(f, opts, treeCsum, repo.ObjectDirtree, res)
	if err != nil {
		return fmt.Errorf("Fetching dirtree %s: %w", treeCsum, err)
	}
	t, err := object.ParseDirtree(raw)
	if err != nil {
		return err
	}
	if err := pullMeta(f, opts, metaCsum, repo.ObjectDirmeta, seen, res); err != nil {
		return err
	}
	for _, file := range t.Files {
		// subpath: skip content objects outside the requested subpaths.
		if !pullSubpathMatches(opts.Subpaths, dirPath, file.Name, false) {
			continue
		}
		if err := pullFile(f, opts, file.Checksum, seen, res); err != nil {
			return err
		}
	}
	for _, d := range t.Dirs {
		// subpath: only descend into directories that lead to (or are inside)
		// a requested subpath.
		if !pullSubpathMatches(opts.Subpaths, dirPath, d.Name, true) {
			continue
		}
		if err := pullMeta(f, opts, d.Meta, repo.ObjectDirmeta, seen, res); err != nil {
			return err
		}
		if err := pullTree(f, opts, dirPath+d.Name+"/", d.Tree, d.Meta, seen, res); err != nil {
			return err
		}
	}
	return nil
}

func pullMeta(f *fetcher, opts RemoteOptions, csum string, typ repo.ObjectType, seen map[string]bool, res *RemoteResult) error {
	if csum == "" || seen["m:"+csum] {
		return nil
	}
	seen["m:"+csum] = true
	if _, err := localOrFetchMeta(f, opts, csum, typ, res); err != nil {
		return fmt.Errorf("Fetching %s %s: %w", typ, csum, err)
	}
	return nil
}

func pullFile(f *fetcher, opts RemoteOptions, csum string, seen map[string]bool, res *RemoteResult) error {
	if seen["f:"+csum] {
		return nil
	}
	seen["f:"+csum] = true
	if !opts.DryRun {
		existed, herr := opts.Repo.HasObject(csum, repo.ObjectFile)
		if herr != nil {
			return herr
		}
		if existed {
			// pull: content objects already present are never re-fetched.
			return nil
		}
	}
	raw, fromCache, err := fetchObject(f, csum, repo.ObjectFile)
	if err != nil {
		return fmt.Errorf("Fetching file %s: %w", csum, err)
	}
	f.countObject(repo.ObjectFile, fromCache)
	res.FetchedObjects++
	if opts.DryRun {
		return nil
	}
	existed := false
	// checksum: trusted_http_direct mirrors the archive payload verbatim
	// (only possible when mirroring into an archive repository); everything
	// else decodes and verifies the content checksum, like libostree.
	trustedDirect := opts.TrustedHTTP && opts.Repo.Mode().IsArchive()
	if trustedDirect {
		// bareuseronly-files: validate the mode even for a direct copy.
		if opts.BareUserOnlyFiles {
			if hdr, _, perr := object.ParseArchiveHeader(raw); perr == nil {
				if err := validateBareUserOnlyMode(hdr.Mode, csum); err != nil {
					return err
				}
			}
		}
		if err := opts.Repo.WriteRawObject(csum, repo.ObjectFile, raw); err != nil {
			return err
		}
		res.ContentBytesWritten += uint64(len(raw))
		return nil
	}
	hdr, rest, err := object.ParseArchiveHeader(raw)
	if err != nil {
		// Bare source: content is raw bytes.
		if err := opts.Repo.WriteFileRawObject(csum, raw, repo.FileObjectMeta{}); err != nil {
			return err
		}
		if !existed {
			res.ContentBytesWritten += uint64(len(raw))
		}
		return nil
	}
	var content []byte
	if hdr.IsRegular() {
		fr := flate.NewReader(bytes.NewReader(rest))
		content, err = io.ReadAll(fr)
		fr.Close()
		if err != nil {
			return err
		}
	}
	// bareuseronly-files: reject content that cannot be stored canonically.
	if opts.BareUserOnlyFiles {
		if err := validateBareUserOnlyMode(hdr.Mode, csum); err != nil {
			return err
		}
	}
	if !f.local {
		// checksum: verify the decoded content against the expected checksum.
		if err := verifyContentChecksum(hdr, content, csum); err != nil {
			return err
		}
	}
	if opts.Repo.Mode().IsArchive() {
		// The remote archive object can be stored verbatim.
		if err := opts.Repo.WriteRawObject(csum, repo.ObjectFile, raw); err != nil {
			return err
		}
	} else if err := writeContent(opts.Repo, csum, hdr, content); err != nil {
		return err
	}
	if !existed {
		if hdr.IsSymlink() {
			res.ContentBytesWritten += uint64(len(hdr.SymlinkTarget))
		} else {
			res.ContentBytesWritten += uint64(len(content))
		}
	}
	return nil
}

// fetchObject retrieves one object, consulting --localcache-repo and
// --cache-dir before the network.  The bool result reports whether the bytes
// came from a local cache repository.
func fetchObject(f *fetcher, csum string, typ repo.ObjectType) ([]byte, bool, error) {
	if data, ok, err := f.lookupLocalCache(csum, typ); err != nil {
		return nil, false, err
	} else if ok {
		return data, true, nil
	}
	if data, ok := f.readDiskCache(csum, typ); ok {
		return data, false, nil
	}

	base := path.Join("objects", csum[:2], csum[2:]+"."+typ.String())
	// Like libostree, only detached commit metadata comes from the metadata
	// mirror list; all other objects (including commits and dirtrees) are
	// fetched from contenturl when it is configured.
	get := f.getContent
	if typ == repo.ObjectCommitmeta {
		get = f.get
	}
	if typ == repo.ObjectFile || typ == repo.ObjectFileXattrs || typ == repo.ObjectFileXattrsLink {
		data, err := get(base + "z")
		if err == nil {
			f.writeDiskCache(csum, typ, data)
			return data, false, nil
		}
		// Only fall back to the uncompressed object when it really is absent;
		// propagating transient errors here keeps their HTTP status visible.
		if !errors.Is(err, fs404) {
			return nil, false, err
		}
	} else if typ == repo.ObjectCommitmeta {
		// commitmeta is never compressed
	}
	data, err := get(base)
	if err != nil {
		if errors.Is(err, fs404) {
			return nil, false, fmt.Errorf("%w (404)", repo.ErrObjectNotFound)
		}
		return nil, false, err
	}
	// limit: metadata objects over OSTREE_MAX_METADATA_SIZE are rejected the
	// same way the C fetcher refuses the download.
	if typ.IsMeta() {
		if err := checkMetadataSize(base, len(data)); err != nil {
			return nil, false, err
		}
	}
	f.writeDiskCache(csum, typ, data)
	return data, false, nil
}

// writeContent encodes a content object for the destination repository mode.
func writeContent(r *repo.Repo, csum string, hdr *object.FileHeader, content []byte) error {
	xattrs := map[string][]byte{}
	for _, x := range hdr.Xattrs {
		xattrs[x.Name] = x.Value
	}
	switch r.Mode() {
	case repo.ModeArchive:
		var payload []byte
		if hdr.IsRegular() {
			hdr.Size = uint64(len(content))
			header, err := hdr.EncodeArchive()
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			zw, err := flate.NewWriter(&buf, 6)
			if err != nil {
				return err
			}
			if _, err := zw.Write(content); err != nil {
				return err
			}
			if err := zw.Close(); err != nil {
				return err
			}
			payload = append(header, buf.Bytes()...)
		} else {
			header, err := hdr.EncodeArchive()
			if err != nil {
				return err
			}
			payload = header
		}
		return r.WriteFileRawObject(csum, payload, repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Xattrs: xattrs})
	case repo.ModeBare:
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode, Xattrs: xattrs}
		if hdr.IsSymlink() {
			return r.WriteSymlinkObject(csum, repo.ObjectFile, hdr.SymlinkTarget, meta)
		}
		return r.WriteFileRawObject(csum, content, meta)
	case repo.ModeBareUser, repo.ModeBareUserOnly:
		meta := repo.FileObjectMeta{UID: hdr.UID, GID: hdr.GID, Mode: hdr.Mode}
		if r.Mode() == repo.ModeBareUser {
			ostreemeta, err := object.EncodeFileMeta(hdr.UID, hdr.GID, hdr.Mode, hdr.Xattrs)
			if err != nil {
				return err
			}
			meta.Ostreemeta = ostreemeta
		}
		if hdr.IsSymlink() {
			if r.Mode() == repo.ModeBareUserOnly {
				return r.WriteSymlinkObject(csum, repo.ObjectFile, hdr.SymlinkTarget, meta)
			}
			return r.WriteFileRawObject(csum, append([]byte(hdr.SymlinkTarget), 0), meta)
		}
		return r.WriteFileRawObject(csum, content, meta)
	default:
		return fmt.Errorf("Unsupported repository mode")
	}
}

var _ = tree.LoadFile

// listLocalRefs enumerates refs/heads in a local repository.
func listLocalRefs(base string) (map[string]string, error) {
	root := filepath.Join(base, "refs", "heads")
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = strings.TrimSpace(string(data))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
