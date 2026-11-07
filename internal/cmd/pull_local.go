package cmd

import (
	"fmt"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/pull"
)

// pullLocalOptions mirrors ot-builtin-pull-local.c, in the same order, so
// that --help output matches the C binary.
func pullLocalOptions() []cli.Option {
	return []cli.Option{
		{LongName: "commit-metadata-only", ArgType: cli.ArgNone, Description: "Fetch only the commit metadata"},
		{LongName: "remote", ArgType: cli.ArgString, ArgDescription: "REMOTE", Description: "Add REMOTE to refspec"},
		{LongName: "disable-fsync", ArgType: cli.ArgNone, Description: "Do not invoke fsync()"},
		{LongName: "per-object-fsync", ArgType: cli.ArgNone, Description: "Perform writes in such a way that avoids stalling concurrent processes"},
		{LongName: "untrusted", ArgType: cli.ArgNone, Description: "Verify checksums of local sources (always enabled for HTTP pulls)"},
		{LongName: "bareuseronly-files", ArgType: cli.ArgNone, Description: "Reject regular files with mode outside of 0775 (world writable, suid, etc.)"},
		{LongName: "require-static-deltas", ArgType: cli.ArgNone, Description: "Require static deltas"},
		{LongName: "disable-static-deltas", ArgType: cli.ArgNone, Description: "Do not use static deltas"},
		{LongName: "gpg-verify", ArgType: cli.ArgNone, Description: "GPG verify commits (must specify --remote)"},
		{LongName: "gpg-verify-summary", ArgType: cli.ArgNone, Description: "GPG verify summary (must specify --remote)"},
		{LongName: "disable-verify-bindings", ArgType: cli.ArgNone, Description: "Do not verify commit bindings"},
		{LongName: "depth", ArgType: cli.ArgInt, ArgDescription: "DEPTH", Description: "Traverse DEPTH parents (-1=infinite) (default: 0)"},
	}
}

func runPullLocal(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if len(ctx.Args) < 1 {
		return fmt.Errorf("DESTINATION must be specified")
	}
	opts := pull.Options{
		Refs:                  ctx.Args[1:],
		Depth:                 0,
		Untrusted:             ctx.Options.Has("untrusted"),
		Remote:                ctx.Options.String("remote"),
		CommitMetadataOnly:    ctx.Options.Has("commit-metadata-only"),
		BareUserOnlyFiles:     ctx.Options.Has("bareuseronly-files"),
		DisableFsync:          ctx.Options.Has("disable-fsync"),
		PerObjectFsync:        ctx.Options.Has("per-object-fsync"),
		RequireStaticDeltas:   ctx.Options.Has("require-static-deltas"),
		DisableStaticDeltas:   ctx.Options.Has("disable-static-deltas"),
		GPGVerify:             ctx.Options.Has("gpg-verify"),
		GPGVerifySummary:      ctx.Options.Has("gpg-verify-summary"),
		DisableVerifyBindings: ctx.Options.Has("disable-verify-bindings"),
		Stdout:                ctx.Stdout,
	}
	if ctx.Options.Has("depth") {
		opts.Depth = ctx.Options.Int("depth")
		opts.DepthSet = true
	}
	return pull.Local(r, ctx.Args[0], opts)
}
