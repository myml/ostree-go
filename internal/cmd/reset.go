package cmd

import (
	"fmt"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/tree"
)

func runReset(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("A ref argument is required")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if len(ctx.Args) == 1 {
		// Deleting a ref is allowed with a single argument in libostree's
		// reset implementation only via refs --delete; require two.
		return fmt.Errorf("A commit argument is required")
	}
	if len(ctx.Args) > 2 {
		return fmt.Errorf("Too many arguments")
	}
	checksum, err := tree.ResolveRef(r, ctx.Args[1])
	if err != nil {
		return err
	}
	unlock, err := r.Lock(true)
	if err != nil {
		return err
	}
	defer unlock()
	if err := r.WriteRef(ctx.Args[0], checksum); err != nil {
		return err
	}
	maybeUpdateSummary(r, "auto-update-summary")
	return nil
}
