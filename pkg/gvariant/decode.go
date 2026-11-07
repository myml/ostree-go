package gvariant

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"
)

// Decode deserializes data according to the GVariant type t.
func Decode(t *Type, data []byte) (Value, error) {
	v, err := decodeValue(t, data, 0)
	if err != nil {
		return nil, err
	}
	// Reject trailing bytes and non-canonical padding. This is cheap for
	// the small metadata objects OSTree uses.
	size, err := EncodedSize(t, v)
	if err != nil {
		return nil, err
	}
	if size != len(data) {
		return nil, fmt.Errorf("gvariant: %s decodes to %d bytes, but input is %d bytes", t, size, len(data))
	}
	return v, nil
}

// IsNormalForm reports whether data is the canonical serialization of a
// value of type t.
func IsNormalForm(t *Type, data []byte) bool {
	v, err := Decode(t, data)
	if err != nil {
		return false
	}
	out, err := Encode(t, v)
	if err != nil {
		return false
	}
	return bytes.Equal(out, data)
}

func decodeValue(t *Type, data []byte, depth int) (Value, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("gvariant: maximum nesting depth exceeded")
	}
	switch t.Kind {
	case KindByte:
		need(data, 1, t)
		return data[0], nil
	case KindBoolean:
		need(data, 1, t)
		if data[0] > 1 {
			return nil, fmt.Errorf("gvariant: invalid boolean value %d", data[0])
		}
		return data[0] == 1, nil
	case KindInt16:
		need(data, 2, t)
		return int16(binary.LittleEndian.Uint16(data)), nil
	case KindUint16:
		need(data, 2, t)
		return binary.LittleEndian.Uint16(data), nil
	case KindInt32, KindHandle:
		need(data, 4, t)
		return int32(binary.LittleEndian.Uint32(data)), nil
	case KindUint32:
		need(data, 4, t)
		return binary.LittleEndian.Uint32(data), nil
	case KindInt64:
		need(data, 8, t)
		return int64(binary.LittleEndian.Uint64(data)), nil
	case KindUint64:
		need(data, 8, t)
		return binary.LittleEndian.Uint64(data), nil
	case KindDouble:
		need(data, 8, t)
		return math.Float64frombits(binary.LittleEndian.Uint64(data)), nil
	case KindString, KindObjectPath, KindSignature:
		if len(data) == 0 || data[len(data)-1] != 0 {
			return nil, fmt.Errorf("gvariant: unterminated string")
		}
		if bytes.IndexByte(data[:len(data)-1], 0) >= 0 {
			return nil, fmt.Errorf("gvariant: string contains embedded NUL")
		}
		s := string(data[:len(data)-1])
		if !utf8.ValidString(s) {
			return nil, fmt.Errorf("gvariant: invalid UTF-8 string")
		}
		return s, nil
	case KindVariant:
		idx := bytes.LastIndexByte(data, 0)
		if idx < 0 || idx == len(data)-1 {
			return nil, fmt.Errorf("gvariant: malformed variant")
		}
		ts := string(data[idx+1:])
		ct, err := ParseType(ts)
		if err != nil {
			return nil, fmt.Errorf("gvariant: variant type: %w", err)
		}
		child, err := decodeValue(ct, data[:idx], depth+1)
		if err != nil {
			return nil, err
		}
		cs, err := EncodedSize(ct, child)
		if err != nil {
			return nil, err
		}
		if cs != idx {
			return nil, fmt.Errorf("gvariant: variant child size %d != %d", cs, idx)
		}
		return Variant{Type: ct, Value: child}, nil
	case KindMaybe:
		if len(data) == 0 {
			return Maybe{Nothing: true}, nil
		}
		if t.Elem.isFixedSize() {
			sz, _ := t.Elem.FixedSize()
			if len(data) != sz {
				return nil, fmt.Errorf("gvariant: invalid maybe size %d", len(data))
			}
			v, err := decodeValue(t.Elem, data, depth+1)
			if err != nil {
				return nil, err
			}
			return Maybe{Value: v}, nil
		}
		switch data[0] {
		case 0:
			return Maybe{Nothing: true}, nil
		case 1:
			v, err := decodeValue(t.Elem, data[1:], depth+1)
			if err != nil {
				return nil, err
			}
			return Maybe{Value: v}, nil
		default:
			return nil, fmt.Errorf("gvariant: invalid maybe tag %d", data[0])
		}
	case KindArray:
		return decodeArray(t, data, depth)
	case KindTuple:
		vals, err := decodeMembers(t, data, depth)
		if err != nil {
			return nil, err
		}
		return vals, nil
	case KindDictEntry:
		vals, err := decodeMembers(t, data, depth)
		if err != nil {
			return nil, err
		}
		return DictEntry{Key: vals[0], Value: vals[1]}, nil
	default:
		return nil, fmt.Errorf("gvariant: invalid type kind %d", t.Kind)
	}
}

func decodeArray(t *Type, data []byte, depth int) (Value, error) {
	if len(data) == 0 {
		switch t.Elem.Kind {
		case KindByte:
			return []byte{}, nil
		case KindDictEntry:
			return Dict{}, nil
		default:
			return []Value{}, nil
		}
	}
	if elemSize, fixed := t.Elem.FixedSize(); fixed {
		if len(data)%elemSize != 0 {
			return nil, fmt.Errorf("gvariant: array size %d not a multiple of element size %d", len(data), elemSize)
		}
		n := len(data) / elemSize
		if t.Elem.Kind == KindByte {
			out := make([]byte, n)
			copy(out, data)
			return out, nil
		}
		out := make([]Value, n)
		for i := 0; i < n; i++ {
			v, err := decodeValue(t.Elem, data[i*elemSize:(i+1)*elemSize], depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		if t.Elem.Kind == KindDictEntry {
			return toDict(out), nil
		}
		return out, nil
	}

	w := OffsetSize(len(data))
	if w == 0 {
		return nil, fmt.Errorf("gvariant: malformed variable-size array")
	}
	if len(data) < w {
		return nil, fmt.Errorf("gvariant: array too short for offsets")
	}
	lastEnd := int(readUintLE(data[len(data)-w:], w))
	if lastEnd > len(data) {
		return nil, fmt.Errorf("gvariant: array offset %d out of bounds", lastEnd)
	}
	if (len(data)-lastEnd)%w != 0 {
		return nil, fmt.Errorf("gvariant: array offset table not aligned")
	}
	n := (len(data) - lastEnd) / w
	if n == 0 {
		return nil, fmt.Errorf("gvariant: array has no offsets")
	}
	ends := make([]int, n)
	for i := 0; i < n; i++ {
		ends[i] = int(readUintLE(data[lastEnd+i*w:], w))
		if ends[i] > lastEnd {
			return nil, fmt.Errorf("gvariant: array offset out of bounds")
		}
		if i > 0 && ends[i] < ends[i-1] {
			return nil, fmt.Errorf("gvariant: array offsets not ordered")
		}
	}
	if ends[n-1] != lastEnd {
		return nil, fmt.Errorf("gvariant: array last offset mismatch")
	}

	out := make([]Value, n)
	prev := 0
	for i := 0; i < n; i++ {
		start := prev
		if i > 0 {
			start = alignUp(prev, t.Elem.Alignment())
		}
		end := ends[i]
		if start > end {
			return nil, fmt.Errorf("gvariant: array element %d start %d > end %d", i, start, end)
		}
		v, err := decodeValue(t.Elem, data[start:end], depth+1)
		if err != nil {
			return nil, err
		}
		sz, err := EncodedSize(t.Elem, v)
		if err != nil {
			return nil, err
		}
		if sz != end-start {
			return nil, fmt.Errorf("gvariant: array element %d size mismatch", i)
		}
		out[i] = v
		prev = end
	}
	if t.Elem.Kind == KindDictEntry {
		return toDict(out), nil
	}
	return out, nil
}

func toDict(vals []Value) Dict {
	d := make(Dict, len(vals))
	for i, v := range vals {
		d[i] = v.(DictEntry)
	}
	return d
}

// decodeMembers decodes a tuple or dict entry.
func decodeMembers(t *Type, data []byte, depth int) ([]Value, error) {
	members := t.members()
	nvar := 0
	for _, m := range members {
		if !m.isFixedSize() {
			nvar++
		}
	}
	stored := nvar
	if t.isLastMemberVariable() && stored > 0 {
		stored--
	}
	w := 0
	tableSize := 0
	if stored > 0 {
		w = OffsetSize(len(data))
		tableSize = stored * w
	}
	if tableSize > len(data) {
		return nil, fmt.Errorf("gvariant: tuple offset table out of bounds")
	}
	bodySize := len(data) - tableSize

	// varEnds[j] is the end offset of the j-th variable member.
	varEnds := make([]int, nvar)
	nextStored := stored - 1
	for j := 0; j < nvar; j++ {
		if t.isLastMemberVariable() && j == nvar-1 {
			varEnds[j] = bodySize
			continue
		}
		if stored == 0 || nextStored < 0 {
			return nil, fmt.Errorf("gvariant: missing tuple offset")
		}
		pos := bodySize + nextStored*w
		varEnds[j] = int(readUintLE(data[pos:], w))
		nextStored--
		if varEnds[j] > bodySize {
			return nil, fmt.Errorf("gvariant: tuple offset out of bounds")
		}
	}

	out := make([]Value, len(members))
	cur := 0
	vi := 0
	for i, m := range members {
		start := alignUp(cur, m.Alignment())
		if start > bodySize {
			return nil, fmt.Errorf("gvariant: tuple member %d start out of bounds", i)
		}
		var end int
		if _, fixed := m.FixedSize(); fixed {
			sz, _ := m.FixedSize()
			end = start + sz
		} else {
			end = varEnds[vi]
			vi++
		}
		if end < start || end > bodySize {
			return nil, fmt.Errorf("gvariant: tuple member %d bounds [%d,%d) invalid", i, start, end)
		}
		v, err := decodeValue(m, data[start:end], depth+1)
		if err != nil {
			return nil, err
		}
		sz, err := EncodedSize(m, v)
		if err != nil {
			return nil, err
		}
		if sz != end-start {
			return nil, fmt.Errorf("gvariant: tuple member %d size mismatch", i)
		}
		out[i] = v
		cur = end
	}
	if cur != bodySize {
		return nil, fmt.Errorf("gvariant: tuple trailing data")
	}
	return out, nil
}

func need(data []byte, n int, t *Type) error {
	if len(data) != n {
		return fmt.Errorf("gvariant: %s needs %d bytes, got %d", t, n, len(data))
	}
	return nil
}

func readUintLE(b []byte, w int) uint64 {
	var v uint64
	for i := 0; i < w; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v
}
