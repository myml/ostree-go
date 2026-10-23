package cmd

import (
	"fmt"
	"os"

	"ostree-go/internal/cli"
	"ostree-go/internal/export"
)

func exportOptions() []cli.Option {
	return []cli.Option{
		{LongName: "subpath", ArgType: cli.ArgString, ArgDescription: "PATH", Description: "Export sub-directory PATH"},
		{LongName: "prefix", ArgType: cli.ArgString, ArgDescription: "PREFIX", Description: "Add PATH prefix to all exported files"},
		{LongName: "output", ShortName: 'o', ArgType: cli.ArgFilename, ArgDescription: "FILE", Description: "Write tar to FILE"},
	}
}

func runExport(ctx *cli.Context) error {
	if len(ctx.Args) != 1 {
		return fmt.Errorf("Exactly one argument must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	out := ctx.Stdout
	if file := ctx.Options.String("output"); file != "" {
		f, err := os.Create(file)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	return export.Export(export.Options{
		Repo:    r,
		Commit:  ctx.Args[0],
		Subpath: ctx.Options.String("subpath"),
		Prefix:  ctx.Options.String("prefix"),
		Output:  out,
	})
}
