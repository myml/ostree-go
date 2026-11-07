package object

import (
	"bytes"
	"compress/flate"
	"io"
	"os"
	"testing"

	"github.com/myml/ostree-go/pkg/checksum"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDirmeta(t *testing.T) {
	data := readFixture(t, "dirmeta.bin")
	d, err := ParseDirmeta(data)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode != 040755 || d.UID != 0 || d.GID != 0 {
		t.Fatalf("unexpected dirmeta: %+v", d)
	}
	out, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("dirmeta round-trip differs")
	}
	if got := checksum.Hex(checksum.Sum(data)); got != "446a0ef11b7cc167f3b603e585c7eeeeb675faa412d5ec73f62988eb0b6c5488" {
		t.Fatalf("unexpected checksum %s", got)
	}
}

func TestDirtree(t *testing.T) {
	data := readFixture(t, "dirtree-root.bin")
	d, err := ParseDirtree(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Files) != 1 || d.Files[0].Name != "a.txt" {
		t.Fatalf("unexpected files: %+v", d.Files)
	}
	if len(d.Dirs) != 1 || d.Dirs[0].Name != "sub" {
		t.Fatalf("unexpected dirs: %+v", d.Dirs)
	}
	out, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("dirtree round-trip differs")
	}
}

func TestCommit(t *testing.T) {
	data := readFixture(t, "commit.bin")
	c, err := ParseCommit(data)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != "subj" || c.Body != "body" || c.Timestamp != 0 {
		t.Fatalf("unexpected commit: %+v", c)
	}
	if c.Parent != "" {
		t.Fatalf("unexpected parent %q", c.Parent)
	}
	v, ok := c.GetMetadata("ostree.ref-binding")
	if !ok {
		t.Fatal("missing ref binding")
	}
	_ = v
	out, err := c.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("commit round-trip differs")
	}
	sum, err := c.Checksum()
	if err != nil {
		t.Fatal(err)
	}
	if sum != "9b5d3fcd8cd1d92206f1858169dec6c87cf6f8fde842921828a01a66e021441f" {
		t.Fatalf("unexpected commit checksum %s", sum)
	}
	content, err := c.ContentChecksum()
	if err != nil {
		t.Fatal(err)
	}
	if len(content) != 64 {
		t.Fatalf("unexpected content checksum %q", content)
	}
}

func TestFileHeaderRoundTrip(t *testing.T) {
	h := &FileHeader{Size: 6, UID: 0, GID: 0, Mode: 0100644}
	stream, err := h.EncodeStream()
	if err != nil {
		t.Fatal(err)
	}
	got, rest, err := ParseStreamHeader(stream)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || got.Mode != 0100644 || got.UID != 0 {
		t.Fatalf("unexpected parsed header: %+v", got)
	}
	arch, err := h.EncodeArchive()
	if err != nil {
		t.Fatal(err)
	}
	got, rest, err = ParseArchiveHeader(arch)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 0 || got.Size != 6 || got.Mode != 0100644 {
		t.Fatalf("unexpected parsed archive header: %+v", got)
	}
}

func TestFileChecksum(t *testing.T) {
	// The file object for "hi\n" in the probe bare repo has this name.
	h := &FileHeader{UID: 0, GID: 0, Mode: 0100644}
	sum, err := h.Checksum([]byte("hi\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sum != "fbf7cf77a1b50d9f074cb6e9ef6ddb65b6532ea89e4f4ca1b07a05b4cd1be259" {
		t.Fatalf("unexpected file checksum %s", sum)
	}
}

func TestSummary(t *testing.T) {
	data := readFixture(t, "summary.bin")
	s, err := ParseSummary(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Refs) != 1 || s.Refs[0].Name != "test" {
		t.Fatalf("unexpected refs: %+v", s.Refs)
	}
	if s.Refs[0].Commit != "9b5d3fcd8cd1d92206f1858169dec6c87cf6f8fde842921828a01a66e021441f" {
		t.Fatalf("unexpected commit: %s", s.Refs[0].Commit)
	}
	out, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("summary round-trip differs")
	}
}

func TestArchiveFile(t *testing.T) {
	data := readFixture(t, "filez.bin")
	h, rest, err := ParseArchiveHeader(data)
	if err != nil {
		t.Fatal(err)
	}
	if h.Size != 6 || h.Mode != 0100644 || h.UID != 0 || h.GID != 0 {
		t.Fatalf("unexpected header: %+v", h)
	}
	if !h.IsRegular() {
		t.Fatal("expected regular file")
	}
	r := flate.NewReader(bytes.NewReader(rest))
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello\n" {
		t.Fatalf("unexpected content %q", content)
	}
}
