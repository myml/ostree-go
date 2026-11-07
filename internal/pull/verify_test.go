package pull

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/myml/ostree-go/internal/gpg"
	"github.com/myml/ostree-go/internal/gvariant"
	"github.com/myml/ostree-go/internal/repo"
	"github.com/myml/ostree-go/internal/sign"
)

func encodeDetached(t *testing.T, key string, sigs ...[]byte) []byte {
	t.Helper()
	var arr []gvariant.Value
	for _, sig := range sigs {
		arr = append(arr, sig)
	}
	dict := gvariant.Dict{}
	dict.Set(key, gvariant.Variant{Type: gvariant.MustParseType("aay"), Value: arr})
	data, err := gvariant.Encode(gvariant.MustParseType("a{sv}"), dict)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func newVerifyTestRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "repo"), repo.ModeArchive, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestVerifyCommitSignapiDummy(t *testing.T) {
	t.Setenv("OSTREE_DUMMY_SIGN_ENABLED", "1")
	r := newVerifyTestRepo(t)
	data := []byte("signed commit payload")

	engine, err := sign.GetByName(sign.NameDummy)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SetSK("dummysign"); err != nil {
		t.Fatal(err)
	}
	sig, err := engine.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	detached := encodeDetached(t, engine.MetadataKey(), sig)

	cfg := r.Config()
	cfg.SetRemote("origin", "url", "http://example.com/repo")
	cfg.SetRemote("origin", "gpg-verify", "false")
	cfg.SetRemote("origin", "sign-verify", "dummy")
	cfg.SetRemote("origin", "verification-dummy-key", "dummysign")
	verifier, err := NewPullVerifier(r, "origin", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyCommit("deadbeef", data, detached); err != nil {
		t.Fatalf("VerifyCommit: %v", err)
	}

	// A wrong key must be rejected.
	cfg.SetRemote("origin", "verification-dummy-key", "wrong")
	verifier, err = NewPullVerifier(r, "origin", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyCommit("deadbeef", data, detached); err == nil {
		t.Fatal("VerifyCommit accepted a wrong dummy key")
	}

	// A missing signature must be rejected.
	if err := verifier.VerifyCommit("deadbeef", data, nil); err == nil {
		t.Fatal("VerifyCommit accepted a missing signature")
	}
}

func TestVerifyCommitSignapiUnknownType(t *testing.T) {
	r := newVerifyTestRepo(t)
	cfg := r.Config()
	cfg.SetRemote("origin", "sign-verify", "nosuchsystem")
	cfg.SetRemote("origin", "verification-nosuchsystem-key", "x")
	_, err := NewPullVerifier(r, "origin", VerifyOptions{})
	if err == nil || !strings.Contains(err.Error(), "Requested signature type is not implemented") {
		t.Fatalf("NewPullVerifier error = %v", err)
	}
}

func TestVerifyCommitSignapiMissingKeys(t *testing.T) {
	r := newVerifyTestRepo(t)
	r.Config().SetRemote("origin", "sign-verify", "dummy")
	_, err := NewPullVerifier(r, "origin", VerifyOptions{})
	if err == nil || !strings.Contains(err.Error(), "No keys found for required signapi type dummy") {
		t.Fatalf("NewPullVerifier error = %v", err)
	}
}

func TestVerifySummarySignapi(t *testing.T) {
	t.Setenv("OSTREE_DUMMY_SIGN_ENABLED", "1")
	r := newVerifyTestRepo(t)
	data := []byte("summary bytes")
	engine, err := sign.GetByName(sign.NameDummy)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.SetSK("dummysign"); err != nil {
		t.Fatal(err)
	}
	sig, err := engine.Sign(data)
	if err != nil {
		t.Fatal(err)
	}
	sigData := encodeDetached(t, engine.MetadataKey(), sig)
	cfg := r.Config()
	cfg.SetRemote("origin", "sign-verify-summary", "dummy")
	cfg.SetRemote("origin", "verification-dummy-key", "dummysign")
	cacheDir := t.TempDir()
	verifier, err := NewPullVerifier(r, "origin", VerifyOptions{CacheDir: cacheDir})
	if err != nil {
		t.Fatal(err)
	}
	if !verifier.NeedsSummary() {
		t.Fatal("NeedsSummary = false")
	}
	if err := verifier.VerifyAndCacheSummary(data, sigData); err != nil {
		t.Fatalf("VerifyAndCacheSummary: %v", err)
	}
	for _, name := range []string{"origin", "origin.sig"} {
		if _, err := os.Stat(filepath.Join(cacheDir, "summaries", name)); err != nil {
			t.Fatalf("cache file %s: %v", name, err)
		}
	}

	// Invalid signature bytes must report "No signatures found".
	if err := verifier.VerifySummary(data, []byte("invalid\n")); err == nil ||
		!strings.Contains(err.Error(), "No signatures found") {
		t.Fatalf("invalid signature error = %v", err)
	}
}

func TestVerifyCommitGPG(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("gpg", "--batch", "--homedir", home, "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-generate-key", "Test <t@example.com>", "rsa2048", "sign", "0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate gpg key: %v: %s", err, out)
	}
	data := []byte("gpg signed commit")
	sig, err := gpg.SignData(data, "", home)
	if err != nil {
		t.Fatal(err)
	}
	export, err := exec.Command("gpg", "--batch", "--homedir", home, "--export").Output()
	if err != nil {
		t.Fatal(err)
	}
	r := newVerifyTestRepo(t)
	r.Config().SetRemote("origin", "url", "http://example.com/repo")
	keyring := gpg.RemoteKeyringPath(r.Path(), "origin")
	if err := os.WriteFile(keyring, export, 0o644); err != nil {
		t.Fatal(err)
	}
	detached := encodeDetached(t, sign.GpgSignaturesKey, sig)
	verifier, err := NewPullVerifier(r, "origin", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := verifier.VerifyCommit("cafebabe", data, detached); err != nil {
		t.Fatalf("VerifyCommit: %v", err)
	}
	if err := verifier.VerifyCommit("cafebabe", []byte("tampered"), detached); err == nil {
		t.Fatal("VerifyCommit accepted tampered data")
	}

	// An unknown signing key must produce the C-compatible error.
	otherHome := t.TempDir()
	if err := os.Chmod(otherHome, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("gpg", "--batch", "--homedir", otherHome, "--pinentry-mode", "loopback",
		"--passphrase", "", "--quick-generate-key", "Other <o@example.com>", "rsa2048", "sign", "0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot generate gpg key: %v: %s", err, out)
	}
	otherSig, err := gpg.SignData(data, "", otherHome)
	if err != nil {
		t.Fatal(err)
	}
	otherDetached := encodeDetached(t, sign.GpgSignaturesKey, otherSig)
	if err := verifier.VerifyCommit("cafebabe", data, otherDetached); err == nil ||
		!strings.Contains(err.Error(), "public key not found") {
		t.Fatalf("unknown key error = %v", err)
	}
}

func TestSplitKeyPathList(t *testing.T) {
	cases := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{"/a.asc", []string{"/a.asc"}, false},
		{"/a.asc,/b.asc", []string{"/a.asc", "/b.asc"}, false},
		{"/a.asc;", []string{"/a.asc"}, false},
		{",/a.asc", []string{"", "/a.asc"}, false},
		{"/a.asc,/b.asc;/c.asc", nil, true},
	}
	for _, tc := range cases {
		got, err := splitKeyPathList(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitKeyPathList(%q) did not fail", tc.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitKeyPathList(%q): %v", tc.raw, err)
			continue
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitKeyPathList(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
