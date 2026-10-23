package cmd

import (
	"fmt"

	"ostree-go/internal/checkout"
	"ostree-go/internal/cli"
	"ostree-go/internal/tree"
)

func checkoutOptions() []cli.Option {
	return []cli.Option{
		{LongName: "user-mode", ShortName: 'U', ArgType: cli.ArgNone, Description: "Do not change file ownership or initialize extended attributes"},
		{LongName: "disable-cache", ArgType: cli.ArgNone, Description: "Do not update or use the internal repository uncompressed object cache"},
		{LongName: "subpath", ArgType: cli.ArgString, ArgDescription: "PATH", Description: "Checkout sub-directory PATH"},
		{LongName: "union", ArgType: cli.ArgNone, Description: "Keep existing directories, overwrite existing files"},
		{LongName: "union-add", ArgType: cli.ArgNone, Description: "Keep existing files/directories, only add new"},
		{LongName: "union-identical", ArgType: cli.ArgNone, Description: "Error out if a file would be replaced with a different version"},
		{LongName: "whiteouts", ArgType: cli.ArgNone, Description: "Process 'whiteout' (Docker style) entries"},
		{LongName: "process-passthrough-whiteouts", ArgType: cli.ArgNone, Description: "Enable overlayfs whiteout extraction into char 0:0 devices"},
		{LongName: "allow-noent", ArgType: cli.ArgNone, Description: "Do nothing if specified path does not exist"},
		{LongName: "from-stdin", ArgType: cli.ArgNone, Description: "Process many checkouts from standard input"},
		{LongName: "from-file", ArgType: cli.ArgFilename, ArgDescription: "FILE", Description: "Process many checkouts from input file"},
		{LongName: "fsync", ArgType: cli.ArgString, ArgDescription: "POLICY", Description: "Specify how to invoke fsync()"},
		{LongName: "require-hardlinks", ShortName: 'H', ArgType: cli.ArgNone, Description: "Do not fall back to full copies if hardlinking fails"},
		{LongName: "force-copy-zerosized", ShortName: 'z', ArgType: cli.ArgNone, Description: "Do not hardlink zero-sized files"},
		{LongName: "force-copy", ShortName: 'C', ArgType: cli.ArgNone, Description: "Never hardlink (but may reflink if available)"},
		{LongName: "bareuseronly-dirs", ShortName: 'M', ArgType: cli.ArgNone, Description: "Suppress mode bits outside of 0775 for directories"},
		{LongName: "skip-list", ArgType: cli.ArgFilename, ArgDescription: "FILE", Description: "File containing list of files to skip"},
		{LongName: "selinux-policy", ArgType: cli.ArgFilename, ArgDescription: "PATH", Description: "Set SELinux labels based on policy in root filesystem PATH; implies --force-copy"},
		{LongName: "selinux-prefix", ArgType: cli.ArgString, ArgDescription: "PREFIX", Description: "When setting SELinux labels, prefix all paths by PREFIX"},
		{LongName: "composefs", ArgType: cli.ArgNone, Description: "Only create a composefs blob"},
		{LongName: "composefs-noverity", ArgType: cli.ArgNone, Description: "Only create a composefs blob, and disable fsverity"},
	}
}

func runCheckout(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("A commit argument is required")
	}
	if len(ctx.Args) > 2 {
		return fmt.Errorf("Too many arguments")
	}
	if ctx.Options.Has("composefs") || ctx.Options.Has("composefs-noverity") {
		return fmt.Errorf("composefs support is not implemented")
	}
	if ctx.Options.Has("from-file") || ctx.Options.Has("from-stdin") {
		return fmt.Errorf("--from-file and --from-stdin are not implemented yet")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	opts := checkout.Options{
		Repo:                        r,
		Commit:                      ctx.Args[0],
		Subpath:                     ctx.Options.String("subpath"),
		UserMode:                    ctx.Options.Has("user-mode"),
		ForceCopy:                   ctx.Options.Has("force-copy"),
		Whiteouts:                   ctx.Options.Has("whiteouts"),
		RequireHardlinks:            ctx.Options.Has("require-hardlinks"),
		Union:                       ctx.Options.Has("union") || ctx.Options.Has("union-add") || ctx.Options.Has("union-identical"),
		UnionMode:                   unionMode(ctx),
		BareUserOnlyDirs:            ctx.Options.Has("bareuseronly-dirs"),
		AllowNoent:                  ctx.Options.Has("allow-noent"),
		DisableCache:                ctx.Options.Has("disable-cache"),
		ProcessPassthroughWhiteouts: ctx.Options.Has("process-passthrough-whiteouts"),
	}
	if len(ctx.Args) == 2 {
		opts.Destination = ctx.Args[1]
	}
	if opts.Subpath != "" {
		if _, err := tree.ResolveRef(r, opts.Commit); err != nil {
			return err
		}
	}
	return checkout.Checkout(opts)
}

func unionMode(ctx *cli.Context) int {
	switch {
	case ctx.Options.Has("union-add"):
		return 2
	case ctx.Options.Has("union-identical"):
		return 3
	case ctx.Options.Has("union"):
		return 1
	default:
		return 0
	}
}
