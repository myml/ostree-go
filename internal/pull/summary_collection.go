package pull

import (
	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
)

// CollectionRef identifies a ref inside an OSTree collection.
type CollectionRef struct {
	CollectionID string
	RefName      string
	// Checksum, when set, overrides the summary lookup (collection-ref pulls
	// may carry an explicit commit).
	Checksum string
}

// CollectionRefKey is the comparable form used for map keys.
func (c CollectionRef) Key() CollectionRef {
	return CollectionRef{CollectionID: c.CollectionID, RefName: c.RefName}
}

// LookupSummaryCollectionRef resolves COLLECTION-ID REF against raw summary
// bytes. It is the exported entry point used by `ostree find-remotes`.
func LookupSummaryCollectionRef(data []byte, collectionID, refName string) (string, bool) {
	summary, err := object.ParseSummary(data)
	if err != nil {
		return "", false
	}
	return summaryLookupCollectionRef(summary, collectionID, refName)
}

// summaryCollectionID returns the main collection ID recorded in a summary's
// additional metadata, or "" when absent.
func summaryCollectionID(s *object.Summary) string {
	id, _ := variantString(s.Metadata, object.MetaSummaryCollectionID)
	return id
}

// summaryCollectionMap decodes ostree.summary.collection-map:
// a{sa(s(taya{sv}))}, a map from collection ID to summary ref entries.
func summaryCollectionMap(s *object.Summary) (gvariant.Dict, bool) {
	v, ok := s.Metadata.Get(object.MetaSummaryCollectionMap)
	if !ok {
		return nil, false
	}
	av, ok := v.(gvariant.Variant)
	if !ok || av.Type.Kind != gvariant.KindArray || av.Type.Elem.Kind != gvariant.KindDictEntry {
		return nil, false
	}
	d, ok := av.Value.(gvariant.Dict)
	return d, ok
}

// summaryLookupCollectionRef resolves a collection ref against a summary,
// following find_remotes_cb() / the collection-map handling in
// ostree-repo-pull.c. It returns the commit checksum and whether the ref was
// found.
func summaryLookupCollectionRef(s *object.Summary, collectionID, refName string) (string, bool) {
	mainID := summaryCollectionID(s)
	// The summary's top-level ref array describes the main collection.
	// Repositories that predate collection metadata (or whose summary writer
	// omits it) list their refs there, so treat a missing main ID as matching
	// any requested collection.
	if collectionID == "" || collectionID == mainID || mainID == "" {
		for _, ref := range s.Refs {
			if ref.Name == refName {
				return ref.Commit, true
			}
		}
		if mainID == "" {
			return "", false
		}
	}
	cm, ok := summaryCollectionMap(s)
	if !ok {
		return "", false
	}
	for _, e := range cm {
		key, ok := e.Key.(string)
		if !ok || key != collectionID {
			continue
		}
		entries, ok := e.Value.([]gvariant.Value)
		if !ok {
			return "", false
		}
		for _, entry := range entries {
			tuple, ok := entry.([]gvariant.Value)
			if !ok || len(tuple) != 2 {
				continue
			}
			name, ok := tuple[0].(string)
			if !ok || name != refName {
				continue
			}
			target, ok := tuple[1].([]gvariant.Value)
			if !ok || len(target) != 3 {
				return "", false
			}
			data, ok := target[1].([]byte)
			if !ok || len(data) != checksum.Size {
				return "", false
			}
			var sum [checksum.Size]byte
			copy(sum[:], data)
			return checksum.Hex(sum), true
		}
	}
	return "", false
}

// summaryAllCollectionRefs returns every ref of the summary that belongs to
// collectionID, including the main collection's top-level refs.
func summaryAllCollectionRefs(s *object.Summary, collectionID string) []CollectionRef {
	mainID := summaryCollectionID(s)
	var out []CollectionRef
	appendMain := collectionID == "" || collectionID == mainID || mainID == ""
	if appendMain {
		id := collectionID
		if id == "" {
			id = mainID
		}
		for _, ref := range s.Refs {
			out = append(out, CollectionRef{CollectionID: id, RefName: ref.Name, Checksum: ref.Commit})
		}
		if collectionID == "" || mainID == "" {
			return out
		}
	}
	cm, ok := summaryCollectionMap(s)
	if !ok {
		return out
	}
	for _, e := range cm {
		key, ok := e.Key.(string)
		if !ok || key != collectionID {
			continue
		}
		entries, ok := e.Value.([]gvariant.Value)
		if !ok {
			continue
		}
		for _, entry := range entries {
			tuple, ok := entry.([]gvariant.Value)
			if !ok || len(tuple) != 2 {
				continue
			}
			name, ok := tuple[0].(string)
			if !ok {
				continue
			}
			target, ok := tuple[1].([]gvariant.Value)
			if !ok || len(target) != 3 {
				continue
			}
			data, ok := target[1].([]byte)
			if !ok || len(data) != checksum.Size {
				continue
			}
			var sum [checksum.Size]byte
			copy(sum[:], data)
			out = append(out, CollectionRef{CollectionID: key, RefName: name, Checksum: checksum.Hex(sum)})
		}
	}
	return out
}
