package cli

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// helpEntryMinWidth is the width GLib reserves for the built-in
// "-?, --help" entry (10 columns) when computing the option column.
const helpEntryMinWidth = len("-?, --help")

// helpSpec is everything needed to render one help page in the exact
// GOption layout used by the C CLI.
type helpSpec struct {
	// prog is the program name as printed in the usage line, e.g.
	// "/usr/bin/ostree refs".  The C CLI uses argv[0] verbatim.
	prog string
	// usageParam is g_option_context_new()'s parameter string.  An
	// empty string means no parameter (GOption treats "" like NULL).
	usageParam string
	// summary is the GOption summary block (description and/or the
	// builtin command list), already formatted.
	summary string
	// options are the main group entries in display/parse order.
	options []Option
}

// render produces the exact bytes printed by `--help`.
//
// Layout, in order:
//
//	Usage:
//	  PROG [OPTION?] PARAM
//
//	SUMMARY
//
//	Help Options:
//	  -h, --help        Show help options
//
//	Application Options:
//	  ...
//
// with a trailing empty line (the whole text ends in "\n\n").
func (s *helpSpec) render() string {
	var b strings.Builder

	b.WriteString("Usage:\n  ")
	b.WriteString(s.prog)
	b.WriteString(" [OPTION?]")
	if s.usageParam != "" {
		b.WriteString(" ")
		b.WriteString(s.usageParam)
	}
	b.WriteString("\n\n")

	if s.summary != "" {
		b.WriteString(s.summary)
		b.WriteString("\n\n")
	}

	// GOption computes one column width shared by the help and
	// application groups.  Hidden entries do not participate.
	maxEntryWidth := helpEntryMinWidth
	for _, o := range s.options {
		if o.Hidden {
			continue
		}
		if w := helpOptionWidth(o); w > maxEntryWidth {
			maxEntryWidth = w
		}
	}
	// GLib appends four columns of padding to the maximum entry width.
	maxLength := maxEntryWidth + 4

	b.WriteString("Help Options:\n")
	// "  -h, --%-*s %s\n" with the width being maxLength-4.
	fmt.Fprintf(&b, "  -h, --%-*s %s\n", maxLength-4, "help", "Show help options")
	b.WriteString("\n")

	visible := 0
	for _, o := range s.options {
		if !o.Hidden && o.LongName != "" {
			visible++
		}
	}
	if visible > 0 {
		b.WriteString("Application Options:\n")
		for _, o := range s.options {
			if o.Hidden || o.LongName == "" {
				continue
			}
			b.WriteString(renderHelpOption(o, maxLength))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// helpOptionWidth is GLib's calculate_max_length() for a single entry:
// the long name, plus "-x, " for a short name, plus "=ARG".
func helpOptionWidth(o Option) int {
	width := utf8.RuneCountInString(o.LongName)
	if o.ShortName != 0 {
		width += 4 // "-x, "
	}
	if o.ArgType.takesValue() && o.ArgDescription != "" {
		width += 1 + utf8.RuneCountInString(o.ArgDescription)
	}
	return width
}

// renderHelpOption formats one "Application Options" entry.
func renderHelpOption(o Option, maxLength int) string {
	var line strings.Builder
	line.WriteString("  ")
	if o.ShortName != 0 {
		fmt.Fprintf(&line, "-%c, ", o.ShortName)
	}
	line.WriteString("--")
	line.WriteString(o.LongName)
	if o.ArgDescription != "" {
		line.WriteString("=")
		line.WriteString(o.ArgDescription)
	}

	// GLib pads with maxLength+4-width(str) spaces and then adds a
	// single separating space before the description.
	padding := maxLength + 4 - utf8.RuneCountInString(line.String())
	if padding < 0 {
		padding = 0
	}
	return line.String() + strings.Repeat(" ", padding) + " " + o.Description + "\n"
}

// renderCommandSummary renders a "Builtin ... Commands:" block in the
// format used by the top level CLI and by root commands.  The first line
// is title, every following line is "  <name padded to width><desc>".
func renderCommandSummary(title string, width int, commands []*Command) string {
	var b strings.Builder
	b.WriteString(title)
	for _, c := range commands {
		if c.isHidden() {
			continue
		}
		fmt.Fprintf(&b, "\n  %-*s%s", width, c.Name, c.Description)
	}
	return b.String()
}

// renderLegacyStaticDeltaUsage reproduces the hand-written usage text of
// `ostree static-delta`, which deliberately does not use GOption help.
func renderLegacyStaticDeltaUsage(subcommands []*Command) string {
	var b strings.Builder
	b.WriteString("Usage:\n  ostree static-delta [OPTION...] COMMAND\n\n")
	b.WriteString("Builtin \"static-delta\" Commands:\n")
	for _, c := range subcommands {
		if c.isHidden() {
			continue
		}
		fmt.Fprintf(&b, "  %-17s%s\n", c.Name, c.Description)
	}
	b.WriteString("\n")
	return b.String()
}
