package prune

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/myml/ostree-go/pkg/commit"
	"github.com/myml/ostree-go/pkg/repo"
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

func newRepo(t *testing.T, mode repo.Mode) *repo.Repo {
	t.Helper()
	r, err := repo.Create(filepath.Join(t.TempDir(), "repo"), mode, nil)
	if err != nil {
		t.Fatalf("Create(%v): %v", mode, err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
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
		Subject:      "prune test",
		Timestamp:    timestamp,
		TimestampSet: timestamp != 0,
	})
	if err != nil {
		t.Fatalf("commit.Create: %v", err)
	}
	return sum
}

func runPrune(t *testing.T, r *repo.Repo, opts Options) string {
	t.Helper()
	var out bytes.Buffer
	opts.Stdout = &out
	if err := Run(r, opts); err != nil {
		t.Fatalf("prune.Run: %v", err)
	}
	return out.String()
}

func objectFiles(t *testing.T, r *repo.Repo) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.Path(), "objects", "*", "*.*"))
	if err != nil {
		t.Fatal(err)
	}
	var base []string
	for _, f := range files {
		base = append(base, filepath.Base(f))
	}
	sort.Strings(base)
	return base
}

func countCommits(t *testing.T, r *repo.Repo) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(r.Path(), "objects", "*", "*.commit"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

// buildHistory creates a three-commit chain on "main" plus an orphan branch
// "extra" pointing at the first commit, with deterministic timestamps.
func buildHistory(t *testing.T, mode repo.Mode) *repo.Repo {
	t.Helper()
	r := newRepo(t, mode)
	tree1 := writeTree(t, map[string]string{"file": "version 1\n"})
	c1 := commitTree(t, r, tree1, "main", "", 1100000000)
	tree2 := writeTree(t, map[string]string{"file": "version 2\n"})
	c2 := commitTree(t, r, tree2, "main", c1, 1200000000)
	tree3 := writeTree(t, map[string]string{"file": "version 3\n"})
	commitTree(t, r, tree3, "main", c2, 1300000000)
	return r
}

func TestPruneNoPruneOutputMatchesC(t *testing.T) {
	requireCOstree(t)
	cases := []struct {
		name     string
		opts     Options
		args     []string
		wantPlan bool
	}{
		{"refs-only", Options{RefsOnly: true, Depth: -1, NoPrune: true}, []string{"--refs-only"}, false},
		{"refs-only-depth0", Options{RefsOnly: true, Depth: 0, NoPrune: true}, []string{"--refs-only", "--depth=0"}, true},
		{"all", Options{Depth: -1, NoPrune: true}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := buildHistory(t, repo.ModeBare)
			args := append([]string{"prune", "--repo=" + r.Path()}, tc.args...)
			args = append(args, "--no-prune")
			cOut, cErr, err := runC(t, args...)
			if err != nil {
				t.Fatalf("C prune: %v (%s)", err, cErr)
			}
			goOut := runPrune(t, r, tc.opts)
			if goOut != cOut {
				t.Errorf("prune output differs from C\nC:  %q\nGo: %q", cOut, goOut)
			}
			if tc.wantPlan != strings.Contains(goOut, "Would delete:") {
				t.Errorf("unexpected plan output:\n%s", goOut)
			}
		})
	}
}

func TestPruneRefsOnlyDepthZero(t *testing.T) {
	requireCOstree(t)
	r := buildHistory(t, repo.ModeBare)
	if got := countCommits(t, r); got != 3 {
		t.Fatalf("commits before prune = %d, want 3", got)
	}
	out := runPrune(t, r, Options{RefsOnly: true, Depth: 0})
	if !strings.Contains(out, "Total objects: ") || !strings.Contains(out, "Deleted ") {
		t.Fatalf("unexpected prune output:\n%s", out)
	}
	if got := countCommits(t, r); got != 1 {
		t.Errorf("commits after depth=0 prune = %d, want 1", got)
	}
	if cOut := mustC(t, "fsck", "--all", "--repo="+r.Path()); !strings.Contains(cOut, "no errors found") {
		t.Errorf("C fsck after prune:\n%s", cOut)
	}
}

func mustC(t *testing.T, args ...string) string {
	t.Helper()
	out, errOut, err := runC(t, args...)
	if err != nil {
		t.Fatalf("ostree %s: %v (%s)", strings.Join(args, " "), err, errOut)
	}
	return out
}

func TestPruneNoPruneKeepsObjects(t *testing.T) {
	r := buildHistory(t, repo.ModeBare)
	before := objectFiles(t, r)
	out := runPrune(t, r, Options{RefsOnly: true, Depth: 0, NoPrune: true})
	if !strings.Contains(out, "Would delete:") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	after := objectFiles(t, r)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Errorf("--no-prune changed objects:\nbefore=%v\nafter=%v", before, after)
	}
}

// buildHistoryAt builds a history on a repository at a fixed path.  Used for
// differential tests where C and Go operate on identical repositories.
func buildHistoryAt(t *testing.T, dir string) *repo.Repo {
	t.Helper()
	r, err := repo.Create(dir, repo.ModeBare, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	tree1 := writeTree(t, map[string]string{"file": "version 1\n"})
	c1 := commitTree(t, r, tree1, "main", "", 1100000000)
	tree2 := writeTree(t, map[string]string{"file": "version 2\n"})
	c2 := commitTree(t, r, tree2, "main", c1, 1200000000)
	tree3 := writeTree(t, map[string]string{"file": "version 3\n"})
	commitTree(t, r, tree3, "main", c2, 1300000000)
	return r
}

func TestPruneDifferentialWithC(t *testing.T) {
	requireCOstree(t)
	cases := []struct {
		name  string
		args  []string
		opts  Options
		check func(t *testing.T, cRepo, goRepo *repo.Repo)
	}{
		{
			name: "keep-younger-than",
			args: []string{"--keep-younger-than=2010-01-01"},
			opts: Options{Depth: -1, KeepYoungerThan: "2010-01-01"},
			check: func(t *testing.T, cRepo, goRepo *repo.Repo) {
				// The tip (2011) is kept; its parent (2008) is older than the
				// cutoff and stops the walk.
				if got := countCommits(t, goRepo); got != 1 {
					t.Errorf("commits = %d, want 1", got)
				}
			},
		},
		{
			name: "depth-1",
			args: []string{"--refs-only", "--depth=1"},
			opts: Options{Depth: 1, RefsOnly: true},
			check: func(t *testing.T, cRepo, goRepo *repo.Repo) {
				if got := countCommits(t, goRepo); got != 2 {
					t.Errorf("commits = %d, want 2", got)
				}
			},
		},
		{
			name: "retain-branch-depth",
			args: []string{"--retain-branch-depth=main=0"},
			opts: Options{Depth: -1, RetainBranchDepth: []string{"main=0"}},
			check: func(t *testing.T, cRepo, goRepo *repo.Repo) {
				// GINT_TO_POINTER(0) is NULL in the C implementation, so a
				// retained depth of 0 falls back to the global depth (-1).
				if got := countCommits(t, goRepo); got != 3 {
					t.Errorf("commits = %d, want 3", got)
				}
			},
		},
		{
			name: "only-branch",
			args: []string{"--only-branch=main", "--depth=0"},
			opts: Options{Depth: 0, OnlyBranch: []string{"main"}},
			check: func(t *testing.T, cRepo, goRepo *repo.Repo) {
				if got := countCommits(t, goRepo); got != 1 {
					t.Errorf("commits = %d, want 1", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cDir := filepath.Join(t.TempDir(), "c-repo")
			goDir := filepath.Join(t.TempDir(), "go-repo")
			cRepo := buildHistoryAt(t, cDir)
			goRepo := buildHistoryAt(t, goDir)

			cOut := mustC(t, append([]string{"prune", "--repo=" + cRepo.Path()}, tc.args...)...)
			goOut := runPrune(t, goRepo, tc.opts)
			if goOut != cOut {
				t.Errorf("prune output differs from C\nC:  %q\nGo: %q", cOut, goOut)
			}
			cObjects := strings.Join(objectFiles(t, cRepo), "\n")
			goObjects := strings.Join(objectFiles(t, goRepo), "\n")
			if goObjects != cObjects {
				t.Errorf("remaining objects differ from C\nC:\n%s\nGo:\n%s", cObjects, goObjects)
			}
			if rest := mustC(t, "fsck", "--all", "--repo="+goRepo.Path()); !strings.Contains(rest, "no errors found") {
				t.Errorf("C fsck after Go prune:\n%s", rest)
			}
			tc.check(t, cRepo, goRepo)
		})
	}
}

func TestPruneKeepUnreferencedYoungerThan(t *testing.T) {
	r := newRepo(t, repo.ModeBare)
	treeOld := writeTree(t, map[string]string{"old": "old\n"})
	old := commitTree(t, r, treeOld, "old-branch", "", 500000000) // 1985-11-05
	treeNew := writeTree(t, map[string]string{"new": "new\n"})
	newer := commitTree(t, r, treeNew, "new-branch", "", 1200000000) // 2008-01-10
	if err := r.DeleteRef("old-branch"); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteRef("new-branch"); err != nil {
		t.Fatal(err)
	}
	out := runPrune(t, r, Options{Depth: -1, KeepUnreferencedYoungerThan: "2000-01-01"})
	if !strings.Contains(out, "Total objects: ") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if ok, _ := r.HasObject(newer, repo.ObjectCommit); !ok {
		t.Error("unreferenced newer commit was pruned")
	}
	if ok, _ := r.HasObject(old, repo.ObjectCommit); ok {
		t.Error("unreferenced old commit was retained")
	}
}

func TestPruneInvalidDate(t *testing.T) {
	r := newRepo(t, repo.ModeBare)
	commitTree(t, r, writeTree(t, map[string]string{"f": "f"}), "main", "", 0)
	var out bytes.Buffer
	err := Run(r, Options{Depth: -1, KeepYoungerThan: "BACON", Stdout: &out})
	if err == nil || !strings.Contains(err.Error(), "Could not parse 'BACON'") {
		t.Fatalf("error = %v, want Could not parse 'BACON'", err)
	}
}

func TestPruneDeleteCommit(t *testing.T) {
	requireCOstree(t)
	r := newRepo(t, repo.ModeBare)
	tree := writeTree(t, map[string]string{"f": "f\n"})
	refCommit := commitTree(t, r, tree, "main", "", 1000)

	// A commit still referenced by a ref must not be deleted.
	err := Run(r, Options{Depth: -1, DeleteCommit: refCommit, Stdout: &bytes.Buffer{}})
	if err == nil || !strings.Contains(err.Error(), "is referenced by 'main'") {
		t.Fatalf("delete referenced commit error = %v", err)
	}

	// Orphan commits can be deleted.
	orphanTree := writeTree(t, map[string]string{"g": "g\n"})
	orphan, err := commit.Create(commit.Options{
		Repo: r, TreeDir: orphanTree, Orphan: true, Subject: "orphan", Timestamp: 2000, TimestampSet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	out := runPrune(t, r, Options{Depth: -1, DeleteCommit: orphan})
	if !strings.Contains(out, "Total objects: ") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	if ok, _ := r.HasObject(orphan, repo.ObjectCommit); ok {
		t.Error("orphan commit still present after --delete-commit")
	}
	if got, err := r.Config().Get("core", "tombstone-commits"); err != nil || got != "true" {
		t.Errorf("tombstone-commits = %q, %v; want true", got, err)
	}
	// The orphan's tree objects are unreachable and pruned too.
	if rest := mustC(t, "fsck", "--all", "--repo="+r.Path()); !strings.Contains(rest, "no errors found") {
		t.Errorf("C fsck after delete-commit:\n%s", rest)
	}
}

func TestPruneCollectionRefs(t *testing.T) {
	r := newRepo(t, repo.ModeBare)
	tree := writeTree(t, map[string]string{"unique": "unique\n"})
	csum := commitTree(t, r, tree, "original", "", 1000)
	if err := r.WriteMirrorRef("org.example.Collection", "some-ref", csum); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteRef("original"); err != nil {
		t.Fatal(err)
	}
	out := runPrune(t, r, Options{RefsOnly: true, Depth: -1})
	if !strings.Contains(out, "Total objects: 4") || !strings.Contains(out, "No unreachable objects") {
		t.Fatalf("collection ref was not retained:\n%s", out)
	}
	// The manual reachability path (triggered by
	// --keep-unreferenced-younger-than) must also keep objects reachable only
	// through a collection ref.
	out = runPrune(t, r, Options{Depth: -1, KeepUnreferencedYoungerThan: "2000-01-01"})
	if !strings.Contains(out, "No unreachable objects") {
		t.Fatalf("collection ref lost in the manual reachability path:\n%s", out)
	}
	// Deleting the collection ref makes every object unreachable.
	rel := filepath.Join(r.Path(), "refs", "mirrors", "org.example.Collection", "some-ref")
	if err := os.Remove(rel); err != nil {
		t.Fatal(err)
	}
	out = runPrune(t, r, Options{RefsOnly: true, Depth: -1})
	for _, pattern := range []string{`(?m)^Total objects: 4$`, `(?m)^Deleted 4 objects, [0-9]+ bytes freed$`} {
		if !regexp.MustCompile(pattern).MatchString(out) {
			t.Fatalf("output does not match %s:\n%s", pattern, out)
		}
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		size uint64
		want string
	}{
		{0, "0 bytes"},
		{173, "173 bytes"},
		{5200, "5.2\u00a0kB"},
		{1500000, "1.5\u00a0MB"},
	}
	for _, tc := range tests {
		if got := formatSize(tc.size); got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.size, got, tc.want)
		}
	}
}
