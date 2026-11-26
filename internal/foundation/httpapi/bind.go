package httpapi

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// The generic binding (since V2.106): a route's body is the context's own api command, its query parameters the api
// query, and its response the api result, each in snake_case JSON. A field's key is its json tag when it has one and
// otherwise Snake of its Go name; an anonymous struct field's fields are the outer struct's.

// Snake is the snake_case key of a Go field name: a run of capitals is one word (HBL is hbl, VesselIMO vessel_imo),
// digits stay with the word before them (SHA256 is sha256, Line2Vessel line2_vessel), and a plural s after a run of
// capitals stays with it (OrderIDs is order_ids).
func Snake(name string) string {
	r := []rune(name)
	var b strings.Builder
	for i, c := range r {
		if i > 0 && unicode.IsUpper(c) {
			prev := r[i-1]
			nextLower := i+1 < len(r) && unicode.IsLower(r[i+1])
			pluralRun := nextLower && r[i+1] == 's' && (i+2 == len(r) || unicode.IsUpper(r[i+2])) && unicode.IsUpper(prev)
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower && !pluralRun) {
				b.WriteByte('_')
			}
		}
		b.WriteRune(unicode.ToLower(c))
	}
	return b.String()
}

// field is one key of a struct as the binding reads and writes it.
type field struct {
	key       string
	index     []int
	omitEmpty bool
}

var fieldCache sync.Map // reflect.Type -> []field

// fieldsOf lists a struct type's keys in declaration order.
func fieldsOf(t reflect.Type) []field {
	if f, ok := fieldCache.Load(t); ok {
		return f.([]field)
	}
	var out []field
	var walk func(t reflect.Type, prefix []int)
	walk = func(t reflect.Type, prefix []int) {
		for i := range t.NumField() {
			sf := t.Field(i)
			index := append(append([]int(nil), prefix...), i)
			tag, hasTag := sf.Tag.Lookup("json")
			name, opts, _ := strings.Cut(tag, ",")
			if name == "-" && opts == "" {
				continue
			}
			if sf.Anonymous && !hasTag && sf.Type.Kind() == reflect.Struct {
				walk(sf.Type, index)
				continue
			}
			if !sf.IsExported() {
				continue
			}
			if name == "" {
				name = Snake(sf.Name)
			}
			out = append(out, field{key: name, index: index, omitEmpty: strings.Contains(","+opts+",", ",omitempty,") || strings.Contains(","+opts+",", ",omitzero,")})
		}
	}
	walk(t, nil)
	fieldCache.Store(t, out)
	return out
}

// keyOf answers the key of the Go field name in a struct type, "" when it has none.
func keyOf(t reflect.Type, goName string) string {
	sf, ok := t.FieldByName(goName)
	if !ok {
		return ""
	}
	for _, f := range fieldsOf(t) {
		if slicesEqual(f.index, sf.Index) {
			return f.key
		}
	}
	return ""
}

func slicesEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var (
	unmarshalerType     = reflect.TypeFor[json.Unmarshaler]()
	marshalerType       = reflect.TypeFor[json.Marshaler]()
	textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// bindable panics when a type cannot be bound: an interface, a channel, a function or a map not keyed by strings. It
// runs when a route is registered.
func bindable(t reflect.Type, seen map[reflect.Type]bool) {
	if seen[t] {
		return
	}
	seen[t] = true
	if t.Implements(marshalerType) || reflect.PointerTo(t).Implements(unmarshalerType) {
		return
	}
	switch t.Kind() {
	case reflect.Interface, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128, reflect.Uintptr:
		panic(fmt.Sprintf("httpapi: %s cannot be bound", t))
	case reflect.Pointer, reflect.Slice, reflect.Array:
		bindable(t.Elem(), seen)
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			panic(fmt.Sprintf("httpapi: %s is not keyed by strings", t))
		}
		bindable(t.Elem(), seen)
	case reflect.Struct:
		for _, f := range fieldsOf(t) {
			bindable(t.FieldByIndex(f.index).Type, seen)
		}
	}
}

// bindError is what a body or a parameter has wrong, for the problem's detail.
type bindError struct{ msg string }

func (e bindError) Error() string { return e.msg }

func wrong(path string) error { return bindError{fmt.Sprintf("field %q has the wrong type", path)} }

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// text refuses a string with a control character other than a line break or a tab.
func text(path, s string) error {
	for _, c := range s {
		if unicode.IsControl(c) && c != '\n' && c != '\r' && c != '\t' {
			return bindError{fmt.Sprintf("field %q has a control character", path)}
		}
	}
	return nil
}

// decodeBody reads raw (checked by checkKeys) into v, a pointer to a struct; skip names the top-level keys the server
// sets, which a body may not send.
func decodeBody(raw []byte, v reflect.Value, skip map[string]bool) error {
	if !utf8.Valid(raw) {
		return bindError{"the body is not UTF-8"}
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var top map[string]any
	if err := d.Decode(&top); err != nil {
		return bindError{"send one JSON object"}
	}
	for k := range skip {
		if _, sent := top[k]; sent {
			return bindError{fmt.Sprintf("unknown field %q", k)}
		}
	}
	return assign(top, v.Elem(), "")
}

// assign sets v from a decoded JSON value x.
func assign(x any, v reflect.Value, path string) error {
	t := v.Type()
	if x == nil {
		switch t.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map:
			v.SetZero()
			return nil
		}
		return wrong(path)
	}
	if reflect.PointerTo(t).Implements(unmarshalerType) && t.Kind() != reflect.Pointer {
		raw, err := json.Marshal(x)
		if err != nil {
			return wrong(path)
		}
		if err = v.Addr().Interface().(json.Unmarshaler).UnmarshalJSON(raw); err != nil {
			return wrong(path)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		if err := assign(x, p.Elem(), path); err != nil {
			return err
		}
		v.Set(p)
	case reflect.Struct:
		m, ok := x.(map[string]any)
		if !ok {
			return wrong(path)
		}
		byKey := map[string]field{}
		for _, f := range fieldsOf(t) {
			byKey[f.key] = f
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			f, known := byKey[k]
			if !known {
				return bindError{fmt.Sprintf("unknown field %q", join(path, k))}
			}
			if err := assign(m[k], v.FieldByIndex(f.index), join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			s, ok := x.(string)
			if !ok {
				return wrong(path)
			}
			raw, _ := json.Marshal(s)
			p := reflect.New(t)
			if err := json.Unmarshal(raw, p.Interface()); err != nil {
				return wrong(path)
			}
			v.Set(p.Elem())
			return nil
		}
		a, ok := x.([]any)
		if !ok {
			return wrong(path)
		}
		s := reflect.MakeSlice(t, len(a), len(a))
		for i, e := range a {
			if err := assign(e, s.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		v.Set(s)
	case reflect.Array:
		a, ok := x.([]any)
		if !ok || len(a) != t.Len() {
			return wrong(path)
		}
		for i, e := range a {
			if err := assign(e, v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := x.(map[string]any)
		if !ok {
			return wrong(path)
		}
		out := reflect.MakeMapWithSize(t, len(m))
		for k, e := range m {
			if err := text(path, k); err != nil {
				return err
			}
			ev := reflect.New(t.Elem()).Elem()
			if err := assign(e, ev, join(path, k)); err != nil {
				return err
			}
			out.SetMapIndex(reflect.ValueOf(k).Convert(t.Key()), ev)
		}
		v.Set(out)
	case reflect.String:
		s, ok := x.(string)
		if !ok {
			return wrong(path)
		}
		if err := text(path, s); err != nil {
			return err
		}
		v.SetString(s)
	case reflect.Bool:
		b, ok := x.(bool)
		if !ok {
			return wrong(path)
		}
		v.SetBool(b)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := x.(json.Number)
		if !ok {
			return wrong(path)
		}
		i, err := strconv.ParseInt(n.String(), 10, t.Bits())
		if err != nil {
			return wrong(path)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, ok := x.(json.Number)
		if !ok {
			return wrong(path)
		}
		u, err := strconv.ParseUint(n.String(), 10, t.Bits())
		if err != nil {
			return wrong(path)
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		n, ok := x.(json.Number)
		if !ok {
			return wrong(path)
		}
		f, err := strconv.ParseFloat(n.String(), t.Bits())
		if err != nil {
			return wrong(path)
		}
		v.SetFloat(f)
	default:
		return wrong(path)
	}
	return nil
}

// setParam sets a field from a path or query parameter's text.
func setParam(v reflect.Value, s, name string) error {
	if err := text(name, s); err != nil {
		return err
	}
	if reflect.PointerTo(v.Type()).Implements(textUnmarshalerType) {
		if err := v.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(s)); err != nil {
			return bindError{fmt.Sprintf("parameter %q has the wrong form", name)}
		}
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return bindError{fmt.Sprintf("parameter %q is a whole number", name)}
		}
		v.SetInt(i)
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return bindError{fmt.Sprintf("parameter %q is true or false", name)}
		}
		v.SetBool(b)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		if err := setParam(p.Elem(), s, name); err != nil {
			return err
		}
		v.Set(p)
	default:
		return bindError{fmt.Sprintf("parameter %q cannot be given in a path or query", name)}
	}
	return nil
}

// encode writes v as JSON with snake_case keys in declaration order.
func encode(buf *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		buf.WriteString("null")
		return nil
	}
	t := v.Type()
	if t.Kind() == reflect.Pointer || t.Kind() == reflect.Interface {
		if v.IsNil() {
			buf.WriteString("null")
			return nil
		}
		if t.Kind() == reflect.Pointer && !t.Implements(marshalerType) {
			return encode(buf, v.Elem())
		}
		if t.Kind() == reflect.Interface {
			return encode(buf, v.Elem())
		}
	}
	if t.Implements(marshalerType) {
		raw, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		buf.Write(raw)
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		buf.WriteByte('{')
		first := true
		for _, f := range fieldsOf(t) {
			fv := v.FieldByIndex(f.index)
			if f.omitEmpty && fv.IsZero() {
				continue
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			key, _ := json.Marshal(f.key)
			buf.Write(key)
			buf.WriteByte(':')
			if err := encode(buf, fv); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 && t.Kind() == reflect.Slice {
			raw, err := json.Marshal(v.Interface())
			if err != nil {
				return err
			}
			buf.Write(raw)
			return nil
		}
		buf.WriteByte('[')
		for i := range v.Len() {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encode(buf, v.Index(i)); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			key, _ := json.Marshal(k.String())
			buf.Write(key)
			buf.WriteByte(':')
			if err := encode(buf, v.MapIndex(k)); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		raw, err := json.Marshal(v.Interface())
		if err != nil {
			return err
		}
		buf.Write(raw)
	}
	return nil
}

// Encode answers v as JSON in the generic binding's keys.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encode(&buf, reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var errNotStruct = errors.New("httpapi: a bound command or query is a struct")
