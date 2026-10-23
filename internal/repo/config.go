package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is a GKeyFile-compatible configuration document as used by OSTree.
//
// Compatibility notes (matching GLib's GKeyFile):
//   - groups and keys keep their insertion order;
//   - comments are discarded on read and never written;
//   - string lists are separated by ';' and serialized with a trailing ';';
//   - values are escaped with \n, \r, \t (leading only), \s (leading spaces)
//     and \\.
//
// The parser is deliberately strict: a malformed line is an error, as it is in
// GKeyFile. Unlike GKeyFile, duplicate keys within a group update the value in
// place instead of being kept as duplicate output lines.
type Config struct {
	groups []*configGroup
	index  map[string]*configGroup
}

type configGroup struct {
	name  string
	keys  []*configKey
	index map[string]*configKey
}

type configKey struct {
	name  string
	value string // raw (still escaped) value, as stored in the file
}

// NewConfig returns an empty configuration.
func NewConfig() *Config {
	return &Config{index: make(map[string]*configGroup)}
}

// ParseConfig parses GKeyFile data.
func ParseConfig(data []byte) (*Config, error) {
	c := NewConfig()
	var cur *configGroup
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			open := strings.IndexByte(line, '[')
			relClose := strings.IndexByte(line[open+1:], ']')
			if relClose < 0 {
				return nil, fmt.Errorf("%w: line %d: malformed group header %q", ErrInvalidConfig, i+1, line)
			}
			close := open + 1 + relClose
			if strings.TrimSpace(line[close+1:]) != "" {
				return nil, fmt.Errorf("%w: line %d: malformed group header %q", ErrInvalidConfig, i+1, line)
			}
			name := strings.TrimSpace(line[open+1 : close])
			if name == "" || strings.ContainsAny(name, "[]") {
				return nil, fmt.Errorf("%w: line %d: invalid group name %q", ErrInvalidConfig, i+1, name)
			}
			cur = c.ensureGroup(name)
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%w: line %d: %q is not a key-value pair, group or comment", ErrInvalidConfig, i+1, line)
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			return nil, fmt.Errorf("%w: line %d: empty key", ErrInvalidConfig, i+1)
		}
		if cur == nil {
			return nil, fmt.Errorf("%w: line %d: key outside of any group", ErrInvalidConfig, i+1)
		}
		value := strings.TrimLeft(line[eq+1:], " \t\v\f\r")
		cur.set(key, value)
	}
	return c, nil
}

// LoadConfig reads and parses a config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseConfig(data)
}

// Clone returns a deep copy of c.
func (c *Config) Clone() *Config {
	out := NewConfig()
	for _, g := range c.groups {
		ng := out.ensureGroup(g.name)
		for _, k := range g.keys {
			ng.set(k.name, k.value)
		}
	}
	return out
}

func (c *Config) ensureGroup(name string) *configGroup {
	if g, ok := c.index[name]; ok {
		return g
	}
	g := &configGroup{name: name, index: make(map[string]*configKey)}
	c.groups = append(c.groups, g)
	c.index[name] = g
	return g
}

func (g *configGroup) find(key string) *configKey {
	return g.index[key]
}

func (g *configGroup) set(key, raw string) {
	if k, ok := g.index[key]; ok {
		k.value = raw
		return
	}
	k := &configKey{name: key, value: raw}
	g.keys = append(g.keys, k)
	g.index[key] = k
}

func (g *configGroup) remove(key string) bool {
	k, ok := g.index[key]
	if !ok {
		return false
	}
	delete(g.index, key)
	for i, existing := range g.keys {
		if existing == k {
			g.keys = append(g.keys[:i], g.keys[i+1:]...)
			break
		}
	}
	return true
}

// Bytes serializes the config to GKeyFile bytes. Groups are separated by one
// blank line, keys are written in insertion order, and a final newline is
// emitted for every group.
func (c *Config) Bytes() []byte {
	var b strings.Builder
	for i, g := range c.groups {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteByte('[')
		b.WriteString(g.name)
		b.WriteString("]\n")
		for _, k := range g.keys {
			b.WriteString(k.name)
			b.WriteByte('=')
			b.WriteString(k.value)
			b.WriteByte('\n')
		}
	}
	return []byte(b.String())
}

// Save atomically writes the configuration to path with mode 0660 (subject to
// umask), matching the repository's config file mode.
func (c *Config) Save(path string) error {
	return writeFileAtomic(path, c.Bytes(), 0o660)
}

// Groups returns group names in file order.
func (c *Config) Groups() []string {
	out := make([]string, 0, len(c.groups))
	for _, g := range c.groups {
		out = append(out, g.name)
	}
	return out
}

// HasGroup reports whether the group exists.
func (c *Config) HasGroup(group string) bool {
	_, ok := c.index[group]
	return ok
}

// HasKey reports whether the key exists in a group.
func (c *Config) HasKey(group, key string) bool {
	g, ok := c.index[group]
	if !ok {
		return false
	}
	return g.find(key) != nil
}

// Keys returns the key names of a group in file order.
func (c *Config) Keys(group string) ([]string, error) {
	g, ok := c.index[group]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrGroupNotFound, group)
	}
	out := make([]string, 0, len(g.keys))
	for _, k := range g.keys {
		out = append(out, k.name)
	}
	return out, nil
}

// GetValue returns the raw (still escaped) value of a key, like
// g_key_file_get_value().
func (c *Config) GetValue(group, key string) (string, error) {
	g, ok := c.index[group]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrGroupNotFound, group)
	}
	k := g.find(key)
	if k == nil {
		return "", fmt.Errorf("%w: %q in group %q", ErrKeyNotFound, key, group)
	}
	return k.value, nil
}

// Get returns the unescaped string value of a key, like
// g_key_file_get_string(). It reports an error for values that contain a
// GKeyFile list separator, matching GLib.
func (c *Config) Get(group, key string) (string, error) {
	raw, err := c.GetValue(group, key)
	if err != nil {
		return "", err
	}
	return unescapeConfigValue(raw, false)
}

// GetRawString returns the unescaped value of a key while preserving list
// separators (';').  It is the string form used for options such as
// remote.gpgkeypath whose value may legitimately contain semicolons.
func (c *Config) GetRawString(group, key string) (string, error) {
	raw, err := c.GetValue(group, key)
	if err != nil {
		return "", err
	}
	return unescapeConfigValue(raw, true)
}

// GetList returns a ';' separated string list. An empty value yields an empty
// list. Separators may be escaped with "\;".
func (c *Config) GetList(group, key string) ([]string, error) {
	raw, err := c.GetValue(group, key)
	if err != nil {
		return nil, err
	}
	return parseConfigList(raw)
}

// GetBool returns a boolean value, or def if the key (or its group) is absent.
// It accepts yes/no/true/false/1/0 like OSTree's _ostree_parse_boolean().
func (c *Config) GetBool(group, key string, def bool) (bool, error) {
	raw, err := c.GetValue(group, key)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrGroupNotFound) {
			return def, nil
		}
		return false, err
	}
	switch strings.TrimSpace(raw) {
	case "yes", "true", "1":
		return true, nil
	case "no", "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%w: invalid boolean %q for %s.%s", ErrInvalidConfig, raw, group, key)
	}
}

// GetInt returns an integer value, or def if the key (or its group) is absent.
func (c *Config) GetInt(group, key string, def int64) (int64, error) {
	raw, err := c.GetValue(group, key)
	if err != nil {
		if errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrGroupNotFound) {
			return def, nil
		}
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid integer %q for %s.%s", ErrInvalidConfig, raw, group, key)
	}
	return v, nil
}

// Set sets a string value with g_key_file_set_string() escaping.
func (c *Config) Set(group, key, value string) {
	c.SetValue(group, key, escapeConfigValue(value, false))
}

// SetValue sets a raw (already escaped) value, like g_key_file_set_value().
func (c *Config) SetValue(group, key, raw string) {
	c.ensureGroup(group).set(key, raw)
}

// SetList sets a ';' separated string list with g_key_file_set_string_list()
// escaping.
func (c *Config) SetList(group, key string, values []string) {
	if len(values) == 0 {
		c.SetValue(group, key, "")
		return
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = escapeConfigValue(v, true)
	}
	c.SetValue(group, key, strings.Join(parts, ";")+";")
}

// Unset removes a key. Removing a nonexistent key or group reports
// ErrKeyNotFound or ErrGroupNotFound respectively. An empty group is kept,
// matching g_key_file_remove_key().
func (c *Config) Unset(group, key string) error {
	g, ok := c.index[group]
	if !ok {
		return fmt.Errorf("%w: %q", ErrGroupNotFound, group)
	}
	if !g.remove(key) {
		return fmt.Errorf("%w: %q in group %q", ErrKeyNotFound, key, group)
	}
	return nil
}

// RemoveGroup removes an entire group.
func (c *Config) RemoveGroup(group string) error {
	g, ok := c.index[group]
	if !ok {
		return fmt.Errorf("%w: %q", ErrGroupNotFound, group)
	}
	delete(c.index, group)
	for i, existing := range c.groups {
		if existing == g {
			c.groups = append(c.groups[:i], c.groups[i+1:]...)
			break
		}
	}
	return nil
}

// RemoteSection returns the GKeyFile group name for a remote:
//
//	[remote "origin"]
func RemoteSection(name string) string { return `remote "` + name + `"` }

// ParseRemoteSection extracts the remote name from a "remote \"name\"" group.
func ParseRemoteSection(section string) (string, bool) {
	const prefix = `remote "`
	if !strings.HasPrefix(section, prefix) || !strings.HasSuffix(section, `"`) {
		return "", false
	}
	name := section[len(prefix) : len(section)-1]
	if name == "" {
		return "", false
	}
	return name, true
}

// RemoteNames returns configured remote names in group order.
func (c *Config) RemoteNames() []string {
	var out []string
	for _, g := range c.groups {
		if name, ok := ParseRemoteSection(g.name); ok {
			out = append(out, name)
		}
	}
	return out
}

// GetRemote returns an option from a [remote "name"] group.
func (c *Config) GetRemote(name, key string) (string, error) {
	return c.Get(RemoteSection(name), key)
}

// SetRemote sets an option in a [remote "name"] group.
func (c *Config) SetRemote(name, key, value string) {
	c.Set(RemoteSection(name), key, value)
}

// escapeConfigValue escapes a value the way g_key_file_escape_string() does.
// Whitespace in the leading whitespace run is escaped so that it survives the
// parser's leading-whitespace trimming. list enables ';' escaping.
func escapeConfigValue(s string, list bool) string {
	var b strings.Builder
	leading := true
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '\n':
			b.WriteString(`\n`)
			// A newline is whitespace, so the leading run continues.
		case '\r':
			b.WriteString(`\r`)
		case '\\':
			b.WriteString(`\\`)
			leading = false
		case '\t':
			if leading {
				b.WriteString(`\t`)
			} else {
				b.WriteByte('\t')
			}
		case ' ':
			if leading {
				b.WriteString(`\s`)
			} else {
				b.WriteByte(' ')
			}
		case ';':
			if list {
				b.WriteString(`\;`)
			} else {
				b.WriteByte(';')
			}
			leading = false
		default:
			b.WriteByte(ch)
			leading = false
		}
	}
	return b.String()
}

// unescapeConfigValue handles the GKeyFile escape sequences. When allowListSep
// is false, an (escaped or unescaped) semicolon is rejected, mirroring
// g_key_file_get_string().
func unescapeConfigValue(raw string, allowListSep bool) (string, error) {
	var b strings.Builder
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch != '\\' {
			if ch == ';' && !allowListSep {
				return "", fmt.Errorf("%w: value %q is a list, not a string", ErrInvalidConfig, raw)
			}
			b.WriteByte(ch)
			continue
		}
		i++
		if i >= len(raw) {
			return "", fmt.Errorf("%w: trailing backslash in value %q", ErrInvalidConfig, raw)
		}
		switch raw[i] {
		case 's':
			b.WriteByte(' ')
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '\\':
			b.WriteByte('\\')
		case ';':
			if !allowListSep {
				return "", fmt.Errorf("%w: value %q is a list, not a string", ErrInvalidConfig, raw)
			}
			b.WriteByte(';')
		default:
			return "", fmt.Errorf("%w: invalid escape \\%c in value %q", ErrInvalidConfig, raw[i], raw)
		}
	}
	return b.String(), nil
}

// parseConfigList splits a raw list value on unescaped semicolons and drops
// the empty element produced by the trailing separator (GKeyFile semantics).
func parseConfigList(raw string) ([]string, error) {
	var rawElems []string
	var cur strings.Builder
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch == '\\' {
			cur.WriteByte(ch)
			if i+1 < len(raw) {
				i++
				cur.WriteByte(raw[i])
			}
			continue
		}
		if ch == ';' {
			rawElems = append(rawElems, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	rawElems = append(rawElems, cur.String())
	// The trailing separator yields a final empty element which GKeyFile
	// does not report.
	if n := len(rawElems); n > 0 && rawElems[n-1] == "" {
		rawElems = rawElems[:n-1]
	}
	out := make([]string, 0, len(rawElems))
	for _, elem := range rawElems {
		v, err := unescapeConfigValue(elem, true)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// writeFileAtomic replaces path with data using a temporary file in the same
// directory followed by rename. The file is created with perm (subject to
// umask) and fsynced before the rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	f, tmp, err := createTempIn(dir, ".tmp-"+base+"-", perm)
	if err != nil {
		return err
	}
	cleanup := func() {
		f.Close()
		os.Remove(tmp)
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
