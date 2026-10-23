// Package export streams a commit as a tar archive.
package export

import (
	"archive/tar"
	"fmt"
	"io"
	"path"
	"time"

	"ostree-go/internal/object"
	"ostree-go/internal/repo"
	"ostree-go/internal/tree"
)

// Options controls Export.
type Options struct {
	Repo    *repo.Repo
	Commit  string
	Subpath string
	Prefix  string
	Output  io.Writer
}

// Export writes the commit tree as a tar stream.
func Export(opts Options) error {
	csum, err := tree.ResolveRef(opts.Repo, opts.Commit)
	if err != nil {
		return err
	}
	commit, err := tree.LoadCommit(opts.Repo, csum)
	if err != nil {
		return err
	}
	root, meta := commit.RootTree, commit.RootMeta
	if opts.Subpath != "" {
		root, meta, err = subtree(opts.Repo, root, opts.Subpath)
		if err != nil {
			return err
		}
	}
	e := &exporter{
		repo:   opts.Repo,
		tw:     tar.NewWriter(opts.Output),
		prefix: opts.Prefix,
		links:  map[string]string{},
	}
	if err := e.writeTree(root, meta, "", true); err != nil {
		return err
	}
	return e.tw.Close()
}

func subtree(r *repo.Repo, root, subpath string) (string, string, error) {
	parts := splitPath(subpath)
	cur := root
	meta := ""
	for _, part := range parts {
		t, err := tree.LoadDirtree(r, cur)
		if err != nil {
			return "", "", err
		}
		found, foundMeta := "", ""
		for _, d := range t.Dirs {
			if d.Name == part {
				found, foundMeta = d.Tree, d.Meta
				break
			}
		}
		if found == "" {
			return "", "", fmt.Errorf("No such subpath: %s", subpath)
		}
		cur, meta = found, foundMeta
	}
	return cur, meta, nil
}

type exporter struct {
	repo   *repo.Repo
	tw     *tar.Writer
	prefix string
	links  map[string]string
}

func (e *exporter) tarName(rel string) string {
	name := rel
	if e.prefix != "" {
		name = path.Join(e.prefix, rel)
	}
	return name
}

func (e *exporter) writeTree(treeCsum, metaCsum, rel string, root bool) error {
	t, err := tree.LoadDirtree(e.repo, treeCsum)
	if err != nil {
		return err
	}
	if metaCsum != "" {
		if err := e.writeDir(t, metaCsum, rel, root); err != nil {
			return err
		}
	}
	for _, f := range t.Files {
		if err := e.writeFile(f, path.Join(rel, f.Name)); err != nil {
			return err
		}
	}
	for _, d := range t.Dirs {
		if err := e.writeTree(d.Tree, d.Meta, path.Join(rel, d.Name), false); err != nil {
			return err
		}
	}
	return nil
}

func (e *exporter) writeDir(t *object.Dirtree, metaCsum, rel string, root bool) error {
	meta, err := tree.LoadDirmeta(e.repo, metaCsum)
	if err != nil {
		return err
	}
	name := e.tarName(rel)
	if name == "" {
		name = "."
	}
	hdr := &tar.Header{
		Name:     name + "/",
		Mode:     int64(meta.Mode & 07777),
		Uid:      int(meta.UID),
		Gid:      int(meta.GID),
		Typeflag: tar.TypeDir,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
	applyXattrs(hdr, meta.Xattrs)
	return e.tw.WriteHeader(hdr)
}

func (e *exporter) writeFile(f object.FileEntry, rel string) error {
	loaded, err := tree.LoadFile(e.repo, f.Checksum)
	if err != nil {
		return err
	}
	name := e.tarName(rel)
	if loaded.Header.IsSymlink() {
		hdr := &tar.Header{
			Name:     name,
			Linkname: loaded.Header.SymlinkTarget,
			Mode:     int64(loaded.Header.Mode & 07777),
			Uid:      int(loaded.Header.UID),
			Gid:      int(loaded.Header.GID),
			Typeflag: tar.TypeSymlink,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		applyXattrs(hdr, loaded.Header.Xattrs)
		return e.tw.WriteHeader(hdr)
	}
	if target, ok := e.links[f.Checksum]; ok {
		hdr := &tar.Header{
			Name:     name,
			Linkname: target,
			Typeflag: tar.TypeLink,
			ModTime:  time.Unix(0, 0),
			Format:   tar.FormatPAX,
		}
		return e.tw.WriteHeader(hdr)
	}
	e.links[f.Checksum] = name
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(loaded.Header.Mode & 07777),
		Uid:      int(loaded.Header.UID),
		Gid:      int(loaded.Header.GID),
		Size:     int64(len(loaded.Content)),
		Typeflag: tar.TypeReg,
		ModTime:  time.Unix(0, 0),
		Format:   tar.FormatPAX,
	}
	applyXattrs(hdr, loaded.Header.Xattrs)
	if err := e.tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = e.tw.Write(loaded.Content)
	return err
}

func applyXattrs(hdr *tar.Header, xattrs []object.XAttr) {
	if len(xattrs) == 0 {
		return
	}
	if hdr.PAXRecords == nil {
		hdr.PAXRecords = map[string]string{}
	}
	for _, x := range xattrs {
		hdr.PAXRecords["SCHILY.xattr."+x.Name] = string(x.Value)
	}
}

func splitPath(p string) []string {
	var out []string
	cur := path.Clean(p)
	for cur != "" && cur != "." && cur != "/" {
		out = append([]string{path.Base(cur)}, out...)
		cur = path.Dir(cur)
	}
	return out
}
