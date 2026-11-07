package cmd

import (
	"fmt"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/repo"
)

func initOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "mode",
			ArgType:        cli.ArgString,
			ArgDescription: "MODE",
			Description:    "Initialize repository in given mode (bare, bare-user, bare-user-only, archive)",
		},
		{
			LongName:       "collection-id",
			ArgType:        cli.ArgString,
			ArgDescription: "COLLECTION-ID",
			Description:    "Globally unique ID for this repository as a collection of refs for redistribution to other repositories",
		},
	}
}

func runInit(ctx *cli.Context) error {
	mode := repo.ModeBare
	if s := ctx.Options.String("mode"); s != "" {
		m, err := repo.ParseMode(s)
		if err != nil {
			return err
		}
		mode = m
	}
	opts := &repo.CreateOptions{CollectionID: ctx.Options.String("collection-id")}
	if _, err := repo.Create(ctx.RepoPath, mode, opts); err != nil {
		return fmt.Errorf("Creating repo: %w", err)
	}
	return nil
}
