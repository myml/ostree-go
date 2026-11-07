package pull

import "fmt"

// maxMetadataSize is OSTREE_MAX_METADATA_SIZE (128 MiB): the largest
// metadata object (commit, dirtree, dirmeta, summary) libostree will fetch.
const maxMetadataSize = 128 * 1024 * 1024

// checkMetadataSize mirrors validate_metadata_size() / the fetcher's
// maximum-size check. The error text intentionally contains the same
// "exceeded maximum" wording as _ostree_fetcher's.
func checkMetadataSize(what string, size int) error {
	if size <= maxMetadataSize {
		return nil
	}
	return fmt.Errorf("%s is %d bytes, exceeded maximum of %d bytes", what, size, maxMetadataSize)
}
