package cli

import "io"

// Context is passed to a Command's Run function.  By the time Run is
// called the standard GOption-compatible parsing, --help/--version
// handling and (unless the command opts out) repository opening have
// already happened, so commands only deal with their own semantics.
type Context struct {
	// App is the owning application.
	App *App
	// Command is the command being executed.  For a subcommand this is
	// the subcommand, not its root command.
	Command *Command
	// Path is the human readable command path, e.g. "refs" or
	// "admin status".
	Path string
	// Args are the positional arguments left after option parsing.
	Args []string
	// Options holds all parsed options, including the injected --repo,
	// --sysroot, -v/--verbose and --version entries.
	Options *ParsedOptions
	// Repo is the opened repository, or nil for commands with NoRepo.
	Repo Repo
	// RepoPath is the path that was opened (explicit or default).
	RepoPath string
	// Sysroot is the --sysroot value, or "/" when not given.
	Sysroot string
	// PrintCurrentDir is the hidden --print-current-dir admin flag.
	PrintCurrentDir bool
	// Verbose is the number of -v/--verbose occurrences.
	Verbose int
	// Stdout, Stderr and Stdin are the streams to use.  Never write to
	// os.Stdout/os.Stderr directly from a command.
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
}
