package cmd

import (
	"fmt"
	"sort"
	"strings"

	"ostree-go/internal/cli"
	"ostree-go/internal/pull"
	"ostree-go/internal/repo"
)

// findRemotesOptions mirrors ot-builtin-find-remotes.c, in the same order.
func findRemotesOptions() []cli.Option {
	return []cli.Option{
		{LongName: "cache-dir", ArgType: cli.ArgFilename, ArgDescription: "DIR", Description: "Use custom cache dir"},
		{LongName: "disable-fsync", ArgType: cli.ArgNone, Description: "Do not invoke fsync()"},
		{LongName: "finders", ArgType: cli.ArgString, ArgDescription: "FINDERS", Description: "Use the specified comma separated list of finders (e.g. config,lan,mount)"},
		{LongName: "pull", ArgType: cli.ArgNone, Description: "Pull the updates after finding them"},
		{LongName: "mirror", ArgType: cli.ArgNone, Description: "Do a mirror pull (see ostree pull --mirror)"},
	}
}

// collectionRefRequest is one COLLECTION-ID REF pair from the command line.
type collectionRefRequest struct {
	collection string
	ref        string
}

// findRemotesResult is one remote that can serve (some of) the requested refs.
type findRemotesResult struct {
	remote     string
	url        string
	collection string
	refs       []pull.CollectionRef
	// found maps a requested ref to its checksum.
	found map[string]string
}

func runFindRemotes(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	requests, err := parseFindRemotesArgs(ctx.Args)
	if err != nil {
		return err
	}
	if ctx.Options.Has("mirror") && !ctx.Options.Has("pull") {
		return fmt.Errorf("When --mirror is specified, --pull must also be")
	}
	if err := validateFinders(ctx.Options.String("finders")); err != nil {
		return err
	}

	results, err := findRemotesCandidates(r, requests)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		fmt.Fprintln(ctx.Stdout, "No results.")
		return nil
	}

	foundRefs := 0
	seen := map[collectionRefRequest]bool{}
	for i, result := range results {
		fmt.Fprintf(ctx.Stdout, "Result %d: %s\n", i, result.url)
		fmt.Fprintf(ctx.Stdout, " - Finder: OstreeRepoFinderConfig\n")
		fmt.Fprintf(ctx.Stdout, " - Keyring: (none)\n")
		fmt.Fprintf(ctx.Stdout, " - Priority: 2\n")
		fmt.Fprintf(ctx.Stdout, " - Summary last modified: unknown\n")
		fmt.Fprintf(ctx.Stdout, " - Refs:\n")
		names := make([]string, 0, len(result.found))
		for name := range result.found {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(ctx.Stdout, "   (%s, %s) = %s\n", result.collection, name, result.found[name])
			key := collectionRefRequest{collection: result.collection, ref: name}
			if !seen[key] {
				seen[key] = true
				foundRefs++
			}
		}
	}
	fmt.Fprintf(ctx.Stdout, "%d/%d refs were found.\n", foundRefs, len(requests))
	if foundRefs != len(requests) {
		fmt.Fprintf(ctx.Stdout, "Refs not found in any remote:\n")
		for _, req := range requests {
			if !seen[collectionRefRequest{collection: req.collection, ref: req.ref}] {
				fmt.Fprintf(ctx.Stdout, " - (%s, %s)\n", req.collection, req.ref)
			}
		}
	}
	if !ctx.Options.Has("pull") {
		return nil
	}

	if err := r.WritableError(); err != nil {
		return fmt.Errorf("Cannot write to repository: %v", err)
	}
	for _, result := range results {
		opts := pull.RemoteOptions{
			Repo:             r,
			URL:              result.url,
			Remote:           result.remote,
			RemoteCollection: result.collection,
			CollectionRefs:   result.refs,
			Mirror:           ctx.Options.Has("mirror"),
		}
		if _, err := pull.Remote(opts); err != nil {
			return err
		}
	}
	fmt.Fprintf(ctx.Stdout, "Pulled %d/%d refs successfully.\n", len(requests), len(requests))
	return nil
}

// parseFindRemotesArgs validates the COLLECTION-ID REF pairs.
func parseFindRemotesArgs(args []string) ([]collectionRefRequest, error) {
	if len(args) < 2 {
		return nil, fmt.Errorf("At least one COLLECTION-ID REF pair must be specified")
	}
	if len(args)%2 != 0 {
		return nil, fmt.Errorf("Only complete COLLECTION-ID REF pairs may be specified")
	}
	var out []collectionRefRequest
	for i := 0; i < len(args); i += 2 {
		collection, ref := args[i], args[i+1]
		if err := repo.ValidateCollectionID(collection); err != nil {
			return nil, err
		}
		if err := repo.ValidateRefName(ref); err != nil {
			return nil, err
		}
		out = append(out, collectionRefRequest{collection: collection, ref: ref})
	}
	return out, nil
}

// validateFinders implements validate_finders_list().
func validateFinders(spec string) error {
	if spec == "" {
		return fmt.Errorf("List of finders in --finders option must not be empty")
	}
	valid := map[string]bool{"config": false, "lan": false, "mount": false}
	for _, name := range strings.Split(spec, ",") {
		used, ok := valid[name]
		if !ok || used {
			return fmt.Errorf("Unknown or duplicate finder type given in --finders option: \u2018%s\u2019", name)
		}
		valid[name] = true
		if name != "config" {
			// The lan (Avahi) and mount finders are not implemented.
			return fmt.Errorf("Finder \u2018%s\u2019 is not supported", name)
		}
	}
	return nil
}

// findRemotesCandidates implements the "config" repository finder: a remote
// matches a ref when its configured collection-id matches and its summary
// advertises the ref.
func findRemotesCandidates(r *repo.Repo, requests []collectionRefRequest) ([]findRemotesResult, error) {
	wanted := map[string][]string{}
	for _, req := range requests {
		wanted[req.collection] = append(wanted[req.collection], req.ref)
	}
	var results []findRemotesResult
	seen := map[string]bool{}
	for current := r; current != nil; current = current.Parent() {
		cfg := current.Config()
		for _, name := range cfg.RemoteNames() {
			if seen[name] {
				continue
			}
			seen[name] = true
			collection, err := cfg.GetRemote(name, "collection-id")
			if err != nil || collection == "" {
				continue
			}
			refs, ok := wanted[collection]
			if !ok {
				continue
			}
			url, err := cfg.GetRemote(name, "url")
			if err != nil || url == "" {
				continue
			}
			summary, err := pull.FetchSummaryRaw(url)
			if err != nil {
				continue
			}
			result := findRemotesResult{
				remote:     name,
				url:        url,
				collection: collection,
				found:      map[string]string{},
			}
			for _, ref := range refs {
				csum, ok := pull.LookupSummaryCollectionRef(summary, collection, ref)
				if !ok {
					continue
				}
				result.found[ref] = csum
				result.refs = append(result.refs, pull.CollectionRef{
					CollectionID: collection,
					RefName:      ref,
					Checksum:     csum,
				})
			}
			if len(result.refs) > 0 {
				results = append(results, result)
			}
		}
	}
	return results, nil
}
