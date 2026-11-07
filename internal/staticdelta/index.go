package staticdelta

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/myml/ostree-go/internal/checksum"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/object"
	"github.com/myml/ostree-go/internal/repo"
)

// ListIndexes enumerates the target commits that have a static delta index,
// like ostree_repo_list_static_delta_indexes().
func ListIndexes(r *repo.Repo) ([]string, error) {
	root := filepath.Join(r.Path(), DeltaIndexesDir)
	fanouts, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, fanout := range fanouts {
		if !fanout.IsDir() || len(fanout.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, fanout.Name()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(name, ".index") {
				continue
			}
			base := strings.TrimSuffix(name, ".index")
			sum, err := ParseModifiedBase64(fanout.Name() + base)
			if err != nil {
				continue
			}
			out = append(out, checksum.Hex(sum))
		}
	}
	sort.Strings(out)
	return out, nil
}

// SuperblockDigest returns the sha256 of a delta's on-disk superblock bytes,
// matching _ostree_repo_static_delta_superblock_digest().
func SuperblockDigest(r *repo.Repo, from, to string) ([]byte, error) {
	rel, err := DeltaSuperblockPath(from, to)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(r.Path(), rel))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return sum[:], nil
}

// ParseDeltaDict extracts the "ostree.static-deltas" dictionary from a
// summary or a delta index top-level a{sv}.  The returned map is keyed by
// delta name and holds the 32-byte superblock digest.
func ParseDeltaDict(dict gvariant.Dict) map[string][32]byte {
	if dict == nil {
		return nil
	}
	v, ok := dict.Get(object.MetaSummaryStaticDeltas)
	if !ok {
		return nil
	}
	var inner gvariant.Value
	if variant, ok := v.(gvariant.Variant); ok {
		inner = variant.Value
	} else {
		inner = v
	}
	deltas, ok := inner.(gvariant.Dict)
	if !ok {
		return nil
	}
	out := make(map[string][32]byte, len(deltas))
	for _, e := range deltas {
		name, ok := e.Key.(string)
		if !ok {
			continue
		}
		var digest []byte
		switch value := e.Value.(type) {
		case gvariant.Variant:
			digest, _ = value.Value.([]byte)
		case []byte:
			digest = value
		}
		if len(digest) != 32 {
			continue
		}
		var sum [32]byte
		copy(sum[:], digest)
		out[name] = sum
	}
	return out
}

// LoadIndexDeltas reads the delta index for a target commit.  The second
// return value is false when no index file exists.
func LoadIndexDeltas(r *repo.Repo, to string) (map[string][32]byte, bool, error) {
	rel, err := DeltaIndexPath(to)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(filepath.Join(r.Path(), rel))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	v, err := gvariant.Decode(gvariant.MustParseType("a{sv}"), data)
	if err != nil {
		return nil, true, fmt.Errorf("parsing delta index: %w", err)
	}
	dict, ok := v.(gvariant.Dict)
	if !ok {
		return nil, true, fmt.Errorf("parsing delta index: not a dictionary")
	}
	return ParseDeltaDict(dict), true, nil
}

// EncodeIndexDeltas serializes the a{sv} delta index.
func EncodeIndexDeltas(deltas map[string][32]byte) ([]byte, error) {
	inner := gvariant.Dict{}
	for _, name := range sortedKeys(deltas) {
		sum := deltas[name]
		inner.Set(name, gvariant.Variant{
			Type:  gvariant.MustParseType("ay"),
			Value: sum[:],
		})
	}
	top := gvariant.Dict{}
	top.Set(object.MetaSummaryStaticDeltas, gvariant.Variant{
		Type:  gvariant.MustParseType("a{sv}"),
		Value: inner,
	})
	return gvariant.Encode(gvariant.MustParseType("a{sv}"), top)
}

func sortedKeys(m map[string][32]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ReindexOptions controls Reindex.
type ReindexOptions struct {
	// ToCommit limits the regeneration to one target checksum.
	ToCommit string
}

// Reindex regenerates the delta indexes, matching
// ostree_repo_static_delta_reindex().
func Reindex(r *repo.Repo, opts ReindexOptions) error {
	unlock, err := r.Lock(true)
	if err != nil {
		return err
	}
	defer unlock()

	// Pull relies on this configuration key to know that indexes may exist.
	indexed, err := r.Config().GetBool("core", "indexed-deltas", false)
	if err != nil {
		return err
	}
	if !indexed {
		cfg := r.CloneConfig()
		cfg.Set("core", "indexed-deltas", "true")
		if err := r.WriteConfig(cfg); err != nil {
			return err
		}
	}

	// target checksum -> list of from checksums ("" is a scratch delta)
	targets := map[string][]string{}
	// Old indexes must be regenerated or removed.
	oldIndexes, err := ListIndexes(r)
	if err != nil {
		return err
	}
	for _, to := range oldIndexes {
		if opts.ToCommit == "" || opts.ToCommit == to {
			targets[to] = nil
		}
	}
	if opts.ToCommit != "" {
		if !checksum.IsHex(opts.ToCommit) {
			return fmt.Errorf("Invalid rev %s", opts.ToCommit)
		}
		targets[opts.ToCommit] = nil
	} else {
		// Start from a clean slate: ListIndexes seeded the deletion set,
		// existing targets without deltas stay nil and are removed below.
	}
	names, err := List(r)
	if err != nil {
		return err
	}
	for _, name := range names {
		from, to, err := ParseDeltaName(name)
		if err != nil {
			return err
		}
		if opts.ToCommit != "" && to != opts.ToCommit {
			continue
		}
		targets[to] = append(targets[to], from)
	}

	for to, froms := range targets {
		rel, err := DeltaIndexPath(to)
		if err != nil {
			return err
		}
		abs := filepath.Join(r.Path(), rel)
		if len(froms) == 0 {
			if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		froms = append([]string(nil), froms...)
		sort.Strings(froms)
		deltas := map[string][32]byte{}
		for _, from := range froms {
			digest, err := SuperblockDigest(r, from, to)
			if err != nil {
				// A stale directory without superblock is ignored.
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return err
			}
			name := to
			if from != "" {
				name = from + "-" + to
			}
			var sum [32]byte
			copy(sum[:], digest)
			deltas[name] = sum
		}
		if len(deltas) == 0 {
			_ = os.Remove(abs)
			continue
		}
		data, err := EncodeIndexDeltas(deltas)
		if err != nil {
			return err
		}
		if existing, err := os.ReadFile(abs); err == nil && bytes.Equal(existing, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, data, 0644); err != nil {
			return err
		}
	}
	return nil
}
