// Package cli implements the command-line frontend of the Go OSTree
// rewrite: the command table, a GOption-compatible argument parser, GLib
// style help rendering, dispatch and exit-code/error handling.
//
// The parser is deliberately hand written.  The C OSTree binary uses
// GLib's GOption parser and the observable behavior (accepted syntax,
// error strings, exit codes) is part of the compatibility contract, so
// neither the standard library flag package nor pflag can be used.
package cli

import (
	"fmt"
	"math"
	"strconv"
)

// ArgType mirrors GOptionArg.  Only the variants used by the OSTree CLI
// are modeled; the parser accepts all of them so later layers can add
// option tables without touching the framework.
type ArgType int

const (
	// ArgNone is G_OPTION_ARG_NONE: the option takes no argument.
	ArgNone ArgType = iota
	// ArgString is G_OPTION_ARG_STRING.
	ArgString
	// ArgFilename is G_OPTION_ARG_FILENAME (equivalent to ArgString
	// for parsing purposes; the distinction matters for i18n only).
	ArgFilename
	// ArgInt is G_OPTION_ARG_INT.
	ArgInt
	// ArgStringArray is G_OPTION_ARG_STRING_ARRAY.
	ArgStringArray
	// ArgFilenameArray is G_OPTION_ARG_FILENAME_ARRAY.
	ArgFilenameArray
)

// takesValue reports whether the option consumes an argument.
func (t ArgType) takesValue() bool { return t != ArgNone }

// Option describes one command line option.  The fields have the same
// meaning as the corresponding GOptionEntry fields.
type Option struct {
	// LongName is the option name without the leading "--".  It may be
	// empty for short-only options (none exist in OSTree today).
	LongName string
	// ShortName is the single-character option name, or 0.
	ShortName byte
	// ArgType is the GOption argument type.
	ArgType ArgType
	// ArgDescription is the placeholder shown in --help as "=VALUE".
	// An empty string means no "=VALUE" is rendered.
	ArgDescription string
	// Description is the help text.
	Description string
	// Hidden options are accepted by the parser but omitted from help.
	Hidden bool
}

// canonicalName is the key used to record a parsed option.  Options are
// keyed by their long name (matching how callers think about them) and
// fall back to "-x" for short-only options.
func (o Option) canonicalName() string {
	if o.LongName != "" {
		return o.LongName
	}
	return "-" + string(o.ShortName)
}

// ParsedOptions holds the result of parsing a command line.
type ParsedOptions struct {
	flags  map[string]int
	values map[string][]string
}

func newParsedOptions() *ParsedOptions {
	return &ParsedOptions{
		flags:  make(map[string]int),
		values: make(map[string][]string),
	}
}

func (p *ParsedOptions) addFlag(name string) { p.flags[name]++ }

func (p *ParsedOptions) addValue(name, value string) {
	p.values[name] = append(p.values[name], value)
}

// Has reports whether the option was seen at least once.
func (p *ParsedOptions) Has(name string) bool {
	if p == nil {
		return false
	}
	return p.flags[name] > 0 || len(p.values[name]) > 0
}

// Count returns how many times the option was seen.  It is mostly useful
// for repeated flags such as -vvv.
func (p *ParsedOptions) Count(name string) int {
	if p == nil {
		return 0
	}
	return p.flags[name] + len(p.values[name])
}

// String returns the last value given for a string option, or "".
func (p *ParsedOptions) String(name string) string {
	if p == nil {
		return ""
	}
	values := p.values[name]
	if len(values) == 0 {
		return ""
	}
	return values[len(values)-1]
}

// Strings returns all values given for an array option.
func (p *ParsedOptions) Strings(name string) []string {
	if p == nil {
		return nil
	}
	values := p.values[name]
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
}

// Int returns the (already validated) integer value of an option.
func (p *ParsedOptions) Int(name string) int {
	raw := p.String(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// parseInteger mirrors GLib's parse_int() including its error strings.
func parseInteger(value, optionName string) (int64, error) {
	if value == "" {
		return 0, fmt.Errorf("Cannot parse integer value “%s” for %s", value, optionName)
	}
	n, err := strconv.ParseInt(value, 0, 64)
	if err != nil {
		if nerr, ok := err.(*strconv.NumError); ok && nerr.Err == strconv.ErrRange {
			return 0, fmt.Errorf("Integer value “%s” for %s out of range", value, optionName)
		}
		return 0, fmt.Errorf("Cannot parse integer value “%s” for %s", value, optionName)
	}
	// GOption stores ints in a gint.
	if n < math.MinInt32 || n > math.MaxInt32 {
		return 0, fmt.Errorf("Integer value “%s” for %s out of range", value, optionName)
	}
	return n, nil
}

// record stores a parsed option after validating it.
func (p *ParsedOptions) record(o Option, optionName, value string) error {
	if !o.ArgType.takesValue() {
		// GLib accepts "--flag=value" for G_OPTION_ARG_NONE and
		// silently ignores the value; keep that behavior.
		p.addFlag(o.canonicalName())
		return nil
	}

	switch o.ArgType {
	case ArgInt:
		n, err := parseInteger(value, optionName)
		if err != nil {
			return err
		}
		p.addValue(o.canonicalName(), strconv.FormatInt(n, 10))
	default:
		p.addValue(o.canonicalName(), value)
	}
	return nil
}

// findShortOption returns the option with the given short name.
func findShortOption(options []Option, name byte) (Option, bool) {
	for _, o := range options {
		if o.ShortName == name {
			return o, true
		}
	}
	return Option{}, false
}

// hasShortHelpEntry reports whether any option claims the -h short name,
// in which case -h is not the built-in help flag (GLib semantics).
func hasShortHelpEntry(options []Option) bool {
	_, ok := findShortOption(options, 'h')
	return ok
}
