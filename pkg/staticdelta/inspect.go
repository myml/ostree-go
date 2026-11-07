package staticdelta

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
	"github.com/myml/ostree-go/pkg/repo"
)

// List enumerates the static deltas in the repository, returning the
// delta names ("<from>-<to>" or "<to>") in sorted order.
func List(r *repo.Repo) ([]string, error) {
	root := filepath.Join(r.Path(), "deltas")
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
		subs, err := os.ReadDir(filepath.Join(root, fanout.Name()))
		if err != nil {
			return nil, err
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			dir := filepath.Join(root, fanout.Name(), sub.Name())
			if _, err := os.Stat(filepath.Join(dir, "superblock")); err != nil {
				continue
			}
			from, to, ok := decodeDeltaDirName(fanout.Name(), sub.Name())
			if !ok {
				continue
			}
			if from != "" {
				out = append(out, from+"-"+to)
			} else {
				out = append(out, to)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// LoadSuperblock reads the superblock of a delta.  The identifier may be
// either a delta name ("<from>-<to>" / "<to>") or, when it contains a
// slash, a path to the superblock file itself.
func LoadSuperblock(r *repo.Repo, id string) (*Superblock, string, error) {
	path, err := superblockPath(r, id)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "", fmt.Errorf("No such delta: %s", id)
		}
		return nil, "", err
	}
	sb, err := ParseSuperblock(data)
	if err != nil {
		return nil, "", err
	}
	return sb, path, nil
}

func superblockPath(r *repo.Repo, id string) (string, error) {
	if strings.ContainsRune(id, '/') {
		return id, nil
	}
	from, to, err := ParseDeltaName(id)
	if err != nil {
		return "", err
	}
	dir, err := DeltaDir(from, to)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.Path(), dir, "superblock"), nil
}

// Show prints the same report as `ostree static-delta show`.
func Show(r *repo.Repo, w io.Writer, id string) error {
	sb, path, err := LoadSuperblock(r, id)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Delta: %s\n", id)
	if sb.Signed {
		fmt.Fprintln(w, "Signed: yes")
	} else {
		fmt.Fprintln(w, "Signed: no")
	}
	if sb.HasFrom {
		fmt.Fprintf(w, "From: %s\n", checksum.Hex(sb.From))
	} else {
		fmt.Fprintln(w, "From <scratch>")
	}
	fmt.Fprintf(w, "To: %s\n", checksum.Hex(sb.To))

	swap := sb.NeedsSwap()
	endianness, heuristic := sb.GetEndianness()
	desc := endianness.String()
	if heuristic {
		desc += " (heuristic)"
	}
	fmt.Fprintf(w, "Endianness: %s\n", desc)
	fmt.Fprintf(w, "Timestamp: %d\n", sb.Timestamp)
	fmt.Fprintf(w, "Number of parents: %d\n", len(sb.Parents))

	fmt.Fprintf(w, "Number of fallback entries: %d\n", len(sb.Fallbacks))
	var totalFallbackSize, totalFallbackUSize uint64
	for _, f := range sb.Fallbacks {
		fmt.Fprintf(w, "  %s\n", checksum.Hex(f.Csum))
		totalFallbackSize += Swap64(swap, f.Size)
		totalFallbackUSize += Swap64(swap, f.USize)
	}
	fmt.Fprintf(w, "Total Fallback Size: %d (%s)\n", totalFallbackSize, formatSize(totalFallbackSize))
	fmt.Fprintf(w, "Total Fallback Uncompressed Size: %d (%s)\n",
		totalFallbackUSize, formatSize(totalFallbackUSize))

	fmt.Fprintf(w, "Number of parts: %d\n", len(sb.Parts))
	var totalSize, totalUSize uint64
	for i, p := range sb.Parts {
		size := Swap64(swap, p.Size)
		usize := Swap64(swap, p.USize)
		totalSize += size
		totalUSize += usize
		fmt.Fprintf(w, "PartMeta%d: nobjects=%d size=%d usize=%d\n",
			i, len(p.Objects), size, usize)

		partBytes, err := loadPartBytes(r, sb, path, i)
		if err != nil {
			continue
		}
		payload, err := DecodePartFile(partBytes)
		if err != nil {
			continue
		}
		stats, err := analyzePayload(payload, p.Objects)
		if err != nil {
			continue
		}
		fmt.Fprintf(w, "PartPayload%d: nmodes=%d nxattrs=%d blobsize=%d opsize=%d\n",
			i, stats.NModes, stats.NXattrs, stats.BlobSize, stats.OpsSize)
		c := stats.OpCounts
		fmt.Fprintf(w, "PartPayloadOps%d: openspliceclose=%d open=%d write=%d setread=%d "+
			"unsetread=%d close=%d bspatch=%d\n",
			i, c[0], c[1], c[2], c[3], c[4], c[5], c[6])
	}
	fmt.Fprintf(w, "Total Part Size: %d (%s)\n", totalSize, formatSize(totalSize))
	fmt.Fprintf(w, "Total Part Uncompressed Size: %d (%s)\n", totalUSize, formatSize(totalUSize))
	overallSize := totalSize + totalFallbackSize
	overallUSize := totalUSize + totalFallbackUSize
	fmt.Fprintf(w, "Total Size: %d (%s)\n", overallSize, formatSize(overallSize))
	fmt.Fprintf(w, "Total Uncompressed Size: %d (%s)\n", overallUSize, formatSize(overallUSize))
	return nil
}

// loadPartBytes returns the raw (compression byte prefixed) part data,
// either from the file next to the superblock or, for inline deltas,
// from the superblock metadata.
func loadPartBytes(r *repo.Repo, sb *Superblock, superPath string, i int) ([]byte, error) {
	from, to := "", checksum.Hex(sb.To)
	if sb.HasFrom {
		from = checksum.Hex(sb.From)
	}
	rel, err := DeltaPartPath(from, to, i)
	if err != nil {
		return nil, err
	}
	partBase := filepath.Base(rel)
	candidates := []string{
		filepath.Join(r.Path(), rel),
		filepath.Join(filepath.Dir(superPath), partBase),
	}
	for _, p := range candidates {
		if data, err := os.ReadFile(p); err == nil {
			return data, nil
		}
	}
	if sb.Metadata != nil {
		if v, ok := sb.Metadata.Get(rel); ok {
			if variant, ok := v.(gvariant.Variant); ok {
				if fields, ok := variant.Value.([]gvariant.Value); ok && len(fields) == 2 {
					comp, _ := fields[0].(uint8)
					payload, _ := fields[1].([]byte)
					return append([]byte{comp}, payload...), nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no part data for part %d", i)
}

// DecodePartFile unwraps the (compression, payload) container and
// decompresses the payload when needed.
func DecodePartFile(fileBytes []byte) ([]byte, error) {
	if len(fileBytes) < 1 {
		return nil, fmt.Errorf("static delta: empty part file")
	}
	comp := fileBytes[0]
	body := fileBytes[1:]
	switch comp {
	case CompressionNone:
		return body, nil
	case CompressionLZMA:
		return decompressXZ(body)
	default:
		return nil, fmt.Errorf("static delta: invalid compression type %d", comp)
	}
}

// PayloadStats holds the counters printed by `static delta show`.
type PayloadStats struct {
	NModes   uint64
	NXattrs  uint64
	BlobSize uint64
	OpsSize  uint64
	OpCounts [opCount]uint64
}

// analyzePayload decodes a part payload and walks the opcode stream with
// the part's object list, mirroring _ostree_static_delta_part_execute()
// with stats_only=TRUE.
func analyzePayload(payload []byte, objects []ObjectRef) (*PayloadStats, error) {
	v, err := gvariant.Decode(PartPayloadType, payload)
	if err != nil {
		return nil, err
	}
	fields, ok := v.([]gvariant.Value)
	if !ok || len(fields) != 4 {
		return nil, fmt.Errorf("static delta: malformed part payload")
	}
	modes, ok := fields[0].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed mode table")
	}
	xattrs, ok := fields[1].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed xattr table")
	}
	blob, ok := fields[2].([]byte)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed payload blob")
	}
	ops, ok := fields[3].([]byte)
	if !ok {
		return nil, fmt.Errorf("static delta: malformed operation stream")
	}

	st := &PayloadStats{
		NModes:   uint64(len(modes)),
		NXattrs:  uint64(len(xattrs)),
		BlobSize: uint64(len(blob)),
		OpsSize:  uint64(len(ops)),
	}
	objIndex := 0
	skip := func() error {
		_, n := readUvarint(ops)
		if n <= 0 {
			return fmt.Errorf("static delta: unexpected end of operation stream")
		}
		ops = ops[n:]
		return nil
	}
	skipN := func(n int) error {
		for i := 0; i < n; i++ {
			if err := skip(); err != nil {
				return err
			}
		}
		return nil
	}
	for len(ops) > 0 {
		op := ops[0]
		ops = ops[1:]
		var slot int
		switch op {
		case OpOpenSpliceAndClose:
			slot = 0
			if objIndex >= len(objects) {
				return nil, fmt.Errorf("static delta: more opcodes than objects")
			}
			if objects[objIndex].Type.IsMeta() {
				if err := skipN(2); err != nil {
					return nil, err
				}
			} else {
				if err := skipN(4); err != nil {
					return nil, err
				}
			}
			// OPEN_SPLICE_AND_CLOSE also closes the object.
			objIndex++
		case OpOpen:
			slot = 1
			if err := skipN(3); err != nil {
				return nil, err
			}
		case OpWrite:
			slot = 2
			if err := skipN(2); err != nil {
				return nil, err
			}
		case OpSetReadSource:
			slot = 3
			if err := skipN(1); err != nil {
				return nil, err
			}
		case OpUnsetReadSource:
			slot = 4
		case OpClose:
			slot = 5
			objIndex++
		case OpBspatch:
			slot = 6
			if err := skipN(2); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("static delta: unknown opcode %d", op)
		}
		st.OpCounts[slot]++
	}
	return st, nil
}

// Delete removes a static delta directory.
func Delete(r *repo.Repo, id string) error {
	from, to, err := ParseDeltaName(id)
	if err != nil {
		return err
	}
	dir, err := DeltaDir(from, to)
	if err != nil {
		return err
	}
	abs := filepath.Join(r.Path(), dir)
	if _, err := os.Stat(abs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("Can't find delta %s", id)
		}
		return err
	}
	return os.RemoveAll(abs)
}

// FormatSize mirrors g_format_size(): SI units, one decimal place.  It is
// also used for the pull "Delta update" progress line.
func FormatSize(n uint64) string { return formatSize(n) }

// formatSize mirrors g_format_size(): SI units, one decimal place.
func formatSize(n uint64) string {
	if n < 1000 {
		return fmt.Sprintf("%d bytes", n)
	}
	units := []string{"kB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(n)
	unit := -1
	for value >= 1000 && unit < len(units)-1 {
		value /= 1000
		unit++
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}
