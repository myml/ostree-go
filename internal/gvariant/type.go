// Package gvariant implements the subset of the GVariant serialization
// format used by OSTree.
//
// The implementation is signature-driven: a Type is parsed once from a
// GVariant type string, then Decode/Encode convert between serialized
// bytes and a Go value tree.
package gvariant

import (
	"fmt"
	"strings"
)

// Kind identifies a GVariant type.
type Kind uint8

const (
	KindByte Kind = iota
	KindBoolean
	KindInt16
	KindUint16
	KindInt32
	KindUint32
	KindInt64
	KindUint64
	KindHandle
	KindDouble
	KindString
	KindObjectPath
	KindSignature
	KindVariant
	KindArray
	KindTuple
	KindDictEntry
	KindMaybe
)

// Type is a parsed GVariant type signature.
type Type struct {
	Kind    Kind
	Elem    *Type   // array / maybe
	Key     *Type   // dict entry
	Value   *Type   // dict entry
	Members []*Type // tuple
	text    string  // canonical type string
}

// ParseType parses a complete GVariant type string.
func ParseType(s string) (*Type, error) {
	p := &parser{s: s}
	t, err := p.parse()
	if err != nil {
		return nil, err
	}
	if p.pos != len(p.s) {
		return nil, fmt.Errorf("gvariant: trailing characters in type %q", s)
	}
	if !t.IsDefinite() {
		return nil, fmt.Errorf("gvariant: indefinite type %q", s)
	}
	t.text = s
	return t, nil
}

// MustParseType is ParseType that panics, for use with constants.
func MustParseType(s string) *Type {
	t, err := ParseType(s)
	if err != nil {
		panic(err)
	}
	return t
}

// String returns the canonical type string.
func (t *Type) String() string {
	if t == nil {
		return ""
	}
	if t.text != "" {
		return t.text
	}
	var b strings.Builder
	t.writeString(&b)
	t.text = b.String()
	return t.text
}

func (t *Type) writeString(b *strings.Builder) {
	switch t.Kind {
	case KindByte:
		b.WriteByte('y')
	case KindBoolean:
		b.WriteByte('b')
	case KindInt16:
		b.WriteByte('n')
	case KindUint16:
		b.WriteByte('q')
	case KindInt32:
		b.WriteByte('i')
	case KindUint32:
		b.WriteByte('u')
	case KindInt64:
		b.WriteByte('x')
	case KindUint64:
		b.WriteByte('t')
	case KindHandle:
		b.WriteByte('h')
	case KindDouble:
		b.WriteByte('d')
	case KindString:
		b.WriteByte('s')
	case KindObjectPath:
		b.WriteByte('o')
	case KindSignature:
		b.WriteByte('g')
	case KindVariant:
		b.WriteByte('v')
	case KindArray:
		b.WriteByte('a')
		t.Elem.writeString(b)
	case KindMaybe:
		b.WriteByte('m')
		t.Elem.writeString(b)
	case KindTuple:
		b.WriteByte('(')
		for _, m := range t.Members {
			m.writeString(b)
		}
		b.WriteByte(')')
	case KindDictEntry:
		b.WriteByte('{')
		t.Key.writeString(b)
		t.Value.writeString(b)
		b.WriteByte('}')
	default:
		panic("gvariant: invalid type kind")
	}
}

// IsDefinite reports whether the type contains no indefinite elements.
func (t *Type) IsDefinite() bool {
	switch t.Kind {
	case KindMaybe:
		return false
	case KindArray:
		return t.Elem.IsDefinite()
	case KindTuple:
		for _, m := range t.Members {
			if !m.IsDefinite() {
				return false
			}
		}
		return true
	case KindDictEntry:
		return t.Key.IsDefinite() && t.Value.IsDefinite()
	default:
		return true
	}
}

// Alignment returns the serialization alignment of the type.
func (t *Type) Alignment() int {
	switch t.Kind {
	case KindByte, KindString, KindObjectPath, KindSignature, KindMaybe:
		if t.Kind == KindMaybe && !t.Elem.isFixedSize() {
			return 1
		}
		if t.Kind == KindMaybe {
			return t.Elem.Alignment()
		}
		return 1
	case KindInt16, KindUint16:
		return 2
	case KindInt32, KindUint32, KindHandle:
		return 4
	case KindBoolean:
		return 1
	case KindInt64, KindUint64, KindDouble:
		return 8
	case KindVariant:
		return 8
	case KindArray:
		return t.Elem.Alignment()
	case KindTuple:
		a := 1
		for _, m := range t.Members {
			if x := m.Alignment(); x > a {
				a = x
			}
		}
		return a
	case KindDictEntry:
		a := t.Key.Alignment()
		if x := t.Value.Alignment(); x > a {
			a = x
		}
		return a
	default:
		panic("gvariant: invalid type kind")
	}
}

// isFixedSize reports whether the type has a fixed serialized size.
func (t *Type) isFixedSize() bool {
	_, ok := t.FixedSize()
	return ok
}

// FixedSize returns the fixed serialized size, if the type has one.
func (t *Type) FixedSize() (int, bool) {
	switch t.Kind {
	case KindByte:
		return 1, true
	case KindInt32, KindUint32, KindHandle:
		return 4, true
	case KindBoolean:
		return 1, true
	case KindInt16, KindUint16:
		return 2, true
	case KindInt64, KindUint64, KindDouble:
		return 8, true
	case KindString, KindObjectPath, KindSignature, KindVariant, KindArray, KindMaybe:
		return 0, false
	case KindTuple:
		size := 0
		for _, m := range t.Members {
			s, ok := m.FixedSize()
			if !ok {
				return 0, false
			}
			size = alignUp(size, m.Alignment()) + s
		}
		return size, true
	case KindDictEntry:
		ks, ok := t.Key.FixedSize()
		if !ok {
			return 0, false
		}
		vs, ok := t.Value.FixedSize()
		if !ok {
			return 0, false
		}
		size := alignUp(0, t.Key.Alignment()) + ks
		size = alignUp(size, t.Value.Alignment()) + vs
		return size, true
	default:
		return 0, false
	}
}

func (t *Type) isLastMemberVariable() bool {
	if t.Kind == KindDictEntry {
		return !t.Value.isFixedSize()
	}
	if t.Kind != KindTuple || len(t.Members) == 0 {
		return false
	}
	return !t.Members[len(t.Members)-1].isFixedSize()
}

// members returns the members of a tuple or dict entry.
func (t *Type) members() []*Type {
	if t.Kind == KindDictEntry {
		return []*Type{t.Key, t.Value}
	}
	return t.Members
}

func alignUp(n, align int) int {
	if align <= 1 {
		return n
	}
	return (n + align - 1) &^ (align - 1)
}

// OffsetSize returns the size in bytes of framing offsets in a container
// whose total serialized size is total.
func OffsetSize(total int) int {
	switch {
	case total > 1<<32-1:
		return 8
	case total > 1<<16-1:
		return 4
	case total > 1<<8-1:
		return 2
	case total > 0:
		return 1
	default:
		return 0
	}
}

// offsetWidthFor returns the minimal width able to hold body+width*n.
func offsetWidthFor(body, n int) int {
	if n == 0 {
		return 0
	}
	for _, w := range []int{1, 2, 4, 8} {
		var max int
		switch w {
		case 1:
			max = 1<<8 - 1
		case 2:
			max = 1<<16 - 1
		case 4:
			max = 1<<32 - 1
		default:
			max = 1<<63 - 1
		}
		if body+w*n <= max {
			return w
		}
	}
	return 8
}

type parser struct {
	s   string
	pos int
}

func (p *parser) parse() (*Type, error) {
	if p.pos >= len(p.s) {
		return nil, fmt.Errorf("gvariant: unexpected end of type %q", p.s)
	}
	c := p.s[p.pos]
	switch c {
	case 'y':
		p.pos++
		return &Type{Kind: KindByte}, nil
	case 'b':
		p.pos++
		return &Type{Kind: KindBoolean}, nil
	case 'n':
		p.pos++
		return &Type{Kind: KindInt16}, nil
	case 'q':
		p.pos++
		return &Type{Kind: KindUint16}, nil
	case 'i':
		p.pos++
		return &Type{Kind: KindInt32}, nil
	case 'u':
		p.pos++
		return &Type{Kind: KindUint32}, nil
	case 'x':
		p.pos++
		return &Type{Kind: KindInt64}, nil
	case 't':
		p.pos++
		return &Type{Kind: KindUint64}, nil
	case 'h':
		p.pos++
		return &Type{Kind: KindHandle}, nil
	case 'd':
		p.pos++
		return &Type{Kind: KindDouble}, nil
	case 's':
		p.pos++
		return &Type{Kind: KindString}, nil
	case 'o':
		p.pos++
		return &Type{Kind: KindObjectPath}, nil
	case 'g':
		p.pos++
		return &Type{Kind: KindSignature}, nil
	case 'v':
		p.pos++
		return &Type{Kind: KindVariant}, nil
	case 'a':
		p.pos++
		elem, err := p.parse()
		if err != nil {
			return nil, err
		}
		return &Type{Kind: KindArray, Elem: elem}, nil
	case 'm':
		p.pos++
		elem, err := p.parse()
		if err != nil {
			return nil, err
		}
		return &Type{Kind: KindMaybe, Elem: elem}, nil
	case '(':
		p.pos++
		var members []*Type
		for {
			if p.pos >= len(p.s) {
				return nil, fmt.Errorf("gvariant: unterminated tuple in %q", p.s)
			}
			if p.s[p.pos] == ')' {
				p.pos++
				break
			}
			m, err := p.parse()
			if err != nil {
				return nil, err
			}
			members = append(members, m)
		}
		return &Type{Kind: KindTuple, Members: members}, nil
	case '{':
		p.pos++
		key, err := p.parse()
		if err != nil {
			return nil, err
		}
		value, err := p.parse()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.s) || p.s[p.pos] != '}' {
			return nil, fmt.Errorf("gvariant: missing '}' in %q", p.s)
		}
		p.pos++
		return &Type{Kind: KindDictEntry, Key: key, Value: value}, nil
	default:
		return nil, fmt.Errorf("gvariant: invalid type character %q in %q", c, p.s)
	}
}
