package cli

import (
	"fmt"
)

// parseAction is the result of a help-ish option encountered while
// parsing.  GLib prints the help text and exits immediately from inside
// the parser; we return the action to the caller instead so that output
// goes through the App's writers.
type parseAction int

const (
	parseActionNone parseAction = iota
	parseActionHelp
	parseActionHelpAll
)

// parseOptions implements the subset of GOption parsing that the OSTree
// CLI depends on.
//
// Semantics (all verified against glib/goption.c and the 2025.2 C
// binary):
//
//   - parsing continues after positional arguments (GNU style);
//   - long options match exactly, never by abbreviation;
//   - --name=value is supported, --name value is supported for options
//     that take a value;
//   - "--flag=value" is accepted for flags and the value ignored;
//   - short options can be bundled (-vvv), but a short option that takes
//     an argument never consumes the remainder of the bundle: it always
//     takes the next argv element.  This means -mfoo is parsed as -m -f
//     -o -o with the following argument (if any) as -m's value;
//   - a lone "--" terminates option parsing and is removed;
//   - "-" is a positional argument;
//   - unknown options are reported with the complete argv element;
//   - --help/--help-all behave like GLib: print help and exit 0.
func parseOptions(options []Option, args []string) (*ParsedOptions, []string, parseAction, error) {
	parsed := newParsedOptions()
	positional := make([]string, 0, len(args))
	builtinHelp := !hasShortHelpEntry(options)

	for i := 0; i < len(args); i++ {
		arg := args[i]

		// A lone "-" and anything not starting with "-" is positional.
		if len(arg) < 2 || arg[0] != '-' {
			positional = append(positional, arg)
			continue
		}

		if arg == "--" {
			// A lone "--" terminates option parsing and is removed;
			// everything after it is positional.
			positional = append(positional, args[i+1:]...)
			break
		}

		if arg[1] == '-' {
			action, err := parseLongArg(options, args, &i, arg, parsed)
			if err != nil {
				return nil, nil, parseActionNone, err
			}
			if action != parseActionNone {
				return parsed, positional, action, nil
			}
			continue
		}

		// Short option bundle.
		bundle := arg[1:]
		startI := i
		newI := i
		for j := 0; j < len(bundle); j++ {
			c := bundle[j]
			if builtinHelp && (c == '?' || c == 'h') {
				return parsed, positional, parseActionHelp, nil
			}

			o, ok := findShortOption(options, c)
			if !ok {
				// GLib reports the complete argv element, not the
				// single character that failed to match.
				return nil, nil, parseActionNone, fmt.Errorf("Unknown option %s", arg)
			}

			if !o.ArgType.takesValue() {
				parsed.record(o, "-"+string(c), "")
				continue
			}

			// An earlier option in the same bundle already consumed
			// the next argv element; GOption rejects that.
			if newI > startI {
				return nil, nil, parseActionNone, fmt.Errorf("Error parsing option -%c", c)
			}
			if i < len(args)-1 {
				i++
				newI = i
				value := args[i]
				if err := parsed.record(o, "-"+string(c), value); err != nil {
					return nil, nil, parseActionNone, err
				}
				continue
			}
			return nil, nil, parseActionNone, fmt.Errorf("Missing argument for -%c", c)
		}
	}

	return parsed, positional, parseActionNone, nil
}

// parseLongArg handles one "--..." token.  It returns a non-zero action
// for the built-in help options.
func parseLongArg(options []Option, args []string, idx *int, arg string, parsed *ParsedOptions) (parseAction, error) {
	body := arg[2:]

	// Built-in help options.  GLib also supports --help-<group>, but
	// the OSTree CLI never registers option groups, so such tokens fall
	// through and become "Unknown option".
	switch body {
	case "help":
		return parseActionHelp, nil
	case "help-all":
		return parseActionHelpAll, nil
	}

	for _, o := range options {
		if o.LongName == "" {
			continue
		}

		if !o.ArgType.takesValue() && body == o.LongName {
			parsed.record(o, "--"+o.LongName, "")
			return parseActionNone, nil
		}

		name := o.LongName
		if len(body) < len(name) || body[:len(name)] != name {
			continue
		}
		if len(body) != len(name) && body[len(name)] != '=' {
			continue
		}

		var value string
		if len(body) > len(name) {
			// body[len(name)] == '='
			value = body[len(name)+1:]
		} else if *idx < len(args)-1 {
			*idx = *idx + 1
			value = args[*idx]
		} else {
			return parseActionNone, fmt.Errorf("Missing argument for --%s", name)
		}

		if err := parsed.record(o, "--"+name, value); err != nil {
			return parseActionNone, err
		}
		return parseActionNone, nil
	}

	return parseActionNone, fmt.Errorf("Unknown option %s", arg)
}
