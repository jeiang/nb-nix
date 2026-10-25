package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Doc is a JSON object decoded with json.Number for numbers.
type Doc = map[string]any

// toDoc round-trips v through JSON into generic values.
func toDoc(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// hashOf returns a stable hash of a JSON value (encoding/json sorts map keys).
func hashOf(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var unmarshalerType = reflect.TypeFor[json.Unmarshaler]()

// project keeps only the parts of v that the Go type t (an API request type)
// has JSON fields for. It turns a response document into request shape.
func project(v any, t reflect.Type) any {
	if v == nil {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return v // union or custom type: keep as is
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		out := map[string]any{}
		for i := range t.NumField() {
			f := t.Field(i)
			name := jsonName(f)
			if name == "" {
				continue
			}
			if fv, ok := m[name]; ok {
				out[name] = project(fv, f.Type)
			}
		}
		return out
	case reflect.Slice:
		l, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = project(e, t.Elem())
		}
		return out
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		out := map[string]any{}
		for k, e := range m {
			out[k] = project(e, t.Elem())
		}
		return out
	default:
		return v
	}
}

func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "" {
		return f.Name
	}
	return name
}

// overlay returns base with top's values on top. Objects merge key by key;
// lists of objects merge element by element (the result has top's length);
// any other value in top replaces the base value.
func overlay(base, top any) any {
	switch t := top.(type) {
	case map[string]any:
		b, _ := base.(map[string]any)
		out := make(map[string]any, len(b)+len(t))
		for k, v := range b {
			out[k] = v
		}
		for k, v := range t {
			out[k] = overlay(b[k], v)
		}
		return out
	case []any:
		b, _ := base.([]any)
		out := make([]any, len(t))
		for i, v := range t {
			if _, isObj := v.(map[string]any); isObj && i < len(b) {
				out[i] = overlay(b[i], v)
			} else {
				out[i] = v
			}
		}
		return out
	default:
		return top
	}
}

// deepCopy copies a JSON value.
func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = deepCopy(e)
		}
		return out
	default:
		return v
	}
}

// path is a parsed reference path such as "rules[].sources[]" or
// "rules[].authorized_groups{}" (map keys).
func parsePath(p string) []string {
	var segs []string
	for _, part := range strings.Split(p, ".") {
		for {
			if name, rest, ok := strings.Cut(part, "[]"); ok {
				if name != "" {
					segs = append(segs, name)
				}
				segs = append(segs, "[]")
				part = rest
				continue
			}
			if name, rest, ok := strings.Cut(part, "{}"); ok {
				if name != "" {
					segs = append(segs, name)
				}
				segs = append(segs, "{}")
				part = rest
				continue
			}
			if part != "" {
				segs = append(segs, part)
			}
			break
		}
	}
	return segs
}

// walkStrings calls fn for every string at path in v and stores its result.
// parent is the innermost object that contains the value (for map keys, the
// map's parent object). It returns the rewritten value.
func walkStrings(v any, segs []string, parent map[string]any, fn func(s string, parent map[string]any) (string, error)) (any, error) {
	if len(segs) == 0 {
		s, ok := v.(string)
		if !ok {
			return v, nil
		}
		return fn(s, parent)
	}
	switch segs[0] {
	case "[]":
		l, ok := v.([]any)
		if !ok {
			return v, nil
		}
		out := make([]any, len(l))
		for i, e := range l {
			r, err := walkStrings(e, segs[1:], parent, fn)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case "{}":
		m, ok := v.(map[string]any)
		if !ok {
			return v, nil
		}
		out := make(map[string]any, len(m))
		for k, e := range m {
			nk, err := fn(k, parent)
			if err != nil {
				return nil, err
			}
			out[nk] = e
		}
		return out, nil
	default:
		m, ok := v.(map[string]any)
		if !ok {
			return v, nil
		}
		e, ok := m[segs[0]]
		if !ok {
			return v, nil
		}
		r, err := walkStrings(e, segs[1:], m, fn)
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, len(m))
		for k, x := range m {
			out[k] = x
		}
		out[segs[0]] = r
		return out, nil
	}
}

// sortAt sorts the string list found at path (a path ending in "[]") so that
// unordered lists compare equal.
func sortAt(v any, segs []string) any {
	if len(segs) == 0 {
		return v
	}
	if len(segs) == 1 && segs[0] == "[]" {
		l, ok := v.([]any)
		if !ok {
			return v
		}
		out := append([]any(nil), l...)
		sort.SliceStable(out, func(i, j int) bool { return fmt.Sprint(out[i]) < fmt.Sprint(out[j]) })
		return out
	}
	switch segs[0] {
	case "[]":
		l, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = sortAt(e, segs[1:])
		}
		return out
	case "{}":
		return v
	default:
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		e, ok := m[segs[0]]
		if !ok {
			return v
		}
		out := make(map[string]any, len(m))
		for k, x := range m {
			out[k] = x
		}
		out[segs[0]] = sortAt(e, segs[1:])
		return out
	}
}

// Change is one differing value between the live and desired documents.
type Change struct {
	Path string `json:"path"`
	Old  any    `json:"old"`
	New  any    `json:"new"`
}

// diff lists the differences between a and b. Missing keys and null compare
// equal, as do empty and missing lists.
func diff(prefix string, a, b any) []Change {
	if isEmpty(a) && isEmpty(b) {
		return nil
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := map[string]struct{}{}
		for k := range am {
			keys[k] = struct{}{}
		}
		for k := range bm {
			keys[k] = struct{}{}
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		var out []Change
		for _, k := range sorted {
			out = append(out, diff(join(prefix, k), am[k], bm[k])...)
		}
		return out
	}
	al, alok := a.([]any)
	bl, blok := b.([]any)
	if alok && blok && len(al) == len(bl) && allObjects(al) && allObjects(bl) {
		var out []Change
		for i := range al {
			out = append(out, diff(fmt.Sprintf("%s[%d]", prefix, i), al[i], bl[i])...)
		}
		return out
	}
	if equalJSON(a, b) {
		return nil
	}
	return []Change{{Path: prefix, Old: a, New: b}}
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

func allObjects(l []any) bool {
	for _, e := range l {
		if _, ok := e.(map[string]any); !ok {
			return false
		}
	}
	return true
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		for _, e := range t {
			if !isEmpty(e) {
				return false
			}
		}
		return true
	}
	return false
}

func equalJSON(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// get returns the value at a dotted path of object keys.
func get(v any, path string) any {
	for _, k := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

// numberOf returns a JSON number as float64.
func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
