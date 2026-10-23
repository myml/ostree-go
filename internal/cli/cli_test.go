package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// goldenProg is the argv[0] used when the goldens were captured from the
// C binary.  testProg is what the Go tests use, so the normalizer is
// exercised for real.
const (
	goldenProg = "/usr/bin/ostree"
	testProg   = "/opt/go-ostree/ostree"
)

// goldenCase is one row of testdata/cases.tsv.
type goldenCase struct {
	name string
	args []string
}

// loadGoldenCases reads the shared case list.  Keeping the list in a
// data file means the collector script and the test cannot drift.
func loadGoldenCases(t *testing.T) []goldenCase {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", "cases.tsv"))
	if err != nil {
		t.Fatalf("reading cases.tsv: %v", err)
	}

	var cases []goldenCase
	for lineNo, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) == 0 || fields[0] == "" {
			t.Fatalf("cases.tsv:%d: empty case name", lineNo+1)
		}
		cases = append(cases, goldenCase{name: fields[0], args: fields[1:]})
	}
	if len(cases) == 0 {
		t.Fatal("cases.tsv contains no cases")
	}
	return cases
}

// newGoldenApp builds an App whose observable behavior does not depend
// on the host: a fake cwd, a failing $OSTREE_REPO default and no tty.
func newGoldenApp(t *testing.T, cwd string) (*App, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()

	app := NewApp()
	// Git and Features are normalized by normalizeOutput; the values
	// only need to prove the YAML shape is right.
	app.Git = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	app.Features = []string{"test-feature"}

	var stdout, stderr bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stderr

	env := map[string]string{"OSTREE_REPO": filepath.Join(cwd, "nonexistent-repo")}
	app.LookupEnv = func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
	app.Getwd = func() (string, error) { return cwd, nil }
	app.OpenRepo = OpenRepoDefault
	app.StdoutIsTTY = func(io.Writer) bool { return false }

	return app, &stdout, &stderr
}

func readGolden(t *testing.T, name, extension string) string {
	t.Helper()

	path := filepath.Join("testdata", name+extension)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	return string(data)
}

func TestGolden(t *testing.T) {
	for _, tc := range loadGoldenCases(t) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Each case gets its own cwd so a stray `objects`/`config`
			// can never make the default repo lookup succeed.
			cwd := t.TempDir()
			app, stdout, stderr := newGoldenApp(t, cwd)

			argv := append([]string{testProg}, tc.args...)
			gotExit := app.Run(argv)

			wantOut := readGolden(t, tc.name, ".out")
			wantErr := readGolden(t, tc.name, ".err")
			wantExit, err := strconv.Atoi(strings.TrimSpace(readGolden(t, tc.name, ".exit")))
			if err != nil {
				t.Fatalf("bad .exit golden for %s: %v", tc.name, err)
			}

			gotOut := normalizeOutput(stdout.String())
			gotErr := normalizeOutput(stderr.String())
			wantOut = normalizeOutput(wantOut)
			wantErr = normalizeOutput(wantErr)

			if gotExit != wantExit {
				t.Errorf("exit code = %d, want %d", gotExit, wantExit)
			}
			if gotOut != wantOut {
				t.Errorf("stdout mismatch\n%s", diffOutput(gotOut, wantOut))
			}
			if gotErr != wantErr {
				t.Errorf("stderr mismatch\n%s", diffOutput(gotErr, wantErr))
			}
		})
	}
}

// normalizeOutput removes the parts of the output that legitimately
// differ between the C reference build and the Go binary: the program
// path in usage lines, the Git revision and the feature list.
func normalizeOutput(s string) string {
	s = strings.ReplaceAll(s, goldenProg, "@PROG@")
	s = strings.ReplaceAll(s, testProg, "@PROG@")
	if strings.HasPrefix(s, "libostree:\n") {
		s = normalizeVersionYAML(s)
	}
	return s
}

func normalizeVersionYAML(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	inFeatures := false
	wroteFeatures := false
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, " Git: "):
			// The reference build may or may not carry a Git line.
			continue
		case line == " Features:":
			out = append(out, line)
			inFeatures = true
		case inFeatures && strings.HasPrefix(line, "  - "):
			if !wroteFeatures {
				out = append(out, "  - @FEATURES@")
				wroteFeatures = true
			}
		default:
			inFeatures = false
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

func diffOutput(got, want string) string {
	gotLines := strings.Split(got, "\n")
	wantLines := strings.Split(want, "\n")
	limit := len(gotLines)
	if len(wantLines) > limit {
		limit = len(wantLines)
	}
	for i := 0; i < limit; i++ {
		var g, w string
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			return fmt.Sprintf("first difference at line %d:\n  got: %q\n want: %q\n\n--- got ---\n%s\n--- want ---\n%s",
				i+1, g, w, got, want)
		}
	}
	// Only line terminators differ.
	return fmt.Sprintf("outputs differ only in trailing newlines:\n  got=%q\n want=%q", got, want)
}

// TestColorizedError locks down the ANSI sequence emitted when stdout is
// a tty (the color decision is made on stdout, not stderr).
func TestColorizedError(t *testing.T) {
	app, _, stderr := newGoldenApp(t, t.TempDir())
	app.StdoutIsTTY = func(io.Writer) bool { return true }

	if exit := app.Run([]string{testProg, "refs", "--bogus"}); exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}

	want := "\x1b[31m\x1b[1merror:\x1b[0m\x1b[22m Unknown option --bogus\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestVersionRendering checks the full YAML, including the optional Git
// and DevelBuild lines, without any normalization.
func TestVersionRendering(t *testing.T) {
	app := NewApp()
	app.Version = "1.2.3"
	app.Git = "abc123"
	app.DevelBuild = true
	app.Features = []string{"alpha", "beta"}
	var stdout, stderr bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stderr
	app.StdoutIsTTY = func(io.Writer) bool { return false }

	if exit := app.Run([]string{testProg, "--version"}); exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", exit, stderr.String())
	}

	want := "libostree:\n" +
		" Version: '1.2.3'\n" +
		" Git: abc123\n" +
		" DevelBuild: yes\n" +
		" Features:\n" +
		"  - alpha\n" +
		"  - beta\n"
	if got := stdout.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

// TestVersionNoFeatures checks the release-style output with no features
// and no Git revision, which is what NewApp currently produces.
func TestVersionNoFeatures(t *testing.T) {
	app := NewApp()
	var stdout, stderr bytes.Buffer
	app.Stdout = &stdout
	app.Stderr = &stderr
	app.StdoutIsTTY = func(io.Writer) bool { return false }

	if exit := app.Run([]string{testProg, "--version"}); exit != 0 {
		t.Fatalf("exit = %d, want 0", exit)
	}

	want := "libostree:\n Version: '2025.2'\n Features:\n"
	if got := stdout.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

// TestNotImplementedCommand documents the L1 behavior for registered but
// unimplemented commands: standard parse + repo resolution, then a
// "not implemented" error on stderr with exit 1.
func TestNotImplementedCommand(t *testing.T) {
	cwd := t.TempDir()
	repoDir := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config"), []byte("[core]\nrepo_version=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, stdout, stderr := newGoldenApp(t, cwd)
	if exit := app.Run([]string{testProg, "--repo=" + repoDir, "refs"}); exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	want := "error: refs: not implemented\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestRepoOptionBeforeCommand locks down the C quirk where `--repo PATH`
// before the command makes PATH the command name.
func TestRepoOptionBeforeCommand(t *testing.T) {
	cwd := t.TempDir()
	app, stdout, stderr := newGoldenApp(t, cwd)

	// The first non-option argument is PATH, so this is an unknown
	// command and --repo is unknown to the top-level context.
	exit := app.Run([]string{testProg, "--repo", filepath.Join(cwd, "somewhere"), "refs"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasSuffix(stderr.String(), "error: Unknown option --repo\n") {
		t.Errorf("stderr does not end with unknown --repo: %q", stderr.String())
	}

	// With --repo=..., the command name is refs and the repo is opened.
	stdout.Reset()
	stderr.Reset()
	exit = app.Run([]string{testProg, "--repo=" + filepath.Join(cwd, "missing"), "refs"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	want := "error: opening repo: opendir(" + filepath.Join(cwd, "missing") + "): No such file or directory\n"
	if got := stderr.String(); got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestDoubleDashPositional checks that "--" stops option processing.
func TestDoubleDashPositional(t *testing.T) {
	cwd := t.TempDir()
	repoDir := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config"), []byte("[core]\nrepo_version=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --help after -- is a positional argument, so the command runs (in
	// L1 that means "not implemented") instead of printing help.
	app, stdout, stderr := newGoldenApp(t, cwd)
	exit := app.Run([]string{testProg, "--repo=" + repoDir, "refs", "--", "--help"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty (help must not be printed)", stdout.String())
	}
	if got, want := stderr.String(), "error: refs: not implemented\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestUnknownOptionAfterDoubleDash checks that an unknown-looking option
// after -- is accepted as a positional argument.
func TestUnknownOptionAfterDoubleDash(t *testing.T) {
	cwd := t.TempDir()
	repoDir := filepath.Join(cwd, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "config"), []byte("[core]\nrepo_version=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	app, _, stderr := newGoldenApp(t, cwd)
	exit := app.Run([]string{testProg, "--repo=" + repoDir, "refs", "--", "--bogus"})
	if exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if got, want := stderr.String(), "error: refs: not implemented\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// TestAdminRootDispatch covers the admin root command (its help page is
// not byte-golden tested because the installed 2025.2 binary was built
// from a source revision that pads the subcommand column differently;
// see the L1 known differences).
func TestAdminRootDispatch(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantEnd string
	}{
		{"no-subcommand", []string{"admin"}, "error: No \"admin\" subcommand specified\n"},
		{"unknown-subcommand", []string{"admin", "bogus"}, "error: Unknown \"admin\" subcommand 'bogus'\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app, stdout, stderr := newGoldenApp(t, t.TempDir())
			argv := append([]string{testProg}, tc.args...)
			if exit := app.Run(argv); exit != 1 {
				t.Fatalf("exit = %d, want 1", exit)
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !strings.Contains(stderr.String(), "Builtin \"admin\" Commands:") {
				t.Errorf("stderr does not contain the admin summary:\n%s", stderr.String())
			}
			if !strings.HasSuffix(stderr.String(), tc.wantEnd) {
				t.Errorf("stderr does not end with %q:\n%s", tc.wantEnd, stderr.String())
			}
		})
	}
}

// TestAdminOptionInjection checks that admin commands get --sysroot and
// the hidden --print-current-dir but never --repo.
func TestAdminOptionInjection(t *testing.T) {
	app, stdout, stderr := newGoldenApp(t, t.TempDir())
	if exit := app.Run([]string{testProg, "admin", "--help"}); exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", exit, stderr.String())
	}
	help := stdout.String()
	if !strings.Contains(help, "  --sysroot=PATH") {
		t.Errorf("admin --help is missing --sysroot=PATH:\n%s", help)
	}
	if strings.Contains(help, "\n  --print-current-dir") {
		t.Errorf("hidden --print-current-dir leaked into help:\n%s", help)
	}
	if strings.Contains(help, "--repo=PATH") {
		t.Errorf("admin --help must not contain --repo:\n%s", help)
	}

	stdout.Reset()
	stderr.Reset()
	if exit := app.Run([]string{testProg, "admin", "status", "--help"}); exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", exit, stderr.String())
	}
	help = stdout.String()
	if !strings.Contains(help, "  "+testProg+" admin status [OPTION?]\n") {
		t.Errorf("admin status usage line is wrong:\n%s", help)
	}
	if !strings.Contains(help, "  --sysroot=PATH") {
		t.Errorf("admin status --help is missing --sysroot=PATH:\n%s", help)
	}
}

// TestVersionWinsOverUnknownCommand mirrors the C flow: options are
// parsed before the unknown-command error is raised, so --version exits
// successfully even when the command word is bogus.
func TestVersionWinsOverUnknownCommand(t *testing.T) {
	app, stdout, stderr := newGoldenApp(t, t.TempDir())
	if exit := app.Run([]string{testProg, "frobnicate", "--version"}); exit != 0 {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", exit, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "libostree:\n") {
		t.Fatalf("stdout = %q, want version YAML", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

// TestEmptyCommandName covers the C quirk that "" is considered a
// command word (argv[in][0] != '-' is true for the empty string).
func TestEmptyCommandName(t *testing.T) {
	app, stdout, stderr := newGoldenApp(t, t.TempDir())
	if exit := app.Run([]string{testProg, "", "refs"}); exit != 1 {
		t.Fatalf("exit = %d, want 1", exit)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.HasSuffix(stderr.String(), "error: Unknown command ''\n") {
		t.Fatalf("stderr does not end with unknown empty command:\n%s", stderr.String())
	}
}
