package cmd

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"ostree-go/internal/checksum"
	"ostree-go/internal/cli"
	"ostree-go/internal/gvariant"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
)

func showOptions() []cli.Option {
	return []cli.Option{
		{LongName: "print-related", ArgType: cli.ArgNone, Description: `Show the "related" commits`},
		{LongName: "print-variant-type", ArgType: cli.ArgString, ArgDescription: "TYPE", Description: "Memory map OBJECT (in this case a filename) to the GVariant type string"},
		{LongName: "list-metadata-keys", ArgType: cli.ArgNone, Description: "List the available metadata keys"},
		{LongName: "print-metadata-key", ArgType: cli.ArgString, ArgDescription: "KEY", Description: "Print string value of metadata key"},
		{LongName: "print-hex", ArgType: cli.ArgNone, Description: "For byte array valued keys, output an unquoted hexadecimal string"},
		{LongName: "list-detached-metadata-keys", ArgType: cli.ArgNone, Description: "List the available detached metadata keys"},
		{LongName: "print-detached-metadata-key", ArgType: cli.ArgString, ArgDescription: "KEY", Description: "Print string value of detached metadata key"},
		{LongName: "print-sizes", ArgType: cli.ArgNone, Description: "Show the commit size metadata"},
		{LongName: "raw", ArgType: cli.ArgNone, Description: "Show raw variant data"},
		{LongName: "no-byteswap", ShortName: 'B', ArgType: cli.ArgNone, Description: "Do not automatically convert variant data from big endian"},
		{LongName: "gpg-homedir", ArgType: cli.ArgFilename, ArgDescription: "HOMEDIR", Description: "GPG Homedir to use when looking for keyrings"},
		{LongName: "gpg-verify-remote", ArgType: cli.ArgString, ArgDescription: "REMOTE", Description: "Use REMOTE name for GPG configuration"},
	}
}

func runShow(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("An object argument is required")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	rev := ctx.Args[0]
	opts := ctx.Options

	switch {
	case opts.Has("print-metadata-key") || opts.Has("print-detached-metadata-key"):
		detached := opts.Has("print-detached-metadata-key")
		key := opts.String("print-metadata-key")
		if detached {
			key = opts.String("print-detached-metadata-key")
		}
		resolved, err := resolveRevision(r, rev)
		if err != nil {
			return err
		}
		meta, err := loadMetadata(r, resolved, detached)
		if err != nil {
			return err
		}
		return showPrintMetadataKey(ctx.Stdout, meta, key, opts.Has("print-hex"), opts.Has("no-byteswap"))
	case opts.Has("list-metadata-keys") || opts.Has("list-detached-metadata-keys"):
		detached := opts.Has("list-detached-metadata-keys")
		resolved, err := resolveRevision(r, rev)
		if err != nil {
			return err
		}
		meta, err := loadMetadata(r, resolved, detached)
		if err != nil {
			return err
		}
		return showListMetadataKeys(ctx.Stdout, meta)
	case opts.Has("print-related"):
		resolved, err := resolveRevision(r, rev)
		if err != nil {
			return err
		}
		return showPrintRelated(ctx.Stdout, r, resolved)
	case opts.Has("print-variant-type"):
		return showPrintVariantFile(ctx.Stdout, rev, opts.String("print-variant-type"))
	case opts.Has("print-sizes"):
		resolved, err := resolveRevision(r, rev)
		if err != nil {
			return err
		}
		return showPrintSizes(ctx.Stdout, r, resolved)
	default:
		return showObject(ctx.Stdout, r, rev, opts.Has("raw"), opts.Has("no-byteswap"))
	}
}

// showObject implements the default `ostree show OBJECT` path.
func showObject(w io.Writer, r *repo.Repo, rev string, raw, unswapped bool) error {
	if csum, typ, ok := splitObjectSpec(rev); ok {
		return showTypedObject(w, r, typ, csum, raw, unswapped)
	}
	if !isLowerHexChecksum(rev) {
		resolved, err := resolveRevision(r, rev)
		if err != nil {
			return err
		}
		return showTypedObject(w, r, repo.ObjectCommit, resolved, raw, unswapped)
	}

	for _, typ := range []repo.ObjectType{repo.ObjectCommit, repo.ObjectDirmeta, repo.ObjectDirtree} {
		exists, err := r.HasObject(rev, typ)
		if err != nil {
			return err
		}
		if exists {
			return showTypedObject(w, r, typ, rev, raw, unswapped)
		}
	}
	exists, err := r.HasObject(rev, repo.ObjectFile)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("Couldn't find file object '%s'", rev)
	}
	return showFileInfo(w, r, rev)
}

// splitObjectSpec accepts the "<checksum>.<type>" spelling requested for the
// Go implementation. libostree's `show` only accepts bare checksums, so this
// is a strict superset of the C behavior.
func splitObjectSpec(rev string) (string, repo.ObjectType, bool) {
	dot := strings.LastIndexByte(rev, '.')
	if dot < 0 || dot == len(rev)-1 {
		return "", 0, false
	}
	csum := rev[:dot]
	if !isLowerHexChecksum(csum) {
		return "", 0, false
	}
	typ, err := repo.ParseObjectType(rev[dot+1:])
	if err != nil {
		return "", 0, false
	}
	return csum, typ, true
}

func showTypedObject(w io.Writer, r *repo.Repo, typ repo.ObjectType, csum string, raw, unswapped bool) error {
	switch typ {
	case repo.ObjectCommit:
		data, err := r.ReadObject(csum, repo.ObjectCommit)
		if err != nil {
			return commitObjectError(err, csum)
		}
		if raw {
			return dumpRawObject(w, repo.ObjectCommit, csum, data, unswapped)
		}
		commit, err := object.ParseCommit(data)
		if err != nil {
			return err
		}
		return dumpCommit(w, csum, commit)
	case repo.ObjectDirtree, repo.ObjectDirmeta:
		data, err := r.ReadObject(csum, typ)
		if err != nil {
			return objectError(err, typ, csum)
		}
		if raw {
			return dumpRawObject(w, typ, csum, data, unswapped)
		}
		_, err = fmt.Fprintf(w, "%s %s\n", typ, csum)
		return err
	case repo.ObjectFile:
		return showFileInfo(w, r, csum)
	default:
		data, err := r.ReadObject(csum, typ)
		if err != nil {
			return objectError(err, typ, csum)
		}
		return dumpRawObject(w, typ, csum, data, unswapped)
	}
}

func commitObjectError(err error, csum string) error {
	if errors.Is(err, repo.ErrObjectNotFound) {
		return fmt.Errorf("No such metadata object %s.commit", csum)
	}
	return err
}

func objectError(err error, typ repo.ObjectType, csum string) error {
	if errors.Is(err, repo.ErrObjectNotFound) {
		return fmt.Errorf("No such metadata object %s.%s", csum, typ)
	}
	return err
}

// showFileInfo implements the file fallback of `ostree show`.
func showFileInfo(w io.Writer, r *repo.Repo, csum string) error {
	fm, err := readFileMetadata(r, csum)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Object: %s\nType: %s\n", csum, repo.ObjectFile); err != nil {
		return err
	}
	switch fm.Mode & userVisibleModeMask {
	case 0100000:
		if _, err := fmt.Fprintf(w, "File Type: regular\nSize: %d\n", fm.Size); err != nil {
			return err
		}
	case 0120000:
		if _, err := fmt.Fprintf(w, "File Type: symlink\nTarget: %s\n", fm.SymlinkTarget); err != nil {
			return err
		}
	default:
		if _, err := fmt.Fprintf(w, "File Type: unknown\n"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Mode: 0%04o\nUid: %d\nGid: %d\n", fm.Mode, fm.UID, fm.GID); err != nil {
		return err
	}
	formatted, err := formatXattrs(fm.Xattrs)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "Extended Attributes: { %s }\n", formatted)
	return err
}

func loadMetadata(r *repo.Repo, resolved string, detached bool) (gvariant.Dict, error) {
	if !detached {
		_, commit, err := readCommitObject(r, resolved)
		if err != nil {
			return nil, err
		}
		return commit.Metadata, nil
	}
	data, err := r.ReadObject(resolved, repo.ObjectCommitmeta)
	if err != nil {
		if errors.Is(err, repo.ErrObjectNotFound) {
			return nil, fmt.Errorf("No detached metadata for commit %s", resolved)
		}
		return nil, err
	}
	v, err := gvariant.Decode(gvariant.MustParseType("a{sv}"), data)
	if err != nil {
		return nil, err
	}
	meta, ok := v.(gvariant.Dict)
	if !ok {
		return nil, fmt.Errorf("No detached metadata for commit %s", resolved)
	}
	return meta, nil
}

func showPrintMetadataKey(w io.Writer, meta gvariant.Dict, key string, printHex, unswapped bool) error {
	v, ok := meta.Get(key)
	if !ok {
		return fmt.Errorf("No such metadata key '%s'", key)
	}
	av, ok := v.(gvariant.Variant)
	if !ok {
		return fmt.Errorf("No such metadata key '%s'", key)
	}
	if printHex {
		if data, ok := av.Value.([]byte); ok && av.Type.Kind == gvariant.KindArray && av.Type.Elem.Kind == gvariant.KindByte {
			_, err := fmt.Fprintf(w, "%s\n", hex.EncodeToString(data))
			return err
		}
	}
	// The default output is host-order; only swap when the host is big
	// endian, matching libostree's behavior for values written by this
	// host.
	swap := !unswapped && hostIsBigEndian()
	formatted, err := gvFormat(av.Type, av.Value, true, swap)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, formatted)
	return err
}

func showListMetadataKeys(w io.Writer, meta gvariant.Dict) error {
	keys := make([]string, 0, len(meta))
	for _, e := range meta {
		if key, ok := e.Key.(string); ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintln(w, key); err != nil {
			return err
		}
	}
	return nil
}

func showPrintRelated(w io.Writer, r *repo.Repo, resolved string) error {
	data, _, err := readCommitObject(r, resolved)
	if err != nil {
		return err
	}
	v, err := gvariant.Decode(object.CommitType, data)
	if err != nil {
		return err
	}
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) < 3 {
		return fmt.Errorf("commit: malformed tuple")
	}
	entries, ok := fields[2].([]gvariant.Value)
	if !ok {
		return fmt.Errorf("commit: malformed related")
	}
	for _, e := range entries {
		pair, ok := e.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return fmt.Errorf("commit: malformed related entry")
		}
		name, _ := pair[0].(string)
		sum, _ := pair[1].([]byte)
		var hexsum string
		if len(sum) == checksum.Size {
			var arr [checksum.Size]byte
			copy(arr[:], sum)
			hexsum = checksum.Hex(arr)
		}
		if _, err := fmt.Fprintf(w, "%s %s\n", name, hexsum); err != nil {
			return err
		}
	}
	return nil
}

func showPrintVariantFile(w io.Writer, filename, typeString string) error {
	t, err := gvariant.ParseType(typeString)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	v, err := gvariant.Decode(t, data)
	if err != nil {
		return err
	}
	formatted, err := gvFormat(t, v, true, true)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, formatted)
	return err
}

func showPrintSizes(w io.Writer, r *repo.Repo, resolved string) error {
	_, commit, err := readCommitObject(r, resolved)
	if err != nil {
		return fmt.Errorf("Failed to read commit: %w", err)
	}
	v, ok := commit.Metadata.Get(object.MetaSizes)
	if !ok {
		return fmt.Errorf("No metadata key ostree.sizes in commit")
	}
	av, ok := v.(gvariant.Variant)
	if !ok || av.Type.Kind != gvariant.KindArray || av.Type.Elem.Kind != gvariant.KindArray || av.Type.Elem.Elem.Kind != gvariant.KindByte {
		return fmt.Errorf("No metadata key ostree.sizes in commit")
	}
	entries, ok := av.Value.([]gvariant.Value)
	if !ok {
		return fmt.Errorf("No metadata key ostree.sizes in commit")
	}

	var newArchived, newUnpacked, archived, unpacked int64
	var newObjects, objects uint64
	for _, e := range entries {
		data, ok := e.([]byte)
		if !ok {
			return fmt.Errorf("Could not read ostree.sizes metadata entry")
		}
		entry, err := parseSizesEntry(data)
		if err != nil {
			return err
		}
		archived += int64(entry.archived)
		unpacked += int64(entry.unpacked)
		objects++
		exists, err := r.HasObject(entry.checksum, entry.objtype)
		if err != nil {
			return err
		}
		if !exists {
			newArchived += int64(entry.archived)
			newUnpacked += int64(entry.unpacked)
			newObjects++
		}
	}

	_, err = fmt.Fprintf(w,
		"Compressed size (needed/total): %s/%s\n"+
			"Unpacked size (needed/total): %s/%s\n"+
			"Number of objects (needed/total): %d/%d\n",
		formatSize(newArchived), formatSize(archived),
		formatSize(newUnpacked), formatSize(unpacked),
		newObjects, objects)
	return err
}

type sizesEntry struct {
	checksum string
	objtype  repo.ObjectType
	archived uint64
	unpacked uint64
}

func parseSizesEntry(data []byte) (*sizesEntry, error) {
	if len(data) < checksum.Size+2 {
		return nil, fmt.Errorf("Invalid ostree.sizes metadata entry of length %d", len(data))
	}
	var csum [checksum.Size]byte
	copy(csum[:], data[:checksum.Size])
	rest := data[checksum.Size:]

	archived, n, ok := readVaruint64(rest)
	if !ok {
		return nil, fmt.Errorf("Unexpected EOF reading ostree.sizes varint")
	}
	rest = rest[n:]
	unpacked, n, ok := readVaruint64(rest)
	if !ok {
		return nil, fmt.Errorf("Unexpected EOF reading ostree.sizes varint")
	}
	rest = rest[n:]

	objtype := repo.ObjectFile
	if len(rest) > 0 {
		objtype = repo.ObjectType(rest[0])
		if !objtype.Valid() {
			return nil, fmt.Errorf("Unexpected ostree.sizes object type %d", rest[0])
		}
	}
	return &sizesEntry{
		checksum: checksum.Hex(csum),
		objtype:  objtype,
		archived: archived,
		unpacked: unpacked,
	}, nil
}

func readVaruint64(data []byte) (uint64, int, bool) {
	var result uint64
	for i := 0; i < 10; i++ {
		if i >= len(data) {
			return 0, 0, false
		}
		b := data[i]
		result |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return result, i + 1, true
		}
	}
	return 0, 0, false
}

// formatSize mirrors g_format_size(): SI units, one decimal place and a
// non-breaking space between the number and the unit.
func formatSize(size int64) string {
	if size < 1000 {
		if size == 1 {
			return "1 byte"
		}
		return fmt.Sprintf("%d bytes", size)
	}
	units := []string{"kB", "MB", "GB", "TB", "PB", "EB"}
	unit := int64(1000)
	idx := 0
	for idx+1 < len(units) && size >= unit*1000 {
		unit *= 1000
		idx++
	}
	return fmt.Sprintf("%.1f\u00a0%s", float64(size)/float64(unit), units[idx])
}

func hostIsBigEndian() bool {
	var v uint16 = 1
	var b [2]byte
	b[0] = byte(v)
	return b[0] == 0
}
