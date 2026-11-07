package gvariant

import "fmt"

// Value is a Go representation of a GVariant value. The concrete type
// depends on the GVariant type:
//
//	y -> uint8, n -> int16, q -> uint16, i -> int32, u -> uint32,
//	x -> int64, t -> uint64, h -> int32, d -> float64, b -> bool,
//	s/o/g -> string, v -> Variant, ay -> []byte,
//	array -> []Value (or Dict for a{...}), tuple -> []Value,
//	dict entry -> DictEntry, maybe -> Maybe.
type Value = any

// DictEntry is one entry of a dictionary (array of dict entries).
type DictEntry struct {
	Key   Value
	Value Value
}

// Dict is an array of dictionary entries. It is used for types such as
// a{sv}.
type Dict []DictEntry

// Variant is a GVariant of type 'v': a value plus its type.
type Variant struct {
	Type  *Type
	Value Value
}

// Maybe represents a GVariant maybe type. Nothing is true when no value
// is present.
type Maybe struct {
	Value   Value
	Nothing bool
}

// Get returns the entry for key, if present. Keys are compared by their
// string representation.
func (d Dict) Get(key string) (Value, bool) {
	for _, e := range d {
		if s, ok := e.Key.(string); ok && s == key {
			return e.Value, true
		}
	}
	return nil, false
}

// Set replaces or inserts an entry. It does not sort.
func (d *Dict) Set(key string, value Value) {
	for i, e := range *d {
		if s, ok := e.Key.(string); ok && s == key {
			(*d)[i].Value = value
			return
		}
	}
	*d = append(*d, DictEntry{Key: key, Value: value})
}

// Delete removes an entry, if present.
func (d *Dict) Delete(key string) {
	out := (*d)[:0]
	for _, e := range *d {
		if s, ok := e.Key.(string); ok && s == key {
			continue
		}
		out = append(out, e)
	}
	*d = out
}

func typeName(v Value) string {
	switch v.(type) {
	case nil:
		return "nil"
	case bool:
		return "bool"
	case uint8:
		return "uint8"
	case int16:
		return "int16"
	case uint16:
		return "uint16"
	case int32:
		return "int32"
	case uint32:
		return "uint32"
	case int64:
		return "int64"
	case uint64:
		return "uint64"
	case float64:
		return "float64"
	case string:
		return "string"
	case []byte:
		return "[]byte"
	case []Value:
		return "[]Value"
	case Dict:
		return "Dict"
	case Variant:
		return "Variant"
	case Maybe:
		return "Maybe"
	default:
		return fmt.Sprintf("%T", v)
	}
}
