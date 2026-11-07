package cmd

import (
	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/prune"
)

// pruneOptions mirrors ot-builtin-prune.c, in the same order, so that --help
// output matches the C binary.
func pruneOptions() []cli.Option {
	return []cli.Option{
		{LongName: "no-prune", ArgType: cli.ArgNone, Description: "Only display unreachable objects; don't delete"},
		{LongName: "refs-only", ArgType: cli.ArgNone, Description: "Only compute reachability via refs"},
		{LongName: "depth", ArgType: cli.ArgInt, ArgDescription: "DEPTH", Description: "Only traverse DEPTH parents for each commit (default: -1=infinite)"},
		{LongName: "delete-commit", ArgType: cli.ArgString, ArgDescription: "COMMIT", Description: "Specify a commit to delete"},
		{LongName: "keep-younger-than", ArgType: cli.ArgString, ArgDescription: "DATE", Description: "Prune all commits older than the specified date"},
		{LongName: "keep-unreferenced-younger-than", ArgType: cli.ArgString, ArgDescription: "DATE", Description: "Keep unreferenced commits newer than the specified date"},
		{LongName: "static-deltas-only", ArgType: cli.ArgNone, Description: "Change the behavior of delete-commit and keep-younger-than to prune only static deltas"},
		{LongName: "retain-branch-depth", ArgType: cli.ArgStringArray, ArgDescription: "BRANCH=DEPTH", Description: "Additionally retain BRANCH=DEPTH commits"},
		{LongName: "only-branch", ArgType: cli.ArgStringArray, ArgDescription: "BRANCH", Description: "Only prune BRANCH (may be specified multiple times)"},
		{LongName: "commit-only", ArgType: cli.ArgNone, Description: "Only traverse and delete commit objects."},
	}
}

func runPrune(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	opts := prune.Options{
		NoPrune:                     ctx.Options.Has("no-prune"),
		RefsOnly:                    ctx.Options.Has("refs-only"),
		Depth:                       -1,
		DeleteCommit:                ctx.Options.String("delete-commit"),
		KeepYoungerThan:             ctx.Options.String("keep-younger-than"),
		KeepUnreferencedYoungerThan: ctx.Options.String("keep-unreferenced-younger-than"),
		StaticDeltasOnly:            ctx.Options.Has("static-deltas-only"),
		RetainBranchDepth:           ctx.Options.Strings("retain-branch-depth"),
		OnlyBranch:                  ctx.Options.Strings("only-branch"),
		CommitOnly:                  ctx.Options.Has("commit-only"),
		Stdout:                      ctx.Stdout,
	}
	if ctx.Options.Has("depth") {
		opts.Depth = ctx.Options.Int("depth")
	}
	return prune.Run(r, opts)
}
