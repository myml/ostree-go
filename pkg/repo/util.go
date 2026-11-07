package repo

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
)

// isMissingPath reports whether err means that a path does not exist. ENOTDIR
// (a path component is a regular file) is treated as missing here so callers
// can report a proper ref conflict instead of a raw syscall error.
func isMissingPath(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// ValidateChecksum checks that s is a valid OSTree checksum: exactly 64
// lowercase hexadecimal characters. This matches
// ostree_validate_checksum_string().
func ValidateChecksum(s string) error {
	if len(s) != 64 {
		return fmt.Errorf("%w: %d characters, expected 64", ErrInvalidChecksum, len(s))
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%w: invalid character %q at offset %d", ErrInvalidChecksum, string(c), i)
		}
	}
	return nil
}

// IsChecksum reports whether s is a valid checksum string.
func IsChecksum(s string) bool { return ValidateChecksum(s) == nil }

func isASCIIAlnum(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

// validRefFragment implements libostree's OSTREE_REF_FRAGMENT_REGEXP:
// [\w\d][-._\w\d]*, restricted to ASCII.
func validRefFragment(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if i == 0 {
			if !isASCIIAlnum(c) && c != '_' {
				return false
			}
			continue
		}
		if !isASCIIAlnum(c) && c != '_' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// ValidateRefName validates a ref name: one or more '/'-separated fragments,
// each matching the libostree ref fragment syntax. Empty fragments, "." and
// ".." are therefore rejected.
func ValidateRefName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty name", ErrInvalidRef)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("%w: %q", ErrInvalidRef, name)
	}
	for _, frag := range strings.Split(name, "/") {
		if !validRefFragment(frag) {
			return fmt.Errorf("%w: invalid fragment %q in %q", ErrInvalidRef, frag, name)
		}
	}
	return nil
}

// ValidateRemoteName validates a remote name (the same fragment syntax).
func ValidateRemoteName(name string) error {
	if !validRefFragment(name) {
		return fmt.Errorf("%w: %q", ErrInvalidRemote, name)
	}
	return nil
}

// ValidateCollectionID validates an OSTree collection ID. Collection IDs use
// the D-Bus interface name syntax; libostree reuses
// g_dbus_is_interface_name().
func ValidateCollectionID(id string) error {
	if id == "" || len(id) > 255 {
		return fmt.Errorf("%w: %q", ErrInvalidCollection, id)
	}
	parts := strings.Split(id, ".")
	if len(parts) < 2 {
		return fmt.Errorf("%w: %q needs at least two elements", ErrInvalidCollection, id)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("%w: %q contains an empty element", ErrInvalidCollection, id)
		}
		for i := 0; i < len(part); i++ {
			c := part[i]
			if i == 0 {
				ok := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
				if !ok {
					return fmt.Errorf("%w: %q has an invalid element start", ErrInvalidCollection, id)
				}
				continue
			}
			if !isASCIIAlnum(c) && c != '_' {
				return fmt.Errorf("%w: %q has an invalid character", ErrInvalidCollection, id)
			}
		}
	}
	return nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand should never fail; fall back to a fixed but unique-ish
		// suffix so that callers still make progress.
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}

// createTempIn creates a uniquely named file in dir with the given prefix and
// mode, returning the open file and its path.
func createTempIn(dir, prefix string, perm os.FileMode) (*os.File, string, error) {
	for i := 0; i < 100; i++ {
		path := dir + string(os.PathSeparator) + prefix + randomHex(8)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, path, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not create a temporary file in %s", dir)
}

// dirIsEmpty reports whether the directory rel contains no entries.
func (r *Repo) dirIsEmpty(rel string) (bool, error) {
	f, err := r.root.Open(rel)
	if err != nil {
		return false, err
	}
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err != nil && err != io.EOF {
		return false, err
	}
	return len(names) == 0, nil
}
