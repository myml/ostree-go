package object

import (
	"bytes"
	"fmt"
	"math/bits"
	"sort"

	"github.com/myml/ostree-go/internal/gvariant"
)

// XAttr is one extended attribute. Name does not include the trailing NUL.
type XAttr struct {
	Name  string
	Value []byte
}

// Canonicalize sorts xattrs by name and validates them.
func Canonicalize(xattrs []XAttr) ([]XAttr, error) {
	out := make([]XAttr, len(xattrs))
	copy(out, xattrs)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for i, x := range out {
		if x.Name == "" {
			return nil, fmt.Errorf("empty xattr name")
		}
		if bytes.IndexByte([]byte(x.Name), 0) >= 0 {
			return nil, fmt.Errorf("xattr name %q contains NUL", x.Name)
		}
		if i > 0 && out[i-1].Name == x.Name {
			return nil, fmt.Errorf("duplicate xattr %q", x.Name)
		}
	}
	return out, nil
}

// encodeXattrs produces the GVariant value for a(ayay): each name is a
// bytestring (NUL-terminated bytes) and each value is a byte array.
func encodeXattrs(xattrs []XAttr) ([]gvariant.Value, error) {
	canon, err := Canonicalize(xattrs)
	if err != nil {
		return nil, err
	}
	out := make([]gvariant.Value, len(canon))
	for i, x := range canon {
		name := append([]byte(nil), x.Name...)
		name = append(name, 0)
		out[i] = []gvariant.Value{name, x.Value}
	}
	return out, nil
}

// decodeXattrs parses a(ayay).
func decodeXattrs(v gvariant.Value) ([]XAttr, error) {
	arr, ok := v.([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("xattrs: expected array, got %T", v)
	}
	out := make([]XAttr, 0, len(arr))
	for _, e := range arr {
		pair, ok := e.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("xattrs: malformed entry")
		}
		name, ok := pair[0].([]byte)
		if !ok || len(name) == 0 || name[len(name)-1] != 0 {
			return nil, fmt.Errorf("xattrs: malformed name")
		}
		if bytes.IndexByte(name[:len(name)-1], 0) >= 0 {
			return nil, fmt.Errorf("xattrs: name contains NUL")
		}
		value, ok := pair[1].([]byte)
		if !ok {
			return nil, fmt.Errorf("xattrs: malformed value")
		}
		out = append(out, XAttr{Name: string(name[:len(name)-1]), Value: value})
	}
	// Normal form requires sorting by name.
	for i := 1; i < len(out); i++ {
		if out[i-1].Name >= out[i].Name {
			return nil, fmt.Errorf("xattrs: not sorted or duplicate at %q", out[i].Name)
		}
	}
	return out, nil
}

// be32 converts a host uint32 to the value that produces big-endian
// bytes in the (little-endian) GVariant encoding. OSTree stores uid,
// gid, mode and size using GUINT32_TO_BE/GUINT64_TO_BE.
func be32(v uint32) uint32 { return bits.ReverseBytes32(v) }

// be64 is the 64-bit equivalent of be32.
func be64(v uint64) uint64 { return bits.ReverseBytes64(v) }

// fromBE32 converts a big-endian stored value back to host order.
func fromBE32(v uint32) uint32 { return bits.ReverseBytes32(v) }

func fromBE64(v uint64) uint64 { return bits.ReverseBytes64(v) }
