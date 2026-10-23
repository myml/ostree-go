package gvariant

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"
)

const maxDepth = 128

// Encode serializes v according to the GVariant type t.
func Encode(t *Type, v Value) ([]byte, error) {
	size, err := EncodedSize(t, v)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if err := writeValue(t, v, buf, 0, 0); err != nil {
		return nil, err
	}
	return buf, nil
}

// EncodedSize returns the number of bytes Encode(t, v) would produce.
func EncodedSize(t *Type, v Value) (int, error) {
	return encodedSize(t, v, 0)
}

func encodedSize(t *Type, v Value, depth int) (int, error) {
	if depth > maxDepth {
		return 0, fmt.Errorf("gvariant: maximum nesting depth exceeded")
	}
	switch t.Kind {
	case KindByte:
		return 1, checkType(t, v, uint8(0))
	case KindBoolean:
		return 1, checkType(t, v, false)
	case KindInt16:
		return 2, checkType(t, v, int16(0))
	case KindUint16:
		return 2, checkType(t, v, uint16(0))
	case KindInt32, KindHandle:
		return 4, checkType(t, v, int32(0))
	case KindUint32:
		return 4, checkType(t, v, uint32(0))
	case KindInt64:
		return 8, checkType(t, v, int64(0))
	case KindUint64:
		return 8, checkType(t, v, uint64(0))
	case KindDouble:
		return 8, checkType(t, v, float64(0))
	case KindString, KindObjectPath, KindSignature:
		s, ok := v.(string)
		if !ok {
			return 0, typeError(t, v)
		}
		if !utf8.ValidString(s) {
			return 0, fmt.Errorf("gvariant: invalid UTF-8 string")
		}
		if bytes.IndexByte([]byte(s), 0) >= 0 {
			return 0, fmt.Errorf("gvariant: string contains NUL")
		}
		return len(s) + 1, nil
	case KindVariant:
		av, ok := v.(Variant)
		if !ok || av.Type == nil {
			return 0, typeError(t, v)
		}
		if !av.Type.IsDefinite() {
			return 0, fmt.Errorf("gvariant: variant contains indefinite type %q", av.Type)
		}
		cs, err := encodedSize(av.Type, av.Value, depth+1)
		if err != nil {
			return 0, err
		}
		return cs + 1 + len(av.Type.String()), nil
	case KindMaybe:
		m, ok := v.(Maybe)
		if !ok {
			return 0, typeError(t, v)
		}
		if m.Nothing {
			if t.Elem.isFixedSize() {
				return 0, nil
			}
			return 1, nil
		}
		cs, err := encodedSize(t.Elem, m.Value, depth+1)
		if err != nil {
			return 0, err
		}
		if t.Elem.isFixedSize() {
			return cs, nil
		}
		return 1 + cs, nil
	case KindArray:
		return arraySize(t, v, depth)
	case KindTuple:
		return tupleSize(t, v, depth)
	case KindDictEntry:
		return dictEntrySize(t, v, depth)
	default:
		return 0, fmt.Errorf("gvariant: invalid type kind %d", t.Kind)
	}
}

func arraySize(t *Type, v Value, depth int) (int, error) {
	if b, ok := v.([]byte); ok && t.Elem.Kind == KindByte {
		return len(b), nil
	}
	elems, err := arrayElems(t, v)
	if err != nil {
		return 0, err
	}
	elemSize, fixed := t.Elem.FixedSize()
	if fixed {
		return len(elems) * elemSize, nil
	}
	body := 0
	for _, e := range elems {
		body = alignUp(body, t.Elem.Alignment())
		s, err := encodedSize(t.Elem, e, depth+1)
		if err != nil {
			return 0, err
		}
		body += s
	}
	w := offsetWidthFor(body, len(elems))
	return body + w*len(elems), nil
}

func tupleSize(t *Type, v Value, depth int) (int, error) {
	members := t.members()
	vals, err := tupleValues(t, v)
	if err != nil {
		return 0, err
	}
	if len(vals) != len(members) {
		return 0, fmt.Errorf("gvariant: %s expects %d members, got %d", t, len(members), len(vals))
	}
	body := 0
	nvar := 0
	for i, m := range members {
		body = alignUp(body, m.Alignment())
		s, err := encodedSize(m, vals[i], depth+1)
		if err != nil {
			return 0, err
		}
		body += s
		if _, fixed := m.FixedSize(); !fixed {
			nvar++
		}
	}
	stored := nvar
	if t.isLastMemberVariable() && stored > 0 {
		stored--
	}
	w := offsetWidthFor(body, stored)
	return body + w*stored, nil
}

func dictEntrySize(t *Type, v Value, depth int) (int, error) {
	vals, err := dictEntryValues(v)
	if err != nil {
		return 0, err
	}
	body := alignUp(0, t.Key.Alignment())
	ks, err := encodedSize(t.Key, vals.Key, depth+1)
	if err != nil {
		return 0, err
	}
	body += ks
	body = alignUp(body, t.Value.Alignment())
	vs, err := encodedSize(t.Value, vals.Value, depth+1)
	if err != nil {
		return 0, err
	}
	body += vs
	nvar := 0
	if !t.Key.isFixedSize() {
		nvar++
	}
	if !t.Value.isFixedSize() {
		nvar++
	}
	stored := nvar
	if t.isLastMemberVariable() && stored > 0 {
		stored--
	}
	w := offsetWidthFor(body, stored)
	return body + w*stored, nil
}

// writeValue writes v into buf at off. buf must be large enough.
func writeValue(t *Type, v Value, buf []byte, off, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("gvariant: maximum nesting depth exceeded")
	}
	switch t.Kind {
	case KindByte:
		buf[off] = v.(uint8)
	case KindBoolean:
		if v.(bool) {
			buf[off] = 1
		} else {
			buf[off] = 0
		}
	case KindInt16:
		binary.LittleEndian.PutUint16(buf[off:], uint16(v.(int16)))
	case KindUint16:
		binary.LittleEndian.PutUint16(buf[off:], v.(uint16))
	case KindInt32, KindHandle:
		binary.LittleEndian.PutUint32(buf[off:], uint32(v.(int32)))
	case KindUint32:
		binary.LittleEndian.PutUint32(buf[off:], v.(uint32))
	case KindInt64:
		binary.LittleEndian.PutUint64(buf[off:], uint64(v.(int64)))
	case KindUint64:
		binary.LittleEndian.PutUint64(buf[off:], v.(uint64))
	case KindDouble:
		binary.LittleEndian.PutUint64(buf[off:], math.Float64bits(v.(float64)))
	case KindString, KindObjectPath, KindSignature:
		s := v.(string)
		copy(buf[off:], s)
		buf[off+len(s)] = 0
	case KindVariant:
		av := v.(Variant)
		cs, err := encodedSize(av.Type, av.Value, depth+1)
		if err != nil {
			return err
		}
		if err := writeValue(av.Type, av.Value, buf, off, depth+1); err != nil {
			return err
		}
		buf[off+cs] = 0
		copy(buf[off+cs+1:], av.Type.String())
	case KindMaybe:
		m := v.(Maybe)
		if m.Nothing {
			if !t.Elem.isFixedSize() {
				buf[off] = 0
			}
			return nil
		}
		if !t.Elem.isFixedSize() {
			buf[off] = 1
			off++
		}
		return writeValue(t.Elem, m.Value, buf, off, depth+1)
	case KindArray:
		return writeArray(t, v, buf, off, depth)
	case KindTuple:
		return writeTuple(t, v, buf, off, depth)
	case KindDictEntry:
		return writeDictEntry(t, v, buf, off, depth)
	default:
		return fmt.Errorf("gvariant: invalid type kind %d", t.Kind)
	}
	return nil
}

func writeArray(t *Type, v Value, buf []byte, off, depth int) error {
	if b, ok := v.([]byte); ok && t.Elem.Kind == KindByte {
		copy(buf[off:], b)
		return nil
	}
	elems, err := arrayElems(t, v)
	if err != nil {
		return err
	}
	elemSize, fixed := t.Elem.FixedSize()
	if fixed {
		cur := off
		for _, e := range elems {
			cur = alignUp(cur, t.Elem.Alignment())
			if err := writeValue(t.Elem, e, buf, cur, depth+1); err != nil {
				return err
			}
			cur += elemSize
		}
		return nil
	}
	bodyStart := off
	cur := off
	ends := make([]int, 0, len(elems))
	for _, e := range elems {
		cur = alignUp(cur, t.Elem.Alignment())
		if err := writeValue(t.Elem, e, buf, cur, depth+1); err != nil {
			return err
		}
		s, _ := encodedSize(t.Elem, e, depth+1)
		cur += s
		ends = append(ends, cur-bodyStart)
	}
	body := cur - bodyStart
	w := offsetWidthFor(body, len(elems))
	// The buffer size was computed by encodedSize using the same rule; if
	// these differ it is a programming error, checked by the caller.
	pos := off + body
	for _, e := range ends {
		if pos+w > len(buf) {
			return fmt.Errorf("gvariant: internal error: buffer too small for array offsets")
		}
		putUintLE(buf[pos:], uint64(e), w)
		pos += w
	}
	return nil
}

func writeTuple(t *Type, v Value, buf []byte, off, depth int) error {
	members := t.members()
	vals := v.([]Value)
	cur := off
	varEnds := make([]int, 0, len(members))
	for i, m := range members {
		cur = alignUp(cur, m.Alignment())
		if err := writeValue(m, vals[i], buf, cur, depth+1); err != nil {
			return err
		}
		s, _ := encodedSize(m, vals[i], depth+1)
		cur += s
		if _, fixed := m.FixedSize(); !fixed {
			if !(t.isLastMemberVariable() && i == len(members)-1) {
				varEnds = append(varEnds, cur-off)
			}
		}
	}
	body := cur - off
	stored := len(varEnds)
	w := offsetWidthFor(body, stored)
	pos := off + body
	for i := stored - 1; i >= 0; i-- {
		if pos+w > len(buf) {
			return fmt.Errorf("gvariant: internal error: buffer too small for tuple offsets")
		}
		putUintLE(buf[pos:], uint64(varEnds[i]), w)
		pos += w
	}
	return nil
}

func writeDictEntry(t *Type, v Value, buf []byte, off, depth int) error {
	de, err := dictEntryValues(v)
	if err != nil {
		return err
	}
	cur := alignUp(off, t.Key.Alignment())
	ks, err := encodedSize(t.Key, de.Key, depth+1)
	if err != nil {
		return err
	}
	if err := writeValue(t.Key, de.Key, buf, cur, depth+1); err != nil {
		return err
	}
	varEnds := []int{}
	if !t.Key.isFixedSize() {
		varEnds = append(varEnds, cur+ks-off)
	}
	cur += ks
	cur = alignUp(cur, t.Value.Alignment())
	vs, err := encodedSize(t.Value, de.Value, depth+1)
	if err != nil {
		return err
	}
	if err := writeValue(t.Value, de.Value, buf, cur, depth+1); err != nil {
		return err
	}
	body := cur + vs - off
	stored := len(varEnds)
	w := offsetWidthFor(body, stored)
	pos := off + body
	for i := stored - 1; i >= 0; i-- {
		if pos+w > len(buf) {
			return fmt.Errorf("gvariant: internal error: buffer too small for dict offsets")
		}
		putUintLE(buf[pos:], uint64(varEnds[i]), w)
		pos += w
	}
	return nil
}

// sortDictEntries returns entries sorted by key, as required by GVariant
// normal form for dictionaries.
func sortDictEntries(d Dict) ([]DictEntry, error) {
	out := make([]DictEntry, len(d))
	copy(out, d)
	type keyed struct {
		e   DictEntry
		key []byte
	}
	keys := make([]keyed, len(out))
	for i, e := range out {
		// Keys are almost always strings; handle that without recursion.
		if s, ok := e.Key.(string); ok {
			keys[i] = keyed{e: e, key: []byte(s)}
			continue
		}
		// Fall back to a stable textual form for unusual key types.
		keys[i] = keyed{e: e, key: []byte(fmt.Sprintf("%v", e.Key))}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		return bytes.Compare(keys[i].key, keys[j].key) < 0
	})
	for i := range keys {
		out[i] = keys[i].e
	}
	for i := 1; i < len(out); i++ {
		a, b := keys[i-1].key, keys[i].key
		if bytes.Equal(a, b) {
			return nil, fmt.Errorf("gvariant: duplicate dictionary key %q", a)
		}
	}
	return out, nil
}

func arrayElems(t *Type, v Value) ([]Value, error) {
	if t.Elem.Kind == KindDictEntry {
		d, ok := v.(Dict)
		if !ok {
			return nil, typeError(t, v)
		}
		out := make([]Value, len(d))
		for i, e := range d {
			out[i] = e
		}
		return out, nil
	}
	elems, ok := v.([]Value)
	if !ok {
		return nil, typeError(t, v)
	}
	return elems, nil
}

func tupleValues(t *Type, v Value) ([]Value, error) {
	if t.Kind == KindDictEntry {
		de, err := dictEntryValues(v)
		if err != nil {
			return nil, err
		}
		return []Value{de.Key, de.Value}, nil
	}
	vals, ok := v.([]Value)
	if !ok {
		return nil, typeError(t, v)
	}
	return vals, nil
}

func dictEntryValues(v Value) (DictEntry, error) {
	switch x := v.(type) {
	case DictEntry:
		return x, nil
	case []Value:
		if len(x) == 2 {
			return DictEntry{Key: x[0], Value: x[1]}, nil
		}
	}
	return DictEntry{}, fmt.Errorf("gvariant: dict entry must be DictEntry or []Value of length 2, got %s", typeName(v))
}

func checkType(t *Type, v Value, zero any) error {
	if v == nil {
		return typeError(t, v)
	}
	if _, ok := zero.(uint8); ok {
		if _, ok := v.(uint8); ok {
			return nil
		}
	} else if _, ok := zero.(bool); ok {
		if _, ok := v.(bool); ok {
			return nil
		}
	} else if _, ok := zero.(int16); ok {
		if _, ok := v.(int16); ok {
			return nil
		}
	} else if _, ok := zero.(uint16); ok {
		if _, ok := v.(uint16); ok {
			return nil
		}
	} else if _, ok := zero.(int32); ok {
		if _, ok := v.(int32); ok {
			return nil
		}
	} else if _, ok := zero.(uint32); ok {
		if _, ok := v.(uint32); ok {
			return nil
		}
	} else if _, ok := zero.(int64); ok {
		if _, ok := v.(int64); ok {
			return nil
		}
	} else if _, ok := zero.(uint64); ok {
		if _, ok := v.(uint64); ok {
			return nil
		}
	} else if _, ok := zero.(float64); ok {
		if _, ok := v.(float64); ok {
			return nil
		}
	}
	return typeError(t, v)
}

func typeError(t *Type, v Value) error {
	return fmt.Errorf("gvariant: expected %s, got %s", t, typeName(v))
}

func putUintLE(buf []byte, v uint64, w int) {
	for i := 0; i < w; i++ {
		buf[i] = byte(v >> (8 * i))
	}
}
