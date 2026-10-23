package object

import (
	"bytes"
	"compress/flate"
	"fmt"
)

// Deflate compresses data as a raw DEFLATE (RFC 1951) stream, the format used
// for ".filez" objects in archive repositories.
//
// Go's compress/flate produces a valid stream but its bit-level choices differ
// from zlib's, which libostree uses. For small inputs that cannot contain any
// LZ77 match the reference implementation emits a single fixed-Huffman block,
// and reproducing that byte for byte matters for `commit --generate-sizes`
// (the ostree.sizes metadata stores the on-disk compressed size) and for
// repository interoperability. Those inputs therefore take a dedicated
// encoder; everything else falls back to compress/flate.
func Deflate(data []byte, level int) ([]byte, error) {
	if level == flate.DefaultCompression {
		level = 6
	}
	if len(data) <= fixedLiteralLimit && !hasTripleRepeat(data) {
		return deflateFixedLiterals(data), nil
	}
	var buf bytes.Buffer
	zw, err := flate.NewWriter(&buf, level)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(data); err != nil {
		zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// fixedLiteralLimit bounds the inputs handled by deflateFixedLiterals. Up to
// this size zlib always prefers the fixed-Huffman block over a stored block
// when the input is incompressible.
const fixedLiteralLimit = 32

// hasTripleRepeat reports whether data contains the same three byte substring
// twice, i.e. whether zlib's match finder could emit a length/distance pair
// (the minimum match length is three).
func hasTripleRepeat(data []byte) bool {
	for i := 0; i+3 <= len(data); i++ {
		for j := i + 1; j+3 <= len(data); j++ {
			if data[i] == data[j] && data[i+1] == data[j+1] && data[i+2] == data[j+2] {
				return true
			}
		}
	}
	return false
}

// deflateFixedLiterals encodes data as one final fixed-Huffman block of
// literals. zlib produces exactly these bytes for inputs without matches.
func deflateFixedLiterals(data []byte) []byte {
	var out []byte
	var bits uint64
	var nbits uint
	// put appends the low n bits of v to the stream (deflate sends the bit
	// stream least significant bit first).
	put := func(v uint32, n uint) {
		bits |= uint64(v) << nbits
		nbits += n
		for nbits >= 8 {
			out = append(out, byte(bits))
			bits >>= 8
			nbits -= 8
		}
	}
	// putHuff appends a Huffman code, which is sent most significant bit
	// first.
	putHuff := func(sym uint32) {
		code, n := fixedHuffmanCode(sym)
		var rev uint32
		for i := uint(0); i < n; i++ {
			rev = rev<<1 | (code>>i)&1
		}
		put(rev, n)
	}
	put(1, 1) // BFINAL
	put(1, 2) // BTYPE = fixed Huffman
	for _, b := range data {
		putHuff(uint32(b))
	}
	putHuff(256) // end of block
	if nbits > 0 {
		out = append(out, byte(bits))
	}
	return out
}

// fixedHuffmanCode returns the RFC 1951 fixed-Huffman code and length for a
// literal/length/end-of-block symbol.
func fixedHuffmanCode(sym uint32) (uint32, uint) {
	switch {
	case sym <= 143:
		return 0x30 + sym, 8
	case sym <= 255:
		return 0x190 + (sym - 144), 9
	case sym <= 279:
		return sym - 256, 7
	case sym <= 287:
		return 0xC0 + (sym - 280), 8
	default:
		panic(fmt.Sprintf("deflate: invalid literal symbol %d", sym))
	}
}
