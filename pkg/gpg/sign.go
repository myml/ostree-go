package gpg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// SignData produces a binary detached OpenPGP signature over data using the
// system gpg binary, like sign_data() in the C implementation.
func SignData(data []byte, keyID, homedir string) ([]byte, error) {
	bin, err := gpgBinary()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "ostree-gpg-sign-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	dataPath := filepath.Join(dir, "data")
	sigPath := filepath.Join(dir, "signature")
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return nil, err
	}
	args := []string{"--batch", "--no-tty", "--yes", "--pinentry-mode", "loopback"}
	if homedir != "" {
		args = append(args, "--homedir", homedir)
	}
	if keyID != "" {
		args = append(args, "--local-user", keyID)
	}
	args = append(args, "--detach-sign", "--output", sigPath, dataPath)
	cmd := exec.Command(bin, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return os.ReadFile(sigPath)
}

// ValidateKeyID mirrors ot_validate_gpg_key_id().
func ValidateKeyID(keyID string) error {
	if keyID == "" {
		return fmt.Errorf("Invalid empty GPG key ID")
	}
	return nil
}

// SignatureKeyIDs returns the issuer key IDs of the signature blobs.
func SignatureKeyIDs(sigs [][]byte) []string {
	var out []string
	for _, sig := range sigs {
		ids, err := SignatureIssuerKeyIDs(sig)
		if err != nil {
			continue
		}
		out = append(out, ids...)
	}
	return out
}

// SignaturesMatchKeyID reports whether any signature was issued by keyID.
func SignaturesMatchKeyID(sigs [][]byte, keyID string) bool {
	for _, issuer := range SignatureKeyIDs(sigs) {
		if KeyIDMatches(issuer, keyID) {
			return true
		}
	}
	return false
}
