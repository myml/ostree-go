package staticdelta

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/sign"
	"github.com/myml/ostree-go/internal/tree"
)

// CommitBytes returns the target commit object exactly as it is serialized
// in the superblock.
func (sb *Superblock) CommitBytes() ([]byte, error) {
	return gvariant.Encode(object.CommitType, sb.Commit)
}

// PartHaveAllObjects reports whether every object created by a delta part is
// already present in r, matching _ostree_repo_static_delta_part_have_all_objects().
func PartHaveAllObjects(r *repo.Repo, objects []ObjectRef) (bool, error) {
	for _, o := range objects {
		have, err := r.HasObject(o.Hex(), o.Type)
		if err != nil {
			return false, err
		}
		if !have {
			return false, nil
		}
	}
	return true, nil
}

// VerifyPartChecksum checks the sha256 of the compressed part file against
// the digest stored in the superblock.
func VerifyPartChecksum(fileBytes []byte, expected [32]byte) error {
	actual := sha256.Sum256(fileBytes)
	if actual != expected {
		return fmt.Errorf("Checksum mismatch in static delta part; expected=%s actual=%s",
			checksum.Hex(expected), checksum.Hex(actual))
	}
	return nil
}

// ApplyPartFile verifies and executes one compressed part file.
func ApplyPartFile(r *repo.Repo, objects []ObjectRef, fileBytes []byte) error {
	payload, err := DecodePartFile(fileBytes)
	if err != nil {
		return err
	}
	return ExecutePart(r, objects, payload)
}

// ExecutePart executes one decompressed part payload: the opcode stream and
// its mode/xattr/blob tables (OSTREE_STATIC_DELTA_PART_PAYLOAD_FORMAT_V0).
func ExecutePart(r *repo.Repo, objects []ObjectRef, payload []byte) error {
	v, err := gvariant.Decode(PartPayloadType, payload)
	if err != nil {
		return fmt.Errorf("static delta: parsing part payload: %w", err)
	}
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) != 4 {
		return fmt.Errorf("static delta: malformed part payload")
	}
	modeVals, ok := fields[0].([]gvariant.Value)
	if !ok {
		return fmt.Errorf("static delta: malformed mode table")
	}
	xattrVals, ok := fields[1].([]gvariant.Value)
	if !ok {
		return fmt.Errorf("static delta: malformed xattr table")
	}
	blob, ok := fields[2].([]byte)
	if !ok {
		return fmt.Errorf("static delta: malformed payload blob")
	}
	ops, ok := fields[3].([]byte)
	if !ok {
		return fmt.Errorf("static delta: malformed operation stream")
	}

	st := &executeState{
		repo:    r,
		objects: objects,
		modes:   modeVals,
		xattrs:  xattrVals,
		blob:    blob,
	}
	for len(ops) > 0 {
		op := ops[0]
		ops = ops[1:]
		var err error
		switch op {
		case OpOpenSpliceAndClose:
			err = st.openSpliceAndClose(&ops)
		case OpOpen:
			err = st.open(&ops)
		case OpWrite:
			err = st.write(&ops)
		case OpSetReadSource:
			err = st.setReadSource(&ops)
		case OpUnsetReadSource:
			st.readSrc = nil
		case OpClose:
			err = st.close()
		case OpBspatch:
			err = st.bspatch(&ops)
		default:
			err = fmt.Errorf("static delta: unknown opcode %d", op)
		}
		if err != nil {
			return err
		}
	}
	if st.pending != nil {
		return fmt.Errorf("static delta: unterminated object in operation stream")
	}
	return nil
}

type executeState struct {
	repo    *repo.Repo
	objects []ObjectRef

	modes  []gvariant.Value
	xattrs []gvariant.Value
	blob   []byte

	objIndex int
	pending  *pendingObject
	readSrc  []byte
}

type pendingObject struct {
	obj  ObjectRef
	meta object.FileHeader
	// size is the content size declared by OPEN; bspatch needs it because
	// the patch stream itself carries no output length.
	size           uint64
	body           []byte
	pendingSkipped bool
}

func readVarint(ops *[]byte) (uint64, error) {
	v, n := readUvarint(*ops)
	if n <= 0 {
		return 0, fmt.Errorf("static delta: unexpected end of operation stream")
	}
	*ops = (*ops)[n:]
	return v, nil
}

func (st *executeState) validateBlob(offset, length uint64) error {
	if offset+length < offset || offset+length > uint64(len(st.blob)) {
		return fmt.Errorf("static delta: invalid offset/length %d/%d", offset, length)
	}
	return nil
}

func (st *executeState) nextObject() (ObjectRef, error) {
	if st.objIndex >= len(st.objects) {
		return ObjectRef{}, fmt.Errorf("static delta: more opcodes than objects")
	}
	return st.objects[st.objIndex], nil
}

// readModeXattrs decodes the (mode-offset, xattr-offset) pair used by content
// objects.  The uid/gid/mode triple is always stored big endian.
func (st *executeState) readModeXattrs(ops *[]byte) (*object.FileHeader, error) {
	modeOff, err := readVarint(ops)
	if err != nil {
		return nil, err
	}
	xattrOff, err := readVarint(ops)
	if err != nil {
		return nil, err
	}
	if modeOff >= uint64(len(st.modes)) {
		return nil, fmt.Errorf("Static delta mode offset %d out of range", modeOff)
	}
	if xattrOff >= uint64(len(st.xattrs)) {
		return nil, fmt.Errorf("Static delta xattr offset %d out of range", xattrOff)
	}
	modeFields, ok := st.modes[modeOff].([]gvariant.Value)
	if !ok || len(modeFields) != 3 {
		return nil, fmt.Errorf("static delta: malformed mode entry")
	}
	uid, _ := modeFields[0].(uint32)
	gid, _ := modeFields[1].(uint32)
	mode, _ := modeFields[2].(uint32)
	// Values were stored with GUINT32_TO_BE.
	h := &object.FileHeader{
		UID:  Swap32(true, uid),
		GID:  Swap32(true, gid),
		Mode: Swap32(true, mode),
	}
	xattrs, err := decodeXattrTable(st.xattrs[xattrOff])
	if err != nil {
		return nil, err
	}
	h.Xattrs = xattrs
	return h, nil
}

func decodeXattrTable(v gvariant.Value) ([]object.XAttr, error) {
	entries, ok := v.([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed xattr entry")
	}
	out := make([]object.XAttr, 0, len(entries))
	for _, e := range entries {
		pair, ok := e.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("static delta: malformed xattr pair")
		}
		name, _ := pair[0].([]byte)
		value, _ := pair[1].([]byte)
		n := strings.TrimSuffix(string(name), "\x00")
		out = append(out, object.XAttr{Name: n, Value: value})
	}
	return out, nil
}

func (st *executeState) openSpliceAndClose(ops *[]byte) error {
	obj, err := st.nextObject()
	if err != nil {
		return err
	}
	st.objIndex++
	if obj.Type.IsMeta() {
		length, err := readVarint(ops)
		if err != nil {
			return err
		}
		offset, err := readVarint(ops)
		if err != nil {
			return err
		}
		if err := st.validateBlob(offset, length); err != nil {
			return err
		}
		return st.writeMetadata(obj, st.blob[offset:offset+length])
	}
	header, err := st.readModeXattrs(ops)
	if err != nil {
		return err
	}
	size, err := readVarint(ops)
	if err != nil {
		return err
	}
	offset, err := readVarint(ops)
	if err != nil {
		return err
	}
	if err := st.validateBlob(offset, size); err != nil {
		return err
	}
	return st.writeFileObject(obj, header, st.blob[offset:offset+size])
}

func (st *executeState) open(ops *[]byte) error {
	obj, err := st.nextObject()
	if err != nil {
		return err
	}
	header, err := st.readModeXattrs(ops)
	if err != nil {
		return err
	}
	size, err := readVarint(ops)
	if err != nil {
		return err
	}
	have, err := st.repo.HasObject(obj.Hex(), repo.ObjectFile)
	if err != nil {
		return err
	}
	if have {
		// The object is already stored; consume the following ops without
		// writing it again, like state->have_obj in the C implementation.
		st.pending = &pendingObject{obj: obj, meta: *header, size: size}
		st.pending.pendingSkipped = true
		return nil
	}
	st.pending = &pendingObject{obj: obj, meta: *header, size: size}
	return nil
}

func (st *executeState) write(ops *[]byte) error {
	size, err := readVarint(ops)
	if err != nil {
		return err
	}
	offset, err := readVarint(ops)
	if err != nil {
		return err
	}
	if st.pending == nil || st.pending.pendingSkipped {
		return nil
	}
	if st.readSrc != nil {
		if offset+size > uint64(len(st.readSrc)) {
			return fmt.Errorf("static delta: read source too short")
		}
		st.pending.body = append(st.pending.body, st.readSrc[offset:offset+size]...)
		return nil
	}
	if err := st.validateBlob(offset, size); err != nil {
		return err
	}
	st.pending.body = append(st.pending.body, st.blob[offset:offset+size]...)
	return nil
}

func (st *executeState) setReadSource(ops *[]byte) error {
	offset, err := readVarint(ops)
	if err != nil {
		return err
	}
	if err := st.validateBlob(offset, 32); err != nil {
		return err
	}
	source := checksum.Hex(toArray32(st.blob[offset : offset+32]))
	f, err := tree.LoadFile(st.repo, source)
	if err != nil {
		return fmt.Errorf("static delta: reading source object %s: %w", source, err)
	}
	st.readSrc = f.Content
	return nil
}

// bspatch consumes an OSTREE_STATIC_DELTA_OP_BSPATCH operation.  The patch
// bytes are stored in the payload blob and applied to the current read
// source; OPEN must have declared the output size.
func (st *executeState) bspatch(ops *[]byte) error {
	offset, err := readVarint(ops)
	if err != nil {
		return err
	}
	length, err := readVarint(ops)
	if err != nil {
		return err
	}
	if err := st.validateBlob(offset, length); err != nil {
		return err
	}
	if st.pending == nil || st.pending.pendingSkipped {
		return nil
	}
	if st.readSrc == nil {
		return fmt.Errorf("static delta: bspatch without a read source")
	}
	out, err := bspatch(st.readSrc, int64(st.pending.size), st.blob[offset:offset+length])
	if err != nil {
		return err
	}
	if uint64(len(out)) != st.pending.size {
		return fmt.Errorf("static delta: bspatch produced %d bytes, expected %d",
			len(out), st.pending.size)
	}
	st.pending.body = out
	return nil
}

func (st *executeState) close() error {
	p := st.pending
	st.pending = nil
	if p == nil {
		return nil
	}
	st.objIndex++
	if p.pendingSkipped {
		return nil
	}
	return st.writeFileObject(p.obj, &p.meta, p.body)
}

func toArray32(b []byte) (out [32]byte) {
	copy(out[:], b)
	return out
}

func (st *executeState) writeMetadata(obj ObjectRef, data []byte) error {
	actual, err := st.repo.WriteObject(obj.Type, data)
	if err != nil {
		return fmt.Errorf("static delta: writing %s %s: %w", obj.Type, obj.Hex(), err)
	}
	if actual != obj.Hex() {
		return fmt.Errorf("static delta: checksum mismatch for %s %s (got %s)",
			obj.Type, obj.Hex(), actual)
	}
	return nil
}

func (st *executeState) writeFileObject(obj ObjectRef, header *object.FileHeader, content []byte) error {
	if header.Mode&0170000 == 0120000 {
		target := string(content)
		if strings.IndexByte(target, 0) >= 0 {
			target = target[:strings.IndexByte(target, 0)]
		}
		header.SymlinkTarget = target
		if err := validateContentChecksum(header, nil, obj.Hex()); err != nil {
			return err
		}
		return writeDeltaContent(st.repo, obj.Hex(), header, nil)
	}
	if err := validateContentChecksum(header, content, obj.Hex()); err != nil {
		return err
	}
	return writeDeltaContent(st.repo, obj.Hex(), header, content)
}

func validateContentChecksum(h *object.FileHeader, content []byte, expected string) error {
	actual, err := h.Checksum(content)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("static delta: content checksum mismatch for %s (got %s)", expected, actual)
	}
	return nil
}

// writeDeltaContent stores a content object for the repository mode, reusing
// the same encoding rules as pull's object importer.
func writeDeltaContent(r *repo.Repo, csum string, h *object.FileHeader, content []byte) error {
	xattrs := make(map[string][]byte, len(h.Xattrs))
	for _, x := range h.Xattrs {
		xattrs[x.Name] = x.Value
	}
	switch r.Mode() {
	case repo.ModeArchive:
		var payload []byte
		if h.IsRegular() {
			h.Size = uint64(len(content))
			header, err := h.EncodeArchive()
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
			header, err := h.EncodeArchive()
			if err != nil {
				return err
			}
			payload = header
		}
		return r.WriteFileRawObject(csum, payload,
			repo.FileObjectMeta{UID: h.UID, GID: h.GID, Mode: h.Mode, Xattrs: xattrs})
	case repo.ModeBare:
		meta := repo.FileObjectMeta{UID: h.UID, GID: h.GID, Mode: h.Mode, Xattrs: xattrs}
		if h.IsSymlink() {
			return r.WriteSymlinkObject(csum, repo.ObjectFile, h.SymlinkTarget, meta)
		}
		return r.WriteFileRawObject(csum, content, meta)
	case repo.ModeBareUser:
		ostreemeta, err := object.EncodeFileMeta(h.UID, h.GID, h.Mode, h.Xattrs)
		if err != nil {
			return err
		}
		meta := repo.FileObjectMeta{UID: h.UID, GID: h.GID, Mode: h.Mode, Ostreemeta: ostreemeta}
		if h.IsSymlink() {
			return r.WriteFileRawObject(csum, append([]byte(h.SymlinkTarget), 0), meta)
		}
		return r.WriteFileRawObject(csum, content, meta)
	case repo.ModeBareUserOnly:
		if h.IsSymlink() {
			return r.WriteSymlinkObject(csum, repo.ObjectFile, h.SymlinkTarget,
				repo.FileObjectMeta{Mode: h.Mode})
		}
		return r.WriteFileRawObject(csum, content, repo.FileObjectMeta{Mode: h.Mode})
	default:
		return fmt.Errorf("Unsupported repository mode")
	}
}

// --- apply-offline ------------------------------------------------------

// ApplyOfflineOptions controls ApplyOffline.
type ApplyOfflineOptions struct {
	// Repo is the destination repository.
	Repo *repo.Repo
	// Path points at a delta directory (containing superblock and parts) or
	// directly at the superblock file.
	Path string
	// Sign is the signature engine used to verify a signed delta, or nil.
	Sign *sign.Engine
	// SkipValidation executes the delta without checksumming the parts.
	SkipValidation bool
	// Out receives the success message of the signing engine, when any.
	Out *string
}

// ApplyOffline applies an already-downloaded static delta, matching
// ostree_repo_static_delta_execute_offline_with_signature().
func ApplyOffline(opts ApplyOfflineOptions) error {
	r := opts.Repo
	superPath := opts.Path
	if fi, err := os.Stat(superPath); err == nil && fi.IsDir() {
		superPath = filepath.Join(superPath, "superblock")
	}
	data, err := os.ReadFile(superPath)
	if err != nil {
		return fmt.Errorf("opening static delta: %w", err)
	}
	sb, err := ParseSuperblock(data)
	if err != nil {
		return err
	}

	signed, err := superblockIsSigned(data)
	if err != nil {
		return err
	}
	if signed {
		verifyDeltas, _ := r.Config().GetBool("core", "sign-verify-deltas", false)
		if verifyDeltas && opts.Sign == nil {
			return fmt.Errorf("Key is mandatory to check delta signature")
		}
		if opts.Sign != nil {
			msg, err := VerifySignatureData(sb, opts.Sign)
			if err != nil {
				return err
			}
			if opts.Out != nil {
				*opts.Out = msg
			}
		}
	}

	from := ""
	if sb.HasFrom {
		from = checksum.Hex(sb.From)
	}
	to := checksum.Hex(sb.To)
	if from != "" {
		have, err := r.HasObject(from, repo.ObjectCommit)
		if err != nil {
			return err
		}
		if !have {
			return fmt.Errorf("Commit %s, which is the delta source, is not in repository", from)
		}
	}
	if len(sb.Fallbacks) > 0 {
		return fmt.Errorf("Cannot execute delta offline: contains nonempty http fallback entries")
	}
	if err := writeDeltaCommit(r, sb, from, to); err != nil {
		return err
	}
	for i, pm := range sb.Parts {
		haveAll, err := PartHaveAllObjects(r, pm.Objects)
		if err != nil {
			return err
		}
		if haveAll {
			continue
		}
		partBytes, err := loadPartBytes(r, sb, superPath, i)
		if err != nil {
			return err
		}
		if !opts.SkipValidation {
			if err := VerifyPartChecksum(partBytes, pm.Checksum); err != nil {
				return err
			}
		}
		if err := ApplyPartFile(r, pm.Objects, partBytes); err != nil {
			return fmt.Errorf("Executing delta part %d: %w", i, err)
		}
	}
	return nil
}

// writeDeltaCommit writes the target commit (and its detached metadata from
// the superblock, when present) if it is not already stored.
func writeDeltaCommit(r *repo.Repo, sb *Superblock, from, to string) error {
	have, err := r.HasObject(to, repo.ObjectCommit)
	if err != nil {
		return err
	}
	if have {
		return nil
	}
	raw, err := sb.CommitBytes()
	if err != nil {
		return err
	}
	if actual := checksum.Hex(checksum.Sum(raw)); actual != to {
		return fmt.Errorf("Static delta target commit has checksum %s, expected %s", actual, to)
	}
	if sb.Metadata != nil {
		rel, err := DeltaPath(from, to, "commitmeta")
		if err != nil {
			return err
		}
		if v, ok := sb.Metadata.Get(rel); ok {
			var dict gvariant.Dict
			switch value := v.(type) {
			case gvariant.Variant:
				dict, _ = value.Value.(gvariant.Dict)
			case gvariant.Dict:
				dict = value
			}
			if dict != nil {
				if err := sign.WriteDetachedMetadata(r, to, dict); err != nil {
					return err
				}
			}
		}
	}
	if _, err := r.WriteObject(repo.ObjectCommit, raw); err != nil {
		return err
	}
	return nil
}

// superblockIsSigned reports whether data is wrapped in the signed
// container and contains at least one "ostree.sign." entry.
func superblockIsSigned(data []byte) (bool, error) {
	sb, err := ParseSuperblock(data)
	if err != nil {
		return false, err
	}
	if !sb.Signed {
		return false, nil
	}
	for _, e := range sb.Signatures {
		if k, ok := e.Key.(string); ok && strings.HasPrefix(k, "ostree.sign.") {
			return true, nil
		}
	}
	return false, nil
}

// VerifySignatureData verifies the signatures of a parsed superblock with
// engine.  It returns the engine's success message, or an error such as
// "no signature for 'ostree.sign.dummy' in static-delta superblock".
func VerifySignatureData(sb *Superblock, engine *sign.Engine) (string, error) {
	if !sb.Signed {
		return "", errors.New("no signatures in static-delta")
	}
	sigs, err := sign.SignatureBytes(sb.Signatures, engine.MetadataKey())
	if err != nil {
		return "", err
	}
	if sigs == nil {
		return "", fmt.Errorf("no signature for '%s' in static-delta superblock", engine.MetadataKey())
	}
	return engine.Verify(sb.Raw(), sigs)
}

// VerifySignature verifies the signature of a delta by name or path,
// mirroring ostree_repo_static_delta_verify_signature().
func VerifySignature(r *repo.Repo, id string, engine *sign.Engine) (string, error) {
	path, err := SuperblockFilePath(r, id)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("No such delta: %s", id)
		}
		return "", err
	}
	sb, err := ParseSuperblock(data)
	if err != nil {
		return "", err
	}
	signed, err := superblockIsSigned(data)
	if err != nil {
		return "", err
	}
	if !signed {
		return "", errors.New("no signatures in static-delta")
	}
	return VerifySignatureData(sb, engine)
}

// SuperblockFilePath resolves a delta identifier or a path to the
// superblock file of that delta.
func SuperblockFilePath(r *repo.Repo, id string) (string, error) {
	if strings.ContainsRune(id, '/') {
		fi, err := os.Stat(id)
		if err != nil {
			return "", err
		}
		if fi.IsDir() {
			return filepath.Join(id, "superblock"), nil
		}
		return id, nil
	}
	from, to, err := ParseDeltaName(id)
	if err != nil {
		return "", err
	}
	rel, err := DeltaSuperblockPath(from, to)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.Path(), rel), nil
}
