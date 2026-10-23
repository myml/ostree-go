package cmd

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"ostree-go/internal/cli"
	"ostree-go/internal/repo"
)

// runRefs implements ot-builtin-refs.c. Like the C implementation it runs one
// "do_ref" operation per PREFIX argument; only --create is limited to a single
// existing ref.
func runRefs(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if len(ctx.Args) > 0 {
		if ctx.Options.Has("create") && len(ctx.Args) > 1 {
			return fmt.Errorf("You must specify only 1 existing ref when creating a new ref")
		}
		for _, prefix := range ctx.Args {
			if err := doRef(ctx, r, prefix); err != nil {
				return err
			}
		}
		return nil
	}
	// Require a prefix when deleting to help avoid accidents.
	if ctx.Options.Has("delete") {
		return fmt.Errorf("At least one PREFIX is required when deleting refs")
	}
	if ctx.Options.Has("create") {
		return fmt.Errorf("You must specify a revision when creating a new ref")
	}
	return doRef(ctx, r, "")
}

// doRef performs a single listing, create or delete operation on refs matching
// prefix.
func doRef(ctx *cli.Context, r *repo.Repo, prefix string) error {
	if ctx.Options.Has("collections") && !ctx.Options.Has("create") && !ctx.Options.Has("delete") {
		return listCollectionRefs(ctx, r, prefix)
	}

	asAlias := ctx.Options.Has("alias")
	var aliases []repo.RefAlias
	if asAlias || ctx.Options.Has("delete") {
		a, err := r.ListRefAliases()
		if err != nil {
			return err
		}
		aliases = a
	}

	switch {
	case ctx.Options.Has("delete"):
		return deleteRefs(ctx, r, prefix, aliases)
	case ctx.Options.Has("create"):
		return createRef(ctx, r, prefix, aliases)
	case asAlias:
		return listAliases(ctx, aliases, prefix)
	default:
		return listRefs(ctx, r, prefix)
	}
}

// listRefs implements plain `ostree refs [PREFIX]` (which also covers --list
// and --revision).
func listRefs(ctx *cli.Context, r *repo.Repo, prefix string) error {
	refs, err := r.ListRefs(prefix)
	if err != nil {
		return err
	}
	// libostree's ostree_repo_list_refs() covers refs/heads and refs/remotes;
	// collection mirrors are only shown with --collections.
	cutPrefix := prefix != "" && !ctx.Options.Has("list")
	type refLine struct {
		name     string
		checksum string
	}
	lines := make([]refLine, 0, len(refs))
	for _, ref := range refs {
		if ref.Kind == repo.RefMirror {
			continue
		}
		name := ref.RefSpec()
		if cutPrefix {
			name = refDisplay(ref, prefix)
		}
		lines = append(lines, refLine{name: name, checksum: ref.Checksum})
	}
	// The C implementation sorts the hash table keys, i.e. the strings it
	// prints.
	sort.Slice(lines, func(i, j int) bool { return lines[i].name < lines[j].name })
	for _, line := range lines {
		if ctx.Options.Has("revision") {
			fmt.Fprintf(ctx.Stdout, "%s\t%s\n", line.name, line.checksum)
		} else {
			fmt.Fprintf(ctx.Stdout, "%s\n", line.name)
		}
	}
	return nil
}

// refDisplay implements libostree's cut_prefix behavior: listing with a PREFIX
// removes the ref path prefix. Note that the remote namespace ("origin:") is
// kept.
func refDisplay(ref repo.Ref, prefix string) string {
	refPrefix := prefix
	if idx := strings.IndexByte(prefix, ':'); idx >= 0 {
		refPrefix = prefix[idx+1:]
		// "<remote>:" and "<remote>:." select the whole remote namespace.
		if refPrefix == "." {
			refPrefix = ""
		}
	}
	if refPrefix == "" || ref.Name == refPrefix {
		return ref.RefSpec()
	}
	rest, ok := strings.CutPrefix(ref.Name, refPrefix+"/")
	if !ok {
		return ref.RefSpec()
	}
	if ref.Kind == repo.RefRemote {
		return ref.Remote + ":" + rest
	}
	return rest
}

// listCollectionRefs keeps the previous --collections listing behavior: all
// ref namespaces are listed, including collection mirrors.
func listCollectionRefs(ctx *cli.Context, r *repo.Repo, prefix string) error {
	refs, err := r.ListRefs("")
	if err != nil {
		return err
	}
	repoCollection, _ := r.Config().Get("core", "collection-id")
	type line struct {
		collection string
		name       string
		csum       string
	}
	var lines []line
	for _, ref := range refs {
		var coll, name string
		switch ref.Kind {
		case repo.RefLocal:
			coll, name = repoCollection, ref.Name
		case repo.RefMirror:
			coll, name = ref.Collection, ref.Name
		default:
			continue
		}
		if prefix != "" {
			if coll != prefix && !strings.HasPrefix(coll+":"+name, prefix) && !refPathMatches(name, prefix) {
				continue
			}
		}
		lines = append(lines, line{coll, name, ref.Checksum})
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].collection != lines[j].collection {
			return lines[i].collection < lines[j].collection
		}
		return lines[i].name < lines[j].name
	})
	for _, l := range lines {
		name := l.name
		if prefix != "" && !ctx.Options.Has("list") {
			name = strings.TrimPrefix(name, prefix+"/")
		}
		display := name
		if l.collection != "" && !ctx.Options.Has("list") {
			display = "(" + l.collection + ", " + name + ")"
		}
		if ctx.Options.Has("revision") {
			fmt.Fprintf(ctx.Stdout, "%s\t%s\n", display, l.csum)
		} else {
			fmt.Fprintf(ctx.Stdout, "%s\n", display)
		}
	}
	return nil
}

// listAliases implements `ostree refs -A [PREFIX]`: only alias refs (symlinks)
// are listed, as "alias -> target".
func listAliases(ctx *cli.Context, aliases []repo.RefAlias, prefix string) error {
	for _, alias := range aliases {
		if prefix != "" && !refPathMatches(alias.Name, prefix) {
			continue
		}
		fmt.Fprintf(ctx.Stdout, "%s -> %s\n", alias.Name, alias.Target)
	}
	return nil
}

// refPathMatches implements libostree's refspec prefix semantics: PREFIX names
// a ref path or a directory of refs, not a string prefix.
func refPathMatches(ref, prefix string) bool {
	if strings.HasSuffix(prefix, "/") {
		return strings.HasPrefix(ref, prefix)
	}
	return ref == prefix || strings.HasPrefix(ref, prefix+"/")
}

// createRef implements `refs PREFIX --create=NEWREF [-A] [--force]`.
func createRef(ctx *cli.Context, r *repo.Repo, prefix string, aliases []repo.RefAlias) error {
	newRef := ctx.Options.String("create")
	asAlias := ctx.Options.Has("alias")
	force := ctx.Options.Has("force")

	remote, refName, err := repo.ParseRefSpec(newRef)
	if err != nil {
		return fmt.Errorf("Invalid refspec %s", newRef)
	}

	// Resolve the name being created (not the source prefix) to detect an
	// existing ref or alias. A directory in the way is not a ref here; the
	// eventual write reports the file/directory conflict.
	_, exists, err := r.ResolveRef(newRef, true, false)
	if err != nil && !errors.Is(err, repo.ErrRefIsDirectory) {
		return err
	}

	// Replacing an alias is allowed without --force, like the C code.
	replacingAlias := asAlias && hasAlias(aliases, newRef)
	if !replacingAlias && !force && exists {
		return fmt.Errorf("--create specified but ref %s already exists", newRef)
	}

	if asAlias {
		if remote != "" {
			return fmt.Errorf("Cannot create alias to remote ref: %s", remote)
		}
		ok, err := refExists(r, prefix)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("Cannot create alias to non-existent ref: %s", prefix)
		}
		return r.WriteAliasRef(refName, prefix)
	}

	sum, _, err := r.ResolveRef(prefix, false, false)
	if err != nil {
		return err
	}
	if remote != "" {
		if ctx.Options.Has("collections") && repo.ValidateCollectionID(remote) == nil {
			return r.WriteMirrorRef(remote, refName, sum)
		}
		return r.WriteRemoteRef(remote, refName, sum)
	}
	return r.WriteRef(refName, sum)
}

func hasAlias(aliases []repo.RefAlias, name string) bool {
	for _, alias := range aliases {
		if alias.Name == name {
			return true
		}
	}
	return false
}

// refExists reports whether prefix names an existing local or remote ref (the
// refspec must match exactly).
func refExists(r *repo.Repo, prefix string) (bool, error) {
	refs, err := r.ListRefs("")
	if err != nil {
		return false, err
	}
	for _, ref := range refs {
		if ref.Kind == repo.RefMirror {
			continue
		}
		if ref.RefSpec() == prefix {
			return true, nil
		}
	}
	return false, nil
}

// deleteRefs implements `refs --delete PREFIX...` for one PREFIX. Refs that
// have aliases cannot be deleted, matching libostree.
func deleteRefs(ctx *cli.Context, r *repo.Repo, prefix string, aliases []repo.RefAlias) error {
	refs, err := r.ListRefs(prefix)
	if err != nil {
		return err
	}
	if ctx.Options.Has("collections") && !strings.Contains(prefix, ":") {
		// A bare collection ID deletes every ref in that collection.
		all, err := r.ListRefs("")
		if err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, ref := range refs {
			seen[ref.RelPath()] = true
		}
		repoCollection, _ := r.Config().Get("core", "collection-id")
		for _, ref := range all {
			match := false
			if ref.Kind == repo.RefMirror && ref.Collection == prefix {
				match = true
			}
			if ref.Kind == repo.RefLocal && repoCollection != "" && repoCollection == prefix {
				match = true
			}
			if match && !seen[ref.RelPath()] {
				refs = append(refs, ref)
			}
		}
	}
	unlock, err := r.Lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	for _, ref := range refs {
		if ref.Kind == repo.RefMirror && !ctx.Options.Has("collections") {
			continue
		}
		for _, alias := range aliases {
			if alias.Target == ref.Name {
				return fmt.Errorf("Ref '%s' has an active alias: '%s'", ref.Name, alias.Name)
			}
		}
		if err := r.DeleteRefObject(ref); err != nil {
			return err
		}
	}
	maybeUpdateSummary(r, "auto-update-summary")
	return nil
}
