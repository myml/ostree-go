package repo

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeObjectChecksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestObjectTypeTable(t *testing.T) {
	tests := []struct {
		typ    ObjectType
		suffix string
		meta   bool
	}{
		{ObjectFile, "file", false},
		{ObjectDirtree, "dirtree", true},
		{ObjectDirmeta, "dirmeta", true},
		{ObjectCommit, "commit", true},
		{ObjectTombstoneCommit, "tombstone-commit", true},
		{ObjectCommitmeta, "commitmeta", true},
		{ObjectPayloadLink, "payload-link", false},
		{ObjectFileXattrs, "file-xattrs", false},
		{ObjectFileXattrsLink, "file-xattrs-link", false},
	}
	for _, tc := range tests {
		if got := tc.typ.String(); got != tc.suffix {
			t.Errorf("%d.String() = %q, want %q", int(tc.typ), got, tc.suffix)
		}
		if got := tc.typ.IsMeta(); got != tc.meta {
			t.Errorf("%s.IsMeta() = %v, want %v", tc.typ, got, tc.meta)
		}
		if !tc.typ.Valid() {
			t.Errorf("%s.Valid() = false", tc.typ)
		}
		back, err := ParseObjectType(tc.suffix)
		if err != nil || back != tc.typ {
			t.Errorf("ParseObjectType(%q) = %v, %v; want %v", tc.suffix, back, err, tc.typ)
		}
	}
	if ObjectType(0).Valid() || ObjectType(10).Valid() {
		t.Error("out-of-range object types should be invalid")
	}
	if _, err := ParseObjectType("bogus"); !errors.Is(err, ErrInvalidObjectType) {
		t.Errorf("ParseObjectType(bogus) error = %v", err)
	}
	// The metadata range is exactly 2..6, as in OSTREE_OBJECT_TYPE_IS_META.
	for i := 1; i <= 9; i++ {
		want := i >= 2 && i <= 6
		if got := ObjectType(i).IsMeta(); got != want {
			t.Errorf("type %d IsMeta() = %v, want %v", i, got, want)
		}
	}
}

func TestLoosePath(t *testing.T) {
	checksum := "ab" + strings.Repeat("cd", 31)
	rest := checksum[2:]
	tests := []struct {
		mode   Mode
		typ    ObjectType
		suffix string
	}{
		{ModeBare, ObjectFile, ".file"},
		{ModeBare, ObjectDirtree, ".dirtree"},
		{ModeBare, ObjectDirmeta, ".dirmeta"},
		{ModeBare, ObjectCommit, ".commit"},
		{ModeBare, ObjectTombstoneCommit, ".tombstone-commit"},
		{ModeBare, ObjectCommitmeta, ".commitmeta"},
		{ModeBare, ObjectPayloadLink, ".payload-link"},
		{ModeBare, ObjectFileXattrs, ".file-xattrs"},
		{ModeBare, ObjectFileXattrsLink, ".file-xattrs-link"},
		{ModeBareUser, ObjectFile, ".file"},
		{ModeBareUserOnly, ObjectFile, ".file"},
		{ModeBareSplitXattrs, ObjectFile, ".file"},
		{ModeArchive, ObjectFile, ".filez"},
		{ModeArchiveZ2, ObjectFile, ".filez"},
		{ModeArchive, ObjectDirtree, ".dirtree"},
		{ModeArchive, ObjectDirmeta, ".dirmeta"},
		{ModeArchive, ObjectCommit, ".commit"},
		{ModeArchive, ObjectTombstoneCommit, ".tombstone-commit"},
		{ModeArchive, ObjectCommitmeta, ".commitmeta"},
		{ModeArchive, ObjectPayloadLink, ".payload-linkz"},
		{ModeArchive, ObjectFileXattrs, ".file-xattrsz"},
		{ModeArchive, ObjectFileXattrsLink, ".file-xattrs-linkz"},
	}
	for _, tc := range tests {
		got, err := LoosePath(checksum, tc.typ, tc.mode)
		if err != nil {
			t.Errorf("LoosePath(%v, %v): %v", tc.mode, tc.typ, err)
			continue
		}
		want := "ab/" + rest + tc.suffix
		if got != want {
			t.Errorf("LoosePath(%v, %v) = %q, want %q", tc.mode, tc.typ, got, want)
		}
	}
	for _, tc := range []struct {
		csum string
		typ  ObjectType
		mode Mode
	}{
		{"short", ObjectFile, ModeBare},
		{strings.ToUpper(checksum), ObjectFile, ModeBare},
		{checksum + "a", ObjectFile, ModeBare},
		{checksum, ObjectType(0), ModeBare},
		{checksum, ObjectType(10), ModeBare},
		{checksum, ObjectFile, Mode(9)},
	} {
		if _, err := LoosePath(tc.csum, tc.typ, tc.mode); err == nil {
			t.Errorf("LoosePath(%q, %v, %v) unexpectedly succeeded", tc.csum, tc.typ, tc.mode)
		}
	}
}

func TestWriteReadObjectAllModes(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	for _, mode := range []Mode{ModeBare, ModeBareUser, ModeBareUserOnly, ModeArchive, ModeBareSplitXattrs} {
		t.Run(mode.String(), func(t *testing.T) {
			r := newTestRepo(t, mode)
			checksum, err := r.WriteObject(ObjectDirmeta, data)
			if err != nil {
				t.Fatalf("WriteObject: %v", err)
			}
			if want := writeObjectChecksum(data); checksum != want {
				t.Errorf("checksum = %s, want %s", checksum, want)
			}
			rel, err := LoosePath(checksum, ObjectDirmeta, mode)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(r.Path(), "objects", filepath.FromSlash(rel))); err != nil {
				t.Errorf("object file missing: %v", err)
			}
			got, err := r.ReadObject(checksum, ObjectDirmeta)
			if err != nil {
				t.Fatalf("ReadObject: %v", err)
			}
			if !bytes.Equal(got, data) {
				t.Error("ReadObject returned different bytes")
			}
			ok, err := r.HasObject(checksum, ObjectDirmeta)
			if err != nil || !ok {
				t.Errorf("HasObject = %v, %v", ok, err)
			}
			// Staging files must not be left behind.
			entries, err := os.ReadDir(filepath.Join(r.Path(), "tmp"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("tmp/ has leftover entries: %v", entries)
			}
			// Reopen and read again.
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			r2, err := Open(r.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer r2.Close()
			got, err = r2.ReadObject(checksum, ObjectDirmeta)
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("ReadObject after reopen = %d bytes, %v", len(got), err)
			}
		})
	}
}

func TestArchiveFileObjectsAreRawDeflate(t *testing.T) {
	payload := []byte("(uuuusa(ayay)) file header plus content")
	r := newTestRepo(t, ModeArchive)
	checksum, err := r.WriteObject(ObjectFile, payload)
	if err != nil {
		t.Fatal(err)
	}
	if want := writeObjectChecksum(payload); checksum != want {
		t.Errorf("checksum = %s, want sha256 of uncompressed input %s", checksum, want)
	}
	rel, _ := LoosePath(checksum, ObjectFile, ModeArchive)
	if !strings.HasSuffix(rel, ".filez") {
		t.Fatalf("archive file object path = %q", rel)
	}
	raw, err := r.ReadObject(checksum, ObjectFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, payload) {
		t.Error("ReadObject must return the raw .filez bytes, not decompressed data")
	}
	zr := flate.NewReader(bytes.NewReader(raw))
	defer zr.Close()
	decoded, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(decoded, payload) {
		t.Error("decompressed payload mismatch")
	}
	// The uncompressed name must not exist for a plain archive repo.
	uncompressed, _ := LoosePath(checksum, ObjectFile, ModeBare)
	if _, err := os.Stat(filepath.Join(r.Path(), "objects", filepath.FromSlash(uncompressed))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unexpected uncompressed object, stat err = %v", err)
	}
}

func TestBareFileObjectsAreVerbatim(t *testing.T) {
	payload := []byte("file object bytes")
	r := newTestRepo(t, ModeBare)
	checksum, err := r.WriteObject(ObjectFile, payload)
	if err != nil {
		t.Fatal(err)
	}
	rel, _ := LoosePath(checksum, ObjectFile, ModeBare)
	if !strings.HasSuffix(rel, ".file") {
		t.Fatalf("bare file object path = %q", rel)
	}
	got, err := r.ReadObject(checksum, ObjectFile)
	if err != nil || !bytes.Equal(got, payload) {
		t.Errorf("ReadObject = %q, %v", got, err)
	}
}

func TestReadObjectMissingAndInvalid(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	missing := testChecksum("missing")
	if _, err := r.ReadObject(missing, ObjectDirmeta); !errors.Is(err, ErrObjectNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadObject(missing) error = %v, want ErrObjectNotFound and fs.ErrNotExist", err)
	}
	if ok, err := r.HasObject(missing, ObjectDirmeta); err != nil || ok {
		t.Errorf("HasObject(missing) = %v, %v", ok, err)
	}
	if _, err := r.ReadObject("BAD", ObjectDirmeta); !errors.Is(err, ErrInvalidChecksum) {
		t.Errorf("ReadObject(bad checksum) error = %v", err)
	}
	if _, err := r.ReadObject(strings.ToUpper(testChecksum("x")), ObjectDirmeta); !errors.Is(err, ErrInvalidChecksum) {
		t.Errorf("ReadObject(uppercase) error = %v", err)
	}
	if _, err := r.ReadObject(missing, ObjectType(0)); !errors.Is(err, ErrInvalidObjectType) {
		t.Errorf("ReadObject(bad type) error = %v", err)
	}
	if _, err := r.WriteObject(ObjectType(0), []byte("x")); !errors.Is(err, ErrInvalidObjectType) {
		t.Errorf("WriteObject(bad type) error = %v", err)
	}
}

func TestWriteObjectIsIdempotent(t *testing.T) {
	r := newTestRepo(t, ModeBare)
	data := []byte("same bytes")
	c1, err := r.WriteObject(ObjectCommit, data)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.WriteObject(ObjectCommit, data)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Errorf("checksums differ: %s vs %s", c1, c2)
	}
	got, err := r.ReadObject(c1, ObjectCommit)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("ReadObject = %q, %v", got, err)
	}
}

func TestObjectDirMode(t *testing.T) {
	withUmask(t, 0)
	r := newTestRepo(t, ModeBare)
	checksum, err := r.WriteObject(ObjectDirmeta, []byte("meta"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.Path(), "objects", checksum[:2])
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o777 {
		t.Errorf("objects/%s perm = %o, want 0777 (umask 0)", checksum[:2], perm)
	}
}

func TestObjectPathHelpers(t *testing.T) {
	r := newTestRepo(t, ModeArchive)
	checksum := testChecksum("obj")
	rel, err := r.ObjectRelPath(checksum, ObjectFile)
	if err != nil {
		t.Fatal(err)
	}
	wantRel := "objects/" + checksum[:2] + "/" + checksum[2:] + ".filez"
	if rel != wantRel {
		t.Errorf("ObjectRelPath = %q, want %q", rel, wantRel)
	}
	abs, err := r.ObjectPath(checksum, ObjectFile)
	if err != nil {
		t.Fatal(err)
	}
	if abs != filepath.Join(r.Path(), filepath.FromSlash(wantRel)) {
		t.Errorf("ObjectPath = %q", abs)
	}
}

func TestObjectValidationHelpers(t *testing.T) {
	if err := ValidateChecksum(strings.Repeat("a", 64)); err != nil {
		t.Errorf("valid checksum rejected: %v", err)
	}
	for _, bad := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		if err := ValidateChecksum(bad); !errors.Is(err, ErrInvalidChecksum) {
			t.Errorf("ValidateChecksum(%q) = %v, want ErrInvalidChecksum", bad, err)
		}
	}
	if !IsChecksum(strings.Repeat("f", 64)) || IsChecksum("xyz") {
		t.Error("IsChecksum mismatch")
	}
	if !reflect.DeepEqual(createDirs, []string{"tmp", "extensions", "state", "refs", "refs/heads", "refs/mirrors", "refs/remotes", "objects"}) {
		t.Errorf("createDirs = %v", createDirs)
	}
}
