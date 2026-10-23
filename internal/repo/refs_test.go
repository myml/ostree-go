package repo

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func refPath(r *Repo, rel string) string {
	return filepath.Join(r.Path(), filepath.FromSlash(rel))
}

func TestRefReadWriteLocalRemoteMirror(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c1 := testChecksum("one")

	if err := r.WriteRef("os/one", c1); err != nil {
		t.Fatalf("WriteRef: %v", err)
	}
	data, err := os.ReadFile(refPath(r, "refs/heads/os/one"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != c1+"\n" {
		t.Errorf("ref content = %q, want %q", data, c1+"\n")
	}
	if got, err := r.ReadRef("os/one"); err != nil || got != c1 {
		t.Errorf("ReadRef = %q, %v", got, err)
	}

	if err := r.WriteRemoteRef("origin", "main", c1); err != nil {
		t.Fatalf("WriteRemoteRef: %v", err)
	}
	if got, err := r.ReadRemoteRef("origin", "main"); err != nil || got != c1 {
		t.Errorf("ReadRemoteRef = %q, %v", got, err)
	}
	if _, err := os.Stat(refPath(r, "refs/remotes/origin/main")); err != nil {
		t.Errorf("remote ref file: %v", err)
	}

	if err := r.WriteMirrorRef("org.example.Collection", "main", c1); err != nil {
		t.Fatalf("WriteMirrorRef: %v", err)
	}
	if got, err := r.ReadMirrorRef("org.example.Collection", "main"); err != nil || got != c1 {
		t.Errorf("ReadMirrorRef = %q, %v", got, err)
	}
	if _, err := os.Stat(refPath(r, "refs/mirrors/org.example.Collection/main")); err != nil {
		t.Errorf("mirror ref file: %v", err)
	}

	// Reading strips surrounding whitespace before validating.
	if err := os.WriteFile(refPath(r, "refs/heads/ws"), []byte("  "+c1+"  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadRef("ws"); err != nil || got != c1 {
		t.Errorf("ReadRef(ws) = %q, %v", got, err)
	}
	// ...but requires exactly 64 lowercase hex characters.
	if err := os.WriteFile(refPath(r, "refs/heads/bad"), []byte(strings.ToUpper(c1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadRef("bad"); !errors.Is(err, ErrInvalidChecksum) {
		t.Errorf("ReadRef(bad checksum) error = %v", err)
	}
	if _, err := r.ReadRef("does-not-exist"); err == nil {
		t.Error("ReadRef(missing) unexpectedly succeeded")
	}
	if err := r.WriteRef("bad-write", "ABCDEF"); !errors.Is(err, ErrInvalidChecksum) {
		t.Errorf("WriteRef(bad checksum) error = %v", err)
	}
}

func TestRefInvalidNames(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c := testChecksum("c")
	badNames := []string{
		"", ".", "..", "../evil", "a/../b", "a/./b", "/a", "a/", "a//b",
		"-a", ".a", "a b", "a\\b", "a:b", "a\nb", "a]b",
	}
	for _, name := range badNames {
		if err := r.WriteRef(name, c); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("WriteRef(%q) error = %v, want ErrInvalidRef", name, err)
		}
		if _, err := r.ReadRef(name); !errors.Is(err, ErrInvalidRef) {
			t.Errorf("ReadRef(%q) error = %v, want ErrInvalidRef", name, err)
		}
	}
	if err := r.WriteRemoteRef("bad remote", "main", c); !errors.Is(err, ErrInvalidRemote) {
		t.Errorf("WriteRemoteRef(bad remote) error = %v", err)
	}
	if _, err := r.ReadRemoteRef("bad remote", "main"); !errors.Is(err, ErrInvalidRemote) {
		t.Errorf("ReadRemoteRef(bad remote) error = %v", err)
	}
	if err := r.WriteMirrorRef("nodots", "main", c); !errors.Is(err, ErrInvalidCollection) {
		t.Errorf("WriteMirrorRef(bad collection) error = %v", err)
	}
	if _, err := r.ReadMirrorRef("nodots", "main"); !errors.Is(err, ErrInvalidCollection) {
		t.Errorf("ReadMirrorRef(bad collection) error = %v", err)
	}

	// Names that look like checksums are rejected for ref creation.
	checksumName := strings.Repeat("a", 64)
	if err := r.WriteRef(checksumName, c); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("WriteRef(checksum-like) error = %v, want ErrInvalidRef", err)
	}
	// Valid unusual-but-legal fragments.
	for _, name := range []string{"a", "A", "0", "_x", "a.b-c_d", "a.b/c.d", "a..b"} {
		if err := r.WriteRef(name, c); err != nil {
			t.Errorf("WriteRef(%q): %v", name, err)
		}
	}
}

func TestRefFileDirectoryConflicts(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c1 := testChecksum("c1")
	c2 := testChecksum("c2")

	if err := r.WriteRef("topic/sub", c1); err != nil {
		t.Fatal(err)
	}
	err := r.WriteRef("topic", c2)
	if !errors.Is(err, ErrRefConflict) {
		t.Errorf("WriteRef(topic) over topic/sub error = %v, want ErrRefConflict", err)
	}
	if !strings.Contains(err.Error(), "sub") || !strings.Contains(err.Error(), "topic") {
		t.Errorf("conflict error %q should mention the existing ref and prefix", err)
	}
	if got, err := r.ReadRef("topic/sub"); err != nil || got != c1 {
		t.Errorf("existing ref changed: %q, %v", got, err)
	}

	// Writing below a regular file is also a conflict.
	if err := r.WriteRef("plain", c1); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRef("plain/sub", c2); !errors.Is(err, ErrRefConflict) {
		t.Errorf("WriteRef(plain/sub) below a file error = %v, want ErrRefConflict", err)
	}

	// An empty directory in the way is removed, matching libostree.
	if err := os.MkdirAll(refPath(r, "refs/heads/empty"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRef("empty", c1); err != nil {
		t.Errorf("WriteRef over empty dir: %v", err)
	}
	st, err := os.Stat(refPath(r, "refs/heads/empty"))
	if err != nil || !st.Mode().IsRegular() {
		t.Errorf("empty dir was not replaced by a ref file: %v, %v", st, err)
	}

	// Junk files (e.g. rsync metadata) do not count as refs and are removed.
	if err := os.MkdirAll(refPath(r, "refs/heads/junk"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath(r, "refs/heads/junk/.rsync_info"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRef("junk", c2); err != nil {
		t.Errorf("WriteRef over junk dir: %v", err)
	}
	st, err = os.Stat(refPath(r, "refs/heads/junk"))
	if err != nil || !st.Mode().IsRegular() {
		t.Errorf("junk dir was not replaced by a ref file: %v, %v", st, err)
	}
	if _, err := os.Lstat(refPath(r, "refs/heads/junk/.rsync_info")); err == nil {
		t.Error("junk file survived")
	}
}

func TestRefWriteAtomicityAndPermissions(t *testing.T) {
	withUmask(t, 0)
	r := newTestRepo(t, ModeBare)
	c := testChecksum("perm")
	if err := r.WriteRef("perm/ref", c); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(refPath(r, "refs/heads/perm/ref"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o644 {
		t.Errorf("ref perm = %o, want 0644", perm)
	}
	entries, err := os.ReadDir(refPath(r, "refs/heads/perm"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestRefListPrefixSemantics(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	refs := map[string]string{
		"test":          testChecksum("test"),
		"testdir/child": testChecksum("child"),
		"testdir2/foo":  testChecksum("foo"),
	}
	for name, c := range refs {
		if err := r.WriteRef(name, c); err != nil {
			t.Fatal(err)
		}
	}
	originMain := testChecksum("origin-main")
	originTopic := testChecksum("origin-topic")
	if err := r.WriteRemoteRef("origin", "main", originMain); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteRemoteRef("origin", "topic/x", originTopic); err != nil {
		t.Fatal(err)
	}
	mirror := testChecksum("mirror")
	if err := r.WriteMirrorRef("org.example.Collection", "main", mirror); err != nil {
		t.Fatal(err)
	}

	names := func(refs []Ref) []string {
		out := make([]string, len(refs))
		for i, ref := range refs {
			out[i] = ref.RefSpec()
		}
		return out
	}

	all, err := r.ListRefs("")
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []string{
		"test", "testdir/child", "testdir2/foo",
		"origin:main", "origin:topic/x", "org.example.Collection:main",
	}
	if got := names(all); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("ListRefs(\"\") = %q, want %q", got, wantAll)
	}

	cases := []struct {
		prefix string
		want   []string
	}{
		// Path prefix, not string prefix: "te" does not match "test".
		{"te", []string{}},
		{"testd", []string{}},
		{"test", []string{"test"}},
		{"testdir", []string{"testdir/child"}},
		{"testdir/", []string{}},
		{"testdir/c", []string{}},
		{"testdir2", []string{"testdir2/foo"}},
		// Remote namespace selection.
		{"origin:", []string{"origin:main", "origin:topic/x"}},
		{"origin:main", []string{"origin:main"}},
		{"origin:m", []string{}},
		{"origin:topic", []string{"origin:topic/x"}},
		{"origin:topic/x", []string{"origin:topic/x"}},
		{"missing:", []string{}},
		// Mirror namespace selection.
		{"org.example.Collection:", []string{"org.example.Collection:main"}},
	}
	for _, tc := range cases {
		got, err := r.ListRefs(tc.prefix)
		if err != nil {
			t.Errorf("ListRefs(%q): %v", tc.prefix, err)
			continue
		}
		if gotNames := names(got); !reflect.DeepEqual(gotNames, tc.want) {
			t.Errorf("ListRefs(%q) = %q, want %q", tc.prefix, gotNames, tc.want)
		}
	}

	local, err := r.ListLocalRefs("")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := names(local), []string{"test", "testdir/child", "testdir2/foo"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListLocalRefs = %q, want %q", got, want)
	}
}

func TestRefListSkipsInvalidEntries(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c := testChecksum("x")
	if err := r.WriteRef("good", c); err != nil {
		t.Fatal(err)
	}
	// Ignored because the name is not a valid ref fragment.
	if err := os.WriteFile(refPath(r, "refs/heads/.rsync_info"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(refPath(r, "refs/heads/valid/.hidden"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(refPath(r, "refs/remotes/bad remote"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(refPath(r, "refs/remotes/bad remote/x"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath(r, "refs/remotes/bad remote/x/ref"), []byte(c+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(refPath(r, "refs/mirrors/nodots"), 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(refPath(r, "refs/mirrors/nodots/ref"), []byte(c+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	refs, err := r.ListRefs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "good" {
		t.Errorf("ListRefs = %+v, want only the good ref", refs)
	}
}

func TestRefListSkipsSpecialFiles(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c := testChecksum("special")
	if err := r.WriteRef("good", c); err != nil {
		t.Fatal(err)
	}
	fifo := refPath(r, "refs/heads/fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// Must not block reading the FIFO; it is not a ref.
	refs, err := r.ListRefs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "good" {
		t.Errorf("ListRefs = %+v, want only good", refs)
	}
}

func TestRefAliasesAreFollowed(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	c := testChecksum("target")
	if err := r.WriteRef("target", c); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../heads/target", refPath(r, "refs/heads/alias")); err != nil {
		t.Fatal(err)
	}
	if got, err := r.ReadRef("alias"); err != nil || got != c {
		t.Errorf("ReadRef(alias) = %q, %v", got, err)
	}
	refs, err := r.ListRefs("")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for _, ref := range refs {
		found[ref.RefSpec()] = ref.Checksum
	}
	if found["alias"] != c || found["target"] != c {
		t.Errorf("alias listing = %+v", refs)
	}
}

func TestRefKindAndPath(t *testing.T) {
	for _, tc := range []struct {
		ref  Ref
		spec string
		rel  string
	}{
		{Ref{Kind: RefLocal, Name: "os/x"}, "os/x", "refs/heads/os/x"},
		{Ref{Kind: RefRemote, Remote: "origin", Name: "os/x"}, "origin:os/x", "refs/remotes/origin/os/x"},
		{Ref{Kind: RefMirror, Collection: "org.example.C", Name: "os/x"}, "org.example.C:os/x", "refs/mirrors/org.example.C/os/x"},
	} {
		if got := tc.ref.RefSpec(); got != tc.spec {
			t.Errorf("RefSpec = %q, want %q", got, tc.spec)
		}
		if got := tc.ref.RelPath(); got != tc.rel {
			t.Errorf("RelPath = %q, want %q", got, tc.rel)
		}
	}
	if RefLocal.String() != "local" || RefRemote.String() != "remote" || RefMirror.String() != "mirror" {
		t.Error("RefKind.String mismatch")
	}
}

func TestRefReadDirectoryError(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	// A directory where a ref file is expected cannot be read as a ref.
	if err := os.MkdirAll(refPath(r, "refs/heads/dirref"), 0o775); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadRef("dirref"); err == nil {
		t.Error("ReadRef(directory) unexpectedly succeeded")
	} else if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadRef(directory) should be an EISDIR-style error, got %v", err)
	}
}
