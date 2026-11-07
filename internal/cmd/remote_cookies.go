package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/internal/pull"
)

// cookieJarPath returns <repo>/<remote>.cookies.txt.
func cookieJarPath(ctx *cli.Context, remote string) (string, error) {
	r, err := repoFrom(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.Path(), remote+".cookies.txt"), nil
}

func runRemoteAddCookie(ctx *cli.Context) error {
	if len(ctx.Args) < 5 {
		return fmt.Errorf("NAME, DOMAIN, PATH, COOKIE_NAME and VALUE must be specified")
	}
	jar, err := cookieJarPath(ctx, ctx.Args[0])
	if err != nil {
		return err
	}
	return pull.AddCookieFile(jar, ctx.Args[1], ctx.Args[2], ctx.Args[3], ctx.Args[4])
}

func runRemoteDeleteCookie(ctx *cli.Context) error {
	if len(ctx.Args) < 4 {
		return fmt.Errorf("NAME, DOMAIN, PATH and COOKIE_NAME must be specified")
	}
	jar, err := cookieJarPath(ctx, ctx.Args[0])
	if err != nil {
		return err
	}
	return pull.DeleteCookieFile(jar, ctx.Args[1], ctx.Args[2], ctx.Args[3])
}

func runRemoteListCookies(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	jar, err := cookieJarPath(ctx, ctx.Args[0])
	if err != nil {
		return err
	}
	return pull.ListCookieFile(jar, ctx.Stdout)
}
