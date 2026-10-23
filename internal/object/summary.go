package object

import (
	"fmt"

	"ostree-go/internal/checksum"
	"ostree-go/internal/gvariant"
)

// Well-known summary additional-metadata keys, matching
// ostree-repo-private.h.
const (
	MetaSummaryLastModified     = "ostree.summary.last-modified"
	MetaSummaryExpires          = "ostree.summary.expires"
	MetaSummaryCollectionID     = "ostree.summary.collection-id"
	MetaSummaryCollectionMap    = "ostree.summary.collection-map"
	MetaSummaryMode             = "ostree.summary.mode"
	MetaSummaryTombstoneCommits = "ostree.summary.tombstone-commits"
	MetaSummaryIndexedDeltas    = "ostree.summary.indexed-deltas"
	MetaSummaryStaticDeltas     = "ostree.static-deltas"
)

// Well-known per-ref commit metadata keys in a summary file.
const (
	MetaSummaryCommitTimestamp = "ostree.commit.timestamp"
	MetaSummaryCommitVersion   = "ostree.commit.version"
)

// SummaryType is the GVariant type of a summary file.
var SummaryType = gvariant.MustParseType("(a(s(taya{sv}))a{sv})")

// MetadataType is the GVariant type of detached metadata and summary
// signatures.
var MetadataType = gvariant.MustParseType("a{sv}")

// SummaryRef is one ref entry in a summary. Size is the serialized size of
// the commit object, as stored in the (taya{sv}) tuple; the commit timestamp
// lives in Metadata under MetaSummaryCommitTimestamp.
type SummaryRef struct {
	Name     string
	Size     uint64
	Commit   string
	Metadata gvariant.Dict
}

// Summary is a parsed summary file.
type Summary struct {
	Refs     []SummaryRef
	Metadata gvariant.Dict
}

// ParseMetadata parses an a{sv} dictionary (detached metadata, summary
// signatures).
func ParseMetadata(data []byte) (gvariant.Dict, error) {
	v, err := gvariant.Decode(MetadataType, data)
	if err != nil {
		return nil, err
	}
	d, ok := v.(gvariant.Dict)
	if !ok {
		return nil, fmt.Errorf("metadata: not a dictionary")
	}
	return d, nil
}

// EncodeMetadata serializes an a{sv} dictionary.
func EncodeMetadata(d gvariant.Dict) ([]byte, error) {
	return gvariant.Encode(MetadataType, d)
}

// ParseSummary parses a summary file.
func ParseSummary(data []byte) (*Summary, error) {
	v, err := gvariant.Decode(SummaryType, data)
	if err != nil {
		return nil, fmt.Errorf("summary: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 2 {
		return nil, fmt.Errorf("summary: malformed tuple")
	}
	refs, ok := f[0].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("summary: refs is not an array")
	}
	s := &Summary{}
	for _, r := range refs {
		pair, ok := r.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("summary: malformed ref entry")
		}
		name, ok := pair[0].(string)
		if !ok {
			return nil, fmt.Errorf("summary: ref name is not a string")
		}
		info, ok := pair[1].([]gvariant.Value)
		if !ok || len(info) != 3 {
			return nil, fmt.Errorf("summary: malformed ref info")
		}
		size, ok := info[0].(uint64)
		if !ok {
			return nil, fmt.Errorf("summary: commit size is not uint64")
		}
		commit, ok := info[1].([]byte)
		if !ok || len(commit) != checksum.Size {
			return nil, fmt.Errorf("summary: invalid commit checksum for %q", name)
		}
		md, ok := info[2].(gvariant.Dict)
		if !ok {
			return nil, fmt.Errorf("summary: ref metadata is not a dictionary")
		}
		s.Refs = append(s.Refs, SummaryRef{
			Name:     name,
			Size:     size,
			Commit:   checksum.Hex(toArray(commit)),
			Metadata: md,
		})
	}
	md, ok := f[1].(gvariant.Dict)
	if !ok {
		return nil, fmt.Errorf("summary: metadata is not a dictionary")
	}
	s.Metadata = md
	return s, nil
}

// Encode serializes the summary.
func (s *Summary) Encode() ([]byte, error) {
	refs := make([]gvariant.Value, len(s.Refs))
	for i, r := range s.Refs {
		commit, err := checksum.FromHex(r.Commit)
		if err != nil {
			return nil, fmt.Errorf("summary ref %q: %w", r.Name, err)
		}
		refs[i] = []gvariant.Value{r.Name, []gvariant.Value{r.Size, commit[:], r.Metadata}}
	}
	return gvariant.Encode(SummaryType, []gvariant.Value{refs, s.Metadata})
}

// Checksum returns the sha256 of the encoded summary.
func (s *Summary) Checksum() (string, error) {
	data, err := s.Encode()
	if err != nil {
		return "", err
	}
	return checksum.Hex(checksum.Sum(data)), nil
}
