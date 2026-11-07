// Package object implements the OSTree object formats: commit, dirtree,
// dirmeta, and file headers.
package object

import (
	"fmt"
)

// Type is an OSTree object type.
type Type int

const (
	TypeFile Type = 1 + iota
	TypeDirtree
	TypeDirmeta
	TypeCommit
	TypeTombstoneCommit
	TypeCommitMeta
	TypePayloadLink
	TypeFileXattrs
	TypeFileXattrsLink
)

var typeNames = map[Type]string{
	TypeFile:            "file",
	TypeDirtree:         "dirtree",
	TypeDirmeta:         "dirmeta",
	TypeCommit:          "commit",
	TypeTombstoneCommit: "tombstone-commit",
	TypeCommitMeta:      "commitmeta",
	TypePayloadLink:     "payload-link",
	TypeFileXattrs:      "file-xattrs",
	TypeFileXattrsLink:  "file-xattrs-link",
}

// String returns the on-disk type suffix.
func (t Type) String() string {
	s, ok := typeNames[t]
	if !ok {
		return fmt.Sprintf("invalid(%d)", int(t))
	}
	return s
}

// IsMeta reports whether the object is stored as a GVariant metadata
// object (and therefore never zlib-compressed).
func (t Type) IsMeta() bool {
	return t >= TypeDirtree && t <= TypeCommitMeta
}

// TypeFromString parses an on-disk object type suffix.
func TypeFromString(s string) (Type, error) {
	for t, name := range typeNames {
		if name == s {
			return t, nil
		}
	}
	return 0, fmt.Errorf("unknown object type %q", s)
}
