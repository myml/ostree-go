package staticdelta

import (
	"bytes"
	"math/rand"
	"testing"
)

func TestBsdiffRoundtrip(t *testing.T) {
	cases := [][2]string{
		{"", ""},
		{"", "hello world"},
		{"hello world", ""},
		{"hello world", "hello world"},
		{"the quick brown fox jumps over the lazy dog",
			"the quick brown cat jumps over the lazy dog"},
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"aaaaaaaaaaaaaaaabaaaaaaaaaaaaaaa"},
	}
	rng := rand.New(rand.NewSource(42))
	big := make([]byte, 200000)
	rng.Read(big)
	modified := append([]byte(nil), big...)
	copy(modified[5000:], []byte("inserted content"))
	modified = append(modified, []byte("tail")...)
	cases = append(cases, [2]string{string(big), string(modified)})

	for i, c := range cases {
		old, new := []byte(c[0]), []byte(c[1])
		patch := bsdiff(old, new)
		out, err := bspatch(old, int64(len(new)), patch)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !bytes.Equal(out, new) {
			t.Fatalf("case %d: patch did not reproduce the target", i)
		}
	}
}

func TestBspatchRejectsTruncated(t *testing.T) {
	patch := bsdiff([]byte("abcdef"), []byte("abXdef"))
	for n := 0; n < len(patch); n++ {
		if _, err := bspatch([]byte("abcdef"), 6, patch[:n]); err == nil {
			t.Fatalf("truncated patch of %d bytes accepted", n)
		}
	}
}

func TestRollsumChunksDeterministic(t *testing.T) {
	data := bytes.Repeat([]byte("libostree delta payload "), 4096)
	groups1, order1 := rollsumChunks(data)
	groups2, order2 := rollsumChunks(data)
	if len(order1) != len(order2) {
		t.Fatalf("chunk count mismatch: %d vs %d", len(order1), len(order2))
	}
	for i := range order1 {
		if order1[i] != order2[i] {
			t.Fatalf("chunk %d order mismatch", i)
		}
	}
	_ = groups1
	_ = groups2
	// Identical content must match completely.
	m := computeRollsumMatches(data, data)
	if m.bufMatches != m.total || m.matchSize != uint64(len(data)) {
		t.Fatalf("identical content did not match: %d/%d, %d bytes",
			m.bufMatches, m.total, m.matchSize)
	}
}

func TestFormatSize(t *testing.T) {
	cases := map[uint64]string{
		0:       "0 bytes",
		999:     "999 bytes",
		1000:    "1.0 kB",
		48829:   "48.8 kB",
		601093:  "601.1 kB",
		1779205: "1.8 MB",
	}
	for n, want := range cases {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}
