package object

import (
	"fmt"

	"github.com/myml/ostree-go/pkg/gvariant"
)

// DirmetaType is the GVariant type of a dirmeta object.
var DirmetaType = gvariant.MustParseType("(uuua(ayay))")

// Dirmeta is a parsed dirmeta object.
type Dirmeta struct {
	UID    uint32
	GID    uint32
	Mode   uint32
	Xattrs []XAttr
}

// ParseDirmeta parses a dirmeta object.
func ParseDirmeta(data []byte) (*Dirmeta, error) {
	v, err := gvariant.Decode(DirmetaType, data)
	if err != nil {
		return nil, fmt.Errorf("dirmeta: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 4 {
		return nil, fmt.Errorf("dirmeta: malformed tuple")
	}
	uid, ok1 := f[0].(uint32)
	gid, ok2 := f[1].(uint32)
	mode, ok3 := f[2].(uint32)
	if !ok1 || !ok2 || !ok3 {
		return nil, fmt.Errorf("dirmeta: malformed numeric fields")
	}
	xattrs, err := decodeXattrs(f[3])
	if err != nil {
		return nil, err
	}
	d := &Dirmeta{UID: fromBE32(uid), GID: fromBE32(gid), Mode: fromBE32(mode), Xattrs: xattrs}
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return d, nil
}

// Encode serializes the dirmeta.
func (d *Dirmeta) Encode() ([]byte, error) {
	xattrs, err := encodeXattrs(d.Xattrs)
	if err != nil {
		return nil, err
	}
	return gvariant.Encode(DirmetaType, []gvariant.Value{be32(d.UID), be32(d.GID), be32(d.Mode), xattrs})
}

// Validate checks the dirmeta invariants enforced by OSTree.
func (d *Dirmeta) Validate() error {
	if d.Mode&modeTypeMask != modeDir {
		return fmt.Errorf("dirmeta: mode %#o is not a directory", d.Mode)
	}
	if d.Mode&^allowedModeBits != 0 {
		return fmt.Errorf("dirmeta: mode %#o has invalid bits", d.Mode)
	}
	if _, err := Canonicalize(d.Xattrs); err != nil {
		return err
	}
	return nil
}
