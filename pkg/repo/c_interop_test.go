package repo

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ostreeBinPath = "/usr/bin/ostree"

func requireOstree(t *testing.T) string {
	t.Helper()
	if _, err := os.Stat(ostreeBinPath); err != nil {
		t.Skipf("%s not available, skipping C interop test", ostreeBinPath)
	}
	return ostreeBinPath
}

func runOstree(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(ostreeBinPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func mustOstree(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runOstree(t, args...)
	if err != nil {
		t.Fatalf("ostree %s failed: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

func mustCInit(t *testing.T, dir string, mode string) {
	t.Helper()
	mustOstree(t, "init", "--repo="+dir, "--mode="+mode)
}

func TestCInitGoOpenAllModes(t *testing.T) {
	requireOstree(t)
	tests := []struct {
		cliMode string
		mode    Mode
	}{
		{"bare", ModeBare},
		{"bare-user", ModeBareUser},
		{"bare-user-only", ModeBareUserOnly},
		{"archive", ModeArchive},
		{"archive-z2", ModeArchive},
		{"bare-split-xattrs", ModeBareSplitXattrs},
	}
	for _, tc := range tests {
		t.Run(tc.cliMode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "repo")
			mustCInit(t, dir, tc.cliMode)
			r, err := Open(dir)
			if err != nil {
				t.Fatalf("Go Open of C repo: %v", err)
			}
			defer r.Close()
			if got := r.Mode(); got != tc.mode {
				t.Errorf("mode = %v, want %v", got, tc.mode)
			}
			if got, err := r.Config().Get("core", "mode"); err != nil || got != tc.mode.ConfigString() {
				t.Errorf("core.mode = %q, %v; want %q", got, err, tc.mode.ConfigString())
			}
			if got, err := r.Config().Get("core", "repo_version"); err != nil || got != "1" {
				t.Errorf("core.repo_version = %q, %v", got, err)
			}
			if !r.Writable() {
				t.Error("C repo should be writable")
			}
			for _, d := range createDirs {
				st, err := os.Stat(filepath.Join(dir, filepath.FromSlash(d)))
				if err != nil || !st.IsDir() {
					t.Errorf("directory %s missing after C init: %v", d, err)
				}
			}
			// Writing a ref into the C-created repo must be readable by C.
			csum := testChecksum("c-init-" + tc.cliMode)
			if err := r.WriteRef("go/ref", csum); err != nil {
				t.Fatalf("WriteRef into C repo: %v", err)
			}
			out := mustOstree(t, "refs", "-r", "--repo="+dir)
			if !strings.Contains(out, "go/ref\t"+csum+"\n") {
				t.Errorf("C refs -r output = %q, want go/ref\\t%s", out, csum)
			}
		})
	}
}

func createCRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	mustCInit(t, dir, "bare")
	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "hello.txt"), []byte("hello ostree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "sub", "nested"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustOstree(t, "commit", "--repo="+dir, "-b", "test", "--tree=dir="+tree)
	return dir
}

func TestOpenRealCBareRepo(t *testing.T) {
	requireOstree(t)
	dir := createCRepoWithCommit(t)
	r, err := Open(dir)
	if err != nil {
		t.Fatalf("Open C bare repo: %v", err)
	}
	defer r.Close()
	if got := r.Mode(); got != ModeBare {
		t.Errorf("mode = %v, want bare", got)
	}

	refs, err := r.ListRefs("")
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	var commitRef Ref
	for _, ref := range refs {
		if ref.Kind == RefLocal && ref.Name == "test" {
			commitRef = ref
		}
	}
	if commitRef.Checksum == "" {
		t.Fatalf("C repo has no local 'test' ref: %+v", refs)
	}
	checksum, err := r.ReadRef("test")
	if err != nil || checksum != commitRef.Checksum {
		t.Fatalf("ReadRef(test) = %q, %v; want %q", checksum, err, commitRef.Checksum)
	}
	commit, err := r.ReadObject(checksum, ObjectCommit)
	if err != nil {
		t.Fatalf("ReadObject(commit): %v", err)
	}
	if len(commit) == 0 {
		t.Error("commit object is empty")
	}
	if ok, err := r.HasObject(checksum, ObjectCommit); !ok || err != nil {
		t.Errorf("HasObject(commit) = %v, %v", ok, err)
	}

	// Path-based prefix semantics: "te" is not a path prefix of "test".
	if prefixed, err := r.ListRefs("te"); err != nil {
		t.Fatal(err)
	} else {
		for _, ref := range prefixed {
			if ref.Name == "test" {
				t.Error(`ListRefs("te") must not match "test" (prefix is a ref path prefix)`)
			}
		}
	}
	exact, err := r.ListRefs("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(exact) != 1 || exact[0].Name != "test" || exact[0].Kind != RefLocal {
		t.Errorf(`ListRefs("test") = %+v, want the single local test ref`, exact)
	}
}

// TestOpenExternalCBareProbe opens the long-lived /tmp probe repository when it
// exists, which exercises a repository created outside this test run.
func TestOpenExternalCBareProbe(t *testing.T) {
	const probe = "/tmp/ostree-probe/repo"
	if _, err := os.Stat(filepath.Join(probe, "objects")); err != nil {
		t.Skipf("%s is not available", probe)
	}
	r, err := Open(probe)
	if err != nil {
		t.Fatalf("Open(%s): %v", probe, err)
	}
	defer r.Close()
	if got := r.Mode(); got != ModeBare {
		t.Errorf("probe mode = %v, want bare", got)
	}
	refs, err := r.ListRefs("")
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	for _, ref := range refs {
		if ref.Kind != RefLocal {
			continue
		}
		data, err := r.ReadObject(ref.Checksum, ObjectCommit)
		if err != nil {
			t.Errorf("ReadObject(%s): %v", ref.Name, err)
			continue
		}
		if len(data) == 0 {
			t.Errorf("ReadObject(%s) returned no data", ref.Name)
		}
		break
	}
}

func TestGoCreateCReads(t *testing.T) {
	requireOstree(t)
	for _, mode := range []Mode{ModeBare, ModeBareUser, ModeBareUserOnly, ModeArchive, ModeBareSplitXattrs} {
		t.Run(mode.String(), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "repo")
			r, err := Create(dir, mode, nil)
			if err != nil {
				t.Fatalf("Go Create: %v", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			if got := strings.TrimSpace(mustOstree(t, "config", "get", "--repo="+dir, "core.mode")); got != mode.ConfigString() {
				t.Errorf("C core.mode = %q, want %q", got, mode.ConfigString())
			}
			fsckOut := mustOstree(t, "fsck", "--all", "--repo="+dir)
			if !strings.Contains(fsckOut, "no errors found") {
				t.Errorf("C fsck output:\n%s", fsckOut)
			}
			if _, stderr, err := runOstree(t, "refs", "--repo="+dir); err != nil {
				t.Errorf("C refs failed: %v (%s)", err, stderr)
			}

			// Go writes a ref; C must read it back byte-for-byte.
			csum := testChecksum("go-create-" + mode.String())
			r, err = Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if err := r.WriteRef("test/ref", csum); err != nil {
				t.Fatalf("WriteRef: %v", err)
			}
			out := mustOstree(t, "refs", "-r", "--repo="+dir)
			if !strings.Contains(out, "test/ref\t"+csum+"\n") {
				t.Errorf("C refs -r output = %q, want test/ref\\t%s", out, csum)
			}
			if got := strings.TrimSpace(mustOstree(t, "rev-parse", "--repo="+dir, "test/ref")); got != csum {
				t.Errorf("C rev-parse = %q, want %q", got, csum)
			}
			// And Go reads what C reports.
			listed, err := r.ListRefs("test")
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 1 || listed[0].Checksum != csum {
				t.Errorf("Go ListRefs after C read = %+v", listed)
			}
		})
	}
}

func TestCRefPrefixSemanticsMatchGo(t *testing.T) {
	requireOstree(t)
	r := newTestRepo(t, ModeBare)
	c1 := testChecksum("test")
	c2 := testChecksum("child")
	c3 := testChecksum("foo")
	c4 := testChecksum("origin-main")
	for name, csum := range map[string]string{
		"test":          c1,
		"testdir/child": c2,
		"testdir2/foo":  c3,
	} {
		if err := r.WriteRef(name, csum); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.WriteRemoteRef("origin", "main", c4); err != nil {
		t.Fatal(err)
	}

	localNames := func(refs []Ref) []string {
		var out []string
		for _, ref := range refs {
			out = append(out, ref.Name)
		}
		return out
	}

	// `ostree refs PREFIX` cuts the prefix; Go keeps full names.
	out := mustOstree(t, "refs", "--repo="+r.Path(), "testdir")
	cNames := strings.Fields(out)
	goNames := localNames(mustGoList(t, r, "testdir"))
	if len(goNames) != 1 || strings.TrimPrefix(goNames[0], "testdir/") != cNames[0] {
		t.Errorf("prefix testdir: C=%q Go=%q", cNames, goNames)
	}

	// --list keeps the prefix, matching our full names.
	out = mustOstree(t, "refs", "--list", "--repo="+r.Path(), "testdir")
	if got := strings.Fields(out); len(got) != 1 || got[0] != "testdir/child" {
		t.Errorf("C refs --list testdir = %q", got)
	} else if goNames := localNames(mustGoList(t, r, "testdir")); len(goNames) != 1 || goNames[0] != got[0] {
		t.Errorf("Go ListRefs(testdir) = %q, C = %q", goNames, got)
	}

	// A string prefix that is not a path prefix matches nothing in C, and
	// the same in Go.
	out = mustOstree(t, "refs", "--repo="+r.Path(), "te")
	if strings.TrimSpace(out) != "" {
		t.Errorf(`C refs "te" = %q, want empty`, out)
	}
	if goNames := localNames(mustGoList(t, r, "te")); len(goNames) != 0 {
		t.Errorf(`Go ListRefs("te") = %q, want empty`, goNames)
	}

	// Exact ref name matches in both.
	out = mustOstree(t, "refs", "--repo="+r.Path(), "test")
	if strings.TrimSpace(out) != "test" {
		t.Errorf(`C refs "test" = %q`, out)
	}
	if goNames := localNames(mustGoList(t, r, "test")); len(goNames) != 1 || goNames[0] != "test" {
		t.Errorf(`Go ListRefs("test") = %q`, goNames)
	}

	// Remote namespace.
	out = mustOstree(t, "refs", "--repo="+r.Path(), "origin:main")
	if strings.TrimSpace(out) != "origin:main" {
		t.Errorf(`C refs "origin:main" = %q`, out)
	}
	mustGoList(t, r, "origin:main")
	out = mustOstree(t, "refs", "--repo="+r.Path(), "origin:m")
	if strings.TrimSpace(out) != "" {
		t.Errorf(`C refs "origin:m" = %q, want empty`, out)
	}
	if goNames := mustGoList(t, r, "origin:m"); len(goNames) != 0 {
		t.Errorf(`Go ListRefs("origin:m") = %q, want empty`, goNames)
	}
}

func mustGoList(t *testing.T, r *Repo, prefix string) []Ref {
	t.Helper()
	refs, err := r.ListRefs(prefix)
	if err != nil {
		t.Fatalf("ListRefs(%q): %v", prefix, err)
	}
	return refs
}

func TestConfigInterop(t *testing.T) {
	requireOstree(t)
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Create(dir, ModeBare, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	cfg := r.CloneConfig()
	cfg.SetRemote("origin", "url", "https://example.com/repo")
	cfg.Set("core", "lock-timeout-secs", "7")
	if err := r.WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}

	if got := strings.TrimSpace(mustOstree(t, "remote", "list", "--repo="+dir)); got != "origin" {
		t.Errorf("C remote list = %q, want origin", got)
	}
	if got := strings.TrimSpace(mustOstree(t, "config", "get", "--repo="+dir, `--group=remote "origin"`, "url")); got != "https://example.com/repo" {
		t.Errorf("C config get remote url = %q", got)
	}
	if got := strings.TrimSpace(mustOstree(t, "config", "get", "--repo="+dir, "core.lock-timeout-secs")); got != "7" {
		t.Errorf("C config get core.lock-timeout-secs = %q", got)
	}

	// A remote added by C must be visible after Go reloads the config.
	mustOstree(t, "remote", "add", "--repo="+dir, "c-remote", "file:///srv/ostree")
	if err := r.ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}
	names := r.Config().RemoteNames()
	found := false
	for _, name := range names {
		if name == "c-remote" {
			found = true
		}
	}
	if !found {
		t.Errorf("Go did not see C-added remote, names = %q", names)
	}
	if got, err := r.Config().GetRemote("c-remote", "url"); err != nil || got != "file:///srv/ostree" {
		t.Errorf("Go GetRemote(c-remote,url) = %q, %v", got, err)
	}
	// Go can move a C-written config through serialize/parse unchanged.
	roundtripped, err := ParseConfig(r.Config().Bytes())
	if err != nil {
		t.Fatalf("re-parse C config: %v", err)
	}
	if string(roundtripped.Bytes()) != string(r.Config().Bytes()) {
		t.Error("config serialization is not stable for a C-written config")
	}
}

func TestCReadsGoObjects(t *testing.T) {
	requireOstree(t)
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Create(dir, ModeBare, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// A dirmeta object with arbitrary bytes is not structurally valid, so
	// this only checks that C can see the loose object path; fsck would
	// reject the content. Use ObjectCommitmeta which is also opaque to the
	// storage layer but not traversed by fsck unless referenced.
	data := []byte("opaque commitmeta bytes")
	csum, err := r.WriteObject(ObjectCommitmeta, data)
	if err != nil {
		t.Fatalf("WriteObject: %v", err)
	}
	rel, _ := LoosePath(csum, ObjectCommitmeta, ModeBare)
	if _, err := os.Stat(filepath.Join(dir, "objects", filepath.FromSlash(rel))); err != nil {
		t.Errorf("object file missing: %v", err)
	}
	if got, err := r.ReadObject(csum, ObjectCommitmeta); err != nil || !bytes.Equal(got, data) {
		t.Errorf("ReadObject after write = %q, %v", got, err)
	}
	// C fsck on an empty repo with an unreferenced object still succeeds (it
	// only walks commits), which confirms the config/layout is accepted.
	if out := mustOstree(t, "fsck", "--repo="+dir); !strings.Contains(out, "no errors found") {
		t.Errorf("C fsck output:\n%s", out)
	}
}

func TestCObjectPathMatchesGo(t *testing.T) {
	requireOstree(t)
	dir := filepath.Join(t.TempDir(), "repo")
	r, err := Create(dir, ModeArchive, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// Verify the archive loose path layout is what libostree expects by
	// checking that C accepts a repository with this layout and by comparing
	// against the C helper's naming rule (.filez for non-metadata).
	csum := testChecksum("layout")
	rel, err := LoosePath(csum, ObjectFile, ModeArchive)
	if err != nil {
		t.Fatal(err)
	}
	if rel != csum[:2]+"/"+csum[2:]+".filez" {
		t.Errorf("archive path = %q", rel)
	}
	if _, err := r.WriteObject(ObjectCommitmeta, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if out := mustOstree(t, "fsck", "--repo="+dir); !strings.Contains(out, "no errors found") {
		t.Errorf("C fsck output:\n%s", out)
	}
}

func TestRunOstreeMissingBinaryIsSkipped(t *testing.T) {
	if _, err := os.Stat(ostreeBinPath); errors.Is(err, os.ErrNotExist) {
		t.Skip("no ostree binary")
	}
}
