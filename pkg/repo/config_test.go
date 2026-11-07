package repo

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestConfigParsePreservesOrderAndDropsComments(t *testing.T) {
	data := "# top comment\n[core]\nrepo_version=1\nmode=archive-z2\n\n" +
		"# a comment inside a group\n[remote \"origin\"]\nurl=https://example.com/repo\nbranches=a;b;\n"
	c, err := ParseConfig([]byte(data))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got, want := c.Groups(), []string{"core", `remote "origin"`}; !reflect.DeepEqual(got, want) {
		t.Errorf("groups = %q, want %q", got, want)
	}
	keys, err := c.Keys("core")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"repo_version", "mode"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("core keys = %q, want %q", keys, want)
	}
	if got, err := c.Get("core", "mode"); err != nil || got != "archive-z2" {
		t.Errorf("core.mode = %q, %v", got, err)
	}
	list, err := c.GetList(`remote "origin"`, "branches")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a", "b"}; !reflect.DeepEqual(list, want) {
		t.Errorf("branches = %q, want %q", list, want)
	}
	want := "[core]\nrepo_version=1\nmode=archive-z2\n\n[remote \"origin\"]\n" +
		"url=https://example.com/repo\nbranches=a;b;\n"
	if got := string(c.Bytes()); got != want {
		t.Errorf("serialized config:\n%q\nwant:\n%q", got, want)
	}
}

func TestConfigDuplicateGroupAndKey(t *testing.T) {
	c, err := ParseConfig([]byte("[core]\na=1\n[core]\nb=2\na=3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := c.Groups(), []string{"core"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("groups = %q, want %q", got, want)
	}
	if got, err := c.Get("core", "a"); err != nil || got != "3" {
		t.Errorf("a = %q, %v; want 3", got, err)
	}
}

func TestConfigEscapingRoundTrip(t *testing.T) {
	c := NewConfig()
	values := map[string]string{
		"leading":       " leading",
		"leading-tab":   "\tleading",
		"many-spaces":   "   three",
		"trailing":      "trailing ",
		"newline":       "a\nb",
		"cr":            "a\rb",
		"backslash":     `a\b`,
		"tab":           "a\tb",
		"brackets":      "[not a group]",
		"equals":        "a=b",
		"hash":          "#not a comment",
		"semicolon-str": "a;b",
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	for _, k := range keys {
		c.Set("g", k, values[k])
	}
	// Round-trip through the parser, as the config file will be.
	c2, err := ParseConfig(c.Bytes())
	if err != nil {
		t.Fatalf("re-parse: %v\nconfig: %q", err, c.Bytes())
	}
	for k, want := range values {
		got, err := c2.Get("g", k)
		if k == "semicolon-str" {
			// GKeyFile treats unescaped semicolons as list separators and
			// g_key_file_get_string() rejects them.
			if err == nil {
				t.Errorf("Get(%q) with a semicolon unexpectedly succeeded: %q", k, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Get(%q): %v", k, err)
			continue
		}
		if got != want {
			t.Errorf("Get(%q) = %q, want %q (serialized: %q)", k, got, want, c.Bytes())
		}
	}
	if raw, err := c.GetValue("g", "leading"); err != nil || raw != `\sleading` {
		t.Errorf("raw leading = %q, %v; want \\sleading", raw, err)
	}
	if raw, err := c.GetValue("g", "newline"); err != nil || raw != `a\nb` {
		t.Errorf("raw newline = %q, %v; want a\\nb", raw, err)
	}
}

func TestConfigLists(t *testing.T) {
	c := NewConfig()
	c.SetList("g", "l", []string{"one", "two;three", "back\\slash", " leading", "trailing "})
	raw, err := c.GetValue("g", "l")
	if err != nil {
		t.Fatal(err)
	}
	if raw != `one;two\;three;back\\slash;\sleading;trailing ;` {
		t.Errorf("raw list = %q", raw)
	}
	got, err := c.GetList("g", "l")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two;three", `back\slash`, " leading", "trailing "}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("list = %q, want %q", got, want)
	}
	if _, err := c.Get("g", "l"); err == nil {
		t.Error("Get on a list value should fail like g_key_file_get_string")
	}

	c.SetList("g", "empty", nil)
	if got, err := c.GetList("g", "empty"); err != nil || len(got) != 0 {
		t.Errorf("empty list = %q, %v", got, err)
	}
	c.SetList("g", "one-empty", []string{""})
	if got, err := c.GetList("g", "one-empty"); err != nil || !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("single-empty list = %q, %v", got, err)
	}

	for raw, want := range map[string][]string{
		"a;":   {"a"},
		"a;;":  {"a", ""},
		";":    {""},
		";;":   {"", ""},
		"a;b;": {"a", "b"},
		"":     {},
	} {
		got, err := parseConfigList(raw)
		if err != nil {
			t.Errorf("parseConfigList(%q): %v", raw, err)
			continue
		}
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseConfigList(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestConfigSetUnsetOrdering(t *testing.T) {
	c, err := ParseConfig([]byte("[core]\na=1\nb=2\nc=3\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Set("core", "b", "X")
	c.Set("core", "z", "Z")
	c.Set("core", "a", "")
	want := "[core]\na=\nb=X\nc=3\nz=Z\n"
	if got := string(c.Bytes()); got != want {
		t.Errorf("after set: %q, want %q", got, want)
	}
	if err := c.Unset("core", "b"); err != nil {
		t.Fatal(err)
	}
	want = "[core]\na=\nc=3\nz=Z\n"
	if got := string(c.Bytes()); got != want {
		t.Errorf("after unset: %q, want %q", got, want)
	}
	if err := c.Unset("core", "missing"); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("unset missing key error = %v, want ErrKeyNotFound", err)
	}
	if err := c.Unset("nope", "a"); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("unset missing group error = %v, want ErrGroupNotFound", err)
	}
	// Removing the last key keeps the empty group, matching GKeyFile.
	for _, k := range []string{"a", "c", "z"} {
		if err := c.Unset("core", k); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := string(c.Bytes()), "[core]\n"; got != want {
		t.Errorf("empty group serialized as %q, want %q", got, want)
	}
	if err := c.RemoveGroup("core"); err != nil {
		t.Fatal(err)
	}
	if got := string(c.Bytes()); got != "" {
		t.Errorf("config after remove group = %q", got)
	}
}

func TestConfigGetBoolAndInt(t *testing.T) {
	c, err := ParseConfig([]byte("[g]\nyes=yes\ntrue=true\none=1\nno=no\nfalse=false\nzero=0\nbad=maybe\nn=42\njunk=12x\n"))
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]bool{"yes": true, "true": true, "one": true, "no": false, "false": false, "zero": false} {
		got, err := c.GetBool("g", key, !want)
		if err != nil || got != want {
			t.Errorf("GetBool(%q) = %v, %v; want %v", key, got, err, want)
		}
	}
	if _, err := c.GetBool("g", "bad", false); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("GetBool(bad) error = %v, want ErrInvalidConfig", err)
	}
	if got, err := c.GetBool("g", "missing", true); err != nil || !got {
		t.Errorf("GetBool(missing) = %v, %v; want default true", got, err)
	}
	if got, err := c.GetInt("g", "n", 0); err != nil || got != 42 {
		t.Errorf("GetInt(n) = %v, %v", got, err)
	}
	if _, err := c.GetInt("g", "junk", 0); !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("GetInt(junk) error = %v, want ErrInvalidConfig", err)
	}
	if got, err := c.GetInt("g", "missing", 7); err != nil || got != 7 {
		t.Errorf("GetInt(missing) = %v, %v", got, err)
	}
}

func TestConfigRemoteHelpers(t *testing.T) {
	c := NewConfig()
	// Groups are ordered by first insertion, so add [core] first.
	c.Set("core", "repo_version", "1")
	c.SetRemote("origin", "url", "https://example.com/repo")
	c.SetRemote("upstream", "url", "file:///srv/repo")
	if got, want := c.RemoteNames(), []string{"origin", "upstream"}; !reflect.DeepEqual(got, want) {
		t.Errorf("remote names = %q, want %q", got, want)
	}
	if got, err := c.GetRemote("origin", "url"); err != nil || got != "https://example.com/repo" {
		t.Errorf("GetRemote = %q, %v", got, err)
	}
	section := RemoteSection("origin")
	if section != `remote "origin"` {
		t.Errorf("RemoteSection = %q", section)
	}
	if name, ok := ParseRemoteSection(section); !ok || name != "origin" {
		t.Errorf("ParseRemoteSection = %q, %v", name, ok)
	}
	if _, ok := ParseRemoteSection("core"); ok {
		t.Error("ParseRemoteSection(core) unexpectedly succeeded")
	}
	want := "[core]\nrepo_version=1\n\n[remote \"origin\"]\nurl=https://example.com/repo\n\n" +
		"[remote \"upstream\"]\nurl=file:///srv/repo\n"
	if got := string(c.Bytes()); got != want {
		t.Errorf("config = %q, want %q", got, want)
	}
}

func TestConfigParseCRLFAndWhitespace(t *testing.T) {
	c, err := ParseConfig([]byte("  [core]  \r\n a = 1 \r\n\t# comment\r\nb= x\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	// Leading whitespace is stripped; trailing whitespace is preserved by
	// GKeyFile ("a = 1 " has the value "1 ").
	if got, err := c.Get("core", "a"); err != nil || got != "1 " {
		t.Errorf("a = %q, %v; want %q", got, err, "1 ")
	}
	if got, err := c.Get("core", "b"); err != nil || got != "x" {
		t.Errorf("b = %q, %v", got, err)
	}
}

func TestConfigParseErrors(t *testing.T) {
	for _, data := range []string{
		"[g] x\na=1\n",
		"[a]b]\nx=1\n",
		"[a[b]\nx=1\n",
		"[g\na=1\n",
		"[g]\n=1\n",
		"a=1\n",
		"[g]\na\n",
	} {
		if _, err := ParseConfig([]byte(data)); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("ParseConfig(%q) error = %v, want ErrInvalidConfig", data, err)
		}
	}
}

func TestConfigSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	c := NewConfig()
	c.Set("core", "repo_version", "1")
	c.Set("core", "mode", "bare")
	c.SetRemote("origin", "url", "https://example.com/x")
	if err := c.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(loaded.Bytes()); got != string(c.Bytes()) {
		t.Errorf("loaded config = %q, want %q", got, c.Bytes())
	}
	if st, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm()&0o007 != 0 {
		t.Errorf("config mode = %v, should not be world accessible", st.Mode().Perm())
	}
}
