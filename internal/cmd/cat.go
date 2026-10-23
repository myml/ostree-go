package cmd

import (
	"bytes"
	"compress/flate"
	"fmt"
	"io"
	"path"
	"strings"

	"ostree-go/internal/cli"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
	"ostree-go/internal/tree"
)

func runCat(ctx *cli.Context) error {
	if len(ctx.Args) < 2 {
		return fmt.Errorf("COMMIT and PATH must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	commitCsum, err := resolveRev(r, ctx.Args[0])
	if err != nil {
		return err
	}
	data, err := r.ReadObject(commitCsum, repo.ObjectCommit)
	if err != nil {
		return err
	}
	commit, err := object.ParseCommit(data)
	if err != nil {
		return err
	}
	for _, p := range ctx.Args[1:] {
		content, err := readPath(r, commit, p)
		if err != nil {
			return err
		}
		if _, err := ctx.Stdout.Write(content); err != nil {
			return err
		}
	}
	return nil
}

// readPath resolves a slash-separated path inside a commit and returns the
// file content. A leading slash is accepted.
func readPath(r *repo.Repo, commit *object.Commit, p string) ([]byte, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil, fmt.Errorf("open directory: %s", "/")
	}
	parts := strings.Split(p, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("Invalid path: %s", p)
		}
	}

	treeCsum := commit.RootTree
	for i, part := range parts {
		data, err := r.ReadObject(treeCsum, repo.ObjectDirtree)
		if err != nil {
			return nil, err
		}
		node, err := object.ParseDirtree(data)
		if err != nil {
			return nil, err
		}
		last := i == len(parts)-1
		if last {
			for _, f := range node.Files {
				if f.Name != part {
					continue
				}
				loaded, err := tree.LoadFile(r, f.Checksum)
				if err != nil {
					return nil, err
				}
				if loaded.Header.IsSymlink() {
					target := loaded.Header.SymlinkTarget
					base := path.Dir(p)
					if strings.HasPrefix(target, "/") {
						base = "/"
					}
					resolved := path.Clean(path.Join(base, target))
					return readPath(r, commit, resolved)
				}
				return loaded.Content, nil
			}
			for _, d := range node.Dirs {
				if d.Name == part {
					return nil, fmt.Errorf("open directory: %s", p)
				}
			}
			return nil, fmt.Errorf("Path %s not found", p)
		}
		found := false
		for _, d := range node.Dirs {
			if d.Name == part {
				treeCsum = d.Tree
				found = true
				break
			}
		}
		if !found {
			if _, err := fileByPath(node, part); err == nil {
				return nil, fmt.Errorf("Path %s is not a directory", p)
			}
			return nil, fmt.Errorf("Path %s not found", p)
		}
	}
	return nil, fmt.Errorf("Path %s not found", p)
}

func fileByPath(tree *object.Dirtree, name string) (string, error) {
	for _, f := range tree.Files {
		if f.Name == name {
			return f.Checksum, nil
		}
	}
	return "", fmt.Errorf("not found")
}

// readFile reads a file object in the current repository mode. In archive
// mode the on-disk object is a length-prefixed header plus raw deflate data;
// elsewhere the object payload is already the uncompressed content.
func readFile(r *repo.Repo, csum string) ([]byte, error) {
	raw, err := r.ReadObject(csum, repo.ObjectFile)
	if err != nil {
		return nil, err
	}
	if !r.Mode().IsArchive() {
		return raw, nil
	}
	h, rest, err := object.ParseArchiveHeader(raw)
	if err != nil {
		return nil, err
	}
	if h.IsSymlink() {
		return []byte(h.SymlinkTarget), nil
	}
	fr := flate.NewReader(bytes.NewReader(rest))
	defer fr.Close()
	content, err := io.ReadAll(fr)
	if err != nil {
		return nil, err
	}
	if uint64(len(content)) != h.Size {
		return nil, fmt.Errorf("file object %s: size mismatch", csum)
	}
	return content, nil
}
