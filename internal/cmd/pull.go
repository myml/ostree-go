package cmd

import (
	"fmt"
	"strings"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/pull"
	"github.com/myml/ostree-go/internal/repo"
)

func pullOptions() []cli.Option {
	return []cli.Option{
		{LongName: "depth", ArgType: cli.ArgInt, ArgDescription: "DEPTH", Description: "Only fetch this number of commits"},
		{LongName: "disable-static-deltas", ArgType: cli.ArgNone, Description: "Do not use static deltas"},
		{LongName: "require-static-deltas", ArgType: cli.ArgNone, Description: "Require static deltas"},
		{LongName: "dry-run", ArgType: cli.ArgNone, Description: "Only print information on what will be downloaded"},
		{LongName: "mirror", ArgType: cli.ArgNone, Description: "Write refs suitable for a mirror"},
		{LongName: "subpath", ArgType: cli.ArgFilenameArray, ArgDescription: "PATH", Description: "Only pull the provided subpath(s)"},
		{LongName: "commit-metadata-only", ArgType: cli.ArgNone, Description: "Fetch only the commit metadata"},
		{LongName: "cache-dir", ArgType: cli.ArgFilename, ArgDescription: "DIR", Description: "Use DIR as content cache"},
		{LongName: "url", ArgType: cli.ArgString, ArgDescription: "URL", Description: "Pull objects from this URL instead of the one from the remote config"},
		{LongName: "localcache-repo", ShortName: 'L', ArgType: cli.ArgFilenameArray, ArgDescription: "REPO", Description: "Add REPO as local cache source for objects during this pull"},
		{LongName: "bareuseronly-files", ArgType: cli.ArgNone, Description: "Reject regular files with mode outside of 0775 (world writable, suid, etc.)"},
		{LongName: "disable-verify-bindings", ArgType: cli.ArgNone, Description: "Do not verify commit bindings"},
		{LongName: "gpg-verify", ArgType: cli.ArgNone, Description: "GPG verify commits"},
		{LongName: "gpg-verify-summary", ArgType: cli.ArgNone, Description: "GPG verify summary"},
		{LongName: "untrusted", ArgType: cli.ArgNone, Description: "Verify checksums of local sources (always enabled for HTTP pulls)"},
		{LongName: "http-trusted", ArgType: cli.ArgNone, Description: "Do not verify checksums of HTTP sources (mostly useful when mirroring)"},
		{LongName: "http-header", ArgType: cli.ArgStringArray, ArgDescription: "NAME=VALUE", Description: "Add an HTTP header"},
		{LongName: "append-user-agent", ArgType: cli.ArgString, ArgDescription: "USER_AGENT", Description: "Append string to user agent"},
		{LongName: "network-retries", ArgType: cli.ArgInt, ArgDescription: "N", Description: "Specifies number of network retries"},
		{LongName: "per-object-fsync", ArgType: cli.ArgNone, Description: "Perform disk writes on every object"},
		{LongName: "fsync", ArgType: cli.ArgString, ArgDescription: "POLICY", Description: "Specify how to invoke fsync()"},
		{LongName: "disable-fsync", ArgType: cli.ArgNone, Description: "Do not invoke fsync()"},
		{LongName: "timestamp-check", ShortName: 'T', ArgType: cli.ArgNone, Description: "Verify commit timestamps are newer than current"},
		{LongName: "timestamp-check-from-rev", ArgType: cli.ArgString, ArgDescription: "REV", Description: "Verify timestamps are newer than provided commits"},
		{LongName: "disable-verification", ArgType: cli.ArgNone, Description: "Disable verification of commit metadata"},
	}
}

func runPull(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if ctx.Options.Has("dry-run") && !ctx.Options.Has("require-static-deltas") {
		return fmt.Errorf("--dry-run requires --require-static-deltas")
	}
	// Archive repositories already store compressed objects, so libostree
	// refuses to use static deltas there.
	if ctx.Options.Has("require-static-deltas") && r.Mode().IsArchive() {
		return fmt.Errorf("Can't use static deltas in an archive repo")
	}
	remoteName, refspec, err := splitRemoteRef(ctx.Args[0])
	if err != nil {
		return err
	}
	if remoteName != "" {
		section := repo.RemoteSection(remoteName)
		if v, cerr := r.Config().Get(section, "custom-backend"); cerr == nil && v != "" {
			return fmt.Errorf("remote '%s' uses custom backend '%s'", remoteName, v)
		}
		if v, cerr := r.Config().Get(section, "unconfigured-state"); cerr == nil && v != "" {
			return fmt.Errorf("%s", v)
		}
	}
	// Argument handling mirrors ostree_builtin_pull(): a "remote:branch"
	// first argument names a single ref (any further arguments are ignored),
	// otherwise the remaining arguments are BRANCH[@COMMIT] refspecs.
	refs := []string{}
	overrides := map[string]string{}
	if refspec != "" {
		refs = append(refs, refspec)
	} else {
		for _, arg := range ctx.Args[1:] {
			name := arg
			if i := strings.LastIndexByte(arg, '@'); i > 0 && i < len(arg)-1 {
				name = arg[:i]
				overrides[name] = arg[i+1:]
			}
			refs = append(refs, name)
		}
	}
	// --timestamp-check / --timestamp-check-from-rev must reject a downgrade
	// before the ref is updated, so resolve the target revisions up front.
	if ctx.Options.Has("timestamp-check") || ctx.Options.Has("timestamp-check-from-rev") {
		if err := checkPullTimestamps(ctx, r, remoteName, refs, overrides); err != nil {
			return err
		}
	}
	collection, _ := r.Config().Get(repo.RemoteSection(remoteName), "collection-id")
	depth := 0
	depthSet := false
	if ctx.Options.Has("depth") {
		depth = ctx.Options.Int("depth")
		depthSet = true
	}
	opts := pull.RemoteOptions{
		Repo:                  r,
		Remote:                remoteName,
		RemoteCollection:      collection,
		CommitMetadataOnly:    ctx.Options.Has("commit-metadata-only"),
		Refs:                  refs,
		Depth:                 depth,
		DepthSet:              depthSet,
		Mirror:                ctx.Options.Has("mirror"),
		DryRun:                ctx.Options.Has("dry-run"),
		Subpaths:              ctx.Options.Strings("subpath"),
		DisableVerifyBindings: ctx.Options.Has("disable-verify-bindings"),
		BareUserOnlyFiles:     ctx.Options.Has("bareuseronly-files"),
		TrustedHTTP:           ctx.Options.Has("http-trusted"),
		RefOverrides:          overrides,
		Quiet:                 false,
		Out:                   ctx.Stdout,
		Err:                   ctx.Stderr,
		RequireStaticDeltas:   ctx.Options.Has("require-static-deltas"),
		Verify: pull.VerifyOptions{
			GPGVerify:        ctx.Options.Has("gpg-verify"),
			GPGVerifySummary: ctx.Options.Has("gpg-verify-summary"),
			CacheDir:         ctx.Options.String("cache-dir"),
		},
	}
	applyPullTransportOptions(ctx, &opts)
	_, err = pull.Remote(opts)
	return err
}

// checkPullTimestamps implements pull's timestamp checking, which libostree
// performs while fetching: a downgrade fails without moving the ref.
func checkPullTimestamps(ctx *cli.Context, r *repo.Repo, remoteName string, refs []string, overrides map[string]string) error {
	url, err := remoteURL(r, remoteName)
	if err != nil {
		return err
	}
	var fromRev string
	var fromTS uint64
	if rev := ctx.Options.String("timestamp-check-from-rev"); rev != "" {
		resolved, ok, err := r.ResolveRef(rev, false, true)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("Refspec '%s' not found", rev)
		}
		_, commit, err := readCommitObject(r, resolved)
		if err != nil {
			return fmt.Errorf("Reading %s for timestamp-check-from-rev", rev)
		}
		fromRev, fromTS = resolved, commit.Timestamp
	}
	checkTimestamps := ctx.Options.Has("timestamp-check")
	for _, ref := range refs {
		if isLowerHexChecksum(ref) {
			continue
		}
		target := overrides[ref]
		if target == "" {
			if data, serr := pull.FetchSummaryRaw(url); serr == nil {
				if summary, perr := object.ParseSummary(data); perr == nil {
					for _, sr := range summary.Refs {
						if sr.Name == ref {
							target = sr.Commit
							break
						}
					}
				}
			}
		}
		if target == "" {
			// No summary entry: nothing to compare against.
			continue
		}
		newTS, err := revisionTimestamp(r, url, target)
		if err != nil {
			return err
		}
		if checkTimestamps {
			if orig, oerr := r.ReadRemoteRef(remoteName, ref); oerr == nil && orig != "" && orig != target {
				origTS, oerr := revisionTimestamp(r, url, orig)
				if oerr != nil {
					return oerr
				}
				// Equal timestamps are accepted for different revisions,
				// matching _ostree_compare_timestamps().
				if newTS < origTS {
					return fmt.Errorf("Upgrade target revision '%s' with timestamp '%d' is chronologically older than current revision '%s' with timestamp '%d'", target, newTS, orig, origTS)
				}
			}
		}
		if fromRev != "" && target != fromRev && newTS < fromTS {
			return fmt.Errorf("Upgrade target revision '%s' with timestamp '%d' is chronologically older than current revision '%s' with timestamp '%d'", target, newTS, fromRev, fromTS)
		}
	}
	return nil
}

// revisionTimestamp returns the commit timestamp of rev, reading it locally
// when possible and otherwise fetching the commit from url.
func revisionTimestamp(r *repo.Repo, url, rev string) (uint64, error) {
	if _, commit, err := readCommitObject(r, rev); err == nil {
		return commit.Timestamp, nil
	}
	return pull.FetchCommitTimestamp(url, rev)
}

// remoteURL returns the configured URL for a remote.
func remoteURL(r *repo.Repo, name string) (string, error) {
	section := repo.RemoteSection(name)
	if !r.Config().HasGroup(section) {
		return "", fmt.Errorf("No such remote: %s", name)
	}
	url, err := r.Config().Get(section, "url")
	if err != nil {
		return "", fmt.Errorf("No \"url\" option in remote")
	}
	return url, nil
}

// splitRemoteRef splits a pull refspec "REMOTE[:REF]" into its remote name and
// optional ref, matching ostree_parse_refspec().
func splitRemoteRef(arg string) (remote, ref string, err error) {
	if i := strings.IndexByte(arg, ':'); i >= 0 {
		remote, ref = arg[:i], arg[i+1:]
	} else {
		remote = arg
	}
	if remote == "" {
		return "", "", fmt.Errorf("Invalid refspec: %s", arg)
	}
	return remote, ref, nil
}

// applyPullTransportOptions copies the transport-related pull options into
// opts.  Kept separate so the command body stays close to the C builtin.
func applyPullTransportOptions(ctx *cli.Context, opts *pull.RemoteOptions) {
	opts.URLOverride = ctx.Options.String("url")
	opts.HTTPHeaders = ctx.Options.Strings("http-header")
	opts.AppendUserAgent = ctx.Options.String("append-user-agent")
	if ctx.Options.Has("network-retries") {
		opts.NetworkRetries = ctx.Options.Int("network-retries")
		opts.NetworkRetriesSet = true
	}
	opts.LocalCacheRepos = ctx.Options.Strings("localcache-repo")
	opts.CacheDir = ctx.Options.String("cache-dir")
}
