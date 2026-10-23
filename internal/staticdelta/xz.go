package staticdelta

import (
	"bytes"
	"fmt"
	"io"

	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// xzDictCap bounds the LZMA2 dictionary size we accept.  libostree's
// compressor uses preset 8 (32 MiB dictionary) and its decoder caps the
// memory limit at 100 MiB (see ostree-lzma-decompressor.c), so the same
// order of magnitude is used here to make decompression bombs
// impractical.
const xzDictCap = 64 << 20

// maxDecompressedPart bounds the size of one decompressed delta part.
// It is only used by the read-only inspection paths in this package.
const maxDecompressedPart = 8 << 30

// compressPart compresses one serialized part payload the way
// finish_part() in ostree-repo-static-delta-compilation.c does: an xz
// stream (LZMA2, CRC64) with a large dictionary.  The returned byte is
// the on-disk compression type ('x').
func compressPart(payload []byte) (byte, []byte, error) {
	var buf bytes.Buffer
	cfg := xz.WriterConfig{
		DictCap: 8 << 20,
		BufSize: 1 << 20,
	}
	w, err := cfg.NewWriter(&buf)
	if err != nil {
		return 0, nil, fmt.Errorf("static delta: xz writer: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		return 0, nil, fmt.Errorf("static delta: compressing part: %w", err)
	}
	if err := w.Close(); err != nil {
		return 0, nil, fmt.Errorf("static delta: compressing part: %w", err)
	}
	return CompressionLZMA, buf.Bytes(), nil
}

// decompressXZ decompresses an xz (or raw .lzma) stream.
func decompressXZ(data []byte) ([]byte, error) {
	cfg := xz.ReaderConfig{DictCap: xzDictCap}
	r, err := cfg.NewReader(bytes.NewReader(data))
	if err != nil {
		// Older deltas used the legacy LZMA alone format.
		lr, lerr := lzma.NewReader(bytes.NewReader(data))
		if lerr != nil {
			return nil, fmt.Errorf("static delta: xz: %w", err)
		}
		return readCapped(lr)
	}
	return readCapped(r)
}

func readCapped(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, maxDecompressedPart+1))
	if err != nil {
		return nil, fmt.Errorf("static delta: decompressing part: %w", err)
	}
	if uint64(len(out)) > maxDecompressedPart {
		return nil, fmt.Errorf("static delta: decompressed part exceeds %d bytes", maxDecompressedPart)
	}
	return out, nil
}
