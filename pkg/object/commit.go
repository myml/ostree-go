package object

import (
	"bytes"
	"fmt"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
)

// CommitType is the GVariant type of a commit object.
var CommitType = gvariant.MustParseType("(a{sv}aya(say)sstayay)")

// Well-known commit metadata keys.
const (
	MetaVersion           = "version"
	MetaArchitecture      = "ostree.architecture"
	MetaEndOfLife         = "ostree.endoflife"
	MetaEndOfLifeRebase   = "ostree.endoflife-rebase"
	MetaSourceTitle       = "ostree.source-title"
	MetaRefBinding        = "ostree.ref-binding"
	MetaCollectionBinding = "ostree.collection-binding"
	MetaSizes             = "ostree.sizes"
	MetaComposefsDigestV0 = "ostree.composefs.digest.v0"
	MetaGpgSignatures     = "ostree.gpgsigs"
)

// Commit is a parsed OSTree commit object.
type Commit struct {
	Metadata  gvariant.Dict
	Parent    string // empty or 64 hex characters
	Related   []string
	Subject   string
	Body      string
	Timestamp uint64
	RootTree  string // 64 hex characters
	RootMeta  string // 64 hex characters
}

// ParseCommit parses a commit object.
func ParseCommit(data []byte) (*Commit, error) {
	v, err := gvariant.Decode(CommitType, data)
	if err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 8 {
		return nil, fmt.Errorf("commit: malformed tuple")
	}
	meta, ok := f[0].(gvariant.Dict)
	if !ok {
		return nil, fmt.Errorf("commit: metadata is not a dictionary")
	}
	c := &Commit{Metadata: meta}
	if b, ok := f[1].([]byte); ok {
		switch len(b) {
		case 0:
		case checksum.Size:
			c.Parent = checksum.Hex(toArray(b))
		default:
			return nil, fmt.Errorf("commit: parent checksum has length %d", len(b))
		}
	} else {
		return nil, fmt.Errorf("commit: parent is not bytes")
	}
	rel, ok := f[2].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("commit: related is not an array")
	}
	for _, r := range rel {
		pair, ok := r.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("commit: malformed related entry")
		}
		name, ok := pair[0].(string)
		if !ok {
			return nil, fmt.Errorf("commit: related name is not a string")
		}
		if b, ok := pair[1].([]byte); ok && len(b) != 0 && len(b) != checksum.Size {
			return nil, fmt.Errorf("commit: related checksum has length %d", len(b))
		}
		c.Related = append(c.Related, name)
	}
	var okbool bool
	if c.Subject, okbool = f[3].(string); !okbool {
		return nil, fmt.Errorf("commit: subject is not a string")
	}
	if c.Body, okbool = f[4].(string); !okbool {
		return nil, fmt.Errorf("commit: body is not a string")
	}
	ts, ok := f[5].(uint64)
	if !ok {
		return nil, fmt.Errorf("commit: timestamp is not uint64")
	}
	c.Timestamp = fromBE64(ts)
	tree, ok := f[6].([]byte)
	if !ok || len(tree) != checksum.Size {
		return nil, fmt.Errorf("commit: invalid root tree checksum")
	}
	metaCsum, ok := f[7].([]byte)
	if !ok || len(metaCsum) != checksum.Size {
		return nil, fmt.Errorf("commit: invalid root dirmeta checksum")
	}
	c.RootTree = checksum.Hex(toArray(tree))
	c.RootMeta = checksum.Hex(toArray(metaCsum))
	return c, nil
}

// Encode serializes the commit.
func (c *Commit) Encode() ([]byte, error) {
	parent := []byte{}
	if c.Parent != "" {
		sum, err := checksum.FromHex(c.Parent)
		if err != nil {
			return nil, fmt.Errorf("commit parent: %w", err)
		}
		parent = append([]byte(nil), sum[:]...)
	}
	related := make([]gvariant.Value, len(c.Related))
	for i, name := range c.Related {
		related[i] = []gvariant.Value{name, []byte{}}
	}
	tree, err := checksum.FromHex(c.RootTree)
	if err != nil {
		return nil, fmt.Errorf("commit root tree: %w", err)
	}
	meta, err := checksum.FromHex(c.RootMeta)
	if err != nil {
		return nil, fmt.Errorf("commit root dirmeta: %w", err)
	}
	val := []gvariant.Value{
		c.Metadata,
		parent,
		related,
		c.Subject,
		c.Body,
		be64(c.Timestamp),
		tree[:],
		meta[:],
	}
	return gvariant.Encode(CommitType, val)
}

// Checksum returns the object checksum of the encoded commit.
func (c *Commit) Checksum() (string, error) {
	data, err := c.Encode()
	if err != nil {
		return "", err
	}
	return checksum.Hex(checksum.Sum(data)), nil
}

// ContentChecksum returns the checksum of the root tree and dirmeta
// checksums (ostree_commit_get_content_checksum).
func (c *Commit) ContentChecksum() (string, error) {
	tree, err := checksum.FromHex(c.RootTree)
	if err != nil {
		return "", err
	}
	meta, err := checksum.FromHex(c.RootMeta)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	buf.Write(tree[:])
	buf.Write(meta[:])
	return checksum.Hex(checksum.Sum(buf.Bytes())), nil
}

// GetMetadata returns a metadata value.
func (c *Commit) GetMetadata(key string) (gvariant.Value, bool) {
	return c.Metadata.Get(key)
}

// GetMetadataString returns a string metadata value.
func (c *Commit) GetMetadataString(key string) (string, bool) {
	v, ok := c.Metadata.Get(key)
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

// SetMetadata sets a metadata value. Plain Go values are wrapped in a
// variant with an inferred type; a gvariant.Variant is stored as-is.
func (c *Commit) SetMetadata(key string, value gvariant.Value) error {
	if _, ok := value.(gvariant.Variant); !ok {
		switch x := value.(type) {
		case []string:
			arr := make([]gvariant.Value, len(x))
			for i, s := range x {
				arr[i] = s
			}
			value = gvariant.Variant{Type: gvariant.MustParseType("as"), Value: arr}
		case bool:
			value = gvariant.Variant{Type: gvariant.MustParseType("b"), Value: x}
		case uint32:
			value = gvariant.Variant{Type: gvariant.MustParseType("u"), Value: x}
		case uint64:
			value = gvariant.Variant{Type: gvariant.MustParseType("t"), Value: x}
		case string:
			value = gvariant.Variant{Type: gvariant.MustParseType("s"), Value: x}
		case []byte:
			value = gvariant.Variant{Type: gvariant.MustParseType("ay"), Value: x}
		default:
			return fmt.Errorf("commit: cannot infer variant type for %T", value)
		}
	}
	c.Metadata.Set(key, value)
	return nil
}

func toArray(b []byte) [checksum.Size]byte {
	var out [checksum.Size]byte
	copy(out[:], b)
	return out
}
