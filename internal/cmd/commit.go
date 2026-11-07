package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/myml/ostree-go/internal/checkout"
	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/commit"
	"github.com/myml/ostree-go/internal/dateparse"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/importtar"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/sign"
	"github.com/myml/ostree-go/internal/tree"
)

func commitOptions() []cli.Option {
	return []cli.Option{
		{LongName: "parent", ArgType: cli.ArgString, ArgDescription: "COMMIT", Description: `Parent commit checksum, or "none"`},
		{LongName: "subject", ShortName: 's', ArgType: cli.ArgString, ArgDescription: "SUBJECT", Description: "One line subject"},
		{LongName: "body", ShortName: 'm', ArgType: cli.ArgString, ArgDescription: "BODY", Description: "Full description"},
		{LongName: "body-file", ShortName: 'F', ArgType: cli.ArgFilename, ArgDescription: "FILE", Description: "Commit message from FILE path"},
		{LongName: "editor", ShortName: 'e', ArgType: cli.ArgNone, Description: "Use an editor to write the commit message"},
		{LongName: "branch", ShortName: 'b', ArgType: cli.ArgString, ArgDescription: "BRANCH", Description: "Branch"},
		{LongName: "orphan", ArgType: cli.ArgNone, Description: "Create a commit without writing a ref"},
		{LongName: "base", ArgType: cli.ArgString, ArgDescription: "REV", Description: "Start from the given commit as a base tree"},
		{LongName: "no-bindings", ArgType: cli.ArgNone, Description: "Do not write any ref bindings"},
		{LongName: "bind-ref", ArgType: cli.ArgStringArray, ArgDescription: "BRANCH", Description: "Add a ref to ref binding commit metadata"},
		{LongName: "tree", ArgType: cli.ArgStringArray, ArgDescription: "dir=PATH or tar=TARFILE or ref=COMMIT", Description: "Overlay the given argument as a tree"},
		{LongName: "add-metadata-string", ArgType: cli.ArgStringArray, ArgDescription: "KEY=VALUE", Description: "Add a key/value pair to metadata"},
		{LongName: "owner-uid", ArgType: cli.ArgInt, ArgDescription: "UID", Description: "Set file ownership user id"},
		{LongName: "owner-gid", ArgType: cli.ArgInt, ArgDescription: "GID", Description: "Set file ownership group id"},
		{LongName: "no-xattrs", ArgType: cli.ArgNone, Description: "Do not import extended attributes"},
		{LongName: "canonical-permissions", ArgType: cli.ArgNone, Description: "Canonicalize permissions in the same way bare-user does for hardlinked files"},
		{LongName: "mode-ro-executables", ArgType: cli.ArgNone, Description: "Ensure executable files are not writable"},
		{LongName: "bootable", ArgType: cli.ArgNone, Description: "Flag this commit as a bootable OSTree"},
		{LongName: "statoverride", ArgType: cli.ArgFilename, ArgDescription: "PATH", Description: "File containing list of modifications to make to permissions"},
		{LongName: "skip-list", ArgType: cli.ArgFilename, ArgDescription: "PATH", Description: "File containing list of files to skip"},
		{LongName: "link-checkout-speedup", ArgType: cli.ArgNone, Description: "Optimize for commits of trees composed of hardlinks into the repository"},
		{LongName: "devino-canonical", ShortName: 'I', ArgType: cli.ArgNone, Description: "Assume hardlinked objects are unmodified"},
		{LongName: "add-metadata", ArgType: cli.ArgStringArray, ArgDescription: "KEY=VALUE", Description: "Add a key/value pair to metadata in GVariant text format"},
		{LongName: "keep-metadata", ArgType: cli.ArgStringArray, ArgDescription: "KEY", Description: "Keep metadata KEY from parent"},
		{LongName: "add-detached-metadata-string", ArgType: cli.ArgStringArray, ArgDescription: "KEY=VALUE", Description: "Add a key/value pair to detached metadata"},
		{LongName: "gpg-sign", ArgType: cli.ArgStringArray, ArgDescription: "KEY-ID", Description: "GPG Key ID to sign the commit with"},
		{LongName: "gpg-homedir", ArgType: cli.ArgFilename, ArgDescription: "HOMEDIR", Description: "GPG Homedir to use when looking for keyrings"},
		{LongName: "sign", ArgType: cli.ArgStringArray, ArgDescription: "KEY_ID", Description: "Sign the commit with"},
		{LongName: "sign-from-file", ArgType: cli.ArgStringArray, ArgDescription: "FILE", Description: "Sign the commit with keys from FILE"},
		{LongName: "sign-type", ArgType: cli.ArgString, ArgDescription: "NAME", Description: "Signature type to use (defaults to 'ed25519')"},
		{LongName: "skip-if-unchanged", ArgType: cli.ArgNone, Description: "If the contents are unchanged from previous commit, do nothing"},
		{LongName: "timestamp", ArgType: cli.ArgString, ArgDescription: "TIMESTAMP", Description: "Override the timestamp of the commit"},
		{LongName: "table-output", ArgType: cli.ArgNone, Description: "Output more information in a KEY: VALUE format"},
		{LongName: "generate-sizes", ArgType: cli.ArgNone, Description: "Generate size information along with commit metadata"},
		{LongName: "consume", ArgType: cli.ArgNone, Description: "Consume (delete) content after commit (for local directories)"},
		{LongName: "fsync", ArgType: cli.ArgString, ArgDescription: "POLICY", Description: "Specify how to invoke fsync()"},
	}
}

func runCommit(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	opts := commit.Options{
		Repo:                 r,
		Branch:               ctx.Options.String("branch"),
		Parent:               ctx.Options.String("parent"),
		Subject:              ctx.Options.String("subject"),
		Body:                 ctx.Options.String("body"),
		NoXattrs:             ctx.Options.Has("no-xattrs"),
		Orphan:               ctx.Options.Has("orphan"),
		NoBindings:           ctx.Options.Has("no-bindings"),
		BindRefs:             ctx.Options.Strings("bind-ref"),
		Consume:              ctx.Options.Has("consume"),
		CanonicalPermissions: ctx.Options.Has("canonical-permissions"),
		ModeROExecutables:    ctx.Options.Has("mode-ro-executables"),
		Bootable:             ctx.Options.Has("bootable"),
		LinkCheckoutSpeedup:  ctx.Options.Has("link-checkout-speedup"),
		DevinoCanonical:      ctx.Options.Has("devino-canonical"),
		GenerateSizes:        ctx.Options.Has("generate-sizes"),
	}
	if uid := ctx.Options.String("owner-uid"); uid != "" {
		n, err := strconv.ParseUint(uid, 10, 32)
		if err != nil {
			return fmt.Errorf("Invalid uid %q", uid)
		}
		v := uint32(n)
		opts.OwnerUID = &v
	}
	if gid := ctx.Options.String("owner-gid"); gid != "" {
		n, err := strconv.ParseUint(gid, 10, 32)
		if err != nil {
			return fmt.Errorf("Invalid gid %q", gid)
		}
		v := uint32(n)
		opts.OwnerGID = &v
	}
	if ts := ctx.Options.String("timestamp"); ts != "" {
		n, err := parseTimestamp(ts)
		if err != nil {
			return err
		}
		opts.Timestamp = n
		opts.TimestampSet = true
	}
	meta := map[string]string{}
	for _, kv := range ctx.Options.Strings("add-metadata-string") {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			return fmt.Errorf("Invalid key/value pair %q", kv)
		}
		if i == 0 {
			return fmt.Errorf("Empty metadata key")
		}
		meta[kv[:i]] = kv[i+1:]
	}
	opts.Metadata = meta
	if len(ctx.Options.Strings("add-metadata")) > 0 {
		opts.RawMetadata = map[string]gvariant.Variant{}
		for _, kv := range ctx.Options.Strings("add-metadata") {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				return fmt.Errorf("Invalid key/value pair %q", kv)
			}
			key, value := kv[:i], kv[i+1:]
			typ, val, err := parseVariantText(value)
			if err != nil {
				return err
			}
			opts.RawMetadata[key] = gvariant.Variant{Type: typ, Value: val}
		}
	}
	opts.KeepMetadata = ctx.Options.Strings("keep-metadata")
	if len(ctx.Options.Strings("add-detached-metadata-string")) > 0 {
		opts.DetachedMetadata = map[string]string{}
		for _, kv := range ctx.Options.Strings("add-detached-metadata-string") {
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				return fmt.Errorf("Invalid key/value pair %q", kv)
			}
			opts.DetachedMetadata[kv[:i]] = kv[i+1:]
		}
	}
	if file := ctx.Options.String("skip-list"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				opts.SkipList = append(opts.SkipList, line)
			}
		}
	}
	if file := ctx.Options.String("statoverride"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				opts.StatOverride = append(opts.StatOverride, line)
			}
		}
	}
	if f := ctx.Options.String("body-file"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		opts.Body = string(data)
	}

	// --tree=dir=PATH overrides the positional path.
	type treeSpec struct {
		kind  string
		value string
	}
	var specs []treeSpec
	var tempDirs []string
	defer func() {
		for _, d := range tempDirs {
			os.RemoveAll(d)
		}
	}()
	if base := ctx.Options.String("base"); base != "" {
		specs = append(specs, treeSpec{kind: "ref", value: base})
	}
	for _, spec := range ctx.Options.Strings("tree") {
		switch {
		case strings.HasPrefix(spec, "dir="):
			specs = append(specs, treeSpec{kind: "dir", value: strings.TrimPrefix(spec, "dir=")})
		case strings.HasPrefix(spec, "tar="):
			file := strings.TrimPrefix(spec, "tar=")
			dir, err := os.MkdirTemp("", "ostree-tar-")
			if err != nil {
				return err
			}
			tempDirs = append(tempDirs, dir)
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			err = importtar.Extract(f, dir)
			f.Close()
			if err != nil {
				return err
			}
			specs = append(specs, treeSpec{kind: "dir", value: dir})
		case strings.HasPrefix(spec, "ref="):
			specs = append(specs, treeSpec{kind: "ref", value: strings.TrimPrefix(spec, "ref=")})
		default:
			return fmt.Errorf("Unsupported --tree argument %q", spec)
		}
	}

	for _, arg := range ctx.Args {
		specs = append(specs, treeSpec{kind: "dir", value: arg})
	}
	switch len(specs) {
	case 0:
		// The C CLI commits the current directory when no tree is given.
		opts.TreeDir = "."
	case 1:
		if specs[0].kind == "ref" {
			csum, err := tree.ResolveRef(r, specs[0].value)
			if err != nil {
				return err
			}
			if hasCommitModifiers(opts) {
				// Modifiers rewrite metadata, so materialize the ref tree
				// first.
				dir, err := os.MkdirTemp("", "ostree-tree-")
				if err != nil {
					return err
				}
				tempDirs = append(tempDirs, dir)
				if err := checkout.Checkout(checkout.Options{Repo: r, Commit: csum, Destination: dir}); err != nil {
					return err
				}
				opts.TreeDir = dir
			} else {
				c, err := tree.LoadCommit(r, csum)
				if err != nil {
					return err
				}
				opts.BaseTree, opts.BaseMeta = c.RootTree, c.RootMeta
			}
		} else {
			opts.TreeDir = specs[0].value
		}
	default:
		// Multiple trees are overlaid in the order given.
		dir, err := os.MkdirTemp("", "ostree-tree-")
		if err != nil {
			return err
		}
		tempDirs = append(tempDirs, dir)
		sawBase := false
		sawDir := false
		ownerOverride := opts.OwnerUID != nil || opts.OwnerGID != nil
		for _, spec := range specs {
			switch spec.kind {
			case "ref":
				if err := checkout.Checkout(checkout.Options{Repo: r, Commit: spec.value, Destination: dir, Union: true}); err != nil {
					return err
				}
				sawBase = true
			case "dir":
				sawDir = true
				// With --base, owner overrides apply only to the overlay
				// trees, not to the base tree's existing metadata.
				if sawBase && sawDir && ownerOverride {
					if err := copyDirIntoOwned(spec.value, dir, opts.OwnerUID, opts.OwnerGID); err != nil {
						return err
					}
				} else if err := copyDirInto(spec.value, dir); err != nil {
					return err
				}
			}
		}
		if sawBase && sawDir && ownerOverride {
			if opts.OwnerUID != nil || opts.OwnerGID != nil {
				var uid, gid int = -1, -1
				if opts.OwnerUID != nil {
					uid = int(*opts.OwnerUID)
				}
				if opts.OwnerGID != nil {
					gid = int(*opts.OwnerGID)
				}
				_ = os.Chown(dir, uid, gid)
			}
			opts.OwnerUID = nil
			opts.OwnerGID = nil
		}
		opts.TreeDir = dir
	}

	for _, keyID := range ctx.Options.Strings("gpg-sign") {
		if err := gpgValidateKeyID(keyID); err != nil {
			return fmt.Errorf("Invalid key ID passed to --gpg-sign")
		}
	}

	unlock, err := r.Lock(false)
	if err != nil {
		return err
	}
	defer unlock()

	var stats commit.Stats
	if ctx.Options.Has("table-output") {
		opts.Stats = &stats
	}
	sum, err := commit.Create(opts)
	if err != nil {
		return err
	}
	if err := applyCommitSignatures(ctx, r, sum); err != nil {
		return err
	}
	if ctx.Options.Has("table-output") {
		fmt.Fprintf(ctx.Stdout, "Commit: %s\n", sum)
		fmt.Fprintf(ctx.Stdout, "Metadata Total: %d\n", stats.MetadataTotal)
		fmt.Fprintf(ctx.Stdout, "Metadata Written: %d\n", stats.MetadataWritten)
		fmt.Fprintf(ctx.Stdout, "Content Total: %d\n", stats.ContentTotal)
		fmt.Fprintf(ctx.Stdout, "Content Written: %d\n", stats.ContentWritten)
		fmt.Fprintf(ctx.Stdout, "Content Cache Hits: %d\n", stats.DevinoCacheHits)
		fmt.Fprintf(ctx.Stdout, "Content Bytes Written: %d\n", stats.ContentBytesWritten)
		return nil
	}
	fmt.Fprintln(ctx.Stdout, sum)
	maybeUpdateSummary(r, "commit-update-summary", "auto-update-summary")
	return nil
}

func parseTimestamp(s string) (uint64, error) {
	if v, ok := strings.CutPrefix(s, "@"); ok {
		n, err := strconv.ParseUint(v, 10, 64)
		if err == nil {
			return n, nil
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err == nil {
		return n, nil
	}
	ts, err := dateparse.Parse(s)
	if err != nil {
		return 0, fmt.Errorf("Invalid timestamp %s", s)
	}
	// libostree casts the signed time_t to guint64, so pre-1970 dates such as
	// "November 05 1955" wrap instead of being rejected.
	return uint64(ts), nil
}

// copyDirInto recursively copies src into dst, replacing existing entries.
func copyDirIntoOwned(src, dst string, uid, gid *uint32) error {
	err := copyDirInto(src, dst)
	if err != nil {
		return err
	}
	u, g := -1, -1
	if uid != nil {
		u = int(*uid)
	}
	if gid != nil {
		g = int(*gid)
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		return os.Lchown(filepath.Join(dst, rel), u, g)
	})
}

func copyDirInto(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()|0700); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			os.RemoveAll(target)
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chmod(target, info.Mode().Perm())
		}
		return nil
	})
}

// parseVariantText parses the small GVariant text subset used by
// --add-metadata (e.g. "uint64 42", "string 'x'", "boolean true").
func parseVariantText(s string) (*gvariant.Type, interface{}, error) {
	fields := strings.SplitN(strings.TrimSpace(s), " ", 2)
	if len(fields) != 2 {
		return nil, nil, fmt.Errorf("Invalid GVariant text %q", s)
	}
	typ, raw := fields[0], strings.TrimSpace(fields[1])
	raw = strings.Trim(raw, "'")
	switch typ {
	case "uint64":
		n, err := strconv.ParseUint(raw, 10, 64)
		return gvariant.MustParseType("t"), n, err
	case "int64":
		n, err := strconv.ParseInt(raw, 10, 64)
		return gvariant.MustParseType("x"), n, err
	case "uint32":
		n, err := strconv.ParseUint(raw, 10, 32)
		return gvariant.MustParseType("u"), uint32(n), err
	case "int32":
		n, err := strconv.ParseInt(raw, 10, 32)
		return gvariant.MustParseType("i"), int32(n), err
	case "byte":
		n, err := strconv.ParseUint(raw, 10, 8)
		return gvariant.MustParseType("y"), uint8(n), err
	case "boolean":
		return gvariant.MustParseType("b"), raw == "true", nil
	case "string":
		return gvariant.MustParseType("s"), raw, nil
	default:
		return nil, nil, fmt.Errorf("Unsupported GVariant type %q", typ)
	}
}

// hasCommitModifiers reports whether options rewrite file/dir metadata.
func hasCommitModifiers(opts commit.Options) bool {
	return opts.OwnerUID != nil || opts.OwnerGID != nil || len(opts.StatOverride) > 0 ||
		opts.CanonicalPermissions || opts.ModeROExecutables || len(opts.SkipList) > 0
}

// applyCommitSignatures implements --gpg-sign, --sign and --sign-from-file
// for `ostree commit`.
func applyCommitSignatures(ctx *cli.Context, r *repo.Repo, commit string) error {
	gpgKeyIDs := ctx.Options.Strings("gpg-sign")
	signKeys := ctx.Options.Strings("sign")
	signFiles := ctx.Options.Strings("sign-from-file")
	if len(gpgKeyIDs) == 0 && len(signKeys) == 0 && len(signFiles) == 0 {
		return nil
	}
	if len(gpgKeyIDs) > 0 {
		homedir := ctx.Options.String("gpg-homedir")
		for _, keyID := range gpgKeyIDs {
			if err := gpgSignCommit(r, commit, keyID, homedir); err != nil {
				return err
			}
		}
	}
	if len(signKeys) > 0 || len(signFiles) > 0 {
		name := ctx.Options.String("sign-type")
		if name == "" {
			name = sign.NameEd25519
		}
		engine, err := sign.GetByName(name)
		if err != nil {
			return err
		}
		for _, keyID := range signKeys {
			if err := engine.SetSK(keyID); err != nil {
				return err
			}
			if err := sign.SignCommit(r, engine, commit); err != nil {
				return err
			}
		}
		for _, file := range signFiles {
			blobs, err := readSecretKeyFile(engine.Name(), file)
			if err != nil {
				return err
			}
			for _, blob := range blobs {
				if err := engine.SetSKBytes(blob); err != nil {
					return err
				}
				if err := sign.SignCommit(r, engine, commit); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// gpgValidateKeyID mirrors ot_validate_gpg_key_id().
func gpgValidateKeyID(keyID string) error {
	if keyID == "" {
		return fmt.Errorf("Invalid empty GPG key ID")
	}
	return nil
}
