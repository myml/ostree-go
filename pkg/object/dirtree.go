package object

import (
	"fmt"
	"sort"

	"github.com/myml/ostree-go/pkg/checksum"
	"github.com/myml/ostree-go/pkg/gvariant"
)

// DirtreeType is the GVariant type of a dirtree object.
var DirtreeType = gvariant.MustParseType("(a(say)a(sayay))")

// FileEntry is one file in a dirtree.
type FileEntry struct {
	Name     string
	Checksum string
}

// DirEntry is one subdirectory in a dirtree.
type DirEntry struct {
	Name string
	Tree string
	Meta string
}

// Dirtree is a parsed dirtree object.
type Dirtree struct {
	Files []FileEntry
	Dirs  []DirEntry
}

// Sort orders entries by name, as required by OSTree.
func (d *Dirtree) Sort() {
	sort.Slice(d.Files, func(i, j int) bool { return d.Files[i].Name < d.Files[j].Name })
	sort.Slice(d.Dirs, func(i, j int) bool { return d.Dirs[i].Name < d.Dirs[j].Name })
}

// ParseDirtree parses a dirtree object.
func ParseDirtree(data []byte) (*Dirtree, error) {
	v, err := gvariant.Decode(DirtreeType, data)
	if err != nil {
		return nil, fmt.Errorf("dirtree: %w", err)
	}
	f, ok := v.([]gvariant.Value)
	if !ok || len(f) != 2 {
		return nil, fmt.Errorf("dirtree: malformed tuple")
	}
	files, ok := f[0].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("dirtree: files is not an array")
	}
	dirs, ok := f[1].([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("dirtree: dirs is not an array")
	}
	d := &Dirtree{}
	for _, e := range files {
		pair, ok := e.([]gvariant.Value)
		if !ok || len(pair) != 2 {
			return nil, fmt.Errorf("dirtree: malformed file entry")
		}
		name, ok := pair[0].(string)
		if !ok {
			return nil, fmt.Errorf("dirtree: file name is not a string")
		}
		sum, ok := pair[1].([]byte)
		if !ok || len(sum) != checksum.Size {
			return nil, fmt.Errorf("dirtree: invalid file checksum for %q", name)
		}
		d.Files = append(d.Files, FileEntry{Name: name, Checksum: checksum.Hex(toArray(sum))})
	}
	for _, e := range dirs {
		triple, ok := e.([]gvariant.Value)
		if !ok || len(triple) != 3 {
			return nil, fmt.Errorf("dirtree: malformed directory entry")
		}
		name, ok := triple[0].(string)
		if !ok {
			return nil, fmt.Errorf("dirtree: directory name is not a string")
		}
		tree, ok1 := triple[1].([]byte)
		meta, ok2 := triple[2].([]byte)
		if !ok1 || !ok2 || len(tree) != checksum.Size || len(meta) != checksum.Size {
			return nil, fmt.Errorf("dirtree: invalid checksums for directory %q", name)
		}
		d.Dirs = append(d.Dirs, DirEntry{Name: name, Tree: checksum.Hex(toArray(tree)), Meta: checksum.Hex(toArray(meta))})
	}
	return d, nil
}

// Encode serializes the dirtree.
func (d *Dirtree) Encode() ([]byte, error) {
	files := make([]gvariant.Value, len(d.Files))
	for i, e := range d.Files {
		sum, err := checksum.FromHex(e.Checksum)
		if err != nil {
			return nil, fmt.Errorf("dirtree file %q: %w", e.Name, err)
		}
		files[i] = []gvariant.Value{e.Name, sum[:]}
	}
	dirs := make([]gvariant.Value, len(d.Dirs))
	for i, e := range d.Dirs {
		tree, err := checksum.FromHex(e.Tree)
		if err != nil {
			return nil, fmt.Errorf("dirtree dir %q tree: %w", e.Name, err)
		}
		meta, err := checksum.FromHex(e.Meta)
		if err != nil {
			return nil, fmt.Errorf("dirtree dir %q meta: %w", e.Name, err)
		}
		dirs[i] = []gvariant.Value{e.Name, tree[:], meta[:]}
	}
	return gvariant.Encode(DirtreeType, []gvariant.Value{files, dirs})
}
