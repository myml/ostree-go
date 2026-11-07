package pull

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/myml/ostree-go/internal/object"
)

// validateDirtreeNames mirrors _ostree_validate_structureof_dirtree(): every
// entry in a dirtree object must be a single, valid path component.  The
// object is validated before it is written, so a malicious remote cannot get
// a ".." name into the repository.
func validateDirtreeNames(raw []byte, csum string) error {
	t, err := object.ParseDirtree(raw)
	if err != nil {
		return fmt.Errorf("%s.dirtree: %w", csum, err)
	}
	for _, f := range t.Files {
		if err := validateFilename(f.Name); err != nil {
			return fmt.Errorf("%s.dirtree: %w", csum, err)
		}
	}
	for _, d := range t.Dirs {
		if err := validateFilename(d.Name); err != nil {
			return fmt.Errorf("%s.dirtree: %w", csum, err)
		}
	}
	return nil
}

// validateFilename implements ot_util_filename_validate().
func validateFilename(name string) error {
	switch name {
	case ".":
		return fmt.Errorf("Invalid self-referential filename '.'")
	case "..":
		return fmt.Errorf("Invalid path uplink filename '..'")
	}
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("Invalid / in filename %s", name)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("Invalid UTF-8 in filename %s", name)
	}
	return nil
}

// verifyContentChecksum recomputes a file object checksum from its decoded
// archive header and content.
func verifyContentChecksum(hdr *object.FileHeader, content []byte, expected string) error {
	var body []byte
	if hdr.IsRegular() {
		body = content
	}
	actual, err := hdr.Checksum(body)
	if err != nil {
		return fmt.Errorf("Checksumming content object %s: %w", expected, err)
	}
	if actual != expected {
		return fmt.Errorf("Corrupted file object; checksum expected='%s' actual='%s'", expected, actual)
	}
	return nil
}
