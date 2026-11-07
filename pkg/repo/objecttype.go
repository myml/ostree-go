package repo

import "fmt"

// ObjectType is an OSTree object type. The numeric values match the libostree
// OstreeObjectType enum, and the string suffixes match
// ostree_object_type_to_string().
type ObjectType int

const (
	ObjectFile            ObjectType = 1
	ObjectDirtree         ObjectType = 2
	ObjectDirmeta         ObjectType = 3
	ObjectCommit          ObjectType = 4
	ObjectTombstoneCommit ObjectType = 5
	ObjectCommitmeta      ObjectType = 6
	ObjectPayloadLink     ObjectType = 7
	ObjectFileXattrs      ObjectType = 8
	ObjectFileXattrsLink  ObjectType = 9
)

// Valid reports whether t is a known object type.
func (t ObjectType) Valid() bool {
	return t >= ObjectFile && t <= ObjectFileXattrsLink
}

// IsMeta reports whether t is stored as a metadata object. Metadata objects
// are never compressed and never get a "z" suffix, even in archive
// repositories (libostree's OSTREE_OBJECT_TYPE_IS_META covers 2..6).
func (t ObjectType) IsMeta() bool {
	return t >= ObjectDirtree && t <= ObjectCommitmeta
}

// String returns the object type suffix used in loose object filenames.
func (t ObjectType) String() string {
	switch t {
	case ObjectFile:
		return "file"
	case ObjectDirtree:
		return "dirtree"
	case ObjectDirmeta:
		return "dirmeta"
	case ObjectCommit:
		return "commit"
	case ObjectTombstoneCommit:
		return "tombstone-commit"
	case ObjectCommitmeta:
		return "commitmeta"
	case ObjectPayloadLink:
		return "payload-link"
	case ObjectFileXattrs:
		return "file-xattrs"
	case ObjectFileXattrsLink:
		return "file-xattrs-link"
	default:
		return fmt.Sprintf("ObjectType(%d)", int(t))
	}
}

// ParseObjectType parses an object type suffix such as "dirtree".
func ParseObjectType(s string) (ObjectType, error) {
	switch s {
	case "file":
		return ObjectFile, nil
	case "dirtree":
		return ObjectDirtree, nil
	case "dirmeta":
		return ObjectDirmeta, nil
	case "commit":
		return ObjectCommit, nil
	case "tombstone-commit":
		return ObjectTombstoneCommit, nil
	case "commitmeta":
		return ObjectCommitmeta, nil
	case "payload-link":
		return ObjectPayloadLink, nil
	case "file-xattrs":
		return ObjectFileXattrs, nil
	case "file-xattrs-link":
		return ObjectFileXattrsLink, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidObjectType, s)
	}
}
