package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/myml/ostree-go/internal/cli"
	"github.com/myml/ostree-go/pkg/repo"
)

func configOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "group",
			ArgType:        cli.ArgString,
			ArgDescription: "NAME",
			Description:    "Group name",
		},
	}
}

func runConfig(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if len(ctx.Args) == 0 {
		return fmt.Errorf("A command is required: get, set or unset")
	}
	cmd := ctx.Args[0]
	groupOpt := ctx.Options.String("group")

	splitKey := func(key string) (string, string, error) {
		if groupOpt != "" {
			return groupOpt, key, nil
		}
		i := strings.IndexByte(key, '.')
		if i <= 0 || i == len(key)-1 {
			return "", "", fmt.Errorf("Key must be in the form section.key")
		}
		return key[:i], key[i+1:], nil
	}

	cfg := r.Config()
	switch cmd {
	case "get":
		if len(ctx.Args) > 2 {
			return fmt.Errorf("Too many arguments given")
		}
		if len(ctx.Args) != 2 {
			return fmt.Errorf("usage: ostree config get KEY")
		}
		group, key, err := splitKey(ctx.Args[1])
		if err != nil {
			return err
		}
		value, err := cfg.Get(group, key)
		if err != nil {
			return fmt.Errorf("Key file does not have key %q in group %q", key, group)
		}
		fmt.Fprintln(ctx.Stdout, value)
		return nil
	case "set":
		if len(ctx.Args) > 3 {
			return fmt.Errorf("Too many arguments given")
		}
		if len(ctx.Args) != 3 {
			return fmt.Errorf("usage: ostree config set KEY VALUE")
		}
		group, key, err := splitKey(ctx.Args[1])
		if err != nil {
			return err
		}
		if group == "core" && key == "min-free-space-size" {
			if !freeSpaceSizeRe.MatchString(ctx.Args[2]) {
				return fmt.Errorf("Invalid min-free-space-size %q", ctx.Args[2])
			}
		}
		cfg.Set(group, key, ctx.Args[2])
		return saveConfig(r, cfg)
	case "unset":
		if len(ctx.Args) > 2 {
			return fmt.Errorf("Too many arguments given")
		}
		if len(ctx.Args) != 2 {
			return fmt.Errorf("usage: ostree config unset KEY")
		}
		group, key, err := splitKey(ctx.Args[1])
		if err != nil {
			return err
		}
		if err := cfg.Unset(group, key); err != nil {
			// Matches libostree: unsetting a missing key or group is a no-op.
			if !errors.Is(err, repo.ErrKeyNotFound) && !errors.Is(err, repo.ErrGroupNotFound) {
				return err
			}
		}
		return saveConfig(r, cfg)
	default:
		return fmt.Errorf("Unknown config command %q", cmd)
	}
}

var freeSpaceSizeRe = regexp.MustCompile(`^[0-9]+([kKmMgGtT][bB]?)?$`)

func saveConfig(r *repo.Repo, cfg *repo.Config) error {
	return cfg.Save(filepath.Join(r.Path(), "config"))
}
