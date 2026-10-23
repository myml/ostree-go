package dateparse

import (
	"testing"
	"time"
)

func TestParseAbsolute(t *testing.T) {
	tests := []struct {
		value string
		want  int64
	}{
		{"2000-01-01", 946684800},
		{"2015-10-29 12:43:29 +0000", 1446122609},
		{"1985-10-25 00:00:00 +0000", 499046400},
		{"October 25 1985", 499046400},
		{"October 5 1985", 497318400},
		// Zero-padded day, used by test-prune.
		{"November 05 1955", -446774400},
		{"Oct 25 1985", 499046400},
		{"25 October 1985", 499046400},
	}
	for _, tc := range tests {
		got, err := Parse(tc.value)
		if err != nil || got != tc.want {
			t.Errorf("Parse(%q) = %d, %v; want %d", tc.value, got, err, tc.want)
		}
	}
}

func TestParseRelative(t *testing.T) {
	if _, err := Parse("BACON"); err == nil {
		t.Error("Parse(BACON) succeeded")
	}
	got, err := Parse("1 week ago")
	if err != nil {
		t.Fatalf("Parse(1 week ago): %v", err)
	}
	want := time.Now().UTC().Unix() - 7*24*3600
	if got < want-5 || got > want+5 {
		t.Errorf("Parse(1 week ago) = %d, want about %d", got, want)
	}
}
