// Package staticdelta implements OSTree static deltas: the on-disk
// superblock format, delta generation, and the inspection commands
// (list/show/delete).
//
// The binary format is defined by libostree in
// src/libostree/ostree-repo-static-delta-private.h.  Everything in this
// package is byte compatible with the C implementation:
//
//	superblock  (a{sv}tayay<commit>aya(uayttay)a(yaytt))
//	meta entry  (uayttay)
//	fallback    (yaytt)
//	part file   [compression byte][(a(uuu)aa(ayay)ayay) payload]
//	signed      (taya{sv}) with magic "OSTSGNDT"
//
// Note the historical quirk that the numeric fields of the meta entries
// are stored in whatever byte order was selected at generation time
// (declared in the superblock metadata as "ostree.endianness"), while
// the timestamp and the mode tables are always big endian.
package staticdelta

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"
	"strings"

	"ostree-go/internal/checksum"
	"ostree-go/internal/gvariant"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
)

// Compression type bytes used by the part files.
const (
	// CompressionNone is the "no compression" part payload.
	CompressionNone byte = 0
	// CompressionLZMA is xz/lzma ("x").
	CompressionLZMA byte = 'x'
)

// Opcodes of the part payload operation stream.
const (
	OpOpenSpliceAndClose byte = 'S'
	OpOpen               byte = 'o'
	OpWrite              byte = 'w'
	OpSetReadSource      byte = 'r'
	OpUnsetReadSource    byte = 'R'
	OpClose              byte = 'c'
	OpBspatch            byte = 'B'

	opCount = 7
)

// PartVersion is OSTREE_DELTAPART_VERSION.
const PartVersion uint32 = 0

// SignedMagic is OSTREE_STATIC_DELTA_SIGNED_MAGIC ("OSTSGNDT").
const SignedMagic uint64 = 0x4F535453474E4454

// MetaEndiannessKey is the superblock metadata key that records the byte
// order used for the numeric fields of the delta parts.
const MetaEndiannessKey = "ostree.endianness"

// DeltaIndexesDir is the repository relative directory holding the
// per-commit delta indexes (written by libostree).
const DeltaIndexesDir = "delta-indexes"

var (
	// SuperblockType is OSTREE_STATIC_DELTA_SUPERBLOCK_FORMAT.
	SuperblockType = gvariant.MustParseType(
		"(a{sv}tayay" + object.CommitType.String() + "aya(uayttay)a(yaytt))")
	// MetaEntryType is OSTREE_STATIC_DELTA_META_ENTRY_FORMAT.
	MetaEntryType = gvariant.MustParseType("(uayttay)")
	// FallbackType is OSTREE_STATIC_DELTA_FALLBACK_FORMAT.
	FallbackType = gvariant.MustParseType("(yaytt)")
	// PartFileType is the on-disk type of a delta part: (compression, payload).
	PartFileType = gvariant.MustParseType("(yay)")
	// PartPayloadType is OSTREE_STATIC_DELTA_PART_PAYLOAD_FORMAT_V0.
	PartPayloadType = gvariant.MustParseType("(a(uuu)aa(ayay)ayay)")
	// SignedType is OSTREE_STATIC_DELTA_SIGNED_FORMAT.
	SignedType = gvariant.MustParseType("(taya{sv})")

	modeEntryType  = gvariant.MustParseType("(uuu)")
	xattrEntryType = gvariant.MustParseType("a(ayay)")
)

// Endianness mirrors OstreeDeltaEndianness.
type Endianness int

// Endianness values.  EndianInvalid means the delta does not declare a
// byte order and the heuristic could not determine one; libostree then
// interprets the values in host byte order.
const (
	EndianBig Endianness = iota
	EndianLittle
	EndianInvalid
)

// String renders the value the way `ostree static-delta show` does.
func (e Endianness) String() string {
	switch e {
	case EndianBig:
		return "big"
	case EndianLittle:
		return "little"
	default:
		return "invalid"
	}
}

// Char returns the metadata byte ('B' or 'l') used for this byte order.
func (e Endianness) Char() byte {
	if e == EndianBig {
		return 'B'
	}
	return 'l'
}

// HostEndianness reports the byte order of the running machine.
func HostEndianness() Endianness {
	if binary.NativeEndian.Uint16([]byte{1, 0}) == 1 {
		return EndianLittle
	}
	return EndianBig
}

// ObjectRef is one entry of a part's object list: an object type byte
// followed by the 32 byte checksum.
type ObjectRef struct {
	Type repo.ObjectType
	Csum [32]byte
}

// Hex returns the checksum in OSTree's hex format.
func (o ObjectRef) Hex() string { return checksum.Hex(o.Csum) }

// PartMeta is one delta part header (OSTREE_STATIC_DELTA_META_ENTRY_FORMAT).
type PartMeta struct {
	Version  uint32
	Checksum [32]byte
	// Size and USize are stored in the byte order declared by the
	// superblock, exactly as read off disk.
	Size  uint64
	USize uint64
	// Objects lists the objects the part creates, in execution order.
	Objects []ObjectRef
}

// Fallback is one "fetch this object over HTTP" entry.
type Fallback struct {
	Type  repo.ObjectType
	Csum  [32]byte
	Size  uint64 // compressed size
	USize uint64
}

// Superblock is a parsed static delta superblock.
type Superblock struct {
	Metadata  gvariant.Dict
	Timestamp uint64
	From      [32]byte
	HasFrom   bool
	To        [32]byte
	// Commit is the parsed commit GVariant of the target commit.
	Commit gvariant.Value
	// Parents holds pairs of (from, to) checksums of prerequisite deltas.
	Parents   [][2][32]byte
	Parts     []PartMeta
	Fallbacks []Fallback
	// Signed records whether the superblock was wrapped in the signed
	// container, and Signatures holds the parsed signature dictionary.
	Signed     bool
	Signatures gvariant.Dict

	raw []byte
}

// Name returns the delta name used by `ostree static-delta list`:
// "<from>-<to>", or just "<to>" for a from-scratch delta.
func (sb *Superblock) Name() string {
	to := checksum.Hex(sb.To)
	if !sb.HasFrom {
		return to
	}
	return checksum.Hex(sb.From) + "-" + to
}

// Raw returns the raw superblock bytes as read from disk (without the
// signed wrapper).
func (sb *Superblock) Raw() []byte { return sb.raw }

// Encode serializes the superblock.  Numeric part fields are written
// exactly as held in the PartMeta values, so callers that want a swapped
// delta must pre-swap them (see Generate).
func (sb *Superblock) Encode() ([]byte, error) {
	meta := sb.Metadata
	if meta == nil {
		meta = gvariant.Dict{}
	}
	fromBytes := []byte{}
	if sb.HasFrom {
		fromBytes = sb.From[:]
	}
	toBytes := sb.To[:]
	parents := make([]byte, 0, len(sb.Parents)*64)
	for _, p := range sb.Parents {
		parents = append(parents, p[0][:]...)
		parents = append(parents, p[1][:]...)
	}

	parts := make([]gvariant.Value, 0, len(sb.Parts))
	for _, p := range sb.Parts {
		objects := make([]byte, 0, len(p.Objects)*33)
		for _, o := range p.Objects {
			objects = append(objects, byte(o.Type))
			objects = append(objects, o.Csum[:]...)
		}
		parts = append(parts, []gvariant.Value{
			p.Version, p.Checksum[:], p.Size, p.USize, objects,
		})
	}

	fallbacks := make([]gvariant.Value, 0, len(sb.Fallbacks))
	for _, f := range sb.Fallbacks {
		fallbacks = append(fallbacks, []gvariant.Value{
			uint8(f.Type), f.Csum[:], f.Size, f.USize,
		})
	}

	return gvariant.Encode(SuperblockType, []gvariant.Value{
		meta,
		bits.ReverseBytes64(sb.Timestamp), // stored big endian
		fromBytes,
		toBytes,
		sb.Commit,
		parents,
		parts,
		fallbacks,
	})
}

// ParseSuperblock parses a superblock, transparently unwrapping the
// signed container when present.  The returned Superblock keeps the raw
// bytes so callers can checksum them (summary/index digests).
func ParseSuperblock(data []byte) (*Superblock, error) {
	inner, signatures, signed, err := unwrapSigned(data)
	if err != nil {
		return nil, err
	}
	sb, err := parseUnsignedSuperblock(inner)
	if err != nil {
		return nil, err
	}
	sb.Signed = signed
	sb.Signatures = signatures
	sb.raw = inner
	return sb, nil
}

// unwrapSigned detects the "(taya{sv})" signed container.
func unwrapSigned(data []byte) (inner []byte, signatures gvariant.Dict, signed bool, err error) {
	v, derr := gvariant.Decode(SignedType, data)
	if derr != nil {
		return data, nil, false, nil
	}
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) != 3 {
		return data, nil, false, nil
	}
	magic, ok := fields[0].(uint64)
	if !ok || bits.ReverseBytes64(magic) != SignedMagic {
		return data, nil, false, nil
	}
	inner, ok = fields[1].([]byte)
	if !ok {
		return nil, nil, false, fmt.Errorf("static delta: malformed signed superblock")
	}
	sigs, ok := fields[2].(gvariant.Dict)
	if !ok {
		return nil, nil, false, fmt.Errorf("static delta: malformed signature dictionary")
	}
	return inner, sigs, true, nil
}

func parseUnsignedSuperblock(data []byte) (*Superblock, error) {
	v, err := gvariant.Decode(SuperblockType, data)
	if err != nil {
		return nil, fmt.Errorf("static delta: parsing superblock: %w", err)
	}
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) != 8 {
		return nil, fmt.Errorf("static delta: superblock has %d fields, expected 8", len(fields))
	}
	sb := &Superblock{}
	if dict, ok := fields[0].(gvariant.Dict); ok {
		sb.Metadata = dict
	} else {
		return nil, fmt.Errorf("static delta: superblock metadata is not a dictionary")
	}
	ts, ok := fields[1].(uint64)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed timestamp")
	}
	sb.Timestamp = bits.ReverseBytes64(ts)

	from, ok := fields[2].([]byte)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed from checksum")
	}
	switch len(from) {
	case 0:
	case 32:
		copy(sb.From[:], from)
		sb.HasFrom = true
	default:
		return nil, fmt.Errorf("static delta: from checksum has length %d", len(from))
	}
	to, ok := fields[3].([]byte)
	if !ok || len(to) != 32 {
		return nil, fmt.Errorf("static delta: malformed to checksum")
	}
	copy(sb.To[:], to)
	sb.Commit = fields[4]

	parents, ok := fields[5].([]byte)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed parent array")
	}
	if len(parents)%64 != 0 {
		return nil, fmt.Errorf("static delta: parent array length %d is not a multiple of 64", len(parents))
	}
	for i := 0; i+64 <= len(parents); i += 64 {
		var pair [2][32]byte
		copy(pair[0][:], parents[i:i+32])
		copy(pair[1][:], parents[i+32:i+64])
		sb.Parents = append(sb.Parents, pair)
	}

	partVals, ok := fields[6].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed part array")
	}
	for i, pv := range partVals {
		pm, err := parsePartMeta(pv)
		if err != nil {
			return nil, fmt.Errorf("static delta: part %d: %w", i, err)
		}
		sb.Parts = append(sb.Parts, pm)
	}

	fallbackVals, ok := fields[7].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed fallback array")
	}
	for i, fv := range fallbackVals {
		f, err := parseFallback(fv)
		if err != nil {
			return nil, fmt.Errorf("static delta: fallback %d: %w", i, err)
		}
		sb.Fallbacks = append(sb.Fallbacks, f)
	}
	return sb, nil
}

func parsePartMeta(v gvariant.Value) (PartMeta, error) {
	var pm PartMeta
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 5 {
		return pm, fmt.Errorf("malformed meta entry")
	}
	ver, ok := f[0].(uint32)
	if !ok {
		return pm, fmt.Errorf("malformed version")
	}
	pm.Version = ver
	csum, ok := f[1].([]byte)
	if !ok || len(csum) != 32 {
		return pm, fmt.Errorf("malformed part checksum")
	}
	copy(pm.Checksum[:], csum)
	size, ok := f[2].(uint64)
	if !ok {
		return pm, fmt.Errorf("malformed size")
	}
	usize, ok := f[3].(uint64)
	if !ok {
		return pm, fmt.Errorf("malformed usize")
	}
	pm.Size, pm.USize = size, usize
	objects, ok := f[4].([]byte)
	if !ok {
		return pm, fmt.Errorf("malformed object array")
	}
	if len(objects)%33 != 0 {
		return pm, fmt.Errorf("object array length %d is not a multiple of 33", len(objects))
	}
	for i := 0; i+33 <= len(objects); i += 33 {
		typ := repo.ObjectType(objects[i])
		if !typ.Valid() {
			return pm, fmt.Errorf("invalid object type %d", objects[i])
		}
		var ref ObjectRef
		ref.Type = typ
		copy(ref.Csum[:], objects[i+1:i+33])
		pm.Objects = append(pm.Objects, ref)
	}
	return pm, nil
}

func parseFallback(v gvariant.Value) (Fallback, error) {
	var f Fallback
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) != 4 {
		return f, fmt.Errorf("malformed fallback entry")
	}
	typ, ok := fields[0].(uint8)
	if !ok || !repo.ObjectType(typ).Valid() {
		return f, fmt.Errorf("invalid fallback object type")
	}
	f.Type = repo.ObjectType(typ)
	csum, ok := fields[1].([]byte)
	if !ok || len(csum) != 32 {
		return f, fmt.Errorf("malformed fallback checksum")
	}
	copy(f.Csum[:], csum)
	f.Size, _ = fields[2].(uint64)
	f.USize, _ = fields[3].(uint64)
	return f, nil
}

// GetEndianness implements _ostree_delta_get_endianness().  When the
// superblock does not declare a byte order, the same size based
// heuristic as libostree is used; the second return value reports
// whether that heuristic was used.
func (sb *Superblock) GetEndianness() (Endianness, bool) {
	if sb.Metadata != nil {
		if v, ok := sb.Metadata.Get(MetaEndiannessKey); ok {
			if variant, ok := v.(gvariant.Variant); ok {
				if c, ok := variant.Value.(uint8); ok {
					switch c {
					case 'l':
						return EndianLittle, false
					case 'B':
						return EndianBig, false
					default:
						return EndianInvalid, false
					}
				}
			}
		}
	}

	var totalSize uint64
	var totalObjects uint64
	byteswapped := false
	for _, p := range sb.Parts {
		totalObjects += uint64(len(p.Objects))
		totalSize += p.Size
		if p.Size > p.USize && p.USize > 0 {
			ratio := float64(p.Size) / float64(p.USize)
			if ratio > 1.2 {
				byteswapped = true
				break
			}
		}
	}
	if !byteswapped && totalObjects > 0 && totalSize/totalObjects > 0xFFFFFFFF {
		byteswapped = true
	}
	if !byteswapped {
		return EndianInvalid, true
	}
	if HostEndianness() == EndianLittle {
		return EndianBig, true
	}
	return EndianLittle, true
}

// NeedsSwap reports whether the numeric fields must be byte swapped on
// this host.
func (sb *Superblock) NeedsSwap() bool {
	e, _ := sb.GetEndianness()
	switch e {
	case EndianBig:
		return HostEndianness() != EndianBig
	case EndianLittle:
		return HostEndianness() != EndianLittle
	default:
		return false
	}
}

// Swap64 converts a stored numeric value into host order.
func Swap64(swap bool, v uint64) uint64 {
	if !swap {
		return v
	}
	return bits.ReverseBytes64(v)
}

// Swap32 converts a stored numeric value into host order.
func Swap32(swap bool, v uint32) uint32 {
	if !swap {
		return v
	}
	return bits.ReverseBytes32(v)
}

// --- naming and on-disk paths -------------------------------------------

// ModifiedBase64 encodes a checksum the way OSTree names delta
// directories: RFC 4648 base64 without "=" padding and with '/' replaced
// by '_'.
func ModifiedBase64(sum [32]byte) string {
	s := base64.StdEncoding.EncodeToString(sum[:])
	s = strings.TrimRight(s, "=")
	return strings.ReplaceAll(s, "/", "_")
}

// ParseModifiedBase64 decodes a delta directory name component.
func ParseModifiedBase64(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 43 {
		return out, fmt.Errorf("invalid modified base64 checksum length %d", len(s))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(s, "_", "/") + "=")
	if err != nil {
		return out, fmt.Errorf("invalid modified base64 checksum: %w", err)
	}
	if len(raw) != 32 {
		return out, fmt.Errorf("invalid modified base64 checksum length %d", len(raw))
	}
	copy(out[:], raw)
	return out, nil
}

// DeltaDir returns the repository relative directory of a delta, e.g.
// "deltas/ab/cdef...-0123..." (from may be empty for a from-scratch
// delta).
func DeltaDir(from, to string) (string, error) {
	toSum, err := checksum.FromHex(to)
	if err != nil {
		return "", err
	}
	toB64 := ModifiedBase64(toSum)
	if from == "" {
		return "deltas/" + toB64[:2] + "/" + toB64[2:], nil
	}
	fromSum, err := checksum.FromHex(from)
	if err != nil {
		return "", err
	}
	fromB64 := ModifiedBase64(fromSum)
	return "deltas/" + fromB64[:2] + "/" + fromB64[2:] + "-" + toB64, nil
}

// DeltaPartPath returns the repository relative path of part i.
func DeltaPartPath(from, to string, i int) (string, error) {
	return DeltaPath(from, to, fmt.Sprintf("%d", i))
}

// DeltaPath returns a repository relative path inside a delta directory.
func DeltaPath(from, to, target string) (string, error) {
	dir, err := DeltaDir(from, to)
	if err != nil {
		return "", err
	}
	if target == "" {
		return dir, nil
	}
	return dir + "/" + target, nil
}

// DeltaSuperblockPath returns the repository relative path of the superblock.
func DeltaSuperblockPath(from, to string) (string, error) {
	return DeltaPath(from, to, "superblock")
}

// ParseDeltaName splits a delta identifier ("<from>-<to>" or "<to>")
// into hex checksums.  from is empty for a from-scratch delta.
// The error text matches ostree_validate_checksum_string().
func ParseDeltaName(name string) (from, to string, err error) {
	if i := strings.IndexByte(name, '-'); i >= 0 {
		from, to = name[:i], name[i+1:]
		if !checksum.IsHex(from) {
			return "", "", fmt.Errorf("Invalid rev %s", from)
		}
	} else {
		to = name
	}
	if !checksum.IsHex(to) {
		return "", "", fmt.Errorf("Invalid rev %s", to)
	}
	return from, to, nil
}

// DeltaIndexPath returns the repository relative path of the delta index
// for a target commit ("delta-indexes/ab/cdef...index").
func DeltaIndexPath(to string) (string, error) {
	sum, err := checksum.FromHex(to)
	if err != nil {
		return "", err
	}
	b64 := ModifiedBase64(sum)
	return "delta-indexes/" + b64[:2] + "/" + b64[2:] + ".index", nil
}

// decodeDeltaDirName reverses the directory naming.  fanout is the
// two-character directory, basename the entry below it.
func decodeDeltaDirName(fanout, basename string) (from, to string, ok bool) {
	if len(fanout) != 2 {
		return "", "", false
	}
	if i := strings.IndexByte(basename, '-'); i >= 0 {
		fromSum, err := ParseModifiedBase64(fanout + basename[:i])
		if err != nil {
			return "", "", false
		}
		toSum, err := ParseModifiedBase64(basename[i+1:])
		if err != nil {
			return "", "", false
		}
		return checksum.Hex(fromSum), checksum.Hex(toSum), true
	}
	toSum, err := ParseModifiedBase64(fanout + basename)
	if err != nil {
		return "", "", false
	}
	return "", checksum.Hex(toSum), true
}

// SortParts sorts object references deterministically; part contents
// must be reproducible for a given set of objects.
func SortParts(objs []ObjectRef) {
	sort.Slice(objs, func(i, j int) bool {
		if objs[i].Type != objs[j].Type {
			return objs[i].Type < objs[j].Type
		}
		return objs[i].Hex() < objs[j].Hex()
	})
}

// EncodeSigned wraps an unsigned superblock in OSTREE_STATIC_DELTA_SIGNED_FORMAT.
// signatures is the a{sv} dictionary keyed by the signature engine's
// detached metadata key (for example "ostree.sign.ed25519").
func EncodeSigned(inner []byte, signatures gvariant.Dict) ([]byte, error) {
	if signatures == nil {
		signatures = gvariant.Dict{}
	}
	return gvariant.Encode(SignedType, []gvariant.Value{
		bits.ReverseBytes64(SignedMagic), inner, signatures,
	})
}
