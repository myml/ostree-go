package object

import (
	"bytes"
	"compress/flate"
	"encoding/hex"
	"io"
	"testing"
)

// zlib (libostree's compressor) emits a single final fixed-Huffman block for
// these incompressible inputs; Deflate must reproduce those exact bytes so
// that object sizes and ostree.sizes metadata match the C implementation.
func TestDeflateMatchesZlibForSmallInputs(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"first\n", "4bcb2c2a2ee10200"},
		{"moo\n", "cbcdcfe70200"},
		{"alien\n", "4bccc94ccde30200"},
		{"hi\n", "cbc8e40200"},
		{"x\n", "abe00200"},
	}
	for _, tc := range cases {
		got, err := Deflate([]byte(tc.input), 6)
		if err != nil {
			t.Fatalf("Deflate(%q): %v", tc.input, err)
		}
		if hex.EncodeToString(got) != tc.want {
			t.Errorf("Deflate(%q) = %x, want %s", tc.input, got, tc.want)
		}
		// The stream must remain a valid raw DEFLATE stream.
		r := flate.NewReader(bytes.NewReader(got))
		out, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("flate.NewReader(%q): %v", tc.input, err)
		}
		if string(out) != tc.input {
			t.Errorf("round trip of %q = %q", tc.input, out)
		}
	}
}

func TestDeflateFallsBackForCompressibleInput(t *testing.T) {
	data := bytes.Repeat([]byte("hello hello hello\n"), 100)
	got, err := Deflate(data, 6)
	if err != nil {
		t.Fatal(err)
	}
	r := flate.NewReader(bytes.NewReader(got))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("round trip mismatch")
	}
	if len(got) >= len(data) {
		t.Fatalf("compressible input not compressed: %d >= %d", len(got), len(data))
	}
}
