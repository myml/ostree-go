package gpg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// generateSigningKey creates a throwaway RSA key and returns its homedir.
func generateSigningKey(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	home, err := os.MkdirTemp(t.TempDir(), "gpg-home-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("gpg", "--batch", "--homedir", home, "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-generate-key", "Test User <t@example.com>", "rsa2048", "sign", "0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate gpg key: %v: %s", err, out)
	}
	return home
}

func TestPureVerifyRSASHA256(t *testing.T) {
	home := generateSigningKey(t)
	data := []byte("ostree signed payload")
	dataPath := filepath.Join(home, "data")
	sigPath := filepath.Join(home, "sig")
	keyringPath := filepath.Join(home, "keyring.gpg")
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("gpg", "--batch", "--homedir", home, "--detach-sign",
		"--output", sigPath, dataPath).CombinedOutput(); err != nil {
		t.Fatalf("sign: %v: %s", err, out)
	}
	export, err := exec.Command("gpg", "--batch", "--homedir", home, "--export").Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyringPath, export, 0o600); err != nil {
		t.Fatal(err)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}

	// The pure Go path must validate the signature on its own.
	res, err := verifyPure(data, sig, []string{keyringPath})
	if err != nil {
		t.Fatalf("verifyPure: %v", err)
	}
	if res.Valid != 1 {
		t.Fatalf("Valid = %d, want 1", res.Valid)
	}
	if len(res.KeyIDs) != 1 || len(res.KeyIDs[0]) != 16 {
		t.Fatalf("KeyIDs = %v", res.KeyIDs)
	}

	// A tampered payload must be rejected.
	if _, err := verifyPure([]byte("tampered"), sig, []string{keyringPath}); err == nil {
		t.Fatal("verifyPure accepted a tampered payload")
	}

	// An empty keyring must produce the "public key not found" error.
	empty := filepath.Join(home, "empty.gpg")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyPure(data, sig, []string{empty}); err == nil ||
		!strings.Contains(err.Error(), "public key not found") {
		t.Fatalf("empty keyring error = %v, want public key not found", err)
	}
}

func TestSignatureIssuerKeyIDs(t *testing.T) {
	home := generateSigningKey(t)
	dataPath := filepath.Join(home, "data")
	sigPath := filepath.Join(home, "sig")
	if err := os.WriteFile(dataPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("gpg", "--batch", "--homedir", home, "--detach-sign",
		"--output", sigPath, dataPath).CombinedOutput(); err != nil {
		t.Fatalf("sign: %v: %s", err, out)
	}
	sig, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := SignatureIssuerKeyIDs(sig)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || len(ids[0]) != 16 {
		t.Fatalf("issuer IDs = %v", ids)
	}
}

func TestWKDURLs(t *testing.T) {
	advanced, direct := WKDURLs("test@test.com")
	wantAdvanced := "https://openpgpkey.test.com/.well-known/openpgpkey/test.com/hu/iffe93qcsgp4c8ncbb378rxjo6cn9q6u?l=test"
	wantDirect := "https://test.com/.well-known/openpgpkey/hu/iffe93qcsgp4c8ncbb378rxjo6cn9q6u?l=test"
	if advanced != wantAdvanced {
		t.Fatalf("advanced = %q, want %q", advanced, wantAdvanced)
	}
	if direct != wantDirect {
		t.Fatalf("direct = %q, want %q", direct, wantDirect)
	}
}

func TestKeyIDMatches(t *testing.T) {
	const fpr = "5E65DE75AB1C501862D476347FCA23D8472CDAFA"
	cases := []struct {
		issuer, keyID string
		want          bool
	}{
		{fpr, fpr, true},
		{fpr, "7FCA23D8472CDAFA", true},
		{fpr, "7FCA23D8472CDAFA!", true},
		{fpr, "D8228CFECA950D41", false},
	}
	for _, tc := range cases {
		if got := KeyIDMatches(tc.issuer, tc.keyID); got != tc.want {
			t.Errorf("KeyIDMatches(%q, %q) = %v, want %v", tc.issuer, tc.keyID, got, tc.want)
		}
	}
}
