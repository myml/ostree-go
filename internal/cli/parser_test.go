package cli

import (
	"reflect"
	"strings"
	"testing"
)

// testOptions is a small option table exercising every parser feature
// (short flags, short values, long values, arrays, hidden entries).
func testOptions() []Option {
	return []Option{
		{LongName: "repo", ArgType: ArgFilename, ArgDescription: "PATH"},
		{LongName: "verbose", ShortName: 'v', ArgType: ArgNone},
		{LongName: "revision", ShortName: 'r', ArgType: ArgNone},
		{LongName: "create", ArgType: ArgString, ArgDescription: "NEWREF"},
		{LongName: "meta", ShortName: 'm', ArgType: ArgString},
	}
}

func TestParseShortAggregation(t *testing.T) {
	parsed, positional, action, err := parseOptions(testOptions(), []string{"-rv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != parseActionNone {
		t.Fatalf("action = %v, want none", action)
	}
	if len(positional) != 0 {
		t.Fatalf("positional = %v, want empty", positional)
	}
	if !parsed.Has("revision") || !parsed.Has("verbose") {
		t.Fatalf("aggregated flags not set: %+v", parsed)
	}
	if !parsed.Has("verbose") || parsed.Count("verbose") != 1 {
		t.Fatalf("verbose count = %d, want 1", parsed.Count("verbose"))
	}
}

func TestParseRepeatedFlagCount(t *testing.T) {
	parsed, _, _, err := parseOptions(testOptions(), []string{"-vvv"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := parsed.Count("verbose"); got != 3 {
		t.Fatalf("verbose count = %d, want 3", got)
	}
}

func TestParseDoubleDash(t *testing.T) {
	parsed, positional, action, err := parseOptions(testOptions(), []string{"--", "--help", "-r"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != parseActionNone {
		t.Fatalf("action = %v, want none (post -- args are positional)", action)
	}
	if parsed.Has("revision") {
		t.Fatal("revision was parsed after --")
	}
	want := []string{"--help", "-r"}
	if !reflect.DeepEqual(positional, want) {
		t.Fatalf("positional = %v, want %v", positional, want)
	}
}

func TestParseBareDashIsPositional(t *testing.T) {
	_, positional, _, err := parseOptions(testOptions(), []string{"-"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(positional, []string{"-"}) {
		t.Fatalf("positional = %v, want [-]", positional)
	}
}

func TestParseGNUOrder(t *testing.T) {
	parsed, positional, _, err := parseOptions(testOptions(), []string{"foo", "-r", "bar"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.Has("revision") {
		t.Fatal("option after positional was not parsed")
	}
	if want := []string{"foo", "bar"}; !reflect.DeepEqual(positional, want) {
		t.Fatalf("positional = %v, want %v", positional, want)
	}
}

func TestParseLongEqualsValue(t *testing.T) {
	parsed, _, _, err := parseOptions(testOptions(), []string{"--repo=/tmp/x", "--create=refs/heads/x"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := parsed.String("repo"); got != "/tmp/x" {
		t.Fatalf("repo = %q, want /tmp/x", got)
	}
	if got := parsed.String("create"); got != "refs/heads/x" {
		t.Fatalf("create = %q, want refs/heads/x", got)
	}
}

func TestParseLongEmptyValue(t *testing.T) {
	parsed, _, _, err := parseOptions(testOptions(), []string{"--repo="})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.Has("repo") {
		t.Fatal("empty --repo= should still count as seen")
	}
	if got := parsed.String("repo"); got != "" {
		t.Fatalf("repo = %q, want empty", got)
	}
}

func TestParseLongValueFromNextArg(t *testing.T) {
	parsed, positional, _, err := parseOptions(testOptions(), []string{"--repo", "-not-an-option"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := parsed.String("repo"); got != "-not-an-option" {
		t.Fatalf("repo = %q, want -not-an-option", got)
	}
	if len(positional) != 0 {
		t.Fatalf("positional = %v, want empty", positional)
	}
}

func TestParseFlagWithValueIsIgnored(t *testing.T) {
	parsed, _, _, err := parseOptions(testOptions(), []string{"--verbose=yes"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.Has("verbose") {
		t.Fatal("--verbose=yes should set verbose")
	}
}

func TestParseUnknownLong(t *testing.T) {
	for _, arg := range []string{"--bogus", "--bogus=x", "--bogus="} {
		_, _, _, err := parseOptions(testOptions(), []string{arg})
		if err == nil || err.Error() != "Unknown option "+arg {
			t.Fatalf("parse(%q) error = %v, want %q", arg, err, "Unknown option "+arg)
		}
	}
}

// TestParseNoLongAbbreviation locks down GOption's exact-match rule.
func TestParseNoLongAbbreviation(t *testing.T) {
	for _, arg := range []string{"--revis", "--repos"} {
		_, _, _, err := parseOptions(testOptions(), []string{arg})
		if err == nil || err.Error() != "Unknown option "+arg {
			t.Fatalf("parse(%q) error = %v, want %q", arg, err, "Unknown option "+arg)
		}
	}
}

func TestParseUnknownShortUsesWholeToken(t *testing.T) {
	for _, arg := range []string{"-z", "-rz", "-r=x"} {
		_, _, _, err := parseOptions(testOptions(), []string{arg})
		if err == nil || err.Error() != "Unknown option "+arg {
			t.Fatalf("parse(%q) error = %v, want %q", arg, err, "Unknown option "+arg)
		}
	}
}

func TestParseMissingArgument(t *testing.T) {
	_, _, _, err := parseOptions(testOptions(), []string{"--repo"})
	if err == nil || err.Error() != "Missing argument for --repo" {
		t.Fatalf("error = %v, want Missing argument for --repo", err)
	}
	_, _, _, err = parseOptions(testOptions(), []string{"-m"})
	if err == nil || err.Error() != "Missing argument for -m" {
		t.Fatalf("error = %v, want Missing argument for -m", err)
	}
}

// TestParseShortValueAttachedNotSupported locks down GOption's behavior:
// a short option with a value always takes the *next* argv element, so
// "-mfoo" parses "-m -f -o -o", and with a following argv element the
// unknown -f is reported using the whole token.
func TestParseShortValueAttachedNotSupported(t *testing.T) {
	_, _, _, err := parseOptions(testOptions(), []string{"-mfoo"})
	if err == nil || err.Error() != "Missing argument for -m" {
		t.Fatalf("error = %v, want Missing argument for -m", err)
	}

	_, _, _, err = parseOptions(testOptions(), []string{"-mfoo", "bar"})
	if err == nil || err.Error() != "Unknown option -mfoo" {
		t.Fatalf("error = %v, want Unknown option -mfoo", err)
	}
}

func TestParseShortValueConsumesNextArg(t *testing.T) {
	parsed, positional, _, err := parseOptions(testOptions(), []string{"-vm", "KEY=VALUE", "rest"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !parsed.Has("verbose") {
		t.Fatal("verbose not set")
	}
	if got := parsed.String("meta"); got != "KEY=VALUE" {
		t.Fatalf("meta = %q, want KEY=VALUE", got)
	}
	if want := []string{"rest"}; !reflect.DeepEqual(positional, want) {
		t.Fatalf("positional = %v, want %v", positional, want)
	}
}

func TestParseTwoShortValueOptionsInBundle(t *testing.T) {
	options := []Option{
		{LongName: "one", ShortName: 'm', ArgType: ArgString},
		{LongName: "two", ShortName: 'n', ArgType: ArgString},
	}
	_, _, _, err := parseOptions(options, []string{"-mn", "x"})
	if err == nil || err.Error() != "Error parsing option -n" {
		t.Fatalf("error = %v, want Error parsing option -n", err)
	}
}

func TestParseHelpActions(t *testing.T) {
	cases := []struct {
		args   []string
		action parseAction
	}{
		{[]string{"--help"}, parseActionHelp},
		{[]string{"--help-all"}, parseActionHelpAll},
		{[]string{"-h"}, parseActionHelp},
		{[]string{"-?"}, parseActionHelp},
		{[]string{"-rh"}, parseActionHelp},
		{[]string{"--help=x"}, parseActionNone}, // unknown option, not help
	}
	for _, tc := range cases {
		_, _, action, err := parseOptions(testOptions(), tc.args)
		if action != tc.action {
			t.Errorf("parse(%v) action = %v, want %v", tc.args, action, tc.action)
		}
		if tc.action == parseActionNone && err == nil {
			t.Errorf("parse(%v) should fail", tc.args)
		}
	}
}

func TestParseHelpBeforeErrorWins(t *testing.T) {
	_, _, action, err := parseOptions(testOptions(), []string{"--help", "--bogus"})
	if err != nil || action != parseActionHelp {
		t.Fatalf("action=%v err=%v, want help with no error", action, err)
	}
	_, _, action, err = parseOptions(testOptions(), []string{"--bogus", "--help"})
	if err == nil || err.Error() != "Unknown option --bogus" || action != parseActionNone {
		t.Fatalf("action=%v err=%v, want unknown option error", action, err)
	}
}

func TestParseIntegerOption(t *testing.T) {
	options := []Option{{LongName: "count", ArgType: ArgInt}}
	parsed, _, _, err := parseOptions(options, []string{"--count=42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := parsed.Int("count"); got != 42 {
		t.Fatalf("count = %d, want 42", got)
	}

	_, _, _, err = parseOptions(options, []string{"--count=x"})
	if err == nil || err.Error() != "Cannot parse integer value “x” for --count" {
		t.Fatalf("error = %v", err)
	}

	_, _, _, err = parseOptions(options, []string{"--count=99999999999"})
	if err == nil || err.Error() != "Integer value “99999999999” for --count out of range" {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractCommand(t *testing.T) {
	cases := []struct {
		args  []string
		name  string
		found bool
		rest  []string
	}{
		{[]string{"refs"}, "refs", true, []string{}},
		{[]string{"-v", "refs", "--repo=/x"}, "refs", true, []string{"-v", "--repo=/x"}},
		// --repo PATH steals PATH as the command name.
		{[]string{"--repo", "/x", "refs"}, "/x", true, []string{"--repo", "refs"}},
		{[]string{}, "", false, []string{}},
		{[]string{"-v"}, "", false, []string{"-v"}},
		// The empty string counts as a (bogus) command.
		{[]string{""}, "", true, []string{}},
		{[]string{"-", "refs"}, "refs", true, []string{"-"}},
	}
	for _, tc := range cases {
		name, found, rest := extractCommand(tc.args)
		if name != tc.name || found != tc.found || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("extractCommand(%v) = (%q, %v, %v), want (%q, %v, %v)",
				tc.args, name, found, rest, tc.name, tc.found, tc.rest)
		}
	}
}

func TestExtractRootCommandTruncatesAtDoubleDash(t *testing.T) {
	cases := []struct {
		args  []string
		name  string
		found bool
		rest  []string
	}{
		{[]string{"status", "--sysroot=/x"}, "status", true, []string{"--sysroot=/x"}},
		{[]string{"--sysroot=/x", "status"}, "status", true, []string{"--sysroot=/x"}},
		// admin/remote stop scanning at "--" and drop the rest.
		{[]string{"--", "--help"}, "", false, []string{}},
		{[]string{"--sysroot=/x", "--", "status"}, "", false, []string{"--sysroot=/x"}},
	}
	for _, tc := range cases {
		name, found, rest := extractRootCommand(tc.args)
		if name != tc.name || found != tc.found || !reflect.DeepEqual(rest, tc.rest) {
			t.Errorf("extractRootCommand(%v) = (%q, %v, %v), want (%q, %v, %v)",
				tc.args, name, found, rest, tc.name, tc.found, tc.rest)
		}
	}
}

// TestHelpRendering checks the GLib two-column layout, hidden entries,
// the absence of "=VALUE" without an arg description and the trailing
// blank line.
func TestHelpRendering(t *testing.T) {
	spec := &helpSpec{
		prog:       "/usr/bin/ostree demo",
		usageParam: "",
		summary:    "Demo command",
		options: []Option{
			{LongName: "flag", ShortName: 'f', ArgType: ArgNone, Description: "A flag"},
			{LongName: "secret", ArgType: ArgNone, Hidden: true, Description: "Hidden"},
			{LongName: "delete", ArgType: ArgNone, Description: "Delete"},
			{LongName: "create", ArgType: ArgString, ArgDescription: "NEWREF", Description: "Create"},
			{LongName: "verbose", ShortName: 'v', ArgType: ArgNone, Description: "Verbose"},
		},
	}

	// Longest entry: --create=NEWREF (13); GLib adds 4 columns and one
	// separating space, so every description starts at column 22.
	want := "Usage:\n" +
		"  /usr/bin/ostree demo [OPTION?]\n" +
		"\n" +
		"Demo command\n" +
		"\n" +
		"Help Options:\n" +
		"  -h, --help          Show help options\n" +
		"\n" +
		"Application Options:\n" +
		"  -f, --flag          A flag\n" +
		"  --delete            Delete\n" +
		"  --create=NEWREF     Create\n" +
		"  -v, --verbose       Verbose\n" +
		"\n"

	if got := spec.render(); got != want {
		t.Fatalf("help mismatch\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	if strings.Contains(spec.render(), "secret") {
		t.Fatal("hidden option leaked into help")
	}
}

// TestHelpWithoutArgDescription verifies that "=VALUE" is only rendered
// when an arg description exists, even for options that take a value.
func TestHelpWithoutArgDescription(t *testing.T) {
	spec := &helpSpec{
		prog: "ostree demo",
		options: []Option{
			{LongName: "silent", ArgType: ArgString, Description: "Silent value"},
		},
	}
	help := spec.render()
	if strings.Contains(help, "=VALUE") || strings.Contains(help, "--silent=") {
		t.Fatalf("unexpected =VALUE in help:\n%s", help)
	}
	if !strings.Contains(help, "  --silent") {
		t.Fatalf("missing --silent in help:\n%s", help)
	}
}

// TestDefaultCommandTables pins the refs/rev-parse/cat option tables
// against the C sources.
func TestDefaultCommandTables(t *testing.T) {
	commands := DefaultCommands()
	if len(commands) != 26 {
		t.Fatalf("len(DefaultCommands()) = %d, want 26", len(commands))
	}

	wantNames := []string{
		"admin", "cat", "checkout", "checksum", "commit", "config", "diff", "export",
		"find-remotes", "create-usb", "fsck", "gpg-sign", "init", "log", "ls", "prune",
		"pull-local", "pull", "refs", "remote", "reset", "rev-parse", "sign", "show",
		"static-delta", "summary",
	}
	for i, want := range wantNames {
		if commands[i].Name != want {
			t.Fatalf("command[%d] = %q, want %q", i, commands[i].Name, want)
		}
	}

	refs := findCommand(commands, "refs")
	wantRefs := []struct {
		long  string
		short byte
		arg   ArgType
		desc  string
	}{
		{"delete", 0, ArgNone, ""},
		{"list", 0, ArgNone, ""},
		{"revision", 'r', ArgNone, ""},
		{"alias", 'A', ArgNone, ""},
		{"create", 0, ArgString, "NEWREF"},
		{"collections", 'c', ArgNone, ""},
		{"force", 0, ArgNone, ""},
	}
	if len(refs.Options) != len(wantRefs) {
		t.Fatalf("refs has %d options, want %d", len(refs.Options), len(wantRefs))
	}
	for i, want := range wantRefs {
		got := refs.Options[i]
		if got.LongName != want.long || got.ShortName != want.short ||
			got.ArgType != want.arg || got.ArgDescription != want.desc {
			t.Errorf("refs option[%d] = %+v, want long=%q short=%q type=%v argdesc=%q",
				i, got, want.long, want.short, want.arg, want.desc)
		}
		if got.Description == "" {
			t.Errorf("refs option %q has no description", got.LongName)
		}
	}

	revParse := findCommand(commands, "rev-parse")
	if len(revParse.Options) != 1 {
		t.Fatalf("rev-parse has %d options, want 1", len(revParse.Options))
	}
	if got := revParse.Options[0]; got.LongName != "single" || got.ShortName != 'S' ||
		got.ArgType != ArgNone || got.Description == "" {
		t.Fatalf("rev-parse --single = %+v", got)
	}

	cat := findCommand(commands, "cat")
	if len(cat.Options) != 0 {
		t.Fatalf("cat has %d options, want 0", len(cat.Options))
	}
	if cat.Usage != "COMMIT PATH..." {
		t.Fatalf("cat usage = %q, want COMMIT PATH...", cat.Usage)
	}
}
