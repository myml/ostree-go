package sign

import (
	"errors"
	"fmt"

	"ostree-go/internal/gvariant"
	"ostree-go/internal/object"
	"ostree-go/internal/repo"
)

// MetadataType is the GVariant type of detached metadata and summary
// signature files.
var metadataType = gvariant.MustParseType("a{sv}")

// ReadDetachedMetadata loads the detached metadata for commit.  A missing
// commitmeta object yields an empty dictionary, matching
// ostree_repo_read_commit_detached_metadata().
func ReadDetachedMetadata(r *repo.Repo, commit string) (gvariant.Dict, error) {
	data, err := r.ReadObject(commit, repo.ObjectCommitmeta)
	if err != nil {
		if errors.Is(err, repo.ErrObjectNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("Failed to read detached metadata: %w", err)
	}
	v, err := gvariant.Decode(metadataType, data)
	if err != nil {
		return nil, fmt.Errorf("Failed to read detached metadata: %w", err)
	}
	dict, ok := v.(gvariant.Dict)
	if !ok {
		return nil, fmt.Errorf("Failed to read detached metadata: not a dictionary")
	}
	return dict, nil
}

// WriteDetachedMetadata stores the detached metadata for commit.
func WriteDetachedMetadata(r *repo.Repo, commit string, dict gvariant.Dict) error {
	data, err := gvariant.Encode(metadataType, dict)
	if err != nil {
		return err
	}
	return r.ReplaceRawObject(commit, repo.ObjectCommitmeta, data)
}

// SignatureBytes extracts the raw signature blobs stored under key.
func SignatureBytes(dict gvariant.Dict, key string) ([][]byte, error) {
	v, ok := dict.Get(key)
	if !ok {
		return nil, nil
	}
	av, ok := v.(gvariant.Variant)
	if !ok {
		return nil, fmt.Errorf("malformed detached metadata value for %q", key)
	}
	arr, ok := av.Value.([]gvariant.Value)
	if !ok {
		return nil, fmt.Errorf("malformed detached metadata value for %q", key)
	}
	out := make([][]byte, 0, len(arr))
	for _, item := range arr {
		b, ok := item.([]byte)
		if !ok {
			return nil, fmt.Errorf("malformed signature for %q", key)
		}
		out = append(out, b)
	}
	return out, nil
}

// appendSignature appends sig to the "aay" array under key.
func appendSignature(dict gvariant.Dict, key string, sig []byte) gvariant.Dict {
	var arr []gvariant.Value
	if v, ok := dict.Get(key); ok {
		if av, ok := v.(gvariant.Variant); ok {
			if existing, ok := av.Value.([]gvariant.Value); ok {
				arr = existing
			}
		}
	}
	arr = append(arr, sig)
	dict.Set(key, gvariant.Variant{Type: gvariant.MustParseType("aay"), Value: arr})
	return dict
}

// SignCommit appends a detached signature to commit using engine.
func SignCommit(r *repo.Repo, engine *Engine, commit string) error {
	commitData, err := r.ReadObject(commit, repo.ObjectCommit)
	if err != nil {
		return fmt.Errorf("Failed to read commit: %w", err)
	}
	dict, err := ReadDetachedMetadata(r, commit)
	if err != nil {
		return err
	}
	sig, err := engine.Sign(commitData)
	if err != nil {
		return fmt.Errorf("Not able to sign the cobject: %w", err)
	}
	if dict == nil {
		dict = gvariant.Dict{}
	}
	dict = appendSignature(dict, engine.MetadataKey(), sig)
	return WriteDetachedMetadata(r, commit, dict)
}

// VerifyCommit verifies the detached signatures of commit.
func VerifyCommit(r *repo.Repo, engine *Engine, commit string) (string, error) {
	commitData, err := r.ReadObject(commit, repo.ObjectCommit)
	if err != nil {
		return "", fmt.Errorf("Failed to read commit: %w", err)
	}
	dict, err := ReadDetachedMetadata(r, commit)
	if err != nil {
		return "", err
	}
	sigs, err := SignatureBytes(dict, engine.MetadataKey())
	if err != nil {
		return "", err
	}
	return engine.Verify(commitData, sigs)
}

// SignatureIssuers returns the raw signature blobs stored for engine.
func SignatureIssuers(r *repo.Repo, engine *Engine, commit string) ([][]byte, error) {
	dict, err := ReadDetachedMetadata(r, commit)
	if err != nil {
		return nil, err
	}
	return SignatureBytes(dict, engine.MetadataKey())
}

// ParseSummarySignature parses a summary.sig payload.  Garbage is treated as
// an empty dictionary, matching the way the C pull code tolerates an invalid
// signature file and reports "No signatures found".
func ParseSummarySignature(data []byte) gvariant.Dict {
	if len(data) == 0 {
		return nil
	}
	v, err := gvariant.Decode(metadataType, data)
	if err != nil {
		return nil
	}
	dict, ok := v.(gvariant.Dict)
	if !ok {
		return nil
	}
	return dict
}

// SignSummary appends signatures for every key to the summary signature
// dictionary and returns the serialized a{sv}.
func SignSummary(summaryData []byte, existing []byte, engine *Engine, keys []string) ([]byte, error) {
	dict := ParseSummarySignature(existing)
	if dict == nil {
		dict = gvariant.Dict{}
	}
	for _, key := range keys {
		if err := engine.SetSK(key); err != nil {
			return nil, err
		}
		sig, err := engine.Sign(summaryData)
		if err != nil {
			return nil, err
		}
		dict = appendSignature(dict, engine.MetadataKey(), sig)
	}
	return gvariant.Encode(metadataType, dict)
}

// VerifySummary verifies a summary signature payload with engine.
func VerifySummary(summaryData, sigData []byte, engine *Engine) (string, error) {
	dict := ParseSummarySignature(sigData)
	var sigs [][]byte
	if dict != nil {
		var err error
		sigs, err = SignatureBytes(dict, engine.MetadataKey())
		if err != nil {
			return "", err
		}
	}
	return engine.Verify(summaryData, sigs)
}

// SignDataWithKeys signs data once per key, returning the appended
// signatures in an aay-compatible slice.
func SignDataWithKeys(engine *Engine, data []byte, keys []string) ([][]byte, error) {
	var out [][]byte
	for _, key := range keys {
		if err := engine.SetSK(key); err != nil {
			return nil, err
		}
		sig, err := engine.Sign(data)
		if err != nil {
			return nil, err
		}
		out = append(out, sig)
	}
	return out, nil
}

// DetachedMetadataFromObject is a convenience for tests.
func DetachedMetadataFromObject(data []byte) (gvariant.Dict, error) {
	return object.ParseMetadata(data)
}

// GpgSignaturesKey is the detached metadata key holding OpenPGP signatures.
const GpgSignaturesKey = "ostree.gpgsigs"

// DetachedMetadataSignatures extracts an "aay" array from dict under key.
func DetachedMetadataSignatures(dict gvariant.Dict, key string) ([][]byte, error) {
	return SignatureBytes(dict, key)
}

// AppendDetachedSignature appends one signature blob under key and rewrites
// the commit's detached metadata.
func AppendDetachedSignature(r *repo.Repo, commit string, dict gvariant.Dict, key string, sig []byte) error {
	if dict == nil {
		dict = gvariant.Dict{}
	}
	dict = appendSignature(dict, key, sig)
	return WriteDetachedMetadata(r, commit, dict)
}

// SetDetachedSignatures replaces the signature array under key (removing the
// key entirely when sigs is empty).
func SetDetachedSignatures(r *repo.Repo, commit string, dict gvariant.Dict, key string, sigs [][]byte) error {
	if dict == nil {
		dict = gvariant.Dict{}
	}
	if len(sigs) == 0 {
		dict.Delete(key)
	} else {
		arr := make([]gvariant.Value, 0, len(sigs))
		for _, sig := range sigs {
			arr = append(arr, sig)
		}
		dict.Set(key, gvariant.Variant{Type: gvariant.MustParseType("aay"), Value: arr})
	}
	return WriteDetachedMetadata(r, commit, dict)
}

// SummarySignatures accumulates detached signatures for a summary file in an
// a{sv} dictionary, one "aay" array per signing engine.
type SummarySignatures struct {
	dict gvariant.Dict
}

// NewSummarySignatures starts from an existing summary.sig payload.
func NewSummarySignatures(existing []byte) *SummarySignatures {
	dict := ParseSummarySignature(existing)
	if dict == nil {
		dict = gvariant.Dict{}
	}
	return &SummarySignatures{dict: dict}
}

// Add appends one raw signature under key.
func (s *SummarySignatures) Add(key string, sig []byte) {
	s.dict = appendSignature(s.dict, key, sig)
}

// Encode serializes the signature dictionary.
func (s *SummarySignatures) Encode() ([]byte, error) {
	return gvariant.Encode(metadataType, s.dict)
}
