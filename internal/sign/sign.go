// Package sign implements the pluggable OSTree commit/summary signing
// engines ("signapi"): dummy, ed25519 and spki.  It mirrors the C
// implementation in src/libostree/ostree-sign*.c, including the detached
// metadata key names, the key file formats and the user-visible messages.
package sign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Signing engine names, matching OSTREE_SIGN_NAME_*.
const (
	NameEd25519 = "ed25519"
	NameSpki    = "spki"
	NameDummy   = "dummy"
)

// Detached metadata keys, matching src/libotcore/otcore.h.
const (
	MetadataKeyEd25519 = "ostree.sign.ed25519"
	MetadataKeySpki    = "ostree.sign.spki"
	MetadataKeyDummy   = "ostree.sign.dummy"
	// MetadataFormat is the GVariant type used by all signapi engines.
	MetadataFormat = "aay"
)

// Key sizes, matching OSTREE_SIGN_ED25519_*.
const (
	Ed25519SeedSize   = 32
	Ed25519PubkeySize = 32
	Ed25519SecretSize = Ed25519SeedSize + Ed25519PubkeySize
	Ed25519SigSize    = 64
)

// Engine is one instantiated signing engine.  An Engine is not safe for
// concurrent use.
type Engine struct {
	name    string
	secret  []byte
	pubkeys [][]byte
	revoked [][]byte
}

// Names returns the implemented engine names.
func Names() []string { return []string{NameEd25519, NameSpki, NameDummy} }

// GetByName instantiates an engine.  The error message matches
// ostree_sign_get_by_name().
func GetByName(name string) (*Engine, error) {
	switch name {
	case NameEd25519:
		return &Engine{name: name}, nil
	case NameSpki:
		return &Engine{name: name}, nil
	case NameDummy:
		return &Engine{name: name}, nil
	}
	return nil, fmt.Errorf("Requested signature type is not implemented")
}

// Name returns the engine name (ed25519, spki, dummy).
func (e *Engine) Name() string { return e.name }

// MetadataKey is the detached metadata key holding this engine's signatures.
func (e *Engine) MetadataKey() string {
	switch e.name {
	case NameEd25519:
		return MetadataKeyEd25519
	case NameSpki:
		return MetadataKeySpki
	}
	return MetadataKeyDummy
}

// MetadataFormat is the GVariant type of the signatures value.
func (e *Engine) MetadataFormat() string { return MetadataFormat }

// ClearKeys drops all loaded secret/public keys.
func (e *Engine) ClearKeys() {
	e.secret = nil
	e.pubkeys = nil
	e.revoked = nil
}

// dummyEnabled mirrors check_dummy_sign_enabled().
func dummyEnabled() error {
	if os.Getenv("OSTREE_DUMMY_SIGN_ENABLED") != "1" {
		return fmt.Errorf("dummy signature type is only for ostree testing")
	}
	return nil
}

// SetSK installs a secret key given as a string (the command line form).
func (e *Engine) SetSK(key string) error {
	e.ClearKeys()
	switch e.name {
	case NameDummy:
		if err := dummyEnabled(); err != nil {
			return err
		}
		e.secret = []byte(key)
		return nil
	case NameEd25519:
		buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil {
			return fmt.Errorf("Invalid ed25519 secret key: Ill-formed input: invalid base64")
		}
		if len(buf) != Ed25519SecretSize {
			return fmt.Errorf("Invalid ed25519 secret key: Ill-formed input: expected %d bytes, got %d bytes", Ed25519SecretSize, len(buf))
		}
		e.secret = buf
		return nil
	case NameSpki:
		buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil {
			return fmt.Errorf("Unknown spki secret key type")
		}
		e.secret = buf
		return nil
	}
	return fmt.Errorf("Unknown signature type")
}

// SetSKBytes installs a raw (already decoded) secret key, as used by
// --sign-from-file.
func (e *Engine) SetSKBytes(buf []byte) error {
	e.ClearKeys()
	switch e.name {
	case NameDummy:
		if err := dummyEnabled(); err != nil {
			return err
		}
		e.secret = append([]byte(nil), buf...)
		return nil
	case NameEd25519:
		if len(buf) != Ed25519SecretSize {
			return fmt.Errorf("Invalid ed25519 secret key: Ill-formed input: expected %d bytes, got %d bytes", Ed25519SecretSize, len(buf))
		}
		e.secret = append([]byte(nil), buf...)
		return nil
	case NameSpki:
		e.secret = append([]byte(nil), buf...)
		return nil
	}
	return fmt.Errorf("Unknown signature type")
}

// SetPK replaces the trusted key set with a single key.
func (e *Engine) SetPK(key string) error {
	e.ClearKeys()
	return e.AddPK(key)
}

// AddPK appends one trusted key given as a string.
func (e *Engine) AddPK(key string) error {
	switch e.name {
	case NameDummy:
		e.pubkeys = append(e.pubkeys, []byte(key))
		return nil
	case NameEd25519:
		buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil {
			return fmt.Errorf("Invalid ed25519 public key: Ill-formed input: invalid base64")
		}
		if len(buf) != Ed25519PubkeySize {
			return fmt.Errorf("Invalid ed25519 public key: Ill-formed input: expected %d bytes, got %d bytes", Ed25519PubkeySize, len(buf))
		}
		e.addPubkey(buf)
		return nil
	case NameSpki:
		buf, err := base64.StdEncoding.DecodeString(strings.TrimSpace(key))
		if err != nil {
			return fmt.Errorf("Unknown spki public key type")
		}
		e.addPubkey(buf)
		return nil
	}
	return fmt.Errorf("Unknown signature type")
}

// AddPKBytes appends one trusted raw key.
func (e *Engine) AddPKBytes(buf []byte) error {
	switch e.name {
	case NameDummy:
		e.pubkeys = append(e.pubkeys, append([]byte(nil), buf...))
		return nil
	case NameEd25519:
		if len(buf) != Ed25519PubkeySize {
			return fmt.Errorf("Invalid ed25519 public key: Ill-formed input: expected %d bytes, got %d bytes", Ed25519PubkeySize, len(buf))
		}
		e.addPubkey(buf)
		return nil
	case NameSpki:
		e.addPubkey(buf)
		return nil
	}
	return fmt.Errorf("Unknown signature type")
}

func (e *Engine) addPubkey(buf []byte) {
	for _, existing := range e.pubkeys {
		if bytes.Equal(existing, buf) {
			return
		}
	}
	e.pubkeys = append(e.pubkeys, append([]byte(nil), buf...))
}

func (e *Engine) addRevoked(buf []byte) {
	for _, existing := range e.revoked {
		if bytes.Equal(existing, buf) {
			return
		}
	}
	e.revoked = append(e.revoked, append([]byte(nil), buf...))
}

func (e *Engine) isRevoked(key []byte) bool {
	for _, r := range e.revoked {
		if bytes.Equal(r, key) {
			return true
		}
	}
	return false
}

// LoadPKOptions mirrors the a{sv} passed to ostree_sign_load_pk().
type LoadPKOptions struct {
	// Filename loads keys from a single file.
	Filename string
	// BaseDir overrides the system-wide key directories.
	BaseDir string
}

// LoadPK loads trusted (and, for the directory scan, revoked) public keys.
func (e *Engine) LoadPK(opts LoadPKOptions) error {
	if opts.Filename != "" {
		return e.loadPKFile(opts.Filename, true)
	}
	switch e.name {
	case NameDummy:
		// The dummy engine has no load_pk implementation in C.
		return nil
	}
	dirs := []string{}
	if opts.BaseDir != "" {
		dirs = append(dirs, opts.BaseDir)
	} else {
		dirs = append(dirs, "/etc/ostree", "/usr/share/ostree")
	}
	loaded := false
	for _, dir := range dirs {
		if e.scanKeys(dir, true) {
			loaded = true
		}
	}
	if !loaded {
		return fmt.Errorf("signature: %s: no keys loaded", e.name)
	}
	for _, dir := range dirs {
		e.scanKeys(dir, false)
	}
	return nil
}

// scanKeys follows _ed25519_load_pk()/_spki_load_pk(): it looks at
// trusted.<name> plus trusted.<name>.d/* in each base directory.
func (e *Engine) scanKeys(base string, trusted bool) bool {
	kind := "trusted"
	if !trusted {
		kind = "revoked"
	}
	var files []string
	baseName := filepath.Join(base, kind+"."+e.name)
	files = append(files, baseName)
	if entries, err := os.ReadDir(baseName + ".d"); err == nil {
		for _, entry := range entries {
			files = append(files, filepath.Join(baseName+".d", entry.Name()))
		}
	}
	loaded := false
	for _, file := range files {
		if err := e.loadPKFile(file, trusted); err == nil {
			loaded = true
		}
	}
	return loaded
}

func (e *Engine) loadPKFile(filename string, trusted bool) error {
	st, err := os.Stat(filename)
	if err != nil || st.IsDir() {
		return fmt.Errorf("File object '%s' is not a regular file", filename)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	blobs, err := readKeyBlobs(e.name, data, true)
	if err != nil {
		return err
	}
	added := false
	var firstErr error
	for _, blob := range blobs {
		if trusted {
			err = e.AddPKBytes(blob)
		} else {
			e.addRevoked(blob)
			err = nil
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		added = true
	}
	if !added {
		if firstErr != nil {
			return firstErr
		}
		return fmt.Errorf("signature: %s: no valid keys in file '%s'", e.name, filename)
	}
	return nil
}

// readKeyBlobs decodes a key file into raw key blobs.  ed25519 keys are
// base64 encoded one per line; spki keys are PEM blocks.
func readKeyBlobs(name string, data []byte, public bool) ([][]byte, error) {
	if name == NameSpki {
		label := "PUBLIC KEY"
		if !public {
			label = "PRIVATE KEY"
		}
		return readPEMBlocks(data, label)
	}
	return readBase64Lines(data)
}

func readBase64Lines(data []byte) ([][]byte, error) {
	var out [][]byte
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		buf, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			// GLib's g_base64_decode() silently ignores invalid input;
			// keep the successfully decoded prefix the same way.
			if buf == nil {
				continue
			}
		}
		out = append(out, buf)
	}
	return out, nil
}

func readPEMBlocks(data []byte, label string) ([][]byte, error) {
	const beginPrefix = "-----BEGIN "
	const suffix = "-----"
	var out [][]byte
	lines := strings.Split(string(data), "\n")
	inBlock := false
	blockLabel := ""
	var buf bytes.Buffer
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !inBlock {
			if strings.HasPrefix(line, beginPrefix) && strings.HasSuffix(line, suffix) {
				start := len(beginPrefix)
				end := strings.LastIndex(line[start:], suffix)
				blockLabel = line[start : start+end]
				if blockLabel != label {
					return nil, fmt.Errorf("Unexpected label %q", blockLabel)
				}
				inBlock = true
				buf.Reset()
			}
			continue
		}
		if strings.HasPrefix(line, "-----END ") && strings.HasSuffix(line, suffix) {
			decoded, err := base64.StdEncoding.DecodeString(buf.String())
			if err != nil {
				return nil, fmt.Errorf("Invalid base64 blob format")
			}
			out = append(out, decoded)
			inBlock = false
			continue
		}
		buf.WriteString(line)
	}
	if inBlock {
		return nil, fmt.Errorf("PEM trailer not found")
	}
	return out, nil
}

// Sign produces a detached signature over data.
func (e *Engine) Sign(data []byte) ([]byte, error) {
	switch e.name {
	case NameDummy:
		if err := dummyEnabled(); err != nil {
			return nil, err
		}
		if e.secret == nil {
			return nil, fmt.Errorf("Not able to sign: secret key is not set")
		}
		return append([]byte(nil), e.secret...), nil
	case NameEd25519:
		if len(e.secret) != Ed25519SecretSize {
			return nil, fmt.Errorf("Not able to sign: secret key is not set")
		}
		return ed25519.Sign(ed25519.PrivateKey(e.secret), data), nil
	case NameSpki:
		if len(e.secret) == 0 {
			return nil, fmt.Errorf("Not able to sign: secret key is not set")
		}
		return spkiSign(e.secret, data)
	}
	return nil, fmt.Errorf("Unknown signature type")
}

// Verify checks sigs against the loaded public keys and returns the success
// message produced by the engine on success.
func (e *Engine) Verify(data []byte, sigs [][]byte) (string, error) {
	switch e.name {
	case NameDummy:
		return e.verifyDummy(data, sigs)
	case NameEd25519:
		return e.verifyEd25519(data, sigs)
	case NameSpki:
		return e.verifySpki(data, sigs)
	}
	return "", fmt.Errorf("Unknown signature type")
}

func (e *Engine) verifyDummy(data []byte, sigs [][]byte) (string, error) {
	if err := dummyEnabled(); err != nil {
		return "", err
	}
	if sigs == nil {
		return "", fmt.Errorf("signature: dummy: commit have no signatures of my type")
	}
	for _, sig := range sigs {
		for _, pk := range e.pubkeys {
			if bytes.Equal(sig, pk) {
				return "dummy: Signature verified", nil
			}
		}
	}
	if len(sigs) > 0 {
		return "", fmt.Errorf("signature: dummy: incorrect signatures found: %d", len(sigs))
	}
	return "", fmt.Errorf("signature: dummy: no signatures")
}

func (e *Engine) verifyEd25519(data []byte, sigs [][]byte) (string, error) {
	if sigs == nil {
		return "", fmt.Errorf("ed25519: commit have no signatures of my type")
	}
	if len(e.pubkeys) == 0 {
		if err := e.LoadPK(LoadPKOptions{}); err != nil {
			return "", err
		}
	}
	var invalid []string
	for _, sig := range sigs {
		if len(sig) != Ed25519SigSize {
			return "", fmt.Errorf("Invalid signature: Ill-formed input: expected %d bytes, got %d bytes", Ed25519SigSize, len(sig))
		}
		for _, pk := range e.pubkeys {
			if e.isRevoked(pk) {
				continue
			}
			if len(pk) != Ed25519PubkeySize {
				continue
			}
			if ed25519.Verify(ed25519.PublicKey(pk), data, sig) {
				return fmt.Sprintf("ed25519: Signature verified successfully with key '%s'", hex.EncodeToString(pk)), nil
			}
			invalid = append(invalid, "key '"+hex.EncodeToString(pk)+"'")
		}
	}
	if len(invalid) > 0 {
		if len(invalid) > 3 {
			return "", fmt.Errorf("ed25519: Signature couldn't be verified; tried %d keys", len(invalid))
		}
		return "", fmt.Errorf("ed25519: Signature couldn't be verified with: %s", strings.Join(invalid, "; "))
	}
	return "", fmt.Errorf("ed25519: no signatures found")
}

func (e *Engine) verifySpki(data []byte, sigs [][]byte) (string, error) {
	if sigs == nil {
		return "", fmt.Errorf("spki: commit have no signatures of my type")
	}
	if len(e.pubkeys) == 0 {
		if err := e.LoadPK(LoadPKOptions{}); err != nil {
			return "", err
		}
	}
	var invalid []string
	for _, sig := range sigs {
		for _, pk := range e.pubkeys {
			if e.isRevoked(pk) {
				continue
			}
			ok, err := spkiVerify(pk, data, sig)
			if err != nil {
				return "", err
			}
			if ok {
				return fmt.Sprintf("spki: Signature verified successfully with key '%s'", hex.EncodeToString(pk)), nil
			}
			invalid = append(invalid, "key '"+hex.EncodeToString(pk)+"'")
		}
	}
	if len(invalid) > 0 {
		if len(invalid) > 3 {
			return "", fmt.Errorf("spki: Signature couldn't be verified; tried %d keys", len(invalid))
		}
		return "", fmt.Errorf("spki: Signature couldn't be verified with: %s", strings.Join(invalid, "; "))
	}
	return "", fmt.Errorf("spki: no signatures found")
}

// signPEM encodes DER key material in a PEM block.
func signPEM(label string, der []byte) []byte {
	var b strings.Builder
	b.WriteString("-----BEGIN " + label + "-----\n")
	enc := base64.StdEncoding.EncodeToString(der)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	if enc != "" {
		b.WriteString(enc + "\n")
	}
	b.WriteString("-----END " + label + "-----\n")
	return []byte(b.String())
}

// spkiSign signs data with the OpenSSL CLI.  The spki engine supports
// arbitrary SubjectPublicKeyInfo algorithms (notably Ed448), which Go's
// standard library does not implement; we therefore use the same crypto
// backend the C implementation links against.
func spkiSign(der, data []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "ostree-spki-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	keyPath := filepath.Join(dir, "key.pem")
	dataPath := filepath.Join(dir, "data")
	sigPath := filepath.Join(dir, "sig")
	if err := os.WriteFile(keyPath, signPEM("PRIVATE KEY", der), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.Command("openssl", "pkeyutl", "-sign", "-inkey", keyPath, "-rawin", "-in", dataPath, "-out", sigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("openssl: Failed to sign: %s", strings.TrimSpace(string(out)))
	}
	return os.ReadFile(sigPath)
}

// spkiVerify verifies a detached signature with the OpenSSL CLI.
func spkiVerify(der, data, sig []byte) (bool, error) {
	dir, err := os.MkdirTemp("", "ostree-spki-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	keyPath := filepath.Join(dir, "key.pem")
	dataPath := filepath.Join(dir, "data")
	sigPath := filepath.Join(dir, "sig")
	if err := os.WriteFile(keyPath, signPEM("PUBLIC KEY", der), 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		return false, err
	}
	cmd := exec.Command("openssl", "pkeyutl", "-verify", "-pubin", "-inkey", keyPath, "-rawin", "-in", dataPath, "-sigfile", sigPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		// A failed verification is not an operational error; the C
		// implementation treats it as a non-matching key.
		_ = out
		return false, nil
	}
	return true, nil
}

// DecodeKeyBlobs decodes a key file into raw key blobs.  It is used by the
// command layer for --sign-from-file (private keys) and is also handy for
// tests.
func DecodeKeyBlobs(name string, data []byte, public bool) ([][]byte, error) {
	return readKeyBlobs(name, data, public)
}
