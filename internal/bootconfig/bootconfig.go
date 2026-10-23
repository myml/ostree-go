// Package bootconfig implements the Boot Loader Specification (BLS) config
// files that OSTree writes to /boot/loader/entries, plus the kernel argument
// manipulation helpers used by the deployment code.
package bootconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is one BLS entry.  Field order on write follows the
// BootLoaderspec document, matching ostree_bootconfig_parser_write_at().
type Config struct {
	// Filename is the basename the entry was read from, if any.
	Filename string
	// Options holds every key/value pair, in insertion order.
	Options map[string]string
	order   []string
	// OverlayInitrds are additional initrd= entries after the primary one.
	OverlayInitrds []string
	// TriesLeft/TriesDone are parsed from the "+LEFT-DONE" suffix.
	TriesLeft uint64
	TriesDone uint64
}

// New returns an empty bootconfig.
func New() *Config {
	return &Config{Options: make(map[string]string)}
}

// Get returns the value for key, or "".
func (c *Config) Get(key string) string {
	return c.Options[key]
}

// Has reports whether key is present.
func (c *Config) Has(key string) bool {
	_, ok := c.Options[key]
	return ok
}

// Set replaces key with value.
func (c *Config) Set(key, value string) {
	if _, ok := c.Options[key]; !ok {
		c.order = append(c.order, key)
	}
	c.Options[key] = value
}

// Delete removes key.
func (c *Config) Delete(key string) {
	if _, ok := c.Options[key]; !ok {
		return
	}
	delete(c.Options, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// Keys returns the option keys in insertion order.
func (c *Config) Keys() []string {
	out := make([]string, len(c.order))
	copy(out, c.order)
	return out
}

// Clone returns a deep copy.
func (c *Config) Clone() *Config {
	out := New()
	for _, k := range c.order {
		out.Set(k, c.Options[k])
	}
	out.Filename = c.Filename
	out.OverlayInitrds = append([]string(nil), c.OverlayInitrds...)
	out.TriesLeft = c.TriesLeft
	out.TriesDone = c.TriesDone
	return out
}

// parseTries parses a suffix of two counters of the form "+LEFT-DONE" from the
// end of a filename (extension included, which is harmless).
func parseTries(name string) (left, done uint64) {
	idx := strings.LastIndexByte(name, '+')
	if idx < 0 {
		return 0, 0
	}
	s := name[idx+1:]
	if s == "" || s[0] == '-' {
		return 0, 0
	}
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, 0
	}
	left, err := strconv.ParseUint(s[:end], 10, 64)
	if err != nil {
		return 0, 0
	}
	if end < len(s) && s[end] == '-' {
		rest := s[end+1:]
		n := 0
		for n < len(rest) && rest[n] >= '0' && rest[n] <= '9' {
			n++
		}
		if n == 0 {
			return 0, 0
		}
		done, err = strconv.ParseUint(rest[:n], 10, 64)
		if err != nil {
			return 0, 0
		}
	}
	return left, done
}

// ParseData parses BLS file contents.
func ParseData(data []byte, name string) *Config {
	c := New()
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || !isAlpha(line[0]) {
			continue
		}
		eq := strings.IndexAny(line, " \t")
		if eq <= 0 {
			continue
		}
		key := line[:eq]
		val := line[eq+1:]
		if key == "initrd" && c.Has("initrd") {
			c.OverlayInitrds = append(c.OverlayInitrds, val)
			continue
		}
		c.Set(key, val)
	}
	c.TriesLeft, c.TriesDone = parseTries(name)
	if name != "" {
		c.Filename = filepath.Base(name)
	}
	return c
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// ParseFile reads and parses a BLS entry.
func ParseFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseData(data, path), nil
}

// Bytes renders the entry in the deterministic order used by libostree.
func (c *Config) Bytes() []byte {
	fields := []string{"title", "version", "options", "devicetree", "linux", "initrd"}
	var b strings.Builder
	written := make(map[string]bool, len(fields))
	for _, key := range fields {
		if v, ok := c.Options[key]; ok {
			writeKey(&b, key, v)
			written[key] = true
		}
	}
	if len(c.OverlayInitrds) > 0 {
		for _, v := range c.OverlayInitrds {
			writeKey(&b, "initrd", v)
		}
	}
	for _, key := range c.order {
		if written[key] {
			continue
		}
		writeKey(&b, key, c.Options[key])
	}
	return []byte(b.String())
}

func writeKey(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}

// WriteFile writes the entry atomically to path.
func (c *Config) WriteFile(path string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(c.Bytes()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// EntryFilename computes the BLS entry filename for a deployment.
// nDeployments is the total number of deployments being written.
func EntryFilename(prefix string, nDeployments, index int, bootCounting uint, c *Config) string {
	name := fmt.Sprintf("%s-%d", prefix, nDeployments-index)
	if bootCounting == 0 {
		return name + ".conf"
	}
	maxTries := bootCounting
	if c.Filename == "" {
		return fmt.Sprintf("%s+%d.conf", name, maxTries)
	}
	if c.TriesLeft == 0 && c.TriesDone == 0 {
		return name + ".conf"
	}
	return fmt.Sprintf("%s+%d-%d.conf", name, c.TriesLeft, c.TriesDone)
}
