package gpg

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VerifyResult describes a successful verification.
type VerifyResult struct {
	// Signatures is the total number of signature packets seen.
	Signatures int
	// Valid is the number of signatures made by a trusted key.
	Valid int
	// KeyIDs lists the signing key IDs.
	KeyIDs []string
	// UIDs lists the signing user IDs.
	UIDs []string
	// Status is the raw gpg status output.
	Status string
}

// SignatureCount returns the number of signatures (gpgme count_all).
func (r *VerifyResult) SignatureCount() int {
	if r == nil {
		return 0
	}
	return r.Signatures
}

// VerifyDetached verifies one or more concatenated binary OpenPGP signature
// packets over data using the given keyring files.  It prefers the system gpg
// binary (like the C implementation, which uses gpgme) and falls back to the
// pure Go OpenPGP verifier when gpg is unavailable.
func VerifyDetached(data, sigs []byte, keyringFiles []string) (*VerifyResult, error) {
	if len(sigs) == 0 {
		return nil, fmt.Errorf("GPG verification enabled, but no signatures found (use gpg-verify=false in remote config to disable)")
	}
	bin, binErr := gpgBinary()
	if binErr == nil {
		return verifyWithGPG(bin, data, sigs, keyringFiles)
	}
	return verifyPure(data, sigs, keyringFiles)
}

// VerifyDetachedInHome verifies using a gpg home directory instead of raw
// keyring files (used by `ostree show --gpg-homedir`).
func VerifyDetachedInHome(data, sigs []byte, homedir string) (*VerifyResult, error) {
	bin, err := gpgBinary()
	if err != nil {
		return nil, err
	}
	var keyrings []string
	if homedir != "" {
		if entries, derr := os.ReadDir(homedir); derr == nil {
			for _, entry := range entries {
				name := entry.Name()
				if entry.IsDir() || !strings.HasSuffix(name, ".gpg") || name == "trustdb.gpg" || name == "secring.gpg" {
					continue
				}
				keyrings = append(keyrings, filepath.Join(homedir, name))
			}
		}
	}
	return verifyWithGPG(bin, data, sigs, keyrings)
}

// buildVerifyHome creates a private gpg home and imports every keyring or
// ASCII key file into it.  The C implementation does the same through gpgme:
// binary keyrings are concatenated and ASCII keys are imported.
func buildVerifyHome(keyringFiles []string) (string, error) {
	home, err := newTempHome()
	if err != nil {
		return "", err
	}
	// Create an empty pubring.gpg first so gpg stays in the legacy keyring
	// format (matching ot_gpgme_ctx_tmp_home_dir + gpgme import).
	if err := os.WriteFile(filepath.Join(home, "pubring.gpg"), nil, 0o644); err != nil {
		os.RemoveAll(home)
		return "", err
	}
	for _, file := range keyringFiles {
		st, err := os.Stat(file)
		if err != nil || st.IsDir() || st.Size() == 0 {
			continue
		}
		if _, err := runGPG(home, nil, "--import", file); err != nil {
			// Ignore unimportable files (e.g. trustdb.gpg from a
			// gpgkeypath directory scan); the remaining keys are still
			// usable.
			continue
		}
	}
	return home, nil
}

func verifyWithGPG(bin string, data, sigs []byte, keyringFiles []string) (*VerifyResult, error) {
	home, err := buildVerifyHome(keyringFiles)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)

	dataPath := filepath.Join(home, "signed-data")
	sigPath := filepath.Join(home, "signatures")
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(sigPath, sigs, 0o600); err != nil {
		return nil, err
	}
	args := []string{"--batch", "--no-tty", "--homedir", home, "--status-fd", "1", "--verify", sigPath, dataPath}
	cmd := exec.Command(bin, args...)
	out, _ := cmd.Output()
	status := string(out)
	return parseVerifyStatus(status)
}

// parseVerifyStatus interprets gpg --status-fd output.
func parseVerifyStatus(status string) (*VerifyResult, error) {
	res := &VerifyResult{Status: status}
	var haveGood bool
	var haveRevoked, haveExpired, haveMissing, haveBad bool
	var firstErrorLine string
	for _, line := range strings.Split(status, "\n") {
		const prefix = "[GNUPG:] "
		if !strings.HasPrefix(line, prefix) {
			if strings.TrimSpace(line) != "" && firstErrorLine == "" && !strings.HasPrefix(line, "gpg:") {
				firstErrorLine = line
			}
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) == 0 {
			continue
		}
		keyword := fields[0]
		switch keyword {
		case "GOODSIG":
			haveGood = true
			res.Valid++
			res.Signatures++
			if len(fields) > 1 {
				res.KeyIDs = append(res.KeyIDs, fields[1])
			}
			if len(fields) > 2 {
				res.UIDs = append(res.UIDs, strings.Join(fields[2:], " "))
			}
		case "BADSIG":
			haveBad = true
			res.Signatures++
			if len(fields) > 1 {
				res.KeyIDs = append(res.KeyIDs, fields[1])
			}
		case "ERRSIG":
			res.Signatures++
			if len(fields) > 1 {
				res.KeyIDs = append(res.KeyIDs, fields[1])
			}
			if len(fields) > 5 {
				switch fields[5] {
				case "9":
					haveMissing = true
				case "4":
					haveExpired = true
				}
			}
		case "NO_PUBKEY":
			haveMissing = true
		case "REVKEYSIG", "KEYREVOKED":
			haveRevoked = true
		case "EXPKEYSIG", "KEYEXPIRED":
			haveExpired = true
		}
	}
	if haveGood {
		return res, nil
	}
	switch {
	case haveRevoked:
		return nil, fmt.Errorf("Key revoked")
	case haveExpired:
		return nil, fmt.Errorf("Key expired")
	case haveMissing:
		return nil, fmt.Errorf("Can't check signature: public key not found")
	case haveBad:
		return nil, fmt.Errorf("BAD signature")
	case res.Signatures == 0:
		return nil, fmt.Errorf("no signatures found")
	}
	if firstErrorLine != "" {
		return nil, fmt.Errorf("%s", firstErrorLine)
	}
	return nil, fmt.Errorf("Can't check signature: public key not found")
}
