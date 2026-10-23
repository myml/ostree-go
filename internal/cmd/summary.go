package cmd

import (
	"crypto/sha256"
	"fmt"
	"math/bits"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"ostree-go/internal/checksum"
	"ostree-go/internal/cli"
	"ostree-go/internal/commit"
	"ostree-go/internal/gpg"
	"ostree-go/internal/gvariant"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
	"ostree-go/internal/sign"
	"ostree-go/internal/staticdelta"
)

// summaryOptions mirrors the GOption entries of ot-builtin-summary.c.
func summaryOptions() []cli.Option {
	return []cli.Option{
		{LongName: "update", ShortName: 'u', ArgType: cli.ArgNone, Description: "Update the summary"},
		{LongName: "view", ShortName: 'v', ArgType: cli.ArgNone, Description: "View the local summary file"},
		{LongName: "raw", ArgType: cli.ArgNone, Description: "View the raw bytes of the summary file"},
		{LongName: "list-metadata-keys", ArgType: cli.ArgNone, Description: "List the available metadata keys"},
		{LongName: "print-metadata-key", ArgType: cli.ArgString, ArgDescription: "KEY", Description: "Print string value of metadata key"},
		{LongName: "gpg-sign", ArgType: cli.ArgStringArray, ArgDescription: "KEY-ID", Description: "GPG Key ID to sign the summary with"},
		{LongName: "gpg-homedir", ArgType: cli.ArgFilename, ArgDescription: "HOMEDIR", Description: "GPG Homedir to use when looking for keyrings"},
		{LongName: "sign", ArgType: cli.ArgStringArray, ArgDescription: "KEY-ID", Description: "Key ID to sign the summary with"},
		{LongName: "sign-type", ArgType: cli.ArgString, ArgDescription: "NAME", Description: "Signature type to use (defaults to 'ed25519')"},
		{LongName: "add-metadata", ShortName: 'm', ArgType: cli.ArgStringArray, ArgDescription: "KEY=VALUE", Description: "Additional metadata field to add to the summary"},
	}
}

// runSummary implements the subset of ot-builtin-summary.c needed by the
// conformance tests: regenerating the summary with --update. Viewing and
// signing are not implemented yet.
func runSummary(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	switch {
	case ctx.Options.Has("update"):
		return updateSummary(ctx, r)
	case ctx.Options.Has("raw"):
		return viewSummaryRaw(ctx, r)
	case ctx.Options.Has("view"):
		return viewSummary(ctx, r)
	case ctx.Options.Has("list-metadata-keys"):
		return listSummaryKeys(ctx, r)
	case ctx.Options.String("print-metadata-key") != "":
		return printSummaryKey(ctx, r, ctx.Options.String("print-metadata-key"))
	default:
		return fmt.Errorf("No option specified; use -u to update summary")
	}
}

func readSummary(r *repo.Repo) (*object.Summary, error) {
	data, err := r.ReadSummaryFile()
	if err != nil {
		return nil, err
	}
	return object.ParseSummary(data)
}

func viewSummaryRaw(ctx *cli.Context, r *repo.Repo) error {
	data, err := r.ReadSummaryFile()
	if err != nil {
		return err
	}
	v, err := gvariant.Decode(object.SummaryType, data)
	if err != nil {
		return err
	}
	formatted, err := gvFormat(object.SummaryType, v, true, false)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout, formatted)
	return err
}

func viewSummary(ctx *cli.Context, r *repo.Repo) error {
	s, err := readSummary(r)
	if err != nil {
		return err
	}
	collection, _ := s.Metadata.Get(object.MetaSummaryCollectionID)
	repoColl, _ := variantString(collection)
	printed := map[string]bool{}
	printRef := func(coll string, ref object.SummaryRef) {
		name := ref.Name
		if coll != "" {
			name = "(" + coll + ", " + ref.Name + ")"
		}
		fmt.Fprintf(ctx.Stdout, "* %s\n", name)
		fmt.Fprintf(ctx.Stdout, "    Latest Commit (%d bytes):\n", ref.Size)
		fmt.Fprintf(ctx.Stdout, "      %s\n", ref.Commit)
		printed[coll+"\x00"+ref.Name] = true
		if v, ok := ref.Metadata.Get(object.MetaSummaryCommitTimestamp); ok {
			ts := metadataUint64(v)
			fmt.Fprintf(ctx.Stdout, "    Timestamp (ostree.commit.timestamp): %s\n", formatSummaryTime(ts))
		}
		if v, ok := ref.Metadata.Get(object.MetaSummaryCommitVersion); ok {
			version, _ := variantString(v)
			fmt.Fprintf(ctx.Stdout, "    Version (ostree.commit.version): %s\n", version)
		}
		fmt.Fprintln(ctx.Stdout)
	}
	for _, ref := range s.Refs {
		printRef(repoColl, ref)
	}
	if v, ok := s.Metadata.Get(object.MetaSummaryCollectionMap); ok {
		if variant, ok := v.(gvariant.Variant); ok {
			if cm, ok := variant.Value.(gvariant.Dict); ok {
				for _, collEntry := range cm {
					coll, _ := collEntry.Key.(string)
					inner, ok := collEntry.Value.(gvariant.Dict)
					if !ok {
						continue
					}
					for _, e := range inner {
						name, _ := e.Key.(string)
						if printed[coll+"\x00"+name] {
							continue
						}
						ref, ok := summaryRefFromInfo(name, e.Value)
						if !ok {
							continue
						}
						printRef(coll, ref)
					}
				}
			}
		}
	}
	for _, key := range dictKeys(s.Metadata) {
		v, _ := s.Metadata.Get(key)
		val := formatSummaryValue(v)
		switch key {
		case object.MetaSummaryMode:
			fmt.Fprintf(ctx.Stdout, "Repository Mode (ostree.summary.mode): %s\n", val)
		case object.MetaSummaryLastModified:
			fmt.Fprintf(ctx.Stdout, "Last-Modified (ostree.summary.last-modified): %s\n", formatSummaryTime(metadataUint64(v)))
		case object.MetaSummaryTombstoneCommits:
			yes := "No"
			if b, ok := variantBool(v); ok && b {
				yes = "Yes"
			}
			fmt.Fprintf(ctx.Stdout, "Has Tombstone Commits (ostree.summary.tombstone-commits): %s\n", yes)
		case object.MetaSummaryCollectionID:
			coll, _ := variantString(v)
			fmt.Fprintf(ctx.Stdout, "Collection ID (ostree.summary.collection-id): %s\n", coll)
		case object.MetaSummaryCollectionMap:
			// Already listed above as (collection, ref) entries.
		case object.MetaSummaryStaticDeltas:
			fmt.Fprintf(ctx.Stdout, "Static Deltas (ostree.static-deltas): %s\n", val)
		default:
			fmt.Fprintf(ctx.Stdout, "%s: %s\n", key, val)
		}
	}
	return nil
}

func listSummaryKeys(ctx *cli.Context, r *repo.Repo) error {
	s, err := readSummary(r)
	if err != nil {
		return err
	}
	for _, key := range dictKeys(s.Metadata) {
		fmt.Fprintln(ctx.Stdout, key)
	}
	return nil
}

func printSummaryKey(ctx *cli.Context, r *repo.Repo, key string) error {
	s, err := readSummary(r)
	if err != nil {
		return err
	}
	v, ok := s.Metadata.Get(key)
	if !ok {
		return fmt.Errorf("No such metadata key '%s'", key)
	}
	variant, ok := v.(gvariant.Variant)
	if !ok {
		return fmt.Errorf("No such metadata key '%s'", key)
	}
	formatted, err := gvFormat(variant.Type, variant.Value, true, hostIsBigEndian())
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(ctx.Stdout, formatted)
	return err
}

func variantString(v gvariant.Value) (string, bool) {
	if variant, ok := v.(gvariant.Variant); ok {
		s, ok := variant.Value.(string)
		return s, ok
	}
	s, ok := v.(string)
	return s, ok
}

func variantBool(v gvariant.Value) (bool, bool) {
	if variant, ok := v.(gvariant.Variant); ok {
		b, ok := variant.Value.(bool)
		return b, ok
	}
	return false, false
}

func metadataUint64(v gvariant.Value) uint64 {
	if variant, ok := v.(gvariant.Variant); ok {
		if n, ok := variant.Value.(uint64); ok {
			return bits.ReverseBytes64(n)
		}
	}
	return 0
}

func formatSummaryValue(v gvariant.Value) string {
	if variant, ok := v.(gvariant.Variant); ok {
		formatted, err := gvFormat(variant.Type, variant.Value, false, false)
		if err == nil {
			return formatted
		}
	}
	return fmt.Sprintf("%v", v)
}

// formatSummaryTime renders a timestamp like GLib's ISO8601 output.
func formatSummaryTime(ts uint64) string {
	t := time.Unix(int64(ts), 0)
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	return fmt.Sprintf("%s%s%02d", t.Format("2006-01-02T15:04:05"), sign, offset/3600)
}

// updateSummary regenerates <repo>/summary, mirroring
// ostree_repo_regenerate_metadata() without signing and without static delta
// index metadata.
func updateSummary(ctx *cli.Context, r *repo.Repo) error {
	if !r.Writable() {
		if err := r.WritableError(); err != nil {
			return fmt.Errorf("Cannot update summary: %w", err)
		}
		return fmt.Errorf("Cannot update summary: repository is not writable")
	}
	gpgKeyIDs := ctx.Options.Strings("gpg-sign")
	signKeys := ctx.Options.Strings("sign")
	for _, keyID := range gpgKeyIDs {
		if keyID == "" {
			return fmt.Errorf("Invalid key ID passed to --gpg-sign")
		}
	}
	added := map[string]gvariant.Variant{}
	for _, kv := range ctx.Options.Strings("add-metadata") {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return fmt.Errorf("Invalid key/value pair %q", kv)
		}
		typ, val, err := parseGVariantText(kv[i+1:])
		if err != nil {
			return err
		}
		added[kv[:i]] = gvariant.Variant{Type: typ, Value: val}
	}
	s, err := buildSummary(r)
	if err != nil {
		return err
	}
	for k, v := range added {
		s.Metadata.Set(k, v)
	}
	data, err := s.Encode()
	if err != nil {
		return err
	}
	if len(gpgKeyIDs) > 0 || len(signKeys) > 0 {
		// Regeneration always starts from an empty signature set: the
		// signature is created alongside the new summary in a staging
		// directory, exactly like ostree_repo_regenerate_metadata().
		sigs := sign.NewSummarySignatures(nil)
		homedir := ctx.Options.String("gpg-homedir")
		for _, keyID := range gpgKeyIDs {
			sig, err := gpg.SignData(data, keyID, homedir)
			if err != nil {
				return err
			}
			sigs.Add(sign.GpgSignaturesKey, sig)
		}
		if len(signKeys) > 0 {
			name := ctx.Options.String("sign-type")
			if name == "" {
				name = sign.NameEd25519
			}
			engine, err := sign.GetByName(name)
			if err != nil {
				return err
			}
			for _, key := range signKeys {
				if err := engine.SetSK(key); err != nil {
					return err
				}
				sig, err := engine.Sign(data)
				if err != nil {
					return err
				}
				sigs.Add(engine.MetadataKey(), sig)
			}
		}
		sigData, err := sigs.Encode()
		if err != nil {
			return err
		}
		if err := r.WriteSummaryAndSignature(data, sigData); err != nil {
			return err
		}
	} else if err := r.WriteSummaryFile(data); err != nil {
		return err
	}
	if collection, cerr := r.Config().Get("core", "collection-id"); cerr == nil && collection != "" {
		if err := writeMetadataCommit(r, added); err != nil {
			return err
		}
	}
	return nil
}

// writeMetadataCommit maintains the ostree-metadata branch in collection
// repositories, mirroring ostree_repo_regenerate_metadata().
func writeMetadataCommit(r *repo.Repo, meta map[string]gvariant.Variant) error {
	dir, err := os.MkdirTemp("", "ostree-metadata-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := commitMetadataBranch(r, dir, meta); err != nil {
		return err
	}
	return nil
}

func commitMetadataBranch(r *repo.Repo, dir string, meta map[string]gvariant.Variant) error {
	opts := commit.Options{
		Repo:        r,
		TreeDir:     dir,
		Branch:      "ostree-metadata",
		Subject:     "",
		BindRefs:    []string{"ostree-metadata"},
		RawMetadata: map[string]gvariant.Variant{},
	}
	for k, v := range meta {
		opts.RawMetadata[k] = v
	}
	_, err := commit.Create(opts)
	return err
}

// parseGVariantText parses a bare GVariant text value as used by summary -m.
func parseGVariantText(raw string) (*gvariant.Type, interface{}, error) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "@") {
		fields := strings.SplitN(raw[1:], " ", 2)
		if len(fields) != 2 {
			return nil, nil, fmt.Errorf("Invalid GVariant text %q", raw)
		}
		typ := fields[0]
		val := strings.TrimSpace(fields[1])
		switch typ {
		case "t":
			n, err := strconv.ParseUint(val, 10, 64)
			return gvariant.MustParseType("t"), n, err
		case "a{sv}":
			if val == "{}" {
				return gvariant.MustParseType("a{sv}"), gvariant.Dict{}, nil
			}
		}
		return nil, nil, fmt.Errorf("Unsupported GVariant text %q", raw)
	}
	if len(raw) >= 2 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
		return gvariant.MustParseType("s"), raw[1 : len(raw)-1], nil
	}
	switch raw {
	case "true":
		return gvariant.MustParseType("b"), true, nil
	case "false":
		return gvariant.MustParseType("b"), false, nil
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return gvariant.MustParseType("x"), n, nil
	}
	return nil, nil, fmt.Errorf("Invalid GVariant text %q", raw)
}

// regenerateSummary rebuilds and writes the summary file.
func regenerateSummary(r *repo.Repo) error {
	s, err := buildSummary(r)
	if err != nil {
		return err
	}
	data, err := s.Encode()
	if err != nil {
		return err
	}
	return r.WriteSummaryFile(data)
}

// maybeUpdateSummary regenerates the summary when one of the given config
// keys is enabled.
func maybeUpdateSummary(r *repo.Repo, keys ...string) {
	if !r.Writable() {
		return
	}
	for _, key := range keys {
		on, err := r.Config().GetBool("core", key, false)
		if err == nil && on {
			_ = regenerateSummary(r)
			return
		}
	}
}

// buildSummary collects the local refs (following aliases) and the standard
// additional metadata keys into an in-memory summary. Remote refs are omitted,
// matching summary_add_ref_entry().
func buildSummary(r *repo.Repo) (*object.Summary, error) {
	refs, err := r.ListRefs("")
	if err != nil {
		return nil, err
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].RefSpec() < refs[j].RefSpec() })

	uint64Type := gvariant.MustParseType("t")
	stringType := gvariant.MustParseType("s")
	boolType := gvariant.MustParseType("b")

	tombstones, err := r.Config().GetBool("core", "tombstone-commits", false)
	if err != nil {
		return nil, err
	}

	md := gvariant.Dict{}
	md.Set(object.MetaSummaryLastModified, gvariant.Variant{
		Type:  uint64Type,
		Value: bits.ReverseBytes64(uint64(time.Now().Unix())),
	})
	md.Set(object.MetaSummaryMode, gvariant.Variant{
		Type:  stringType,
		Value: r.Mode().ConfigString(),
	})
	md.Set(object.MetaSummaryTombstoneCommits, gvariant.Variant{
		Type:  boolType,
		Value: tombstones,
	})
	md.Set(object.MetaSummaryIndexedDeltas, gvariant.Variant{
		Type:  boolType,
		Value: true,
	})
	noDeltasInSummary, err := r.Config().GetBool("core", "no-deltas-in-summary", false)
	if err != nil {
		return nil, err
	}
	if !noDeltasInSummary {
		deltas, err := buildSummaryStaticDeltas(r)
		if err != nil {
			return nil, err
		}
		if deltas != nil {
			md.Set(object.MetaSummaryStaticDeltas, gvariant.Variant{
				Type:  gvariant.MustParseType("a{sv}"),
				Value: deltas,
			})
		}
	}

	collection, _ := r.Config().Get("core", "collection-id")
	s := &object.Summary{Metadata: md}
	collMap := gvariant.Dict{}
	refInfoType := gvariant.MustParseType("(taya{sv})")
	addCollectionRef := func(coll string, sr object.SummaryRef) error {
		commit, err := checksum.FromHex(sr.Commit)
		if err != nil {
			return err
		}
		var inner gvariant.Dict
		if v, ok := collMap.Get(coll); ok {
			inner, _ = v.(gvariant.Dict)
		}
		inner.Set(sr.Name, gvariant.Variant{
			Type:  refInfoType,
			Value: []gvariant.Value{sr.Size, commit[:], sr.Metadata},
		})
		collMap.Set(coll, inner)
		return nil
	}
	for _, ref := range refs {
		if ref.Kind != repo.RefLocal && ref.Kind != repo.RefMirror {
			continue
		}
		data, err := r.ReadObject(ref.Checksum, repo.ObjectCommit)
		if err != nil {
			return nil, fmt.Errorf("Reading commit %s: %w", ref.Checksum, err)
		}
		c, err := object.ParseCommit(data)
		if err != nil {
			return nil, err
		}
		refMeta := gvariant.Dict{}
		refMeta.Set(object.MetaSummaryCommitTimestamp, gvariant.Variant{
			Type:  uint64Type,
			Value: bits.ReverseBytes64(c.Timestamp),
		})
		if version, ok := c.GetMetadataString(object.MetaVersion); ok {
			refMeta.Set(object.MetaSummaryCommitVersion, gvariant.Variant{
				Type:  stringType,
				Value: version,
			})
		}
		sr := object.SummaryRef{
			Name:     ref.Name,
			Size:     uint64(len(data)),
			Commit:   ref.Checksum,
			Metadata: refMeta,
		}
		if ref.Kind == repo.RefLocal {
			s.Refs = append(s.Refs, sr)
			if collection != "" {
				if err := addCollectionRef(collection, sr); err != nil {
					return nil, err
				}
			}
		} else if ref.Collection != "" {
			if err := addCollectionRef(ref.Collection, sr); err != nil {
				return nil, err
			}
		}
	}
	if collection != "" {
		md.Set(object.MetaSummaryCollectionID, gvariant.Variant{Type: stringType, Value: collection})
	}
	if len(collMap) > 0 {
		md.Set(object.MetaSummaryCollectionMap, gvariant.Variant{
			Type:  gvariant.MustParseType("a{sa{sv}}"),
			Value: collMap,
		})
	}
	// Dict.Set may append past the original slice capacity.
	s.Metadata = md
	return s, nil
}

// dictKeys returns dictionary keys in their stored order.
func dictKeys(d gvariant.Dict) []string {
	out := make([]string, 0, len(d))
	for _, e := range d {
		if k, ok := e.Key.(string); ok {
			out = append(out, k)
		}
	}
	return out
}

// buildSummaryStaticDeltas builds the ostree.static-deltas metadata mapping
// each delta name to the SHA-256 digest of its superblock, matching
// _ostree_repo_static_delta_superblock_digest().
func buildSummaryStaticDeltas(r *repo.Repo) (gvariant.Dict, error) {
	names, err := staticdelta.List(r)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, nil
	}
	byteArray := gvariant.MustParseType("ay")
	deltas := gvariant.Dict{}
	for _, name := range names {
		sb, path, err := staticdelta.LoadSuperblock(r, name)
		if err != nil {
			return nil, err
		}
		_ = sb
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		deltas.Set(name, gvariant.Variant{Type: byteArray, Value: sum[:]})
	}
	return deltas, nil
}

// summaryRefFromInfo converts a collection-map ref value into a SummaryRef.
func summaryRefFromInfo(name string, v gvariant.Value) (object.SummaryRef, bool) {
	variant, ok := v.(gvariant.Variant)
	if !ok || variant.Type.String() != "(taya{sv})" {
		return object.SummaryRef{}, false
	}
	fields, ok := variant.Value.([]gvariant.Value)
	if !ok || len(fields) != 3 {
		return object.SummaryRef{}, false
	}
	size, _ := fields[0].(uint64)
	commit, ok := fields[1].([]byte)
	if !ok || len(commit) != 32 {
		return object.SummaryRef{}, false
	}
	meta, _ := fields[2].(gvariant.Dict)
	var sum [32]byte
	copy(sum[:], commit)
	return object.SummaryRef{Name: name, Size: size, Commit: checksum.Hex(sum), Metadata: meta}, true
}
