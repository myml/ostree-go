package cmd

import (
	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/fsck"
)

func fsckOptions() []cli.Option {
	return []cli.Option{
		{LongName: "quiet", ShortName: 'q', ArgType: cli.ArgNone, Description: "Only report errors"},
		{LongName: "add-tombstones", ArgType: cli.ArgNone, Description: "Add tombstones for missing commits"},
		{LongName: "all", ArgType: cli.ArgNone, Description: "Check all commits and refs"},
		{LongName: "delete", ArgType: cli.ArgNone, Description: "Delete corrupted objects, continuing if possible"},
		{LongName: "verify-bindings", ArgType: cli.ArgNone, Description: "Verify ref bindings"},
		{LongName: "verify-back-refs", ArgType: cli.ArgNone, Description: "Verify refs listed in commit bindings exist"},
	}
}

func runFsck(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}

	_, err = fsck.Run(r, fsck.Options{
		All:            ctx.Options.Has("all"),
		Delete:         ctx.Options.Has("delete"),
		Quiet:          ctx.Options.Has("quiet"),
		VerifyBindings: ctx.Options.Has("verify-bindings"),
		VerifyBackRefs: ctx.Options.Has("verify-back-refs"),
		AddTombstones:  ctx.Options.Has("add-tombstones"),
		Out:            ctx.Stdout,
		Err:            ctx.Stderr,
	})
	return err
}
