// Package gpg implements the OpenPGP parts of OSTree: remote keyring import
// and listing, commit/summary signing through the system gpg binary, and
// signature verification.
//
// Keyrings are stored in the same on-disk format as the C implementation: a
// binary OpenPGP keyring in <repo>/<remote>.trustedkeys.gpg.  Import and
// listing are delegated to the system gpg binary (the C implementation uses
// gpgme, which itself drives gpg), while verification has a pure Go OpenPGP
// path for RSA signatures with a gpg fallback for everything else.
package gpg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// KeyringName returns the repository-relative keyring file name for remote.
func KeyringName(remote string) string {
	return remote + ".trustedkeys.gpg"
}

// RemoteKeyringPath returns the keyring path inside repoPath.
func RemoteKeyringPath(repoPath, remote string) string {
	return filepath.Join(repoPath, KeyringName(remote))
}

// GlobalKeyringFiles returns the keyring files used for the global/default
// keyring, mirroring _ostree_gpg_verifier_add_global_keyring_dir().
func GlobalKeyringFiles() []string {
	dir := os.Getenv("OSTREE_GPG_HOME")
	if dir == "" {
		dir = "/usr/share/ostree/trusted.gpg.d/"
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".gpg") {
			continue
		}
		if name == "trustdb.gpg" || name == "secring.gpg" {
			continue
		}
		if entry.IsDir() {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out
}

// KeyringFilesForRemote returns the keyring files used to verify remote,
// optionally including the global keyrings when no per-remote keyring exists.
func KeyringFilesForRemote(repoPath, remote string) []string {
	var out []string
	path := RemoteKeyringPath(repoPath, remote)
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		out = append(out, path)
	}
	if len(out) == 0 {
		out = append(out, GlobalKeyringFiles()...)
	}
	return out
}

// Subkey is one key or subkey of an OpenPGP public key.
type Subkey struct {
	Fingerprint string
	Created     int64
	Expires     int64
	Revoked     bool
	Expired     bool
	Invalid     bool
}

// UID is one user ID of an OpenPGP public key.
type UID struct {
	Value       string
	Revoked     bool
	Invalid     bool
	AdvancedURL string
	DirectURL   string
}

// Key is one OpenPGP public key.
type Key struct {
	Fingerprint string
	Created     int64
	Expires     int64
	Revoked     bool
	Expired     bool
	Invalid     bool
	// Subkeys[0] is the primary key; the rest are subkeys.
	Subkeys []Subkey
	UIDs    []UID
}

// FormatTime formats a timestamp the way GLib's "%c" does in the C locale.
func FormatTime(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).In(time.Local).Format("Mon Jan _2 15:04:05 2006")
}

// gpgBinary locates the gpg executable.
func gpgBinary() (string, error) {
	if path, err := exec.LookPath("gpg2"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("gpg"); err == nil {
		return path, nil
	}
	return "", errors.New("gpg executable not found")
}

// runGPG runs gpg with args, feeding stdin, and returns combined output.
func runGPG(homedir string, stdin []byte, args ...string) ([]byte, error) {
	bin, err := gpgBinary()
	if err != nil {
		return nil, err
	}
	full := []string{"--batch", "--no-tty", "--yes"}
	if homedir != "" {
		full = append(full, "--homedir", homedir)
	}
	full = append(full, args...)
	cmd := exec.Command(bin, full...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return out, nil
}

// newTempHome creates a private gpg home directory.
func newTempHome() (string, error) {
	dir, err := os.MkdirTemp("", "ostree-gpg-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// ImportOptions controls Import.
type ImportOptions struct {
	// RepoPath is the repository root directory.
	RepoPath string
	// Remote is the remote name.
	Remote string
	// KeyFiles are binary or ASCII armored key files to import.  When empty
	// and Stdin is nil the user's personal keyring is used.
	KeyFiles []string
	// Stdin supplies key data when --stdin was given.
	Stdin io.Reader
	// KeyIDs optionally restricts which keys are imported.
	KeyIDs []string
}

// Import imports keys into the remote keyring and returns the number of
// newly imported keys, mirroring ostree_repo_remote_gpg_import().
func Import(opts ImportOptions) (int, error) {
	if opts.Remote == "" {
		return 0, fmt.Errorf("GPG: no remote name")
	}
	keyringPath := RemoteKeyringPath(opts.RepoPath, opts.Remote)

	sourceHome, err := newTempHome()
	if err != nil {
		return 0, fmt.Errorf("GPG: %w", err)
	}
	defer os.RemoveAll(sourceHome)

	var sourceData []byte
	switch {
	case opts.Stdin != nil:
		sourceData, err = io.ReadAll(opts.Stdin)
		if err != nil {
			return 0, fmt.Errorf("GPG: %w", err)
		}
		if err := importIntoHome(sourceHome, sourceData); err != nil {
			return 0, fmt.Errorf("GPG: Unable to import keys: %w", err)
		}
	case len(opts.KeyFiles) > 0:
		for _, file := range opts.KeyFiles {
			data, ferr := os.ReadFile(file)
			if ferr != nil {
				return 0, fmt.Errorf("GPG: %w", ferr)
			}
			if ierr := importIntoHome(sourceHome, data); ierr != nil {
				return 0, fmt.Errorf("GPG: Unable to import keys: %w", ierr)
			}
		}
	default:
		// Import from the user's personal keyring.
		args := []string{"--armor", "--export"}
		args = append(args, opts.KeyIDs...)
		data, gerr := runGPG("", nil, args...)
		if gerr != nil {
			return 0, fmt.Errorf("GPG: Unable to export keys: %w", gerr)
		}
		if len(data) == 0 {
			return 0, fmt.Errorf("GPG: Unable to find requested keys")
		}
		if err := importIntoHome(sourceHome, data); err != nil {
			return 0, fmt.Errorf("GPG: Unable to import keys: %w", err)
		}
	}

	// Export the selected keys from the source context.
	exportArgs := []string{"--export"}
	exportArgs = append(exportArgs, opts.KeyIDs...)
	exported, err := runGPG(sourceHome, nil, exportArgs...)
	if err != nil {
		return 0, fmt.Errorf("GPG: Unable to export keys: %w", err)
	}
	if len(exported) == 0 {
		return 0, fmt.Errorf("GPG: Unable to find requested keys")
	}

	// Import them into a copy of the existing remote keyring.
	targetHome, err := newTempHome()
	if err != nil {
		return 0, fmt.Errorf("GPG: %w", err)
	}
	defer os.RemoveAll(targetHome)

	oldFingerprints := map[string]bool{}
	if data, rerr := os.ReadFile(keyringPath); rerr == nil {
		if err := os.WriteFile(filepath.Join(targetHome, "pubring.gpg"), data, 0o644); err == nil {
			if keys, lerr := listKeysInHome(targetHome, nil); lerr == nil {
				for _, k := range keys {
					oldFingerprints[k.Fingerprint] = true
				}
			}
		}
	} else if !os.IsNotExist(rerr) {
		return 0, fmt.Errorf("GPG: %w", rerr)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "pubring.gpg")); err != nil {
		if err := os.WriteFile(filepath.Join(targetHome, "pubring.gpg"), nil, 0o644); err != nil {
			return 0, fmt.Errorf("GPG: %w", err)
		}
	}
	if err := importIntoHome(targetHome, exported); err != nil {
		return 0, fmt.Errorf("GPG: Unable to import keys: %w", err)
	}

	newRing, err := runGPG(targetHome, nil, "--export")
	if err != nil {
		return 0, fmt.Errorf("GPG: Unable to export keys: %w", err)
	}
	newKeys, err := listKeysInHome(targetHome, nil)
	if err != nil {
		return 0, fmt.Errorf("GPG: Unable to list keys: %w", err)
	}
	imported := 0
	for _, k := range newKeys {
		if !oldFingerprints[k.Fingerprint] {
			imported++
		}
	}
	if err := os.MkdirAll(filepath.Dir(keyringPath), 0o755); err != nil {
		return 0, fmt.Errorf("GPG: %w", err)
	}
	tmp := keyringPath + ".tmp"
	if err := os.WriteFile(tmp, newRing, 0o644); err != nil {
		return 0, fmt.Errorf("GPG: %w", err)
	}
	if err := os.Rename(tmp, keyringPath); err != nil {
		os.Remove(tmp)
		return 0, fmt.Errorf("GPG: %w", err)
	}
	return imported, nil
}

func importIntoHome(home string, data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	_, err := runGPG(home, data, "--import")
	return err
}

// ListKeys enumerates the trusted keys for remote (or the global keyrings
// when remote is empty).
func ListKeys(repoPath, remote string) ([]*Key, error) {
	var files []string
	if remote == "" {
		files = GlobalKeyringFiles()
	} else {
		path := RemoteKeyringPath(repoPath, remote)
		if st, err := os.Stat(path); err == nil && !st.IsDir() {
			files = append(files, path)
		}
	}
	return listKeys(files)
}

func listKeys(files []string) ([]*Key, error) {
	if len(files) == 0 {
		return nil, nil
	}
	home, err := newTempHome()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	var combined []byte
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		combined = append(combined, data...)
	}
	if len(combined) == 0 {
		return nil, nil
	}
	if err := os.WriteFile(filepath.Join(home, "pubring.gpg"), combined, 0o644); err != nil {
		return nil, err
	}
	return listKeysInHome(home, nil)
}

func listKeysInHome(home string, keyIDs []string) ([]*Key, error) {
	args := []string{"--with-colons", "--fixed-list-mode", "--list-keys"}
	args = append(args, keyIDs...)
	out, err := runGPG(home, nil, args...)
	if err != nil {
		return nil, err
	}
	return parseColonListing(string(out))
}

// parseColonListing parses `gpg --with-colons --list-keys` output.
func parseColonListing(output string) ([]*Key, error) {
	var keys []*Key
	var cur *Key
	var curSub *Subkey
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) == 0 {
			continue
		}
		get := func(i int) string {
			if i < len(fields) {
				return fields[i]
			}
			return ""
		}
		parseTime := func(s string) int64 {
			if s == "" {
				return 0
			}
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
		switch fields[0] {
		case "pub":
			cur = &Key{}
			curSub = &Subkey{Created: parseTime(get(5)), Expires: parseTime(get(6))}
			applyValidity(get(1), &curSub.Revoked, &curSub.Expired, &curSub.Invalid)
			cur.Created = curSub.Created
			cur.Expires = curSub.Expires
			cur.Revoked = curSub.Revoked
			cur.Expired = curSub.Expired
			cur.Invalid = curSub.Invalid
			cur.Subkeys = append(cur.Subkeys, *curSub)
			keys = append(keys, cur)
		case "sub":
			if cur == nil {
				continue
			}
			sub := Subkey{Created: parseTime(get(5)), Expires: parseTime(get(6))}
			applyValidity(get(1), &sub.Revoked, &sub.Expired, &sub.Invalid)
			cur.Subkeys = append(cur.Subkeys, sub)
			curSub = &cur.Subkeys[len(cur.Subkeys)-1]
		case "fpr":
			if cur == nil {
				continue
			}
			fpr := get(9)
			if curSub == nil {
				continue
			}
			if cur.Fingerprint == "" && len(cur.Subkeys) == 1 && cur.Subkeys[0].Fingerprint == "" {
				cur.Fingerprint = fpr
				cur.Subkeys[0].Fingerprint = fpr
			} else if cur.Subkeys[len(cur.Subkeys)-1].Fingerprint == "" {
				cur.Subkeys[len(cur.Subkeys)-1].Fingerprint = fpr
			}
		case "uid":
			if cur == nil {
				continue
			}
			uid := UID{Value: unescapeColons(get(9))}
			applyValidity(get(1), &uid.Revoked, nil, &uid.Invalid)
			if email := uidEmail(uid.Value); email != "" {
				uid.AdvancedURL, uid.DirectURL = WKDURLs(email)
			}
			cur.UIDs = append(cur.UIDs, uid)
		}
	}
	// Keep the primary key first in the subkey list (gpgme exposes it as
	// key->subkeys too).
	for _, key := range keys {
		if len(key.Subkeys) > 0 && key.Subkeys[0].Fingerprint == "" {
			key.Subkeys[0].Fingerprint = key.Fingerprint
		}
	}
	return keys, nil
}

func applyValidity(validity string, revoked, expired, invalid *bool) {
	switch validity {
	case "r":
		if revoked != nil {
			*revoked = true
		}
	case "e":
		if expired != nil {
			*expired = true
		}
	case "i", "d":
		if invalid != nil {
			*invalid = true
		}
	}
}

// unescapeColons decodes gpg's \\xHH percent-style escaping in colon output.
func unescapeColons(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && s[i+1] == 'x' {
			if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// uidEmail extracts the address from "Name <user@example.com>".
func uidEmail(uid string) string {
	open := strings.LastIndexByte(uid, '<')
	close := strings.LastIndexByte(uid, '>')
	if open < 0 || close <= open {
		return ""
	}
	return uid[open+1 : close]
}

// FingerprintForKeyID resolves a key ID (short or long) to a full
// fingerprint among keys.
func FingerprintForKeyID(keys []*Key, keyID string) string {
	upper := strings.ToUpper(strings.TrimSuffix(keyID, "!"))
	for _, key := range keys {
		if strings.EqualFold(key.Fingerprint, upper) {
			return key.Fingerprint
		}
		if strings.HasSuffix(strings.ToUpper(key.Fingerprint), upper) {
			return key.Fingerprint
		}
	}
	return ""
}
