package pull

import (
	"fmt"

	"github.com/myml/ostree-go/pkg/object"
	"github.com/myml/ostree-go/pkg/repo"
)

// FetchCommitTimestamp downloads one commit object from a remote without
// importing it and returns its timestamp.  It lets `pull --timestamp-check`
// compare revisions before any ref is updated, matching libostree's behavior
// of failing the pull without leaving the ref moved.
func FetchCommitTimestamp(url, csum string) (uint64, error) {
	if err := repo.ValidateChecksum(csum); err != nil {
		return 0, err
	}
	f, err := newFetcher(url)
	if err != nil {
		return 0, err
	}
	data, err := f.get("objects/" + csum[:2] + "/" + csum[2:] + ".commit")
	if err != nil {
		return 0, fmt.Errorf("Fetching commit %s: %w", csum, err)
	}
	commit, err := object.ParseCommit(data)
	if err != nil {
		return 0, fmt.Errorf("Parsing commit %s: %w", csum, err)
	}
	return commit.Timestamp, nil
}
