package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ostree-go/internal/cli"
	"ostree-go/internal/repo"
)

func remoteAddOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:       "set",
			ArgType:        cli.ArgStringArray,
			ArgDescription: "KEY=VALUE",
			Description:    "Set config option KEY=VALUE for remote",
		},
		{
			LongName:    "no-gpg-verify",
			ArgType:     cli.ArgNone,
			Description: "Disable GPG verification",
		},
		{
			LongName:    "no-sign-verify",
			ArgType:     cli.ArgNone,
			Description: "Disable signature verification",
		},
		{
			LongName:    "if-not-exists",
			ArgType:     cli.ArgNone,
			Description: "Do nothing if the provided remote exists",
		},
		{
			LongName:    "force",
			ArgType:     cli.ArgNone,
			Description: "Replace the provided remote if it exists",
		},
		{
			LongName:       "contenturl",
			ArgType:        cli.ArgString,
			ArgDescription: "URL",
			Description:    "Use URL when fetching content",
		},
		{
			LongName:       "custom-backend",
			ArgType:        cli.ArgString,
			ArgDescription: "NAME",
			Description:    "Use custom backend NAME",
		},
		{
			LongName:       "collection-id",
			ArgType:        cli.ArgString,
			ArgDescription: "COLLECTION-ID",
			Description:    "Globally unique ID for this repository as a collection of refs",
		},
		{
			LongName:       "repo",
			ArgType:        cli.ArgFilename,
			ArgDescription: "PATH",
			Description:    "Path to OSTree repository (defaults to /sysroot/ostree/repo)",
		},
		{
			LongName:       "sysroot",
			ArgType:        cli.ArgFilename,
			ArgDescription: "PATH",
			Description:    "Use sysroot at PATH (overrides --repo)",
		},
	}
}

func remoteListOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "show-urls",
			ShortName:   'u',
			ArgType:     cli.ArgNone,
			Description: "Show remote URLs in listing",
		},
	}
}

func remoteDeleteOptions() []cli.Option {
	return []cli.Option{
		{
			LongName:    "if-exists",
			ArgType:     cli.ArgNone,
			Description: "Do nothing if the remote does not exist",
		},
		{
			LongName:       "repo",
			ArgType:        cli.ArgFilename,
			ArgDescription: "PATH",
			Description:    "Path to OSTree repository (defaults to /sysroot/ostree/repo)",
		},
		{
			LongName:       "sysroot",
			ArgType:        cli.ArgFilename,
			ArgDescription: "PATH",
			Description:    "Use sysroot at PATH (overrides --repo)",
		},
	}
}

// remoteRepoPath implements the --repo/--sysroot handling of the remote
// builtins, which do not go through the normal repo injection.
func remoteRepoPath(ctx *cli.Context) string {
	if p := ctx.Options.String("repo"); p != "" {
		return p
	}
	sysroot := ctx.Options.String("sysroot")
	if sysroot == "" {
		sysroot = "/"
	}
	return filepath.Join(sysroot, "ostree", "repo")
}

func runRemoteAdd(ctx *cli.Context) error {
	backend := ctx.Options.String("custom-backend")
	if len(ctx.Args) < 2 && backend == "" {
		return fmt.Errorf("NAME and URL must be specified")
	}
	if ctx.Options.Has("if-not-exists") && ctx.Options.Has("force") {
		return fmt.Errorf("Can only specify one of --if-not-exists and --force")
	}
	name := ctx.Args[0]
	url := ""
	if len(ctx.Args) > 1 {
		url = ctx.Args[1]
	}
	var branches []string
	if len(ctx.Args) > 2 {
		branches = ctx.Args[2:]
	}

	r, err := repo.Open(remoteRepoPath(ctx))
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}
	cfg := r.Config()

	// A sysroot that is a deployment (non-physical) stores remotes in
	// <deployment>/etc/ostree/remotes.d/<name>.conf (get_remotes_d_dir()).
	remotesDir := sysrootRemotesDir(ctx, r)
	confPath := ""
	if remotesDir != "" {
		confPath = filepath.Join(remotesDir, name+".conf")
	}

	exists := cfg.HasGroup(repo.RemoteSection(name))
	confExists := false
	if confPath != "" {
		if _, err := os.Lstat(confPath); err == nil {
			confExists = true
			exists = true
		}
	}
	if exists {
		switch {
		case ctx.Options.Has("if-not-exists"):
			return nil
		case ctx.Options.Has("force"):
			if confExists {
				if err := os.Remove(confPath); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			if err := cfg.RemoveGroup(repo.RemoteSection(name)); err != nil {
				return err
			}
		default:
			return fmt.Errorf("Remote \"%s\" already exists", name)
		}
	}

	target := cfg
	if confPath != "" {
		target = repo.NewConfig()
	}
	switch {
	case strings.HasPrefix(url, "metalink="):
		target.SetRemote(name, "metalink", strings.TrimPrefix(url, "metalink="))
	case url != "":
		target.SetRemote(name, "url", url)
	}
	if backend != "" {
		target.SetRemote(name, "custom-backend", backend)
	}
	if len(branches) > 0 {
		target.SetRemote(name, "branches", strings.Join(branches, ";")+";")
	}
	if ctx.Options.Has("no-gpg-verify") || ctx.Options.Has("no-sign-verify") {
		target.SetRemote(name, "gpg-verify", "false")
	}
	if ctx.Options.Has("no-sign-verify") {
		target.SetRemote(name, "sign-verify", "false")
	}
	if v := ctx.Options.String("contenturl"); v != "" {
		target.SetRemote(name, "contenturl", v)
	}
	if v := ctx.Options.String("collection-id"); v != "" {
		target.SetRemote(name, "collection-id", v)
	}
	for _, kv := range ctx.Options.Strings("set") {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			return fmt.Errorf("Invalid key/value pair %q", kv)
		}
		target.SetRemote(name, kv[:i], kv[i+1:])
	}
	if confPath != "" {
		if err := os.MkdirAll(remotesDir, 0o755); err != nil {
			return err
		}
		return os.WriteFile(confPath, target.Bytes(), 0o644)
	}
	return saveConfig(r, cfg)
}

// sysrootRemotesDir returns the remotes.d directory for a deployment sysroot,
// or "" when remotes belong in the repository configuration.
func sysrootRemotesDir(ctx *cli.Context, r *repo.Repo) string {
	sysrootPath := ctx.Options.String("sysroot")
	if sysrootPath == "" {
		return ""
	}
	abs, err := filepath.Abs(sysrootPath)
	if err != nil {
		return ""
	}
	// Only a deployment root (which has a /sysroot symlink) uses remotes.d;
	// the physical root keeps remotes in the repo config.
	if _, err := os.Lstat(filepath.Join(abs, "sysroot")); err != nil {
		return ""
	}
	addDir, err := r.Config().GetBool("core", "add-remotes-config-dir", true)
	if err != nil || !addDir {
		return ""
	}
	return filepath.Join(abs, "etc", "ostree", "remotes.d")
}

func runRemoteDelete(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	name := ctx.Args[0]
	r, err := repo.Open(remoteRepoPath(ctx))
	if err != nil {
		return fmt.Errorf("opening repo: %w", err)
	}
	cfg := r.Config()
	section := repo.RemoteSection(name)
	if !cfg.HasGroup(section) {
		if ctx.Options.Has("if-exists") {
			return nil
		}
		return fmt.Errorf("Remote \"%s\" not found", name)
	}
	if err := cfg.RemoveGroup(section); err != nil {
		return err
	}
	return saveConfig(r, cfg)
}

func runRemoteList(ctx *cli.Context) error {
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	// libostree also lists remotes inherited from parent repositories.
	type remoteEntry struct {
		name string
		repo *repo.Repo
	}
	seen := map[string]bool{}
	var remotes []remoteEntry
	for current := r; current != nil; current = current.Parent() {
		for _, name := range current.Config().RemoteNames() {
			if seen[name] {
				continue
			}
			seen[name] = true
			remotes = append(remotes, remoteEntry{name: name, repo: current})
		}
	}
	if ctx.Options.Has("show-urls") {
		maxLen := 0
		for _, entry := range remotes {
			if len(entry.name) > maxLen {
				maxLen = len(entry.name)
			}
		}
		for _, entry := range remotes {
			url, err := entry.repo.Config().GetRemote(entry.name, "url")
			if err != nil {
				return err
			}
			fmt.Fprintf(ctx.Stdout, "%-*s  %s\n", maxLen, entry.name, url)
		}
		return nil
	}
	for _, entry := range remotes {
		fmt.Fprintln(ctx.Stdout, entry.name)
	}
	return nil
}

func runRemoteShowURL(ctx *cli.Context) error {
	if len(ctx.Args) < 1 {
		return fmt.Errorf("NAME must be specified")
	}
	r, err := repoFrom(ctx)
	if err != nil {
		return err
	}
	if !r.Config().HasGroup(repo.RemoteSection(ctx.Args[0])) {
		return fmt.Errorf("No such remote: %s", ctx.Args[0])
	}
	url, err := r.Config().GetRemote(ctx.Args[0], "url")
	if err != nil {
		return fmt.Errorf("No \"url\" option in remote")
	}
	fmt.Fprintln(ctx.Stdout, url)
	return nil
}
