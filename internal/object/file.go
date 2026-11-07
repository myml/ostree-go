package object

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/myml/ostree-go/internal/gvariant"
)

const (
	modeTypeMask    = 0170000
	modeDir         = 0040000
	modeReg         = 0100000
	modeLnk         = 0120000
	modeChr         = 0020000
	modeBlk         = 0060000
	modeFifo        = 0010000
	modeSock        = 0140000
	allowedModeBits = modeTypeMask | 07777
)

var (
	fileHeaderType    = gvariant.MustParseType("(uuuusa(ayay))")
	archiveHeaderType = gvariant.MustParseType("(tuuuusa(ayay))")
)

// FileHeader is the metadata header for a file object. It is not stored
// in bare repositories (where the filesystem carries the metadata), but
// it is part of the file object checksum and is stored in archive mode.
type FileHeader struct {
	Size          uint64 // uncompressed size; only meaningful for archive
	UID           uint32
	GID           uint32
	Mode          uint32
	Rdev          uint32
	SymlinkTarget string
	Xattrs        []XAttr
}

// EncodeStream returns the length-prefixed (uuuusa(ayay)) header used in
// checksums and in the non-archive object stream format.
func (h *FileHeader) EncodeStream() ([]byte, error) {
	xattrs, err := encodeXattrs(h.Xattrs)
	if err != nil {
		return nil, err
	}
	v, err := gvariant.Encode(fileHeaderType, []gvariant.Value{
		be32(h.UID), be32(h.GID), be32(h.Mode), h.Rdev, h.SymlinkTarget, xattrs,
	})
	if err != nil {
		return nil, err
	}
	return prefixVariant(v), nil
}

// EncodeArchive returns the length-prefixed (tuuuusa(ayay)) header stored
// in archive repositories.
func (h *FileHeader) EncodeArchive() ([]byte, error) {
	xattrs, err := encodeXattrs(h.Xattrs)
	if err != nil {
		return nil, err
	}
	v, err := gvariant.Encode(archiveHeaderType, []gvariant.Value{
		be64(h.Size), be32(h.UID), be32(h.GID), be32(h.Mode), h.Rdev, h.SymlinkTarget, xattrs,
	})
	if err != nil {
		return nil, err
	}
	return prefixVariant(v), nil
}

// ParseStreamHeader parses a length-prefixed (uuuusa(ayay)) header and
// returns the remaining bytes.
func ParseStreamHeader(data []byte) (*FileHeader, []byte, error) {
	return parseFileHeader(data, false)
}

// ParseArchiveHeader parses a length-prefixed (tuuuusa(ayay)) header and
// returns the remaining bytes.
func ParseArchiveHeader(data []byte) (*FileHeader, []byte, error) {
	return parseFileHeader(data, true)
}

func parseFileHeader(data []byte, archive bool) (*FileHeader, []byte, error) {
	if len(data) < 8 {
		return nil, nil, fmt.Errorf("file header: too short")
	}
	n := int(binary.BigEndian.Uint32(data[:4]))
	if n < 0 || 8+n > len(data) {
		return nil, nil, fmt.Errorf("file header: invalid length %d", n)
	}
	variant := data[8 : 8+n]
	rest := data[8+n:]
	typ := fileHeaderType
	if archive {
		typ = archiveHeaderType
	}
	v, err := gvariant.Decode(typ, variant)
	if err != nil {
		return nil, nil, fmt.Errorf("file header: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok {
		return nil, nil, fmt.Errorf("file header: malformed tuple")
	}
	h := &FileHeader{}
	var idx int
	if archive {
		size, ok := f[0].(uint64)
		if !ok {
			return nil, nil, fmt.Errorf("file header: invalid size")
		}
		h.Size = fromBE64(size)
		idx = 1
	}
	uid, ok1 := f[idx].(uint32)
	gid, ok2 := f[idx+1].(uint32)
	mode, ok3 := f[idx+2].(uint32)
	rdev, ok4 := f[idx+3].(uint32)
	target, ok5 := f[idx+4].(string)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return nil, nil, fmt.Errorf("file header: malformed fields")
	}
	h.UID, h.GID, h.Mode = fromBE32(uid), fromBE32(gid), fromBE32(mode)
	h.Rdev = rdev
	h.SymlinkTarget = target
	xattrs, err := decodeXattrs(f[idx+5])
	if err != nil {
		return nil, nil, err
	}
	h.Xattrs = xattrs
	if err := h.Validate(); err != nil {
		return nil, nil, err
	}
	return h, rest, nil
}

func prefixVariant(v []byte) []byte {
	out := make([]byte, 8+len(v))
	binary.BigEndian.PutUint32(out[:4], uint32(len(v)))
	copy(out[8:], v)
	return out
}

// Validate checks file header invariants.
func (h *FileHeader) Validate() error {
	if h.Mode&^allowedModeBits != 0 {
		return fmt.Errorf("file header: mode %#o has invalid bits", h.Mode)
	}
	switch h.Mode & modeTypeMask {
	case modeReg, modeLnk, modeChr, modeBlk, modeFifo, modeSock:
	default:
		return fmt.Errorf("file header: invalid mode type %#o", h.Mode&modeTypeMask)
	}
	if _, err := Canonicalize(h.Xattrs); err != nil {
		return err
	}
	return nil
}

// IsSymlink reports whether the file is a symbolic link.
func (h *FileHeader) IsSymlink() bool { return h.Mode&modeTypeMask == modeLnk }

// IsRegular reports whether the file is a regular file.
func (h *FileHeader) IsRegular() bool { return h.Mode&modeTypeMask == modeReg }

// ChecksumReader computes the OSTree file object checksum:
// sha256(stream header || uncompressed content).
func (h *FileHeader) ChecksumReader(r io.Reader) (string, error) {
	header, err := h.EncodeStream()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write(header)
	if r != nil {
		if _, err := io.Copy(hash, r); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Checksum computes the file object checksum for in-memory content.
func (h *FileHeader) Checksum(content []byte) (string, error) {
	return h.ChecksumReader(bytesReader(content))
}

type sliceReader struct {
	b []byte
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

// EncodeFileMeta returns the bare-user `user.ostreemeta` payload, a
// (uuua(ayay)) variant with big-endian numeric fields.
func EncodeFileMeta(uid, gid, mode uint32, xattrs []XAttr) ([]byte, error) {
	encoded, err := encodeXattrs(xattrs)
	if err != nil {
		return nil, err
	}
	return gvariant.Encode(DirmetaType, []gvariant.Value{be32(uid), be32(gid), be32(mode), encoded})
}

// ParseFileMeta parses a bare-user `user.ostreemeta` payload.
func ParseFileMeta(data []byte) (*FileHeader, error) {
	v, err := gvariant.Decode(DirmetaType, data)
	if err != nil {
		return nil, fmt.Errorf("file meta: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 4 {
		return nil, fmt.Errorf("file meta: malformed tuple")
	}
	uid, ok1 := f[0].(uint32)
	gid, ok2 := f[1].(uint32)
	mode, ok3 := f[2].(uint32)
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("file meta: malformed numeric fields")
	}
	xattrs, err := decodeXattrs(f[3])
	if err != nil {
		return nil, err
	}
	h := &FileHeader{UID: fromBE32(uid), GID: fromBE32(gid), Mode: fromBE32(mode), Xattrs: xattrs}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h, nil
}
