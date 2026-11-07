package staticdelta

import (
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
	"github.com/myml/ostree-go/pkg/sign"
	"github.com/myml/ostree-go/pkg/tree"
)

// DefaultMaxChunkSize is libostree's default --max-chunk-size: 32 MB.
const DefaultMaxChunkSize uint64 = 32 * 1000 * 1000

// GenerateOptions controls Generate.
type GenerateOptions struct {
	Repo *repo.Repo
	// From and To are revisions (branch names, checksums or "<rev>^").
	// From may be empty, which means "from scratch".
	From string
	To   string
	// Inline stores the parts in the superblock metadata instead of
	// writing them as separate files.
	Inline bool
	// Filename writes the superblock to this path and the parts next to
	// it instead of into the repository.
	Filename string
	// MaxChunkSize bounds the payload size of one part in bytes.
	MaxChunkSize uint64
	// Verbose enables the generation statistics libostree prints on
	// stderr.
	Verbose bool
	// Log receives verbose output (defaults to os.Stderr).
	Log io.Writer
	// IfNotExists reports success without regenerating an existing delta.
	IfNotExists bool
	// Endianness is the byte order for the numeric delta fields; when
	// EndiannessSet is false the host byte order is used.
	Endianness    Endianness
	EndiannessSet bool
	// SignKeys are the secret keys used to sign the superblock; an empty
	// slice leaves the delta unsigned.  SignType selects the signing
	// engine (default "ed25519").
	SignKeys []string
	SignType string
	// MaxBsdiffSize is the --max-bsdiff-size threshold in bytes; a value of
	// zero disables binary diffs.  libostree's default is 128 MB.
	MaxBsdiffSize uint64
}

// GenerateResult describes a generated delta.
type GenerateResult struct {
	// Name is the delta identifier, "<from>-<to>" or "<to>".
	Name string
	// Existed is true when IfNotExists skipped an existing delta.
	Existed bool
	Parts   int
	Objects int
	// Compressed and Uncompressed are the totals libostree reports.
	Compressed   uint64
	Uncompressed uint64
	Loose        uint64
}

// partBuilder accumulates the objects of the part currently being built.
type partBuilder struct {
	objects []ObjectRef
	payload []byte
	ops     []byte

	modes    []gvariant.Value
	xattrs   []gvariant.Value
	modeSet  map[string]uint64
	xattrSet map[string]uint64

	tablesSize uint64
	usize      uint64
}

func newPartBuilder() *partBuilder {
	return &partBuilder{
		modes:    []gvariant.Value{},
		xattrs:   []gvariant.Value{},
		modeSet:  make(map[string]uint64),
		xattrSet: make(map[string]uint64),
	}
}

// estimate mirrors current_part_size_estimate(): a lower bound on the
// serialized payload, used only to decide when to split a part.
func (p *partBuilder) estimate() uint64 {
	return uint64(len(p.payload)) + uint64(len(p.ops)) + p.tablesSize
}

// modeOffset interns a (uid, gid, mode) triple and returns its index.
func (p *partBuilder) modeOffset(uid, gid, mode uint32) uint64 {
	key := fmt.Sprintf("%d/%d/%d", uid, gid, mode)
	if off, ok := p.modeSet[key]; ok {
		return off
	}
	off := uint64(len(p.modes))
	// The mode table is always big endian, independent of the delta
	// endianness option.
	entry := []gvariant.Value{
		bits.ReverseBytes32(uid), bits.ReverseBytes32(gid), bits.ReverseBytes32(mode),
	}
	p.modes = append(p.modes, entry)
	p.modeSet[key] = off
	p.tablesSize += uint64(encodedSizeOrZero(modeEntryType, entry))
	return off
}

// xattrOffset interns a canonical xattr set and returns its index.
func (p *partBuilder) xattrOffset(xattrs []xattrPair) (uint64, error) {
	entry := make([]gvariant.Value, len(xattrs))
	for i, x := range xattrs {
		name := append(append([]byte(nil), x.Name...), 0)
		entry[i] = []gvariant.Value{name, x.Value}
	}
	encoded, err := gvariant.Encode(xattrEntryType, entry)
	if err != nil {
		return 0, err
	}
	key := string(encoded)
	if off, ok := p.xattrSet[key]; ok {
		return off, nil
	}
	off := uint64(len(p.xattrs))
	p.xattrs = append(p.xattrs, entry)
	p.xattrSet[key] = off
	p.tablesSize += uint64(len(encoded))
	return off, nil
}

func encodedSizeOrZero(t *gvariant.Type, v gvariant.Value) int {
	n, err := gvariant.EncodedSize(t, v)
	if err != nil {
		return 0
	}
	return n
}

func appendOp(ops []byte, op byte) []byte {
	return append(ops, op)
}

func appendVarint(b []byte, v uint64) []byte {
	var tmp [10]byte
	n := putUvarint(tmp[:], v)
	return append(b, tmp[:n]...)
}

// xattrPair is one extended attribute, kept in the order stored in the
// file object header (GVariant normal form requires sorted names).
type xattrPair struct {
	Name  []byte
	Value []byte
}

// generator holds the state of one Generate call.
type generator struct {
	repo *repo.Repo
	opt  GenerateOptions
	log  io.Writer

	from, to string
	fromSum  [32]byte
	toSum    [32]byte
	hasFrom  bool

	swap bool

	relDir string
	absDir string

	parts      []PartMeta
	inlineData map[int][]byte
	current    *partBuilder

	compressed   uint64
	uncompressed uint64
	loose        uint64
	nObjects     int
	// nRollsum/rollsumSize and nBsdiff report the optimizations applied,
	// like builder->n_rollsum, builder->rollsum_size and builder->n_bsdiff.
	nRollsum    int
	rollsumSize uint64
	nBsdiff     int
}

// Generate builds a static delta between two commits and writes it into
// the repository (or to Filename).
func Generate(opt GenerateOptions) (*GenerateResult, error) {
	if opt.Repo == nil {
		return nil, fmt.Errorf("no repository")
	}
	if opt.To == "" {
		return nil, fmt.Errorf("TO revision must be specified")
	}
	if opt.MaxChunkSize == 0 {
		opt.MaxChunkSize = DefaultMaxChunkSize
	}
	log := opt.Log
	if log == nil {
		log = os.Stderr
	}

	g := &generator{repo: opt.Repo, opt: opt, log: log, inlineData: make(map[int][]byte)}

	toSum, err := ResolveRev(opt.Repo, opt.To)
	if err != nil {
		return nil, err
	}
	g.to = toSum
	if err := copyChecksum(&g.toSum, toSum); err != nil {
		return nil, err
	}

	if opt.From != "" {
		fromSum, err := ResolveRev(opt.Repo, opt.From)
		if err != nil {
			return nil, err
		}
		g.from = fromSum
		g.hasFrom = true
		if err := copyChecksum(&g.fromSum, fromSum); err != nil {
			return nil, err
		}
	}

	// Endianness handling: default is the host byte order.
	target := HostEndianness()
	if opt.EndiannessSet {
		if opt.Endianness != EndianBig && opt.Endianness != EndianLittle {
			return nil, fmt.Errorf("Invalid endianness parameter")
		}
		target = opt.Endianness
	}
	g.swap = target != HostEndianness()
	g.opt.Endianness = target
	g.opt.EndiannessSet = true

	relDir, err := DeltaDir(g.from, g.to)
	if err != nil {
		return nil, err
	}
	g.relDir = relDir

	if opt.Filename != "" {
		g.absDir = filepath.Dir(opt.Filename)
	} else {
		g.absDir = filepath.Join(opt.Repo.Path(), relDir)
	}

	res := &GenerateResult{Name: g.name()}

	superPath := opt.Filename
	if superPath == "" {
		superPath = filepath.Join(g.absDir, "superblock")
	}
	if opt.IfNotExists {
		if _, err := os.Stat(superPath); err == nil {
			res.Existed = true
			return res, nil
		}
	}

	if err := os.MkdirAll(g.absDir, 0o755); err != nil {
		return nil, err
	}

	// Compute the object sets.
	toObjs, err := traverseCommit(opt.Repo, g.to)
	if err != nil {
		return nil, err
	}
	fromObjs := map[objectKey]struct{}{}
	if g.hasFrom {
		fromObjs, err = traverseCommit(opt.Repo, g.from)
		if err != nil {
			return nil, err
		}
	}

	var metaKeys, fileKeys []objectKey
	for k := range toObjs {
		if _, ok := fromObjs[k]; ok {
			continue
		}
		// The target commit itself is shipped in the superblock, not
		// in a part.
		if k.typ == repo.ObjectCommit && k.csum == g.to {
			continue
		}
		if k.typ.IsMeta() {
			metaKeys = append(metaKeys, k)
		} else {
			fileKeys = append(fileKeys, k)
		}
	}
	sortObjectKeys(metaKeys)
	sortObjectKeys(fileKeys)

	g.current = newPartBuilder()
	flush := func() error { return g.flushPart() }

	// Metadata objects first, like libostree.
	for _, k := range metaKeys {
		data, err := opt.Repo.ReadObject(k.csum, k.typ)
		if err != nil {
			return nil, err
		}
		if err := g.ensureRoom(uint64(len(data))); err != nil {
			return nil, err
		}
		g.addMetaObject(k, data)
	}

	// Load the new content objects once: the similarity analysis, the
	// rollsum/bsdiff decisions and the emission all need them.
	files := make(map[string]*tree.File, len(fileKeys))
	for _, k := range fileKeys {
		f, err := tree.LoadFile(opt.Repo, k.csum)
		if err != nil {
			return nil, err
		}
		files[k.csum] = f
	}

	// Find content objects that were modified relative to the source commit
	// (_ostree_delta_compute_similar_objects()), then decide per object
	// whether rollsum chunk reuse or a binary diff is profitable.
	modified := map[string]string{}
	if g.hasFrom {
		modified, err = g.computeModifiedFiles(fileKeys, files)
		if err != nil {
			return nil, err
		}
	}
	rollsumKeys := make(map[string]*rollsumMatches)
	bsdiffKeys := make(map[string]string)
	if g.hasFrom {
		for _, k := range fileKeys {
			fromCsum, ok := modified[k.csum]
			if !ok {
				continue
			}
			f := files[k.csum]
			if !f.Header.IsRegular() {
				continue
			}
			fromFile, err := g.loadFile(fromCsum)
			if err != nil {
				continue
			}
			if !fromFile.Header.IsRegular() || fromFile.Header.Mode&0004 == 0 {
				// Only reuse content that any client can read, like
				// check_object_world_readable().
				continue
			}
			fromContent := fromFile.Content
			matches := computeRollsumMatches(fromContent, f.Content)
			if matches.total == 0 {
				continue
			}
			if matches.bufMatches*100/matches.total >= 50 {
				rollsumKeys[k.csum] = matches
				continue
			}
			if opt.MaxBsdiffSize > 0 &&
				uint64(len(fromContent))+uint64(len(f.Content)) <= opt.MaxBsdiffSize {
				bsdiffKeys[k.csum] = fromCsum
			}
		}
	}

	// libostree emits the reused objects in a fixed order: rollsum first,
	// then binary diffs, then plain regular files and symlinks.
	for _, k := range fileKeys {
		matches, ok := rollsumKeys[k.csum]
		if !ok {
			continue
		}
		fromFile, err := g.loadFile(modified[k.csum])
		if err != nil {
			return nil, err
		}
		if err := g.addRollsumObject(k, files[k.csum], modified[k.csum], fromFile.Content, matches); err != nil {
			return nil, err
		}
	}
	for _, k := range fileKeys {
		fromCsum, ok := bsdiffKeys[k.csum]
		if !ok {
			continue
		}
		fromFile, err := g.loadFile(fromCsum)
		if err != nil {
			return nil, err
		}
		if err := g.addBsdiffObject(k, files[k.csum], fromCsum, fromFile.Content); err != nil {
			return nil, err
		}
	}
	for _, k := range fileKeys {
		if _, ok := rollsumKeys[k.csum]; ok {
			continue
		}
		if _, ok := bsdiffKeys[k.csum]; ok {
			continue
		}
		if err := g.addFileObject(k, files[k.csum]); err != nil {
			return nil, err
		}
	}

	// libostree always emits at least one (possibly empty) part.
	if err := flush(); err != nil {
		return nil, err
	}

	commitRaw, err := opt.Repo.ReadObject(g.to, repo.ObjectCommit)
	if err != nil {
		return nil, err
	}
	commitValue, err := gvariant.Decode(object.CommitType, commitRaw)
	if err != nil {
		return nil, fmt.Errorf("static delta: parsing commit %s: %w", g.to, err)
	}

	meta := gvariant.Dict{}
	meta.Set(MetaEndiannessKey, gvariant.Variant{
		Type:  gvariant.MustParseType("y"),
		Value: uint8(target.Char()),
	})
	if opt.Inline {
		for i, data := range g.inlineData {
			relPart, err := DeltaPartPath(g.from, g.to, i)
			if err != nil {
				return nil, err
			}
			meta.Set(relPart, gvariant.Variant{
				Type:  PartFileType,
				Value: []gvariant.Value{data[0], data[1:]},
			})
		}
	}
	// Store the target commit's detached metadata in the superblock so a
	// pull can verify signatures without fetching the commitmeta object.
	if detached, derr := opt.Repo.ReadObject(g.to, repo.ObjectCommitmeta); derr == nil {
		if dict, perr := object.ParseMetadata(detached); perr == nil {
			relMeta, perr := DeltaPath(g.from, g.to, "commitmeta")
			if perr != nil {
				return nil, perr
			}
			meta.Set(relMeta, gvariant.Variant{
				Type:  object.MetadataType,
				Value: dict,
			})
		}
	}

	sb := &Superblock{
		Metadata:  meta,
		Timestamp: uint64(time.Now().Unix()),
		To:        g.toSum,
		Commit:    commitValue,
		Parts:     g.parts,
	}
	if g.hasFrom {
		sb.From = g.fromSum
		sb.HasFrom = true
	}
	sbBytes, err := sb.Encode()
	if err != nil {
		return nil, err
	}
	if len(opt.SignKeys) > 0 {
		signed, err := signSuperblock(sbBytes, opt.SignType, opt.SignKeys)
		if err != nil {
			return nil, err
		}
		sbBytes = signed
	}
	if err := os.WriteFile(superPath, sbBytes, 0o644); err != nil {
		return nil, err
	}

	res.Parts = len(g.parts)
	res.Objects = g.nObjects
	res.Compressed = g.compressed
	res.Uncompressed = g.uncompressed
	res.Loose = g.loose

	if opt.Verbose {
		fmt.Fprintf(log, "uncompressed=%d compressed=%d loose=%d\n",
			g.uncompressed, g.compressed, g.loose)
		fmt.Fprintf(log, "rollsum=%d objects, %d bytes\n", g.nRollsum, g.rollsumSize)
		fmt.Fprintf(log, "bsdiff=%d objects\n", g.nBsdiff)
	}
	return res, nil
}

func (g *generator) name() string {
	to := checksum.Hex(g.toSum)
	if !g.hasFrom {
		return to
	}
	return checksum.Hex(g.fromSum) + "-" + to
}

// signSuperblock wraps an encoded superblock with signatures produced by
// every key in keys, mirroring the signing block at the end of
// ostree_repo_static_delta_generate().
func signSuperblock(inner []byte, signType string, keys []string) ([]byte, error) {
	if signType == "" {
		signType = sign.NameEd25519
	}
	engine, err := sign.GetByName(signType)
	if err != nil {
		return nil, err
	}
	sigs, err := sign.SignDataWithKeys(engine, inner, keys)
	if err != nil {
		return nil, err
	}
	values := make([]gvariant.Value, len(sigs))
	for i, sig := range sigs {
		values[i] = sig
	}
	dict := gvariant.Dict{}
	dict.Set(engine.MetadataKey(), gvariant.Variant{
		Type:  gvariant.MustParseType("aay"),
		Value: values,
	})
	return EncodeSigned(inner, dict)
}

// ResolveRev resolves a revision to a commit checksum, supporting the
// "<rev>^" parent syntax.
func ResolveRev(r *repo.Repo, rev string) (string, error) {
	if len(rev) > 1 && rev[len(rev)-1] == '^' {
		base := rev[:len(rev)-1]
		sum, err := tree.ResolveRef(r, base)
		if err != nil {
			return "", err
		}
		c, err := tree.LoadCommit(r, sum)
		if err != nil {
			return "", err
		}
		if c.Parent == "" {
			return "", fmt.Errorf("Invalid ref: %s", rev)
		}
		return c.Parent, nil
	}
	return tree.ResolveRef(r, rev)
}

func copyChecksum(dst *[32]byte, hexSum string) error {
	sum, err := checksum.FromHex(hexSum)
	if err != nil {
		return err
	}
	*dst = sum
	return nil
}

type objectKey struct {
	typ  repo.ObjectType
	csum string
}

func sortObjectKeys(keys []objectKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].typ != keys[j].typ {
			return keys[i].typ < keys[j].typ
		}
		return keys[i].csum < keys[j].csum
	})
}

// traverseCommit collects the commit object plus every object reachable
// from its root tree.
func traverseCommit(r *repo.Repo, commitCsum string) (map[objectKey]struct{}, error) {
	set := map[objectKey]struct{}{}
	commit, err := tree.LoadCommit(r, commitCsum)
	if err != nil {
		return nil, err
	}
	set[objectKey{repo.ObjectCommit, commitCsum}] = struct{}{}
	// The root dirmeta is referenced by the commit itself, not by any
	// dirtree entry, so it must be added explicitly.
	if commit.RootMeta != "" {
		set[objectKey{repo.ObjectDirmeta, commit.RootMeta}] = struct{}{}
	}
	seenTrees := map[string]bool{}
	if err := traverseTree(r, commit.RootTree, set, seenTrees); err != nil {
		return nil, err
	}
	return set, nil
}

func traverseTree(r *repo.Repo, treeCsum string, set map[objectKey]struct{}, seen map[string]bool) error {
	// The dirtree + dirmeta pair is reached through the parent's
	// DirEntry; the root's dirmeta is reachable from the commit too, but
	// traverse_commit only walks tree objects, so record what the tree
	// references.
	dt, err := tree.LoadDirtree(r, treeCsum)
	if err != nil {
		return err
	}
	if seen[treeCsum] {
		return nil
	}
	seen[treeCsum] = true
	set[objectKey{repo.ObjectDirtree, treeCsum}] = struct{}{}
	for _, f := range dt.Files {
		set[objectKey{repo.ObjectFile, f.Checksum}] = struct{}{}
	}
	for _, d := range dt.Dirs {
		set[objectKey{repo.ObjectDirmeta, d.Meta}] = struct{}{}
		if err := traverseTree(r, d.Tree, set, seen); err != nil {
			return err
		}
	}
	return nil
}

// ensureRoom starts a new part if adding size bytes would exceed
// --max-chunk-size.
func (g *generator) ensureRoom(size uint64) error {
	if len(g.current.objects) == 0 {
		return nil
	}
	if g.current.estimate()+size > g.opt.MaxChunkSize {
		return g.flushPart()
	}
	return nil
}

func (g *generator) addMetaObject(k objectKey, data []byte) {
	p := g.current
	start := uint64(len(p.payload))
	p.payload = append(p.payload, data...)
	p.ops = appendOp(p.ops, OpOpenSpliceAndClose)
	p.ops = appendVarint(p.ops, uint64(len(data)))
	p.ops = appendVarint(p.ops, start)
	p.objects = append(p.objects, ObjectRef{Type: k.typ, Csum: checksumBytes(k.csum)})
	p.usize += uint64(len(data))
	g.nObjects++
	g.recordLoose(k)
}

func (g *generator) addFileObject(k objectKey, f *tree.File) error {
	var content []byte
	if f.Header.IsSymlink() {
		content = []byte(f.Header.SymlinkTarget)
	} else {
		content = f.Content
	}
	if err := g.ensureRoom(uint64(len(content))); err != nil {
		return err
	}
	xattrs := make([]xattrPair, len(f.Header.Xattrs))
	for i, x := range f.Header.Xattrs {
		xattrs[i] = xattrPair{Name: []byte(x.Name), Value: x.Value}
	}
	p := g.current
	modeOff := p.modeOffset(f.Header.UID, f.Header.GID, f.Header.Mode)
	xattrOff, err := p.xattrOffset(xattrs)
	if err != nil {
		return err
	}

	start := uint64(len(p.payload))
	p.payload = append(p.payload, content...)
	p.ops = appendOp(p.ops, OpOpenSpliceAndClose)
	p.ops = appendVarint(p.ops, modeOff)
	p.ops = appendVarint(p.ops, xattrOff)
	p.ops = appendVarint(p.ops, uint64(len(content)))
	p.ops = appendVarint(p.ops, start)
	p.objects = append(p.objects, ObjectRef{Type: k.typ, Csum: checksumBytes(k.csum)})
	// libostree accounts symlinks as zero uncompressed bytes: the size
	// is taken from the file info before the target is substituted.
	if !f.Header.IsSymlink() {
		p.usize += uint64(len(content))
	}
	g.nObjects++
	g.recordLoose(k)
	return nil
}

// loadFile loads (and does not cache) a content object.
func (g *generator) loadFile(csum string) (*tree.File, error) {
	return tree.LoadFile(g.repo, csum)
}

// appendPayloadWrite adds chunk to the payload and emits the matching WRITE
// opcode, mirroring append_payload_chunk_and_write().
func (g *generator) appendPayloadWrite(p *partBuilder, chunk []byte) {
	start := uint64(len(p.payload))
	p.payload = append(p.payload, chunk...)
	p.ops = appendOp(p.ops, OpWrite)
	p.ops = appendVarint(p.ops, uint64(len(chunk)))
	p.ops = appendVarint(p.ops, start)
}

// addRollsumObject emits a content object as rollsum matches plus the
// unmatched chunks, mirroring process_one_rollsum().
func (g *generator) addRollsumObject(k objectKey, f *tree.File, fromCsum string,
	fromContent []byte, matches *rollsumMatches) error {
	content := f.Content
	if err := g.ensureRoom(uint64(len(content))); err != nil {
		return err
	}
	xattrs := make([]xattrPair, len(f.Header.Xattrs))
	for i, x := range f.Header.Xattrs {
		xattrs[i] = xattrPair{Name: []byte(x.Name), Value: x.Value}
	}
	p := g.current
	modeOff := p.modeOffset(f.Header.UID, f.Header.GID, f.Header.Mode)
	xattrOff, err := p.xattrOffset(xattrs)
	if err != nil {
		return err
	}

	// The source checksum lives in the payload; SET_READ_SOURCE refers to
	// it by offset.
	fromSum := checksumBytes(fromCsum)
	fromOff := uint64(len(p.payload))
	p.payload = append(p.payload, fromSum[:]...)

	p.ops = appendOp(p.ops, OpOpen)
	p.ops = appendVarint(p.ops, modeOff)
	p.ops = appendVarint(p.ops, xattrOff)
	p.ops = appendVarint(p.ops, uint64(len(content)))

	writing := uint64(0)
	readingPayload := true
	for _, match := range matches.matches {
		prefix := match.toStart - writing
		if prefix > 0 {
			if !readingPayload {
				p.ops = appendOp(p.ops, OpUnsetReadSource)
				readingPayload = true
			}
			g.appendPayloadWrite(p, content[writing:writing+prefix])
			writing += prefix
		}
		if readingPayload {
			p.ops = appendOp(p.ops, OpSetReadSource)
			p.ops = appendVarint(p.ops, fromOff)
			readingPayload = false
		}
		p.ops = appendOp(p.ops, OpWrite)
		p.ops = appendVarint(p.ops, match.size)
		p.ops = appendVarint(p.ops, match.fromStart)
		writing += match.size
	}
	if !readingPayload {
		p.ops = appendOp(p.ops, OpUnsetReadSource)
	}
	if remainder := uint64(len(content)) - writing; remainder > 0 {
		g.appendPayloadWrite(p, content[writing:])
	}
	p.ops = appendOp(p.ops, OpClose)

	p.objects = append(p.objects, ObjectRef{Type: k.typ, Csum: checksumBytes(k.csum)})
	p.usize += uint64(len(content))
	g.nObjects++
	g.nRollsum++
	g.rollsumSize += matches.matchSize
	g.recordLoose(k)
	return nil
}

// addBsdiffObject emits a binary diff against the source content, mirroring
// process_one_bsdiff().
func (g *generator) addBsdiffObject(k objectKey, f *tree.File, fromCsum string,
	fromContent []byte) error {
	content := f.Content
	if err := g.ensureRoom(uint64(len(content))); err != nil {
		return err
	}
	xattrs := make([]xattrPair, len(f.Header.Xattrs))
	for i, x := range f.Header.Xattrs {
		xattrs[i] = xattrPair{Name: []byte(x.Name), Value: x.Value}
	}
	p := g.current
	modeOff := p.modeOffset(f.Header.UID, f.Header.GID, f.Header.Mode)
	xattrOff, err := p.xattrOffset(xattrs)
	if err != nil {
		return err
	}

	fromSum := checksumBytes(fromCsum)
	p.ops = appendOp(p.ops, OpSetReadSource)
	fromOff := uint64(len(p.payload))
	p.ops = appendVarint(p.ops, fromOff)
	p.payload = append(p.payload, fromSum[:]...)

	p.ops = appendOp(p.ops, OpOpen)
	p.ops = appendVarint(p.ops, modeOff)
	p.ops = appendVarint(p.ops, xattrOff)
	p.ops = appendVarint(p.ops, uint64(len(content)))

	patch := bsdiff(fromContent, content)
	patchOff := uint64(len(p.payload))
	p.ops = appendOp(p.ops, OpBspatch)
	p.ops = appendVarint(p.ops, patchOff)
	p.ops = appendVarint(p.ops, uint64(len(patch)))
	p.payload = append(p.payload, patch...)
	p.ops = appendOp(p.ops, OpClose)
	p.ops = appendOp(p.ops, OpUnsetReadSource)

	p.objects = append(p.objects, ObjectRef{Type: k.typ, Csum: checksumBytes(k.csum)})
	p.usize += uint64(len(content))
	g.nObjects++
	g.nBsdiff++
	g.recordLoose(k)
	return nil
}

// fileSizename mirrors OstreeDeltaContentSizeNames: one size and the set of
// basenames a content object appears under.
type fileSizename struct {
	csum  string
	size  uint64
	names []string
}

// collectFileSizename walks a commit's tree and gathers size and basenames for
// every regular file object accepted by include (nil means all of them).
func (g *generator) collectFileSizename(commitCsum string, include map[string]bool) ([]*fileSizename, error) {
	commit, err := tree.LoadCommit(g.repo, commitCsum)
	if err != nil {
		return nil, err
	}
	byCsum := map[string]*fileSizename{}
	var out []*fileSizename
	seen := map[string]bool{}
	var walk func(treeCsum string) error
	walk = func(treeCsum string) error {
		if seen[treeCsum] {
			return nil
		}
		seen[treeCsum] = true
		dt, err := tree.LoadDirtree(g.repo, treeCsum)
		if err != nil {
			return err
		}
		for _, f := range dt.Files {
			if include != nil && !include[f.Checksum] {
				continue
			}
			info, ok := byCsum[f.Checksum]
			if !ok {
				loaded, err := g.loadFile(f.Checksum)
				if err != nil {
					return err
				}
				if !loaded.Header.IsRegular() {
					continue
				}
				info = &fileSizename{csum: f.Checksum, size: uint64(len(loaded.Content))}
				byCsum[f.Checksum] = info
				out = append(out, info)
			}
			info.names = append(info.names, f.Name)
		}
		for _, d := range dt.Dirs {
			if err := walk(d.Tree); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(commit.RootTree); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].size < out[j].size })
	return out, nil
}

// computeModifiedFiles builds the to-checksum → from-checksum map of content
// objects that look similar enough to be worth a delta, mirroring
// _ostree_delta_compute_similar_objects() with the 30% size threshold.
func (g *generator) computeModifiedFiles(fileKeys []objectKey,
	files map[string]*tree.File) (map[string]string, error) {
	newRegular := map[string]bool{}
	for _, k := range fileKeys {
		if f := files[k.csum]; f != nil && f.Header.IsRegular() {
			newRegular[k.csum] = true
		}
	}
	fromInfos, err := g.collectFileSizename(g.from, nil)
	if err != nil {
		return nil, err
	}
	toInfos, err := g.collectFileSizename(g.to, newRegular)
	if err != nil {
		return nil, err
	}
	modified := map[string]string{}
	lower := 0
	for _, to := range toInfos {
		if !deltaCandidate(to) {
			continue
		}
		minThreshold := uint64(float64(to.size) * (1.0 - 30.0/100.0))
		maxThreshold := uint64(float64(to.size) * (1.0 + 30.0/100.0))
		found := false
		for fuzzy := 0; fuzzy < 2 && !found; fuzzy++ {
			for j := lower; j < len(fromInfos); j++ {
				from := fromInfos[j]
				if !deltaCandidate(from) {
					continue
				}
				if from.size < minThreshold {
					lower++
					continue
				}
				if from.size > maxThreshold {
					break
				}
				if !namesIntersect(from.names, to.names, fuzzy == 1) {
					continue
				}
				modified[to.csum] = from.csum
				found = true
				break
			}
		}
	}
	return modified, nil
}

// deltaCandidate mirrors sizename_is_delta_candidate(): empty objects and
// pre-compressed files are never candidates.
func deltaCandidate(info *fileSizename) bool {
	if info.size == 0 {
		return false
	}
	for _, name := range info.names {
		i := strings.LastIndexByte(name, '.')
		if i < 0 {
			continue
		}
		switch name[i+1:] {
		case "xz", "bz2":
			return false
		}
	}
	return true
}

// namesIntersect mirrors string_array_nonempty_intersection(): exact basename
// equality, or (when fuzzy) equality of the part before the first '.'.
func namesIntersect(a, b []string, fuzzy bool) bool {
	for _, as := range a {
		aDot := strings.IndexByte(as, '.')
		for _, bs := range b {
			bDot := strings.IndexByte(bs, '.')
			if fuzzy && aDot > 0 && bDot > 0 && aDot == bDot && as[:aDot] == bs[:bDot] {
				return true
			}
			if as == bs {
				return true
			}
		}
	}
	return false
}

func (g *generator) recordLoose(k objectKey) {
	rel, err := g.repo.ObjectRelPath(k.csum, k.typ)
	if err != nil {
		return
	}
	if fi, err := os.Stat(filepath.Join(g.repo.Path(), rel)); err == nil {
		g.loose += uint64(fi.Size())
	}
}

// flushPart serializes the current part, writes it (unless inline) and
// records its meta entry.
func (g *generator) flushPart() error {
	p := g.current
	payload, err := gvariant.Encode(PartPayloadType, []gvariant.Value{
		p.modes, p.xattrs, p.payload, p.ops,
	})
	if err != nil {
		return err
	}
	comp, body, err := compressPart(payload)
	if err != nil {
		return err
	}
	fileBytes := make([]byte, 0, len(body)+1)
	fileBytes = append(fileBytes, comp)
	fileBytes = append(fileBytes, body...)
	sum := checksum.Sum(fileBytes)

	index := len(g.parts)
	if g.opt.Inline {
		g.inlineData[index] = fileBytes
	} else {
		partPath := filepath.Join(g.absDir, strconv.Itoa(index))
		if err := os.WriteFile(partPath, fileBytes, 0o644); err != nil {
			return err
		}
	}

	size := uint64(len(fileBytes))
	g.parts = append(g.parts, PartMeta{
		Version:  swap32Value(g.swap, PartVersion),
		Checksum: sum,
		Size:     swap64Value(g.swap, size),
		USize:    swap64Value(g.swap, p.usize),
		Objects:  p.objects,
	})
	g.compressed += size
	g.uncompressed += p.usize

	if g.opt.Verbose {
		fmt.Fprintf(g.log, "part %d n:%d compressed:%d uncompressed:%d\n",
			index+1, len(p.objects), size, p.usize)
	}
	g.current = newPartBuilder()
	return nil
}

func swap64Value(swap bool, v uint64) uint64 {
	if !swap {
		return v
	}
	return bits.ReverseBytes64(v)
}

func swap32Value(swap bool, v uint32) uint32 {
	if !swap {
		return v
	}
	return bits.ReverseBytes32(v)
}

func checksumBytes(hexSum string) (out [32]byte) {
	sum, err := checksum.FromHex(hexSum)
	if err == nil {
		out = sum
	}
	return out
}

// putUvarint is binary.PutUvarint, kept local to make the wire format
// explicit: OSTree's varints are protobuf style little-endian base-128.
func putUvarint(buf []byte, v uint64) int {
	i := 0
	for v >= 0x80 {
		buf[i] = byte(v) | 0x80
		v >>= 7
		i++
	}
	buf[i] = byte(v)
	return i + 1
}

// readUvarint decodes one OSTree varint.  n is <= 0 on error, matching
// encoding/binary.Uvarint.
func readUvarint(buf []byte) (uint64, int) {
	var value uint64
	for i := 0; i < len(buf) && i < 10; i++ {
		b := buf[i]
		value |= uint64(b&0x7f) << (7 * uint(i))
		if b&0x80 == 0 {
			return value, i + 1
		}
	}
	return 0, 0
}
