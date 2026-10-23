package cli

// CommandFlags are per-command behaviors mirrored from
// OstreeBuiltinFlags plus the CLI framework's own extensions.
type CommandFlags uint

const (
	// NoRepo means the command does not get an injected --repo option
	// and does not open a repository before running.
	NoRepo CommandFlags = 1 << iota
	// Hidden keeps the command out of help listings.
	Hidden
	// NoCheck means an explicitly passed --repo is not opened before
	// running the command (used by `ostree init`).
	NoCheck
	// AdminOptions injects the admin global options: --sysroot and the
	// hidden --print-current-dir.  The C CLI adds these before the
	// command's own entries (and before the global entries).
	AdminOptions
	// LegacyRootUsage selects the hand-written, non-GOption usage text
	// used by `ostree static-delta`.
	LegacyRootUsage
)

// Command is one entry of the CLI command table.
//
// A command with Subcommands is a "root" command (`admin`, `remote`,
// `static-delta`): the first non-option argument selects a subcommand.
// Other commands are leaf commands.
type Command struct {
	// Name is the command word, e.g. "refs".
	Name string
	// Description is printed by help and used as the GOption summary
	// for leaf commands.
	Description string
	// Usage is the GOption parameter string, e.g. "[PREFIX]".
	Usage string
	// Flags control repo/option injection and help behavior.
	Flags CommandFlags
	// Options is the command's own GOption entry table.
	Options []Option
	// Subcommands is non-nil for root commands.
	Subcommands []*Command
	// SubcommandNameWidth is the column width used for the subcommand
	// listing (GOption uses 18 for the top level and remote, 25 for
	// admin).  Zero defaults to 18.
	SubcommandNameWidth int
	// Run implements the command.  A nil Run means the command is
	// registered but not implemented yet (L1 skeleton); the framework
	// reports "not implemented" after the standard parse/repo steps.
	Run func(*Context) error
}

// isHidden reports whether the command is marked HIDDEN.
func (c *Command) isHidden() bool { return c.Flags&Hidden != 0 }

// globalOptions are the entries appended last by
// ostree_option_context_parse().
func globalOptions() []Option {
	return []Option{
		{
			LongName:    "verbose",
			ShortName:   'v',
			ArgType:     ArgNone,
			Description: "Print debug information during command processing",
		},
		{
			LongName:    "version",
			ArgType:     ArgNone,
			Description: "Print version information and exit",
		},
	}
}

// repoOption is injected first for every command that needs a repo.
func repoOption() Option {
	return Option{
		LongName:       "repo",
		ArgType:        ArgFilename,
		ArgDescription: "PATH",
		Description:    "Path to OSTree repository (defaults to current directory or /sysroot/ostree/repo)",
	}
}

// adminGlobalOptions are injected for admin commands.
func adminGlobalOptions() []Option {
	return []Option{
		{
			LongName:       "sysroot",
			ArgType:        ArgFilename,
			ArgDescription: "PATH",
			Description:    "Create a new OSTree sysroot at PATH",
		},
		{
			// No description: hidden from --help output.
			LongName: "print-current-dir",
			ArgType:  ArgNone,
			Hidden:   true,
		},
	}
}

// DefaultCommands returns the 26 top-level commands in the exact order
// of the C ostree main.c table.
func DefaultCommands() []*Command {
	return []*Command{
		{
			Name:                "admin",
			Description:         "Commands for managing a host system booted with ostree",
			Usage:               "--print-current-dir|COMMAND",
			Flags:               NoRepo | AdminOptions,
			Subcommands:         adminSubcommands(),
			SubcommandNameWidth: 25,
		},
		{
			Name:        "cat",
			Description: "Concatenate contents of files",
			Usage:       "COMMIT PATH...",
		},
		{
			Name:        "checkout",
			Description: "Check out a commit into a filesystem tree",
			Usage:       "COMMIT [DESTINATION]",
		},
		{
			Name:        "checksum",
			Description: "Checksum a file or directory",
			Usage:       "PATH",
			Flags:       NoRepo,
		},
		{
			Name:        "commit",
			Description: "Commit a new revision",
			Usage:       "[PATH]",
		},
		{
			Name:        "config",
			Description: "Change repo configuration settings",
			Usage:       "(get KEY|set KEY VALUE|unset KEY)",
		},
		{
			Name:        "diff",
			Description: "Compare directory TARGETDIR against revision REV",
			Usage:       "REV_OR_DIR REV_OR_DIR",
		},
		{
			Name:        "export",
			Description: "Stream COMMIT to stdout in tar format",
			Usage:       "COMMIT",
		},
		{
			Name:        "find-remotes",
			Description: "Find remotes to serve the given refs",
			Usage:       "COLLECTION-ID REF [COLLECTION-ID REF...]",
		},
		{
			Name:        "create-usb",
			Description: "Copy the refs to a USB stick",
			Usage:       "MOUNT-PATH COLLECTION-ID REF [COLLECTION-ID REF...]",
		},
		{
			Name:        "fsck",
			Description: "Check the repository for consistency",
			Usage:       "",
		},
		{
			Name:        "gpg-sign",
			Description: "Sign a commit",
			Usage:       "COMMIT KEY-ID...",
		},
		{
			Name:        "init",
			Description: "Initialize a new empty repository",
			Usage:       "",
			Flags:       NoCheck,
		},
		{
			Name:        "log",
			Description: "Show log starting at commit or ref",
			Usage:       "REV",
		},
		{
			Name:        "ls",
			Description: "List file paths",
			Usage:       "COMMIT [PATH...]",
		},
		{
			Name:        "prune",
			Description: "Search for unreachable objects",
			Usage:       "",
		},
		{
			Name:        "pull-local",
			Description: "Copy data from SRC_REPO",
			Usage:       "SRC_REPO [REFS...]",
		},
		{
			Name:        "pull",
			Description: "Download data from remote repository",
			Usage:       "REMOTE [BRANCH...]",
		},
		{
			Name:        "refs",
			Description: "List refs",
			Usage:       "[PREFIX]",
			Options:     refsOptions(),
		},
		{
			Name:                "remote",
			Description:         "Remote commands that may involve internet access",
			Usage:               "COMMAND",
			Flags:               NoRepo,
			Subcommands:         remoteSubcommands(),
			SubcommandNameWidth: 18,
		},
		{
			Name:        "reset",
			Description: "Reset a REF to a previous COMMIT",
			Usage:       "REF COMMIT",
		},
		{
			Name:        "rev-parse",
			Description: "Output the target of a rev",
			Usage:       "REV",
			Options:     revParseOptions(),
		},
		{
			Name:        "sign",
			Description: "Sign a commit",
			Usage:       "COMMIT KEY-ID...",
		},
		{
			Name:        "show",
			Description: "Output a metadata object",
			Usage:       "OBJECT",
		},
		{
			Name:        "static-delta",
			Description: "Static delta related commands",
			Usage:       "COMMAND",
			Flags:       NoRepo | LegacyRootUsage,
			Subcommands: staticDeltaSubcommands(),
		},
		{
			Name:        "summary",
			Description: "Manage summary metadata",
			Usage:       "",
		},
	}
}

// refsOptions mirrors ot-builtin-refs.c.
func refsOptions() []Option {
	return []Option{
		{
			LongName:    "delete",
			ArgType:     ArgNone,
			Description: "Delete refs which match PREFIX, rather than listing them",
		},
		{
			LongName:    "list",
			ArgType:     ArgNone,
			Description: "Do not remove the prefix from the refs",
		},
		{
			LongName:    "revision",
			ShortName:   'r',
			ArgType:     ArgNone,
			Description: "Show revisions in listing",
		},
		{
			LongName:    "alias",
			ShortName:   'A',
			ArgType:     ArgNone,
			Description: "If used with --create, create an alias, otherwise just list aliases",
		},
		{
			LongName:       "create",
			ArgType:        ArgString,
			ArgDescription: "NEWREF",
			Description:    "Create a new ref for an existing commit",
		},
		{
			LongName:    "collections",
			ShortName:   'c',
			ArgType:     ArgNone,
			Description: "Enable listing collection IDs for refs",
		},
		{
			LongName:    "force",
			ArgType:     ArgNone,
			Description: "Overwrite existing refs when creating",
		},
	}
}

// revParseOptions mirrors ot-builtin-rev-parse.c.
func revParseOptions() []Option {
	return []Option{
		{
			LongName:    "single",
			ShortName:   'S',
			ArgType:     ArgNone,
			Description: "If the repository has exactly one commit, then print it; any other case will result in an error",
		},
	}
}

// adminSubcommands mirrors ot-builtin-admin.c (without the soft-reboot
// commands, which the reference 2025.2 build does not enable).
func adminSubcommands() []*Command {
	sub := func(name, usage, description string, flags CommandFlags) *Command {
		return &Command{
			Name:        name,
			Description: description,
			Usage:       usage,
			Flags:       NoRepo | AdminOptions | flags,
		}
	}

	return []*Command{
		sub("cleanup", "", "Delete untagged deployments and repository objects", 0),
		sub("config-diff", "", "Diff current /etc configuration versus default", 0),
		sub("deploy", "REFSPEC", "Checkout revision REFSPEC as the new default deployment", 0),
		sub("finalize-staged", "", "Internal command to run at shutdown time", Hidden),
		sub("lock-finalization", "",
			"Change the finalization locking state of the staged deployment", 0),
		sub("boot-complete", "",
			"Internal command to run at boot after an update was applied", Hidden),
		sub("state-overlay", "NAME MOUNTPATH",
			"Internal command to assemble a state overlay", Hidden),
		sub("init-fs", "PATH", "Initialize a root filesystem", 0),
		sub("instutil", "COMMAND",
			"Deprecated commands intended for installer programs", Hidden),
		sub("os-init", "STATEROOT", "Initialize empty state for given operating system", 0),
		sub("stateroot-init", "STATEROOT", "Initialize empty state for given operating system", 0),
		sub("pin", "INDEX", `Change the "pinning" state of a deployment, INDEX can be >= 0 or one of booted, pending or rollback strings`, 0),
		sub("post-copy", "", "Update the repo and deployments as needed after a copy", 0),
		sub("set-origin", "REMOTENAME URL [BRANCH]", "Set Origin and create a new origin file", 0),
		sub("status", "", "List deployments", 0),
		sub("switch", "REFSPEC", "Construct new tree from REFSPEC and deploy it", 0),
		sub("undeploy", "INDEX", "Delete deployment INDEX", 0),
		sub("set-default", "INDEX", "Make deployment INDEX the default", 0),
		sub("unlock", "",
			"Make the current deployment mutable (as a hotfix or development)", 0),
		sub("upgrade", "",
			"Construct new tree from current origin and deploy it, if it changed", 0),
		sub("kargs", "COMMAND", "Change kernel arguments", 0),
	}
}

// remoteSubcommands mirrors ot-builtin-remote.c.  Option tables are not
// filled in yet; the commands are registered for dispatch and help.
func remoteSubcommands() []*Command {
	sub := func(name, usage, description string, flags CommandFlags) *Command {
		return &Command{Name: name, Description: description, Usage: usage, Flags: flags}
	}

	return []*Command{
		sub("add", "NAME [metalink=|mirrorlist=]URL [BRANCH...]", "Add a remote repository", NoRepo),
		sub("delete", "NAME", "Delete a remote repository", NoRepo),
		sub("show-url", "NAME", "Show remote repository URL", 0),
		sub("list", "", "List remote repository names", 0),
		sub("gpg-import", "NAME [KEY-ID...]", "Import GPG keys", 0),
		sub("gpg-list-keys", "NAME", "Show remote GPG keys", 0),
		sub("add-cookie", "NAME DOMAIN PATH COOKIE_NAME VALUE", "Add a cookie to remote", 0),
		sub("delete-cookie", "NAME DOMAIN PATH COOKIE_NAME", "Remove one cookie from remote", 0),
		sub("list-cookies", "NAME", "Show remote repository cookies", 0),
		sub("refs", "NAME", "List remote refs", 0),
		sub("summary", "NAME", "Show remote summary", 0),
	}
}

// staticDeltaSubcommands mirrors ot-builtin-static-delta.c.
func staticDeltaSubcommands() []*Command {
	return []*Command{
		{Name: "list", Description: "List static delta files", Usage: "[TO]"},
		{Name: "show", Description: "Dump information on a delta"},
		{Name: "delete", Description: "Remove a delta"},
		{Name: "generate", Description: "Generate static delta files"},
		{Name: "apply-offline", Description: "Apply static delta file"},
		{Name: "verify", Description: "Verify static delta signatures",
			Usage: "STATIC-DELTA-FILE [KEY-ID...]"},
		{Name: "indexes", Description: "List static delta indexes"},
		{Name: "reindex", Description: "Regenerate static delta indexes"},
	}
}
