package gvariant

import (
	"bytes"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEncodeVectors(t *testing.T) {
	u32 := MustParseType("u")
	cases := []struct {
		name string
		typ  string
		val  Value
		hex  string
	}{
		{"array-of-strings", "as", []Value{"a", "bb", "ccc"}, "610062620063636300020509"},
		{"tuple-strings", "(sss)", []Value{"a", "bb", "ccc"}, "6100626200636363000502"},
		{"dict-entry", "{sv}", DictEntry{Key: "k", Value: Variant{Type: u32, Value: uint32(7)}}, "6b0000000000000007000000007502"},
		{"dict", "a{sv}", Dict{{Key: "k", Value: Variant{Type: u32, Value: uint32(7)}}}, "6b00000000000000070000000075020f"},
		{"variant", "v", Variant{Type: u32, Value: uint32(7)}, "070000000075"},
		{"bytes", "ay", []byte("abc"), "616263"},
		{"empty-array", "as", []Value{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ := MustParseType(tc.typ)
			got, err := Encode(typ, tc.val)
			if err != nil {
				t.Fatal(err)
			}
			want := mustHex(t, tc.hex)
			if !bytes.Equal(got, want) {
				t.Fatalf("encode: got %x want %x", got, want)
			}
			dec, err := Decode(typ, got)
			if err != nil {
				t.Fatal(err)
			}
			again, err := Encode(typ, dec)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, got) {
				t.Fatalf("round trip: got %x want %x", again, got)
			}
			if !IsNormalForm(typ, got) {
				t.Fatal("encoded value is not normal form")
			}
		})
	}
}

func TestTupleSay(t *testing.T) {
	typ := MustParseType("(say)")
	csum := bytes.Repeat([]byte{1}, 32)
	val := []Value{"a.txt", csum}
	b, err := Encode(typ, val)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString("612e74787400" + strings.Repeat("01", 32) + "06")
	if !bytes.Equal(b, want) {
		t.Fatalf("got %x want %x", b, want)
	}
	dec, err := Decode(typ, b)
	if err != nil {
		t.Fatal(err)
	}
	got := dec.([]Value)
	if got[0].(string) != "a.txt" || !bytes.Equal(got[1].([]byte), csum) {
		t.Fatalf("unexpected decode: %#v", got)
	}
}

func TestWideOffsets(t *testing.T) {
	typ := MustParseType("as")
	val := []Value{strings.Repeat("a", 300), "b"}
	b, err := Encode(typ, val)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 307 {
		t.Fatalf("unexpected size %d", len(b))
	}
	dec, err := Decode(typ, b)
	if err != nil {
		t.Fatal(err)
	}
	got := dec.([]Value)
	if got[0].(string) != strings.Repeat("a", 300) || got[1].(string) != "b" {
		t.Fatal("wrong decode")
	}
}

func TestRealCommit(t *testing.T) {
	data, err := os.ReadFile("testdata/real/commit.bin")
	if err != nil {
		t.Fatal(err)
	}
	typ := MustParseType("(a{sv}aya(say)sstayay)")
	dec, err := Decode(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	fields := dec.([]Value)
	meta := fields[0].(Dict)
	v, ok := meta.Get("ostree.ref-binding")
	if !ok {
		t.Fatalf("missing ref-binding in %#v", meta)
	}
	rv := v.(Variant)
	if rv.Type.String() != "as" {
		t.Fatalf("unexpected ref-binding type %s", rv.Type)
	}
	if len(rv.Value.([]Value)) != 1 || rv.Value.([]Value)[0].(string) != "test" {
		t.Fatalf("unexpected ref-binding %#v", rv.Value)
	}
	if fields[3].(string) != "subj" || fields[4].(string) != "body" {
		t.Fatalf("unexpected subject/body")
	}
	if fields[5].(uint64) != 0 {
		t.Fatalf("unexpected timestamp")
	}
	out, err := Encode(typ, dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("commit round-trip differs")
	}
}

func TestRealDirmeta(t *testing.T) {
	data, err := os.ReadFile("testdata/real/dirmeta.bin")
	if err != nil {
		t.Fatal(err)
	}
	typ := MustParseType("(uuua(ayay))")
	dec, err := Decode(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	fields := dec.([]Value)
	// OSTree stores these fields big-endian; the raw uint32 is 0xed410000.
	if fields[0].(uint32) != 0 || fields[1].(uint32) != 0 {
		t.Fatalf("unexpected uid/gid")
	}
	out, err := Encode(typ, dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("dirmeta round-trip differs")
	}
}

func TestRealDirtree(t *testing.T) {
	data, err := os.ReadFile("testdata/real/dirtree-root.bin")
	if err != nil {
		t.Fatal(err)
	}
	typ := MustParseType("(a(say)a(sayay))")
	dec, err := Decode(typ, data)
	if err != nil {
		t.Fatal(err)
	}
	fields := dec.([]Value)
	files := fields[0].([]Value)
	dirs := fields[1].([]Value)
	if len(files) != 1 || len(dirs) != 1 {
		t.Fatalf("unexpected dirtree shape: %d files %d dirs", len(files), len(dirs))
	}
	f := files[0].([]Value)
	if f[0].(string) != "a.txt" || len(f[1].([]byte)) != 32 {
		t.Fatalf("unexpected file entry %#v", f)
	}
	d := dirs[0].([]Value)
	if d[0].(string) != "sub" || len(d[1].([]byte)) != 32 || len(d[2].([]byte)) != 32 {
		t.Fatalf("unexpected dir entry %#v", d)
	}
	out, err := Encode(typ, dec)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, data) {
		t.Fatalf("dirtree round-trip differs")
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	cases := []struct {
		typ string
		hex string
	}{
		{"b", "02000000"},
		{"s", "6100 00"},
		{"as", "6100"},
		{"(sss)", "6100626200636363000502ff"},
	}
	for _, tc := range cases {
		_, err := Decode(MustParseType(tc.typ), mustHex(t, strings.ReplaceAll(tc.hex, " ", "")))
		if err == nil {
			t.Fatalf("%s %s: expected error", tc.typ, tc.hex)
		}
	}
}

func TestParseTypeErrors(t *testing.T) {
	for _, s := range []string{"", "a", "(ss", "m", "z", "(*)", "(ss)extra"} {
		if _, err := ParseType(s); err == nil {
			t.Fatalf("%q: expected error", s)
		}
	}
	for _, s := range []string{"y", "as", "a{sv}", "(a{sv}aya(say)sstayay)", "a(uuu)", "a(ayay)"} {
		if _, err := ParseType(s); err != nil {
			t.Fatalf("%q: %v", s, err)
		}
	}
}
