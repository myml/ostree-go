package cmd

import (
	"fmt"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/diff"
)

func diffOptions() []cli.Option {
	return []cli.Option{
		{LongName: "stats", ArgType: cli.ArgNone, Description: "Print various statistics"},
		{LongName: "fs-diff", ArgType: cli.ArgNone, Description: "Print filesystem diff"},
		{LongName: "no-xattrs", ArgType: cli.ArgNone, Description: "Skip output of extended attributes"},
		{LongName: "owner-uid", ArgType: cli.ArgInt, ArgDescription: "UID", Description: "Use file ownership user id for local files"},
		{LongName: "owner-gid", ArgType: cli.ArgInt, ArgDescription: "GID", Description: "Use file ownership group id for local files"},
	}
}

func runDiff(ctx *cli.Context) error {
	if len(ctx.Args) != 2 {
		return fmt.Errorf("Exactly two arguments must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	opts := diff.Options{Repo: r, NoXattrs: ctx.Options.Has("no-xattrs")}
	if ctx.Options.Has("owner-uid") {
		v := uint32(ctx.Options.Int("owner-uid"))
		opts.OwnerUID = &v
	}
	if ctx.Options.Has("owner-gid") {
		v := uint32(ctx.Options.Int("owner-gid"))
		opts.OwnerGID = &v
	}
	lines, err := diff.Diff(opts, ctx.Args[0], ctx.Args[1])
	if err != nil {
		return err
	}
	for _, line := range lines {
		fmt.Fprintln(ctx.Stdout, line)
	}
	return nil
}
