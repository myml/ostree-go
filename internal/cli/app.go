package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// App is the top-level CLI application.  Zero values are filled in by
// NewApp; tests construct an App with NewApp and override the streams,
// environment accessors and repository opener.
type App struct {
	// PackageName is the YAML key printed by --version ("libostree").
	PackageName string
	// Version is the OSTree version string (e.g. "2025.2").
	Version string
	// Git is the source revision; empty means no Git line is printed.
	Git string
	// DevelBuild adds "DevelBuild: yes" to --version.
	DevelBuild bool
	// Features is the feature list reported by --version.  Only claim
	// features that are actually implemented: upstream tests use it to
	// decide whether to skip.
	Features []string
	// Commands is the top-level command table in C order.
	Commands []*Command

	// Stdout, Stderr and Stdin are the process streams.
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader

	// LookupEnv, Getwd and OpenRepo are seams for tests and for the
	// later repository layer.
	LookupEnv func(string) (string, bool)
	Getwd     func() (string, error)
	OpenRepo  RepoOpener

	// StdoutIsTTY decides whether error messages get ANSI colors.  The
	// C CLI checks *stdout* for a tty even though errors go to stderr.
	StdoutIsTTY func(io.Writer) bool
}

// NewApp returns an application with the production defaults.
func NewApp() *App {
	return &App{
		PackageName: "libostree",
		Version:     "2025.2",
		// No subsystem is implemented at L1, so advertise no features.
		// Claiming a feature would make upstream tests stop skipping.
		Features:    nil,
		Commands:    DefaultCommands(),
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		Stdin:       os.Stdin,
		LookupEnv:   os.LookupEnv,
		Getwd:       os.Getwd,
		OpenRepo:    OpenRepoDefault,
		StdoutIsTTY: defaultStdoutIsTTY,
	}
}

func (a *App) initDefaults() {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	if a.LookupEnv == nil {
		a.LookupEnv = os.LookupEnv
	}
	if a.Getwd == nil {
		a.Getwd = os.Getwd
	}
	if a.OpenRepo == nil {
		a.OpenRepo = OpenRepoDefault
	}
	if a.StdoutIsTTY == nil {
		a.StdoutIsTTY = defaultStdoutIsTTY
	}
	if a.Commands == nil {
		a.Commands = DefaultCommands()
	}
}

// defaultStdoutIsTTY implements glnx_stdout_is_tty().
func defaultStdoutIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminal(f)
}

// Run executes the CLI and returns the process exit code (only 0 or 1).
// argv[0] is the program name as invoked and is used verbatim in usage
// output, exactly like the C binary.
func (a *App) Run(argv []string) int {
	a.initDefaults()

	prog := "ostree"
	if len(argv) > 0 && argv[0] != "" {
		prog = argv[0]
	}
	var args []string
	if len(argv) > 1 {
		args = argv[1:]
	}

	// Step one, shared with the C CLI: the first argument that does not
	// begin with '-' is the command name and is removed before options
	// are parsed.  Note that this happens even before option parsing, so
	// `--repo PATH command` steals PATH as the command name.
	name, hasCommand, rest := extractCommand(args)

	var command *Command
	if hasCommand {
		command = findCommand(a.Commands, name)
	}
	if command == nil {
		return a.runUnknownCommand(prog, name, hasCommand, rest)
	}
	if command.Subcommands != nil {
		return a.runRootCommand(prog, command, rest)
	}
	return a.runLeafCommand(prog, command, command.Name, rest)
}

// extractCommand splits off the first non-option argument.
func extractCommand(args []string) (name string, found bool, rest []string) {
	rest = make([]string, 0, len(args))
	for _, arg := range args {
		if !found && isNonOption(arg) {
			name = arg
			found = true
			continue
		}
		rest = append(rest, arg)
	}
	return name, found, rest
}

// extractRootCommand is the admin/remote variant: a lone "--" stops the
// scan *and* truncates the remaining arguments (a C implementation
// quirk worth preserving).
func extractRootCommand(args []string) (name string, found bool, rest []string) {
	rest = make([]string, 0, len(args))
	for _, arg := range args {
		if isNonOption(arg) {
			if !found {
				name = arg
				found = true
				continue
			}
		} else if arg == "--" {
			break
		}
		rest = append(rest, arg)
	}
	return name, found, rest
}

// isNonOption mirrors the C check argv[in][0] != '-'; the empty string
// counts as a (bogus) command name.
func isNonOption(arg string) bool { return arg == "" || arg[0] != '-' }

func findCommand(commands []*Command, name string) *Command {
	for _, command := range commands {
		if command.Name == name {
			return command
		}
	}
	return nil
}

// topHelp builds the help page used for `--help`, for the "no command
// specified" error and for unknown commands.
func (a *App) topHelp(prog string) *helpSpec {
	return &helpSpec{
		prog:       prog,
		usageParam: "COMMAND",
		summary:    renderCommandSummary("Builtin Commands:", 18, a.Commands),
		options:    globalOptions(),
	}
}

// rootHelp builds the help page for a command with subcommands.
func (a *App) rootHelp(prog string, command *Command) *helpSpec {
	width := command.SubcommandNameWidth
	if width == 0 {
		width = 18
	}
	summary := ""
	if command.Description != "" {
		summary = command.Description + "\n\n"
	}
	summary += renderCommandSummary(fmt.Sprintf("Builtin %q Commands:", command.Name),
		width, command.Subcommands)
	return &helpSpec{
		prog:       prog + " " + command.Name,
		usageParam: command.Usage,
		summary:    summary,
		options:    a.optionsFor(command),
	}
}

// optionsFor assembles the option table in the same order as
// ostree_option_context_parse(): repository first, then admin globals,
// then command entries and finally the global entries.
func (a *App) optionsFor(command *Command) []Option {
	options := make([]Option, 0, 8)
	if command.Flags&NoRepo == 0 {
		options = append(options, repoOption())
	}
	if command.Flags&AdminOptions != 0 {
		options = append(options, adminGlobalOptions()...)
	}
	options = append(options, command.Options...)
	options = append(options, globalOptions()...)
	return options
}

// runUnknownCommand handles a missing or unknown command: parse the
// global options (which may still handle --help/--version), then print
// the usage followed by the error.
func (a *App) runUnknownCommand(prog, name string, hasCommand bool, args []string) int {
	spec := a.topHelp(prog)
	parsed, _, action, err := parseOptions(spec.options, args)
	if action != parseActionNone {
		writeHelpTo(a.Stdout, spec)
		return 0
	}
	if err == nil && parsed.Has("version") {
		a.writeVersion()
		return 0
	}
	if err == nil && hasCommand {
		// Look for an external ostree-<command> in PATH, like libostree's
		// CLI extension mechanism.
		if ext, lerr := lookPathRelative("ostree-" + name); lerr == nil {
			if a.Stdout != os.Stdout || a.Stderr != os.Stderr {
				cmd := exec.Command(ext, args...)
				cmd.Stdout = a.Stdout
				cmd.Stderr = a.Stderr
				cmd.Stdin = os.Stdin
				if rerr := cmd.Run(); rerr != nil {
					if ee, ok := rerr.(*exec.ExitError); ok {
						return ee.ExitCode()
					}
					a.printError(rerr.Error())
					return 1
				}
				return 0
			}
			if serr := syscall.Exec(ext, append([]string{ext}, args...), os.Environ()); serr != nil {
				a.printError(serr.Error())
				return 1
			}
		}
	}

	var message string
	switch {
	case err != nil:
		message = err.Error()
	case !hasCommand:
		message = "No command specified"
	default:
		message = fmt.Sprintf("Unknown command '%s'", name)
	}

	writeHelpTo(a.Stderr, spec)
	a.printError(message)
	return 1
}

// runRootCommand implements admin/remote/static-delta style commands.
func (a *App) runRootCommand(prog string, command *Command, args []string) int {
	if command.Flags&LegacyRootUsage != 0 {
		return a.runLegacyRootCommand(prog, command, args)
	}

	name, hasSubcommand, rest := extractRootCommand(args)
	var subcommand *Command
	if hasSubcommand {
		subcommand = findCommand(command.Subcommands, name)
	}
	if subcommand != nil {
		return a.runLeafCommand(prog, subcommand, command.Name+" "+subcommand.Name, rest)
	}

	spec := a.rootHelp(prog, command)
	parsed, _, action, err := parseOptions(spec.options, rest)
	if action != parseActionNone {
		writeHelpTo(a.Stdout, spec)
		return 0
	}
	if err == nil && parsed.Has("version") {
		a.writeVersion()
		return 0
	}
	// A root command may supply a Run for the case where no subcommand was
	// given (the C admin builtin handles `ostree admin --print-current-dir`
	// and the missing-subcommand error itself).
	if err == nil && !hasSubcommand && command.Run != nil {
		return a.runLeafCommand(prog, command, command.Name, rest)
	}
	var message string
	switch {
	case err != nil:
		message = err.Error()
	case !hasSubcommand:
		message = fmt.Sprintf("No %q subcommand specified", command.Name)
	default:
		message = fmt.Sprintf("Unknown %q subcommand '%s'", command.Name, name)
	}

	writeHelpTo(a.Stderr, spec)
	a.printError(message)
	return 1
}

// runLegacyRootCommand reproduces the hand-written dispatch and usage of
// `ostree static-delta`, which does not go through GOption at the root.
func (a *App) runLegacyRootCommand(prog string, command *Command, args []string) int {
	usage := renderLegacyStaticDeltaUsage(command.Subcommands)

	name := ""
	hasSubcommand := false
	wantHelp := false
	for _, arg := range args {
		if isNonOption(arg) {
			name = arg
			hasSubcommand = true
			break
		}
		if arg == "--help" || arg == "-h" {
			wantHelp = true
			break
		}
	}

	if !hasSubcommand && !wantHelp {
		io.WriteString(a.Stderr, usage)
		a.printError("No command specified")
		return 1
	}

	var subcommand *Command
	if hasSubcommand {
		subcommand = findCommand(command.Subcommands, name)
	}
	if wantHelp && subcommand == nil {
		io.WriteString(a.Stdout, usage)
		return 0
	}
	if subcommand == nil {
		io.WriteString(a.Stderr, usage)
		a.printError(fmt.Sprintf("Unknown %q subcommand '%s'", command.Name, name))
		return 1
	}

	// The C implementation does not remove the subcommand from argv.
	return a.runLeafCommand(prog, subcommand, command.Name+" "+subcommand.Name, args)
}

// runLeafCommand runs one non-root command (or a root's subcommand).
func (a *App) runLeafCommand(prog string, command *Command, path string, args []string) int {
	spec := &helpSpec{
		prog:       prog + " " + path,
		usageParam: command.Usage,
		summary:    command.Description,
		options:    a.optionsFor(command),
	}

	parsed, positional, action, err := parseOptions(spec.options, args)
	if action != parseActionNone {
		writeHelpTo(a.Stdout, spec)
		return 0
	}
	if err != nil {
		a.printError(err.Error())
		return 1
	}
	if parsed.Has("version") {
		a.writeVersion()
		return 0
	}

	ctx := &Context{
		App:             a,
		Command:         command,
		Path:            path,
		Args:            positional,
		Options:         parsed,
		Sysroot:         "/",
		PrintCurrentDir: parsed.Has("print-current-dir"),
		Verbose:         parsed.Count("verbose"),
		Stdout:          a.Stdout,
		Stderr:          a.Stderr,
		Stdin:           a.Stdin,
	}
	if sysroot := parsed.String("sysroot"); sysroot != "" {
		ctx.Sysroot = sysroot
	}

	if command.Flags&NoRepo == 0 {
		repo, repoPath, err := a.resolveRepo(command, parsed)
		if err != nil {
			// Only a *default* repository that does not exist is
			// translated into "requires --repo" plus command help.
			if !parsed.Has("repo") && IsRepoNotFound(err) {
				writeHelpTo(a.Stderr, spec)
				a.printError("Command requires a --repo argument")
				return 1
			}
			a.printError(err.Error())
			return 1
		}
		ctx.Repo = repo
		ctx.RepoPath = repoPath
	}

	if command.Run == nil {
		a.printError(fmt.Sprintf("%s: %v", path, ErrNotImplemented))
		return 1
	}
	if err := command.Run(ctx); err != nil {
		a.printError(err.Error())
		return 1
	}
	return 0
}

// resolveRepo mirrors parse_repo_option(): an explicit --repo is opened
// unless the command is marked NoCheck; without --repo the default repo
// is located from the cwd, $OSTREE_REPO or /ostree/repo.
func (a *App) resolveRepo(command *Command, parsed *ParsedOptions) (Repo, string, error) {
	explicit := parsed.Has("repo")
	path := parsed.String("repo")
	if !explicit {
		path = a.defaultRepoPath()
	}
	if explicit && command.Flags&NoCheck != 0 {
		return &defaultRepo{path: path}, path, nil
	}
	repo, err := a.OpenRepo(path)
	if err != nil {
		return nil, path, err
	}
	return repo, path, nil
}

// defaultRepoPath implements ostree_repo_new_default().
func (a *App) defaultRepoPath() string {
	if cwd, err := a.Getwd(); err == nil {
		objects := filepath.Join(cwd, "objects")
		config := filepath.Join(cwd, "config")
		if isDirectory(objects) && isRegularFile(config) {
			return "."
		}
	}
	if value, ok := a.LookupEnv("OSTREE_REPO"); ok && value != "" {
		return value
	}
	// The default repository is $OSTREE_SYSROOT/ostree/repo (the sysroot
	// defaults to "/"), matching get_default_repo_path().
	sysroot := "/"
	if value, ok := a.LookupEnv("OSTREE_SYSROOT"); ok && value != "" {
		sysroot = value
	}
	return filepath.Join(sysroot, "ostree", "repo")
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func writeHelpTo(w io.Writer, spec *helpSpec) {
	io.WriteString(w, spec.render())
}

// lookPathRelative is like exec.LookPath but accepts relative PATH entries,
// which libostree's extension mechanism allows.
func lookPathRelative(name string) (string, error) {
	pathEnv := os.Getenv("PATH")
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		abs := dir
		if !filepath.IsAbs(abs) {
			wd, err := os.Getwd()
			if err != nil {
				continue
			}
			abs = filepath.Join(wd, dir)
		}
		candidate := filepath.Join(abs, name)
		fi, err := os.Stat(candidate)
		if err != nil || fi.IsDir() || fi.Mode()&0111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", exec.ErrNotFound
}
