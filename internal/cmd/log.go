package cmd

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
)

func logOptions() []cli.Option {
	return []cli.Option{
		{LongName: "raw", ArgType: cli.ArgNone, Description: "Show raw variant data"},
	}
}

func runLog(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("A rev argument is required")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	checksum, err := resolveRevision(r, ctx.Args[0])
	if err != nil {
		return err
	}
	raw := ctx.Options.Has("raw")

	seen := map[string]bool{}
	for {
		if seen[checksum] {
			return nil
		}
		seen[checksum] = true

		data, commit, err := readCommitObject(r, checksum)
		if err != nil {
			if len(seen) > 1 && errors.Is(err, repo.ErrObjectNotFound) {
				// `ostree log` walks off the end of the fetched history
				// gracefully instead of failing the command.
				fmt.Fprintln(ctx.Stdout, "<< History beyond this commit not fetched >>")
				return nil
			}
			return err
		}
		if raw {
			if err := dumpRawObject(ctx.Stdout, repo.ObjectCommit, checksum, data, false); err != nil {
				return err
			}
		} else {
			if err := dumpCommit(ctx.Stdout, checksum, commit); err != nil {
				return err
			}
		}
		if commit.Parent == "" {
			return nil
		}
		checksum = commit.Parent
	}
}

// resolveRevision resolves a revision the way ostree_repo_resolve_rev() does.
// Only the trailing "^" parent operator is supported, matching libostree;
// other git-style suffixes are rejected as invalid refspecs.
func resolveRevision(r *repo.Repo, rev string) (string, error) {
	base := rev
	parents := 0
	for strings.HasSuffix(base, "^") {
		parents++
		base = strings.TrimSuffix(base, "^")
	}
	if base == "" || strings.ContainsRune(base, '^') || strings.ContainsRune(base, '~') {
		return "", fmt.Errorf("Invalid refspec %s", rev)
	}

	sum, err := resolveRevisionBase(r, base)
	if err != nil {
		return "", err
	}
	for i := 0; i < parents; i++ {
		_, commit, err := readCommitObject(r, sum)
		if err != nil {
			return "", err
		}
		if commit.Parent == "" {
			return "", fmt.Errorf("Commit %s has no parent", sum)
		}
		sum = commit.Parent
	}
	return sum, nil
}

func resolveRevisionBase(r *repo.Repo, rev string) (string, error) {
	if isLowerHexChecksum(rev) {
		return rev, nil
	}
	// ostree_repo_resolve_rev() also accepts "remote:branch" and falls back
	// to refs/remotes/<name> when the local ref is missing.
	sum, ok, err := r.ResolveRef(rev, true, true)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("Refspec '%s' not found", rev)
	}
	return sum, nil
}

// isLowerHexChecksum mirrors ostree_validate_checksum_string(): exactly 64
// lowercase hexadecimal characters.
func isLowerHexChecksum(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// readCommitObject loads and parses a commit object. A missing object yields
// the same message as libostree.
func readCommitObject(r *repo.Repo, checksum string) ([]byte, *object.Commit, error) {
	data, err := r.ReadObject(checksum, repo.ObjectCommit)
	if err != nil {
		if errors.Is(err, repo.ErrObjectNotFound) {
			return nil, nil, fmt.Errorf("No such metadata object %s.commit", checksum)
		}
		return nil, nil, err
	}
	commit, err := object.ParseCommit(data)
	if err != nil {
		return nil, nil, err
	}
	return data, commit, nil
}

// dumpCommit implements dump_commit() from ot-dump.c.
func dumpCommit(w io.Writer, checksum string, c *object.Commit) error {
	if _, err := fmt.Fprintf(w, "commit %s\n", checksum); err != nil {
		return err
	}
	if c.Parent != "" {
		if _, err := fmt.Fprintf(w, "Parent:  %s\n", c.Parent); err != nil {
			return err
		}
	}
	content, err := c.ContentChecksum()
	if err != nil {
		content = "<invalid commit>"
	}
	if _, err := fmt.Fprintf(w, "ContentChecksum:  %s\n", content); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Date:  %s\n", formatTimestamp(c.Timestamp)); err != nil {
		return err
	}
	if version, ok := commitVersion(c); ok {
		if _, err := fmt.Fprintf(w, "Version: %s\n", version); err != nil {
			return err
		}
	}
	if c.Subject != "" {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if err := dumpIndentedLines(w, c.Subject); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(w, "(no subject)\n"); err != nil {
			return err
		}
	}
	if c.Body != "" {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if err := dumpIndentedLines(w, c.Body); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "\n")
	return err
}

func commitVersion(c *object.Commit) (string, bool) {
	v, ok := c.Metadata.Get(object.MetaVersion)
	if !ok {
		return "", false
	}
	av, ok := v.(gvariant.Variant)
	if !ok {
		return "", false
	}
	s, ok := av.Value.(string)
	return s, ok
}

func formatTimestamp(ts uint64) string {
	return time.Unix(int64(ts), 0).UTC().Format("2006-01-02 15:04:05 +0000")
}

// dumpIndentedLines is dump_indented_lines() from ot-dump.c: every line is
// prefixed with four spaces, and a final unterminated line gets a newline.
func dumpIndentedLines(w io.Writer, data string) error {
	const indent = "    "
	for {
		idx := strings.IndexByte(data, '\n')
		if idx < 0 {
			if data != "" {
				if _, err := fmt.Fprintf(w, "%s%s\n", indent, data); err != nil {
					return err
				}
			}
			return nil
		}
		if _, err := fmt.Fprintf(w, "%s%s", indent, data[:idx+1]); err != nil {
			return err
		}
		data = data[idx+1:]
	}
}

// dumpRawObject prints the `TYPE CHECKSUM` header followed by
// g_variant_print(value, TRUE) after the little-endian byteswap performed by
// ot_dump_variant().
func dumpRawObject(w io.Writer, typ repo.ObjectType, checksum string, data []byte, unswapped bool) error {
	t, v, err := decodeRawObject(typ, data)
	if err != nil {
		return err
	}
	formatted, err := gvFormat(t, v, true, !unswapped)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s %s\n%s\n", typ, checksum, formatted); err != nil {
		return err
	}
	return nil
}

// decodeRawObject decodes the GVariant payload for an object type. The file
// type is not a plain GVariant and is handled separately by callers.
func decodeRawObject(typ repo.ObjectType, data []byte) (*gvariant.Type, gvariant.Value, error) {
	var t *gvariant.Type
	switch typ {
	case repo.ObjectCommit:
		t = object.CommitType
	case repo.ObjectDirtree:
		t = object.DirtreeType
	case repo.ObjectDirmeta:
		t = object.DirmetaType
	case repo.ObjectCommitmeta:
		t = gvariant.MustParseType("a{sv}")
	default:
		return nil, nil, fmt.Errorf("Cannot dump object type %s", typ)
	}
	v, err := gvariant.Decode(t, data)
	if err != nil {
		return nil, nil, err
	}
	return t, v, nil
}

// gvFormat renders v like g_variant_print(v, annotate). When swap is set the
// numeric leaves are byte swapped first, mirroring g_variant_byteswap() on a
// little-endian host (which is what makes OSTree's big-endian fields print in
// host order).
func gvFormat(t *gvariant.Type, v gvariant.Value, annotate, swap bool) (string, error) {
	var b strings.Builder
	if err := gvAppend(&b, t, v, annotate, swap); err != nil {
		return "", err
	}
	return b.String(), nil
}

func gvAppend(b *strings.Builder, t *gvariant.Type, v gvariant.Value, annotate, swap bool) error {
	switch t.Kind {
	case gvariant.KindByte:
		val, ok := v.(uint8)
		if !ok {
			return gvTypeError(t, v)
		}
		if annotate {
			b.WriteString("byte ")
		}
		fmt.Fprintf(b, "0x%02x", val)
	case gvariant.KindBoolean:
		val, ok := v.(bool)
		if !ok {
			return gvTypeError(t, v)
		}
		if val {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case gvariant.KindInt16:
		val, ok := v.(int16)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = int16(bits.ReverseBytes16(uint16(val)))
		}
		if annotate {
			b.WriteString("int16 ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindUint16:
		val, ok := v.(uint16)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = bits.ReverseBytes16(val)
		}
		if annotate {
			b.WriteString("uint16 ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindInt32:
		val, ok := v.(int32)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = int32(bits.ReverseBytes32(uint32(val)))
		}
		// int32 is the default numeric type and is never annotated.
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindUint32:
		val, ok := v.(uint32)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = bits.ReverseBytes32(val)
		}
		if annotate {
			b.WriteString("uint32 ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindInt64:
		val, ok := v.(int64)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = int64(bits.ReverseBytes64(uint64(val)))
		}
		if annotate {
			b.WriteString("int64 ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindUint64:
		val, ok := v.(uint64)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = bits.ReverseBytes64(val)
		}
		if annotate {
			b.WriteString("uint64 ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindHandle:
		val, ok := v.(int32)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = int32(bits.ReverseBytes32(uint32(val)))
		}
		if annotate {
			b.WriteString("handle ")
		}
		fmt.Fprintf(b, "%d", val)
	case gvariant.KindDouble:
		val, ok := v.(float64)
		if !ok {
			return gvTypeError(t, v)
		}
		if swap {
			val = math.Float64frombits(bits.ReverseBytes64(math.Float64bits(val)))
		}
		b.WriteString(gvFormatDouble(val))
	case gvariant.KindString:
		val, ok := v.(string)
		if !ok {
			return gvTypeError(t, v)
		}
		gvQuoteString(b, val)
	case gvariant.KindObjectPath, gvariant.KindSignature:
		val, ok := v.(string)
		if !ok {
			return gvTypeError(t, v)
		}
		if annotate {
			if t.Kind == gvariant.KindObjectPath {
				b.WriteString("objectpath ")
			} else {
				b.WriteString("signature ")
			}
		}
		b.WriteString("'" + val + "'")
	case gvariant.KindVariant:
		av, ok := v.(gvariant.Variant)
		if !ok {
			return gvTypeError(t, v)
		}
		b.WriteByte('<')
		// Nested variants are always annotated.
		if err := gvAppend(b, av.Type, av.Value, true, swap); err != nil {
			return err
		}
		b.WriteByte('>')
	case gvariant.KindMaybe:
		return gvAppendMaybe(b, t, v, annotate, swap)
	case gvariant.KindArray:
		return gvAppendArray(b, t, v, annotate, swap)
	case gvariant.KindTuple:
		return gvAppendTuple(b, t, v, annotate, swap)
	case gvariant.KindDictEntry:
		return gvAppendDictEntry(b, t, v, annotate, swap)
	default:
		return fmt.Errorf("gvariant: cannot print kind %d", t.Kind)
	}
	return nil
}

func gvTypeError(t *gvariant.Type, v gvariant.Value) error {
	return fmt.Errorf("gvariant: value %T does not match type %s", v, t)
}

func gvAppendMaybe(b *strings.Builder, t *gvariant.Type, v gvariant.Value, annotate, swap bool) error {
	if annotate {
		fmt.Fprintf(b, "@%s ", t)
	}
	depth := 0
	for tt := t; tt.Kind == gvariant.KindMaybe; tt = tt.Elem {
		depth++
	}
	cur, ok := v.(gvariant.Maybe)
	if !ok {
		return gvTypeError(t, v)
	}
	for i := 0; i < depth; i++ {
		if cur.Nothing {
			for j := 0; j < i; j++ {
				b.WriteString("just ")
			}
			b.WriteString("nothing")
			return nil
		}
		if i == depth-1 {
			return gvAppend(b, t.Elem, cur.Value, false, swap)
		}
		next, ok := cur.Value.(gvariant.Maybe)
		if !ok {
			return gvTypeError(t, cur.Value)
		}
		cur = next
		t = t.Elem
	}
	return nil
}

func gvAppendArray(b *strings.Builder, t *gvariant.Type, v gvariant.Value, annotate, swap bool) error {
	if t.Elem.Kind == gvariant.KindByte {
		data, ok := v.([]byte)
		if !ok {
			return gvTypeError(t, v)
		}
		// A byte string is a NUL terminated array with exactly one NUL,
		// at the end.
		nul := -1
		for i, c := range data {
			if c == 0 {
				nul = i
				break
			}
		}
		if len(data) == 0 {
			if annotate {
				fmt.Fprintf(b, "@%s ", t)
			}
			b.WriteString("[]")
			return nil
		}
		if nul == len(data)-1 {
			gvQuoteByteString(b, data[:nul])
			return nil
		}
		b.WriteByte('[')
		for i, c := range data {
			if i > 0 {
				b.WriteString(", ")
			}
			if i == 0 && annotate {
				b.WriteString("byte ")
			}
			fmt.Fprintf(b, "0x%02x", c)
		}
		b.WriteByte(']')
		return nil
	}

	if t.Elem.Kind == gvariant.KindDictEntry {
		d, ok := v.(gvariant.Dict)
		if !ok {
			return gvTypeError(t, v)
		}
		if len(d) == 0 {
			if annotate {
				fmt.Fprintf(b, "@%s ", t)
			}
			b.WriteString("{}")
			return nil
		}
		b.WriteByte('{')
		for i, e := range d {
			if i > 0 {
				b.WriteString(", ")
			}
			if err := gvAppend(b, t.Elem.Key, e.Key, annotate, swap); err != nil {
				return err
			}
			b.WriteString(": ")
			if err := gvAppend(b, t.Elem.Value, e.Value, annotate, swap); err != nil {
				return err
			}
			// Only the first entry carries type annotations.
			annotate = false
		}
		b.WriteByte('}')
		return nil
	}

	vals, ok := v.([]gvariant.Value)
	if !ok {
		return gvTypeError(t, v)
	}
	if len(vals) == 0 {
		if annotate {
			fmt.Fprintf(b, "@%s ", t)
		}
		b.WriteString("[]")
		return nil
	}
	b.WriteByte('[')
	for i, e := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		if err := gvAppend(b, t.Elem, e, annotate, swap); err != nil {
			return err
		}
		annotate = false
	}
	b.WriteByte(']')
	return nil
}

func gvAppendTuple(b *strings.Builder, t *gvariant.Type, v gvariant.Value, annotate, swap bool) error {
	vals, ok := v.([]gvariant.Value)
	if !ok || len(vals) != len(t.Members) {
		return gvTypeError(t, v)
	}
	b.WriteByte('(')
	for i, e := range vals {
		if i > 0 {
			b.WriteString(", ")
		}
		if err := gvAppend(b, t.Members[i], e, annotate, swap); err != nil {
			return err
		}
	}
	b.WriteByte(')')
	return nil
}

func gvAppendDictEntry(b *strings.Builder, t *gvariant.Type, v gvariant.Value, annotate, swap bool) error {
	e, ok := v.(gvariant.DictEntry)
	if !ok {
		return gvTypeError(t, v)
	}
	b.WriteByte('{')
	if err := gvAppend(b, t.Key, e.Key, annotate, swap); err != nil {
		return err
	}
	b.WriteString(", ")
	if err := gvAppend(b, t.Value, e.Value, annotate, swap); err != nil {
		return err
	}
	b.WriteByte('}')
	return nil
}

// gvQuoteString mirrors the G_VARIANT_CLASS_STRING case of
// g_variant_print_string().
func gvQuoteString(b *strings.Builder, s string) {
	quote := utf8.RuneError
	if strings.ContainsRune(s, '\'') {
		quote = '"'
	} else {
		quote = '\''
	}
	b.WriteRune(quote)
	for _, r := range s {
		if r == quote || r == '\\' {
			b.WriteByte('\\')
		}
		if gvRuneIsPrint(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('\\')
		if r < 0x10000 {
			switch r {
			case '\a':
				b.WriteByte('a')
			case '\b':
				b.WriteByte('b')
			case '\f':
				b.WriteByte('f')
			case '\n':
				b.WriteByte('n')
			case '\r':
				b.WriteByte('r')
			case '\t':
				b.WriteByte('t')
			case '\v':
				b.WriteByte('v')
			default:
				fmt.Fprintf(b, "u%04x", r)
			}
		} else {
			fmt.Fprintf(b, "U%08x", r)
		}
	}
	b.WriteRune(quote)
}

// gvRuneIsPrint approximates g_unichar_isprint(): graphic characters plus
// spaces (including U+00A0, which Go's unicode.IsPrint rejects).
func gvRuneIsPrint(r rune) bool {
	return unicode.IsPrint(r) || unicode.Is(unicode.Zs, r)
}

// gvQuoteByteString renders a bytestring the way g_strescape()'d output of
// g_variant_print_string() does. data must not contain a NUL.
func gvQuoteByteString(b *strings.Builder, data []byte) {
	quote := byte('\'')
	if strings.ContainsRune(string(data), '\'') {
		quote = '"'
	}
	b.WriteByte('b')
	b.WriteByte(quote)
	for _, c := range data {
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\v':
			b.WriteString(`\v`)
		case '"':
			// g_strescape() always escapes double quotes, even inside a
			// single-quoted byte string.
			b.WriteString(`\"`)
		default:
			if c >= 0x20 && c <= 0x7e {
				b.WriteByte(c)
			} else {
				fmt.Fprintf(b, "\\%03o", c)
			}
		}
	}
	b.WriteByte(quote)
}

// gvFormatDouble mirrors g_ascii_dtostr() plus the ".0" fixup in
// g_variant_print_string().
func gvFormatDouble(v float64) string {
	if math.IsNaN(v) {
		return "nan"
	}
	if math.IsInf(v, 1) {
		return "inf"
	}
	if math.IsInf(v, -1) {
		return "-inf"
	}
	s := strconv.FormatFloat(v, 'g', 17, 64)
	if !strings.ContainsAny(s, ".eEnN") {
		s += ".0"
	}
	return s
}
