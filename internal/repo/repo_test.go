package repo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

// withUmask sets a process umask for the duration of the test. Tests using it
// must not call t.Parallel.
func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func testChecksum(seed string) string {
	return writeObjectChecksum([]byte(seed))
}

func newTestRepo(t *testing.T, mode Mode) *Repo {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Create(dir, mode, nil)
	if err != nil {
		t.Fatalf("Create(%s, %s): %v", dir, mode, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestModeParseStringConfigStringRoundTrip(t *testing.T) {
	tests := []struct {
		parse       string
		mode        Mode
		str         string
		configValue string
	}{
		{"bare", ModeBare, "bare", "bare"},
		{"bare-user", ModeBareUser, "bare-user", "bare-user"},
		{"bare-user-only", ModeBareUserOnly, "bare-user-only", "bare-user-only"},
		{"archive", ModeArchive, "archive", "archive-z2"},
		{"archive-z2", ModeArchive, "archive", "archive-z2"},
		{"bare-split-xattrs", ModeBareSplitXattrs, "bare-split-xattrs", "bare-split-xattrs"},
	}
	for _, tc := range tests {
		mode, err := ParseMode(tc.parse)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", tc.parse, err)
			continue
		}
		if mode != tc.mode {
			t.Errorf("ParseMode(%q) = %v, want %v", tc.parse, mode, tc.mode)
		}
		if got := mode.String(); got != tc.str {
			t.Errorf("%q String() = %q, want %q", tc.parse, got, tc.str)
		}
		if got := mode.ConfigString(); got != tc.configValue {
			t.Errorf("%q ConfigString() = %q, want %q", tc.parse, got, tc.configValue)
		}
		for _, spelling := range []string{mode.String(), mode.ConfigString()} {
			back, err := ParseMode(spelling)
			if err != nil || back != mode {
				t.Errorf("round-trip ParseMode(%q) = %v, %v; want %v", spelling, back, err, mode)
			}
		}
	}
	if ModeArchiveZ2 != ModeArchive {
		t.Error("ModeArchiveZ2 must alias ModeArchive")
	}
	if _, err := ParseMode("bogus"); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("ParseMode(bogus) error = %v, want ErrInvalidMode", err)
	}
	if _, err := ParseMode(""); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("ParseMode(\"\") error = %v, want ErrInvalidMode", err)
	}
	for _, mode := range []Mode{ModeBare, ModeBareUser, ModeBareUserOnly, ModeArchive, ModeBareSplitXattrs} {
		if !mode.Valid() {
			t.Errorf("%v.Valid() = false", mode)
		}
	}
	if Mode(-1).Valid() || Mode(5).Valid() {
		t.Error("out-of-range modes should be invalid")
	}
}

func TestCreateLayoutAllModes(t *testing.T) {
	withUmask(t, 0)
	for _, mode := range []Mode{ModeBare, ModeBareUser, ModeBareUserOnly, ModeArchive, ModeBareSplitXattrs} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "repo")
			r, err := Create(dir, mode, nil)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			defer r.Close()
			if got := r.Mode(); got != mode {
				t.Errorf("Mode() = %v, want %v", got, mode)
			}
			if !r.Writable() {
				t.Error("Writable() = false")
			}
			if !filepath.IsAbs(r.Path()) || r.Path() != dir {
				t.Errorf("Path() = %q, want %q", r.Path(), dir)
			}
			for _, d := range createDirs {
				st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(d)))
				if err != nil {
					t.Errorf("missing dir %s: %v", d, err)
					continue
				}
				if !st.IsDir() {
					t.Errorf("%s is not a directory", d)
				}
				if perm := st.Mode().Perm(); perm != 0o775 {
					t.Errorf("%s perm = %o, want 0775 (umask 0)", d, perm)
				}
			}
			configPath := filepath.Join(dir, "config")
			wantConfig := "[core]\nrepo_version=1\nmode=" + mode.ConfigString() + "\n"
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			if string(data) != wantConfig {
				t.Errorf("config:\n%q\nwant:\n%q", data, wantConfig)
			}
			st, err := os.Stat(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if perm := st.Mode().Perm(); perm != 0o644 {
				t.Errorf("config perm = %o, want 0644 (umask 0)", perm)
			}
			if got, err := r.Config().Get("core", "mode"); err != nil || got != mode.ConfigString() {
				t.Errorf("core.mode = %q, %v; want %q", got, err, mode.ConfigString())
			}
			if got, err := r.Config().Get("core", "repo_version"); err != nil || got != "1" {
				t.Errorf("core.repo_version = %q, %v", got, err)
			}
			if _, err := os.Stat(filepath.Join(dir, ".lock")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf(".lock should be created lazily, stat err = %v", err)
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				t.Errorf("repo root stat: %v", err)
			} else if perm := st.Mode().Perm(); perm != 0o775 {
				t.Errorf("repo root perm = %o, want 0775", perm)
			}
		})
	}
}

func TestCreateCollectionID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Create(dir, ModeBare, &CreateOptions{CollectionID: "org.example.Collection"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer r.Close()
	want := "[core]\nrepo_version=1\nmode=bare\ncollection-id=org.example.Collection\n"
	data, err := os.ReadFile(filepath.Join(dir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Errorf("config = %q, want %q", data, want)
	}
	if _, err := Create(filepath.Join(t.TempDir(), "bad"), ModeBare, &CreateOptions{CollectionID: "nodots"}); !errors.Is(err, ErrInvalidCollection) {
		t.Errorf("invalid collection error = %v, want ErrInvalidCollection", err)
	}
}

func TestCreateExistingRepository(t *testing.T) {
	// A repository created earlier wins over the requested mode: Create is a
	// no-op once objects/ exists.
	dir := filepath.Join(t.TempDir(), "repo")
	first, err := Create(dir, ModeArchive, nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "config"))
	first.Close()
	second, err := Create(dir, ModeBare, nil)
	if err != nil {
		t.Fatalf("Create over existing repo: %v", err)
	}
	defer second.Close()
	if got := second.Mode(); got != ModeArchive {
		t.Errorf("existing repo mode = %v, want archive", got)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "config"))
	if string(after) != string(before) {
		t.Errorf("config was modified by Create: %q -> %q", before, after)
	}

	// objects/ alone is enough to short-circuit creation.
	minimal := t.TempDir()
	if err := os.Mkdir(filepath.Join(minimal, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	third, err := Create(minimal, ModeBareUser, nil)
	if err != nil {
		t.Fatalf("Create over objects-only dir: %v", err)
	}
	defer third.Close()
	if _, err := os.Stat(filepath.Join(minimal, "config")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Create should not write config when objects/ exists, stat err = %v", err)
	}
}

func TestOpenRequiresObjectsDir(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrNotARepo) {
		t.Errorf("Open(missing) error = %v, want ErrNotARepo", err)
	}
	empty := t.TempDir()
	if _, err := Open(empty); !errors.Is(err, ErrNotARepo) {
		t.Errorf("Open(empty) error = %v, want ErrNotARepo", err)
	}
	withFile := t.TempDir()
	if err := os.WriteFile(filepath.Join(withFile, "objects"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(withFile); !errors.Is(err, ErrNotARepo) {
		t.Errorf("Open(objects as file) error = %v, want ErrNotARepo", err)
	}
}

func TestOpenConfigDefaultsAndErrors(t *testing.T) {
	// objects/ without config is opened as an implicit bare repo.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := r.Mode(); got != ModeBare {
		t.Errorf("implicit mode = %v, want bare", got)
	}
	if got, err := r.Config().Get("core", "repo_version"); err != nil || got != "1" {
		t.Errorf("implicit repo_version = %q, %v", got, err)
	}
	r.Close()

	cases := []struct {
		name string
		cfg  string
		want error
	}{
		{"bad-version", "[core]\nrepo_version=2\nmode=bare\n", ErrInvalidConfig},
		{"bad-mode", "[core]\nrepo_version=1\nmode=bogus\n", ErrInvalidMode},
		{"legacy-archive", "[core]\nrepo_version=1\nmode=bare\narchive=true\n", ErrInvalidMode},
		{"bad-locking", "[core]\nrepo_version=1\nmode=bare\nlocking=maybe\n", ErrInvalidConfig},
		{"bad-syntax", "[core\nrepo_version=1\n", ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "objects"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "config"), []byte(tc.cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(dir); !errors.Is(err, tc.want) {
				t.Errorf("Open error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOpenArchiveAliasInConfig(t *testing.T) {
	for _, modeString := range []string{"archive", "archive-z2"} {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "objects"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := "[core]\nrepo_version=1\nmode=" + modeString + "\n"
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte(cfg), 0o600); err != nil {
			t.Fatal(err)
		}
		r, err := Open(dir)
		if err != nil {
			t.Fatalf("Open mode=%s: %v", modeString, err)
		}
		if r.Mode() != ModeArchive {
			t.Errorf("mode=%s opened as %v, want archive", modeString, r.Mode())
		}
		r.Close()
	}
}

func TestWriteConfigRoundTrip(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	cfg := r.CloneConfig()
	cfg.SetRemote("origin", "url", "https://example.com/repo")
	cfg.Set("core", "lock-timeout-secs", "5")
	if err := r.WriteConfig(cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(r.Path(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != string(cfg.Bytes()) {
		t.Errorf("on-disk config = %q, want %q", onDisk, cfg.Bytes())
	}
	if r.Config() == cfg {
		t.Error("WriteConfig should store a clone, not the caller's pointer")
	}
	cfg.Set("core", "mode", "bogus")
	if err := r.WriteConfig(cfg); !errors.Is(err, ErrInvalidMode) {
		t.Errorf("WriteConfig(bad mode) error = %v, want ErrInvalidMode", err)
	}
	after, _ := os.ReadFile(filepath.Join(r.Path(), "config"))
	if string(after) != string(onDisk) {
		t.Error("invalid WriteConfig must not modify the file")
	}

	r.Close()
	reopened, err := Open(r.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Mode(); got != ModeBare {
		t.Errorf("reopened mode = %v", got)
	}
	if got, err := reopened.Config().GetRemote("origin", "url"); err != nil || got != "https://example.com/repo" {
		t.Errorf("reopened remote url = %q, %v", got, err)
	}
}

func TestCloseIdempotent(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	if err := r.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestRepoConfigCachedCopy(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	if got, want := r.Config().Groups(), []string{"core"}; !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %q, want %q", got, want)
	}
	clone := r.CloneConfig()
	clone.Set("core", "x", "y")
	if r.Config().HasKey("core", "x") {
		t.Error("CloneConfig is not a deep copy")
	}
}
