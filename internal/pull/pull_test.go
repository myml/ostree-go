package pull

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ostree-go/internal/commit"
	"ostree-go/internal/repo"
)

const cOstree = "/usr/bin/ostree"

func requireCOstree(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(cOstree); err != nil {
		t.Skipf("%s not available", cOstree)
	}
}

func runC(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(cOstree, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func mustC(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, err := runC(t, args...)
	if err != nil {
		t.Fatalf("ostree %s: %v\nstderr: %s", strings.Join(args, " "), err, stderr)
	}
	return stdout
}

func newRepo(t *testing.T, mode repo.Mode) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "repo"), mode, nil)
	if err != nil {
		t.Fatalf("Create(%v): %v", mode, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// writeSourceTree creates a small tree with a regular file, a subdirectory and
// a symlink.
func writeSourceTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello ostree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "nested"), []byte("nested\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("hello.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func commitTree(t *testing.T, r *repo.Repo, dir, branch, parent string, timestamp uint64) string {
	t.Helper()
	sum, err := commit.Create(commit.Options{
		Repo:         r,
		TreeDir:      dir,
		Branch:       branch,
		Parent:       parent,
		Subject:      "test commit",
		Timestamp:    timestamp,
		TimestampSet: timestamp != 0,
	})
	if err != nil {
		t.Fatalf("commit.Create: %v", err)
	}
	return sum
}

func TestLocalArchiveRoundTrip(t *testing.T) {
	requireCOstree(t)
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	csum := commitTree(t, src, tree, "main", "", 0)

	dst := newRepo(t, repo.ModeArchive)
	if err := Local(dst, src.Path(), Options{}); err != nil {
		t.Fatalf("Local: %v", err)
	}
	got, err := dst.ReadRef("main")
	if err != nil || got != csum {
		t.Fatalf("destination ref = %q, %v; want %q", got, err, csum)
	}
	if out := mustC(t, "fsck", "--all", "--repo="+dst.Path()); !strings.Contains(out, "no errors found") {
		t.Fatalf("C fsck output:\n%s", out)
	}
	checkout := filepath.Join(t.TempDir(), "checkout")
	mustC(t, "--repo="+dst.Path(), "checkout", "main", checkout)
	data, err := os.ReadFile(filepath.Join(checkout, "hello.txt"))
	if err != nil || string(data) != "hello ostree\n" {
		t.Fatalf("checked out hello.txt = %q, %v", data, err)
	}
	if target, err := os.Readlink(filepath.Join(checkout, "link")); err != nil || target != "hello.txt" {
		t.Fatalf("checked out link = %q, %v", target, err)
	}
}

func TestLocalAllSourceModes(t *testing.T) {
	requireCOstree(t)
	tree := writeSourceTree(t)
	modes := []repo.Mode{repo.ModeBare, repo.ModeBareUser, repo.ModeBareUserOnly, repo.ModeArchive}
	for _, srcMode := range modes {
		for _, dstMode := range modes {
			name := srcMode.String() + "-to-" + dstMode.String()
			t.Run(name, func(t *testing.T) {
				if os.Geteuid() != 0 && (srcMode == repo.ModeBareUserOnly || dstMode == repo.ModeBareUserOnly) {
					t.Skip("bare-user-only canonicalizes uid/gid to 0; requires root-owned test trees")
				}
				src := newRepo(t, srcMode)
				commitTree(t, src, tree, "main", "", 0)

				dst := newRepo(t, dstMode)
				if err := Local(dst, src.Path(), Options{}); err != nil {
					t.Fatalf("Local: %v", err)
				}
				if out := mustC(t, "fsck", "--all", "--repo="+dst.Path()); !strings.Contains(out, "no errors found") {
					t.Fatalf("C fsck output after pull:\n%s", out)
				}
				checkout := filepath.Join(t.TempDir(), "checkout")
				mustC(t, "--repo="+dst.Path(), "checkout", "main", checkout)
				if data, err := os.ReadFile(filepath.Join(checkout, "hello.txt")); err != nil || string(data) != "hello ostree\n" {
					t.Fatalf("hello.txt = %q, %v", data, err)
				}
				if target, err := os.Readlink(filepath.Join(checkout, "link")); err != nil || target != "hello.txt" {
					t.Fatalf("link = %q, %v", target, err)
				}
			})
		}
	}
}

func TestLocalRepairsPartialCommit(t *testing.T) {
	requireCOstree(t)
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	csum := commitTree(t, src, tree, "main", "", 0)

	dst := newRepo(t, repo.ModeArchive)
	// Fetch metadata only: the commit must be marked partial.
	if err := Local(dst, src.Path(), Options{CommitMetadataOnly: true}); err != nil {
		t.Fatalf("Local(commit-metadata-only): %v", err)
	}
	marker := filepath.Join(dst.Path(), "state", csum+".commitpartial")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("partial marker missing: %v", err)
	}
	// A full pull must copy the tree and clear the marker.
	if err := Local(dst, src.Path(), Options{}); err != nil {
		t.Fatalf("Local(full): %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("partial marker still present (err=%v)", err)
	}
	if out := mustC(t, "fsck", "--all", "--repo="+dst.Path()); !strings.Contains(out, "no errors found") {
		t.Fatalf("C fsck output:\n%s", out)
	}
}

func TestLocalDepth(t *testing.T) {
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	c1 := commitTree(t, src, tree, "main", "", 1000)
	c2 := commitTree(t, src, tree, "main", c1, 2000)
	c3 := commitTree(t, src, tree, "main", c2, 3000)

	for _, tc := range []struct {
		depth int
		want  int
	}{
		{0, 1},
		{1, 2},
		{-1, 3},
	} {
		dst := newRepo(t, repo.ModeArchive)
		if err := Local(dst, src.Path(), Options{Depth: tc.depth}); err != nil {
			t.Fatalf("Local(depth=%d): %v", tc.depth, err)
		}
		commits, err := filepath.Glob(filepath.Join(dst.Path(), "objects", "*", "*.commit"))
		if err != nil {
			t.Fatal(err)
		}
		if len(commits) != tc.want {
			t.Errorf("depth=%d: %d commit objects, want %d", tc.depth, len(commits), tc.want)
		}
		if got, _ := dst.ReadRef("main"); got != c3 {
			t.Errorf("depth=%d: ref = %q, want %q", tc.depth, got, c3)
		}
	}
}

func TestLocalExplicitRefsRemoteAndCommitmeta(t *testing.T) {
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	csum := commitTree(t, src, tree, "main", "", 0)
	if err := src.WriteRemoteRef("origin", "remote-branch", csum); err != nil {
		t.Fatal(err)
	}
	// Detached metadata is copied verbatim when present.
	meta := []byte{0x00, 0x01, 0x02, 0x03}
	if err := src.WriteRawObject(csum, repo.ObjectCommitmeta, meta); err != nil {
		t.Fatal(err)
	}

	// With no explicit refspec, pull-local enumerates refs/heads and rejects
	// remote refs with libostree's "Invalid ref name" error; remote refs can
	// still be copied by naming them explicitly.
	dst := newRepo(t, repo.ModeArchive)
	if err := Local(dst, src.Path(), Options{}); err == nil ||
		!strings.Contains(err.Error(), "Invalid ref name origin:remote-branch") {
		t.Fatalf("Local(no refs) = %v, want Invalid ref name", err)
	}
	// The commit above is bound to ref "main", so binding verification must be
	// disabled when copying it under a different ref name.
	if err := Local(dst, src.Path(), Options{Refs: []string{"origin:remote-branch"}, DisableVerifyBindings: true}); err != nil {
		t.Fatalf("Local(explicit remote ref): %v", err)
	}
	if got, err := dst.ReadRemoteRef("origin", "remote-branch"); err != nil || got != csum {
		t.Fatalf("remote ref = %q, %v; want %q", got, err, csum)
	}
	got, err := dst.ReadObject(csum, repo.ObjectCommitmeta)
	if err != nil || !bytes.Equal(got, meta) {
		t.Fatalf("commitmeta = %x, %v; want %x", got, err, meta)
	}
}

func TestLocalExplicitRefAndRemoteOverride(t *testing.T) {
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	csum := commitTree(t, src, tree, "main", "", 0)

	dst := newRepo(t, repo.ModeArchive)
	if err := Local(dst, src.Path(), Options{Refs: []string{"main"}, Remote: "upstream"}); err != nil {
		t.Fatalf("Local: %v", err)
	}
	if got, err := dst.ReadRemoteRef("upstream", "main"); err != nil || got != csum {
		t.Fatalf("remote override ref = %q, %v; want %q", got, err, csum)
	}
	if _, err := dst.ReadRef("main"); err == nil {
		t.Error("refs/heads/main must not be written with --remote")
	}

	// A checksum fetches the commit without writing a ref.
	dst2 := newRepo(t, repo.ModeArchive)
	if err := Local(dst2, src.Path(), Options{Refs: []string{csum}}); err != nil {
		t.Fatalf("Local(checksum): %v", err)
	}
	if ok, err := dst2.HasObject(csum, repo.ObjectCommit); !ok || err != nil {
		t.Fatalf("commit not copied: %v, %v", ok, err)
	}
	refs, err := dst2.ListRefs("")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("checksum pull wrote refs: %+v", refs)
	}
}

func TestLocalRejectsCorruptSource(t *testing.T) {
	src := newRepo(t, repo.ModeArchive)
	tree := writeSourceTree(t)
	commitTree(t, src, tree, "main", "", 0)

	// Corrupt one compressed file object in the source.
	matches, err := filepath.Glob(filepath.Join(src.Path(), "objects", "*", "*.filez"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no filez objects: %v", err)
	}
	if err := os.WriteFile(matches[0], []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := newRepo(t, repo.ModeArchive)
	err = Local(dst, src.Path(), Options{})
	if err == nil || !strings.Contains(err.Error(), "content object") {
		t.Fatalf("Local on corrupt source = %v, want content object error", err)
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		size uint64
		want string
	}{
		{0, "0 bytes"},
		{173, "173 bytes"},
		{999, "999 bytes"},
		{5200, "5.2\u00a0kB"},
		{1500000, "1.5\u00a0MB"},
	}
	for _, tc := range tests {
		if got := formatSize(tc.size); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}
