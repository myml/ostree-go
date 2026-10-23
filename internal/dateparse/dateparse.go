// Package dateparse parses the human readable dates accepted by the OSTree
// CLI (commit --timestamp, prune --keep-younger-than and friends).
//
// The C implementation uses gnulib's parse_datetime(); this package
// implements the subset of its syntax that OSTree documents and tests:
// ISO-like timestamps, month-name dates and simple relative expressions such
// as "1 week ago".  Absolute dates without an explicit zone are interpreted
// as UTC, matching the upstream test harness (TZ=UTC).
package dateparse

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Parse converts value into seconds since the Unix epoch.
func Parse(value string) (int64, error) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, fmt.Errorf("empty date")
	}
	lower := strings.ToLower(s)
	now := time.Now().UTC()

	switch lower {
	case "now", "today":
		return now.Unix(), nil
	case "yesterday":
		return now.Add(-24 * time.Hour).Unix(), nil
	case "tomorrow":
		return now.Add(24 * time.Hour).Unix(), nil
	}

	if m := relativeDateRe.FindStringSubmatch(lower); m != nil {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, err
		}
		unit := time.Hour
		switch strings.TrimSuffix(m[2], "s") {
		case "second":
			unit = time.Second
		case "minute":
			unit = time.Minute
		case "hour":
			unit = time.Hour
		case "day":
			unit = 24 * time.Hour
		case "week":
			unit = 7 * 24 * time.Hour
		case "month":
			unit = 30 * 24 * time.Hour
		case "year":
			unit = 365 * 24 * time.Hour
		}
		delta := time.Duration(n) * unit
		if m[3] == "ago" {
			return now.Add(-delta).Unix(), nil
		}
		return now.Add(delta).Unix(), nil
	}

	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Unix(), nil
		}
	}
	return 0, fmt.Errorf("invalid date %q", value)
}

var relativeDateRe = regexp.MustCompile(`^([0-9]+)\s+([a-z]+)\s*(ago)?$`)

var dateLayouts = []string{
	"2006-01-02 15:04:05 -0700",
	"2006-01-02 15:04:05 -07:00",
	"2006-01-02T15:04:05Z07:00",
	"2006-01-02T15:04:05 -0700",
	"2006-01-02 15:04:05 MST",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
	// gnulib's parse_datetime() accepts textual dates with a zero-padded or
	// unpadded day, e.g. "October 25 1985" and "November 05 1955".
	"January 2 2006",
	"January 02 2006",
	"Jan 2 2006",
	"Jan 02 2006",
	"January 2, 2006",
	"January 02, 2006",
	"Jan 2, 2006",
	"Jan 02, 2006",
	"2 January 2006",
	"02 January 2006",
	"2 Jan 2006",
	"02 Jan 2006",
}
