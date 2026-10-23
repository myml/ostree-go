package bootconfig

import (
	"fmt"
	"strings"
)

// KernelArgs is a parsed kernel command line that keeps both the ordering of
// arguments and, per key, the list of values.  It mirrors OstreeKernelArgs.
type KernelArgs struct {
	order []*kargEntry
	table map[string][]*kargEntry
}

type kargEntry struct {
	key   string
	value string // "" means "no value present" (distinct from "key=")
	has   bool
}

// NewKernelArgs returns an empty set.
func NewKernelArgs() *KernelArgs {
	return &KernelArgs{table: make(map[string][]*kargEntry)}
}

// splitKeyEq splits "key=value" into key/value; has is false when no '='.
func splitKeyEq(arg string) (key, value string, has bool) {
	if i := strings.IndexByte(arg, '='); i >= 0 {
		return arg[:i], arg[i+1:], true
	}
	return arg, "", false
}

// SplitKernelArgs splits a command line on spaces, but keeps quoted runs
// together (quotes are preserved in the output).
func SplitKernelArgs(s string) []string {
	var out []string
	if s == "" {
		return out
	}
	quoted := false
	start := 0
	for start < len(s) && s[start] == ' ' {
		start++
	}
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '"':
			quoted = !quoted
		case ' ':
			if !quoted {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if len(s) > start {
		out = append(out, s[start:])
	}
	return out
}

// FromString parses a kernel command line.
func FromString(options string) *KernelArgs {
	k := NewKernelArgs()
	k.ParseAppend(options)
	return k
}

// ParseAppend appends every argument in options.
func (k *KernelArgs) ParseAppend(options string) {
	if options == "" {
		return
	}
	for _, arg := range SplitKernelArgs(options) {
		k.Append(arg)
	}
}

// Append adds arg, keeping earlier values for the same key.
func (k *KernelArgs) Append(arg string) {
	for _, item := range SplitKernelArgs(arg) {
		key, value, has := splitKeyEq(item)
		entry := &kargEntry{key: key, value: value, has: has}
		k.table[key] = append(k.table[key], entry)
		k.order = append(k.order, entry)
	}
}

// AppendArgv appends each element of argv.
func (k *KernelArgs) AppendArgv(argv []string) {
	for _, arg := range argv {
		k.Append(arg)
	}
}

// AppendIfMissing appends arg only if the key is unknown.
func (k *KernelArgs) AppendIfMissing(arg string) {
	if k.Contains(arg) {
		return
	}
	k.Append(arg)
}

// Contains reports whether the key of arg is present.
func (k *KernelArgs) Contains(arg string) bool {
	key, _, _ := splitKeyEq(arg)
	_, ok := k.table[key]
	return ok
}

// Replace replaces (or inserts) arg by key, keeping its position.
func (k *KernelArgs) Replace(arg string) {
	key, value, has := splitKeyEq(arg)
	old, existed := k.table[key]
	if existed {
		// Find the position of the first old entry.
		pos := -1
		for i, e := range k.order {
			if e == old[0] {
				pos = i
				break
			}
		}
		k.removeEntries(old)
		entry := &kargEntry{key: key, value: value, has: has}
		if pos >= 0 && pos <= len(k.order) {
			k.order = append(k.order, nil)
			copy(k.order[pos+1:], k.order[pos:])
			k.order[pos] = entry
		} else {
			k.order = append(k.order, entry)
		}
		k.table[key] = []*kargEntry{entry}
		return
	}
	entry := &kargEntry{key: key, value: value, has: has}
	k.table[key] = []*kargEntry{entry}
	k.order = append(k.order, entry)
}

// ReplaceArgv replaces each element of argv.
func (k *KernelArgs) ReplaceArgv(argv []string) {
	for _, arg := range argv {
		k.Replace(arg)
	}
}

func (k *KernelArgs) removeEntries(entries []*kargEntry) {
	remove := make(map[*kargEntry]bool, len(entries))
	for _, e := range entries {
		remove[e] = true
	}
	out := k.order[:0]
	for _, e := range k.order {
		if !remove[e] {
			out = append(out, e)
		}
	}
	k.order = out
}

// Delete removes an argument: either a specific key=value pair, or, when only
// one value is associated with the key, the key itself.
func (k *KernelArgs) Delete(arg string) error {
	for _, item := range SplitKernelArgs(arg) {
		key, value, has := splitKeyEq(item)
		entries, ok := k.table[key]
		if !ok {
			return fmt.Errorf("No key '%s' found", key)
		}
		if len(entries) == 1 {
			e := entries[0]
			if has {
				if !e.has || e.value != value {
					return fmt.Errorf("No karg '%s=%s' found", key, value)
				}
			}
			k.removeEntries(entries)
			delete(k.table, key)
			continue
		}
		var found *kargEntry
		for _, e := range entries {
			if (!has && !e.has) || (has && e.has && e.value == value) {
				found = e
				break
			}
		}
		if found == nil {
			if !has {
				return fmt.Errorf("Multiple values for key '%s' found", arg)
			}
			return fmt.Errorf("No karg '%s' found", arg)
		}
		out := entries[:0]
		for _, e := range entries {
			if e != found {
				out = append(out, e)
			}
		}
		k.table[key] = out
		for i, e := range k.order {
			if e == found {
				k.order = append(k.order[:i], k.order[i+1:]...)
				break
			}
		}
	}
	return nil
}

// DeleteIfPresent deletes arg only when its key exists.
func (k *KernelArgs) DeleteIfPresent(arg string) error {
	if !k.Contains(arg) {
		return nil
	}
	return k.Delete(arg)
}

// GetLastValue returns the last value associated with key.
func (k *KernelArgs) GetLastValue(key string) (string, bool) {
	entries := k.table[key]
	if len(entries) == 0 {
		return "", false
	}
	return entries[len(entries)-1].value, true
}

// String renders the arguments separated by single spaces.
func (k *KernelArgs) String() string {
	var b strings.Builder
	for i, e := range k.order {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(e.key)
		if e.has {
			b.WriteByte('=')
			b.WriteString(e.value)
		}
	}
	return b.String()
}

// Strv renders each argument separately.
func (k *KernelArgs) Strv() []string {
	out := make([]string, 0, len(k.order))
	for _, e := range k.order {
		if e.has {
			out = append(out, e.key+"="+e.value)
		} else {
			out = append(out, e.key)
		}
	}
	return out
}

// Clone returns a deep copy.
func (k *KernelArgs) Clone() *KernelArgs {
	out := NewKernelArgs()
	for _, e := range k.order {
		entry := &kargEntry{key: e.key, value: e.value, has: e.has}
		out.table[e.key] = append(out.table[e.key], entry)
		out.order = append(out.order, entry)
	}
	return out
}
