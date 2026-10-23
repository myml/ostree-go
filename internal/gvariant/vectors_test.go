package gvariant

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
)

type vectorFile struct {
	Vectors []struct {
		Name string `json:"name"`
		Type string `json:"type"`
		Hex  string `json:"hex"`
		Val  any    `json:"value"`
	} `json:"vectors"`
	Objects []struct {
		Name   string `json:"name"`
		Type   string `json:"type"`
		Hex    string `json:"hex"`
		SHA256 string `json:"sha256"`
	} `json:"objects"`
}

func loadVectors(t *testing.T) *vectorFile {
	t.Helper()
	f, err := os.Open("testdata/vectors.json")
	if err != nil {
		t.Skipf("vectors.json missing: %v", err)
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.UseNumber()
	var vf vectorFile
	if err := dec.Decode(&vf); err != nil {
		t.Fatal(err)
	}
	return &vf
}

func TestGLibVectors(t *testing.T) {
	vf := loadVectors(t)
	if len(vf.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	for _, vec := range vf.Vectors {
		t.Run(vec.Name, func(t *testing.T) {
			typ, err := ParseType(vec.Type)
			if err != nil {
				t.Fatal(err)
			}
			val, err := fromJSON(typ, vec.Val)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Encode(typ, val)
			if err != nil {
				t.Fatal(err)
			}
			want, err := hex.DecodeString(vec.Hex)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("encode mismatch\n got %x\nwant %x", got, want)
			}
			dec, err := Decode(typ, want)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			again, err := Encode(typ, dec)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(again, want) {
				t.Fatalf("round-trip mismatch")
			}
			if !IsNormalForm(typ, want) {
				t.Fatal("not normal form")
			}
		})
	}
}

func TestGLibObjectVectors(t *testing.T) {
	vf := loadVectors(t)
	for _, obj := range vf.Objects {
		t.Run(obj.Name, func(t *testing.T) {
			data, err := hex.DecodeString(obj.Hex)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != obj.SHA256 {
				t.Fatalf("fixture sha256 mismatch")
			}
			typ, err := ParseType(obj.Type)
			if err != nil {
				t.Fatal(err)
			}
			dec, err := Decode(typ, data)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			out, err := Encode(typ, dec)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out, data) {
				t.Fatal("object round-trip mismatch")
			}
		})
	}
}

func fromJSON(t *Type, raw any) (Value, error) {
	switch t.Kind {
	case KindByte:
		n, err := numUint(raw)
		return uint8(n), err
	case KindInt16:
		n, err := numInt(raw)
		return int16(n), err
	case KindUint16:
		n, err := numUint(raw)
		return uint16(n), err
	case KindInt32, KindHandle:
		n, err := numInt(raw)
		return int32(n), err
	case KindUint32:
		n, err := numUint(raw)
		return uint32(n), err
	case KindInt64:
		return numInt(raw)
	case KindUint64:
		return numUint(raw)
	case KindDouble:
		return numFloat(raw)
	case KindBoolean:
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("expected bool, got %T", raw)
		}
		return b, nil
	case KindString, KindObjectPath, KindSignature:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("expected string, got %T", raw)
		}
		return s, nil
	case KindVariant:
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expected variant object, got %T", raw)
		}
		ts, _ := m["type"].(string)
		ct, err := ParseType(ts)
		if err != nil {
			return nil, err
		}
		cv, err := fromJSON(ct, m["value"])
		if err != nil {
			return nil, err
		}
		return Variant{Type: ct, Value: cv}, nil
	case KindArray:
		if t.Elem.Kind == KindByte {
			arr, ok := raw.([]any)
			if !ok {
				return nil, fmt.Errorf("expected byte array, got %T", raw)
			}
			out := make([]byte, len(arr))
			for i, e := range arr {
				n, err := numUint(e)
				if err != nil {
					return nil, err
				}
				out[i] = byte(n)
			}
			return out, nil
		}
		if t.Elem.Kind == KindDictEntry {
			m, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("expected dict object, got %T", raw)
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			d := make(Dict, 0, len(keys))
			for _, k := range keys {
				v, err := fromJSON(t.Elem.Value, m[k])
				if err != nil {
					return nil, err
				}
				d = append(d, DictEntry{Key: k, Value: v})
			}
			return d, nil
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("expected array, got %T", raw)
		}
		out := make([]Value, len(arr))
		for i, e := range arr {
			v, err := fromJSON(t.Elem, e)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case KindTuple:
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("expected tuple array, got %T", raw)
		}
		if len(arr) != len(t.Members) {
			return nil, fmt.Errorf("tuple length %d != %d", len(arr), len(t.Members))
		}
		out := make([]Value, len(arr))
		for i, e := range arr {
			v, err := fromJSON(t.Members[i], e)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case KindDictEntry:
		arr, ok := raw.([]any)
		if !ok || len(arr) != 2 {
			return nil, fmt.Errorf("expected dict entry array, got %T", raw)
		}
		k, err := fromJSON(t.Key, arr[0])
		if err != nil {
			return nil, err
		}
		v, err := fromJSON(t.Value, arr[1])
		if err != nil {
			return nil, err
		}
		return DictEntry{Key: k, Value: v}, nil
	case KindMaybe:
		return nil, fmt.Errorf("maybe not supported")
	default:
		return nil, fmt.Errorf("unsupported kind")
	}
}

func jsonNumber(raw any) (string, bool) {
	switch x := raw.(type) {
	case json.Number:
		return x.String(), true
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case string:
		return x, true
	default:
		return "", false
	}
}

func numInt(raw any) (int64, error) {
	s, ok := jsonNumber(raw)
	if !ok {
		return 0, fmt.Errorf("not a number: %T", raw)
	}
	return strconv.ParseInt(s, 10, 64)
}

func numUint(raw any) (uint64, error) {
	s, ok := jsonNumber(raw)
	if !ok {
		return 0, fmt.Errorf("not a number: %T", raw)
	}
	return strconv.ParseUint(s, 10, 64)
}

func numFloat(raw any) (float64, error) {
	s, ok := jsonNumber(raw)
	if !ok {
		return 0, fmt.Errorf("not a number: %T", raw)
	}
	return strconv.ParseFloat(s, 64)
}
