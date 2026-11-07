package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/tree"
)

func runRevParse(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if ctx.Options.Has("single") {
		if len(ctx.Args) != 0 {
			return fmt.Errorf("Cannot specify arguments with --single")
		}
		commits, err := listCommits(r)
		if err != nil {
			return err
		}
		switch len(commits) {
		case 0:
			return fmt.Errorf("No commit objects found")
		case 1:
			fmt.Fprintln(ctx.Stdout, commits[0])
			return nil
		default:
			return fmt.Errorf("Multiple commit objects found")
		}
	}
	if len(ctx.Args) < 1 {
		return fmt.Errorf("REV must be specified")
	}
	for _, rev := range ctx.Args {
		resolved, err := resolveRev(r, rev)
		if err != nil {
			return err
		}
		fmt.Fprintln(ctx.Stdout, resolved)
	}
	return nil
}

func resolveRev(r *repo.Repo, rev string) (string, error) {
	return tree.ResolveRef(r, rev)
}

// listCommits returns the checksums of all commit objects in the repository.
func listCommits(r *repo.Repo) ([]string, error) {
	var out []string
	root := filepath.Join(r.Path(), "objects")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".commit") {
			return nil
		}
		dir := filepath.Base(filepath.Dir(path))
		out = append(out, dir+strings.TrimSuffix(name, ".commit"))
		return nil
	})
	return out, err
}
