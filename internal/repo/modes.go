package repo

import (
	"fmt"
	"strings"
)

// Mode is an OSTree repository mode. The numeric values match the libostree
// OstreeRepoMode enum.
//
// ModeArchiveZ2 is a legacy alias for ModeArchive; "archive" and "archive-z2"
// are both accepted by ParseMode. On disk, libostree writes the legacy
// "archive-z2" spelling, which ConfigString returns.
type Mode int

const (
	ModeBare            Mode = 0
	ModeBareUser        Mode = 1
	ModeBareUserOnly    Mode = 2
	ModeArchive         Mode = 3
	ModeBareSplitXattrs Mode = 4

	// ModeArchiveZ2 is the legacy name for ModeArchive.
	ModeArchiveZ2 = ModeArchive
)

// Valid reports whether m is a known repository mode.
func (m Mode) Valid() bool {
	return m >= ModeBare && m <= ModeBareSplitXattrs
}

// String returns the canonical user-facing name of the mode.
//
// The archive mode is returned as "archive"; use ConfigString for the on-disk
// spelling used by libostree ("archive-z2").
func (m Mode) String() string {
	switch m {
	case ModeBare:
		return "bare"
	case ModeBareUser:
		return "bare-user"
	case ModeBareUserOnly:
		return "bare-user-only"
	case ModeArchive:
		return "archive"
	case ModeBareSplitXattrs:
		return "bare-split-xattrs"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// ConfigString returns the mode string written to the [core] mode key by
// libostree. It differs from String only for the archive mode.
func (m Mode) ConfigString() string {
	if m == ModeArchive {
		return "archive-z2"
	}
	return m.String()
}

// IsArchive reports whether objects in this mode use the archive layout.
func (m Mode) IsArchive() bool { return m == ModeArchive }

// ParseMode parses a mode name from a config file or command line. Both
// "archive" and the legacy alias "archive-z2" are accepted.
func ParseMode(s string) (Mode, error) {
	switch s {
	case "bare":
		return ModeBare, nil
	case "bare-user":
		return ModeBareUser, nil
	case "bare-user-only":
		return ModeBareUserOnly, nil
	case "archive", "archive-z2":
		return ModeArchive, nil
	case "bare-split-xattrs":
		return ModeBareSplitXattrs, nil
	default:
		return 0, fmt.Errorf("%w: %q", ErrInvalidMode, s)
	}
}

// trimConfigValue trims the ASCII whitespace that GKeyFile may leave around a
// raw value.
func trimConfigValue(s string) string { return strings.TrimSpace(s) }
