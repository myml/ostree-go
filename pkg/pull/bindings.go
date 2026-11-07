package pull

import (
	"fmt"
	"strings"

	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/object"
)

// verifyCommitBindings implements _ostree_repo_verify_bindings(): it checks
// that the commit's ostree.ref-binding metadata contains the requested ref
// (when pulling through a branch) and that its ostree.collection-binding
// matches the collection the commit was fetched from.
//
// A nil/empty collectionID disables the collection check, and an empty
// refName (a commit fetched by checksum) disables the ref check, exactly like
// the C implementation.
func verifyCommitBindings(collectionID, refName string, commit *object.Commit) error {
	refs, haveRefBinding := variantStringArray(commit.Metadata, object.MetaRefBinding)
	if !haveRefBinding {
		if collectionID == "" {
			return nil
		}
		return fmt.Errorf("Expected commit metadata to have ref binding information, found none")
	}
	if refName != "" {
		found := false
		for _, ref := range refs {
			if ref == refName {
				found = true
				break
			}
		}
		if !found {
			var quoted []string
			for _, ref := range refs {
				quoted = append(quoted, "\u2018"+ref+"\u2019")
			}
			refsStr := "no refs"
			if len(quoted) > 0 {
				refsStr = strings.Join(quoted, ", ")
			}
			return fmt.Errorf("Commit has no requested ref \u2018%s\u2019 in ref binding metadata (%s)",
				refName, refsStr)
		}
	}
	if collectionID != "" {
		binding, ok := variantString(commit.Metadata, object.MetaCollectionBinding)
		if !ok {
			return fmt.Errorf("Expected commit metadata to have collection ID binding information, found none")
		}
		if binding != collectionID {
			return fmt.Errorf("Commit has collection ID \u2018%s\u2019 in collection binding metadata, while the remote it came from has collection ID \u2018%s\u2019",
				binding, collectionID)
		}
	}
	return nil
}

// variantString extracts a string value from a metadata dictionary.
func variantString(d gvariant.Dict, key string) (string, bool) {
	v, ok := d.Get(key)
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

// variantStringArray extracts a string array value from a metadata
// dictionary.
func variantStringArray(d gvariant.Dict, key string) ([]string, bool) {
	v, ok := d.Get(key)
	if !ok {
		return nil, false
	}
	av, ok := v.(gvariant.Variant)
	if !ok || av.Type.Kind != gvariant.KindArray || av.Type.Elem.Kind != gvariant.KindString {
		return nil, false
	}
	vals, ok := av.Value.([]gvariant.Value)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(vals))
	for _, e := range vals {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
