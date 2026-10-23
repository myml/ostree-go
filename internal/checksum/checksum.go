// Package checksum implements the SHA-256 object naming used by OSTree.
package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Size is the size of an OSTree checksum in bytes.
const Size = sha256.Size

// Sum returns the SHA-256 digest of data.
func Sum(data []byte) [Size]byte {
	return sha256.Sum256(data)
}

// Hex returns the lowercase hexadecimal form of a digest.
func Hex(sum [Size]byte) string {
	return hex.EncodeToString(sum[:])
}

// FromHex parses a 64-character lowercase or uppercase hex checksum.
func FromHex(s string) ([Size]byte, error) {
	var out [Size]byte
	if len(s) != Size*2 {
		return out, fmt.Errorf("invalid checksum %q: expected %d hex characters", s, Size*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("invalid checksum %q: %w", s, err)
	}
	copy(out[:], b)
	return out, nil
}

// IsHex reports whether s is a syntactically valid checksum.
func IsHex(s string) bool {
	_, err := FromHex(s)
	return err == nil
}
