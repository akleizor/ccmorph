// Package ordjson decodes JSON so an object keeps its key order, and writes it
// back the same way. It exists for files a user also edits (settings.json):
// rewriting one must not reshuffle keys the user placed. Numbers stay as the
// json.Number literal they were read as. Machine-only data should use
// encoding/json and map[string]any instead.
package ordjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Obj is a JSON object whose keys keep the order they were read or added in.
type Obj struct {
	keys []string
	m    map[string]any
}

// New returns an empty object.
func New() *Obj { return &Obj{m: map[string]any{}} }

// Get returns the value under k and whether it exists.
func (o *Obj) Get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }

// Has reports whether k exists.
func (o *Obj) Has(k string) bool { _, ok := o.m[k]; return ok }

// Set stores v; a new key goes last, an existing key keeps its place.
func (o *Obj) Set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

// Delete removes k.
func (o *Obj) Delete(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			return
		}
	}
}

// Keys returns the keys in order.
func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

// Len is the number of keys.
func (o *Obj) Len() int { return len(o.keys) }

// MarshalJSON writes the keys in order.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := encode(k)
		if err != nil {
			return nil, err
		}
		vb, err := encode(o.m[k])
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// Marshal encodes v (no trailing newline), indented two spaces when indent is
// set. v holds what Parse returns plus Go strings, ints and bools, which always
// encode; anything else encodes as null.
func Marshal(v any, indent bool) []byte {
	b, err := encode(v)
	if err != nil {
		return []byte("null")
	}
	if indent {
		var out bytes.Buffer
		if json.Indent(&out, b, "", "  ") == nil {
			return out.Bytes()
		}
	}
	return b
}

// Parse decodes one JSON document: objects become *Obj, arrays []any, numbers
// json.Number. A byte-order mark is the caller's to strip.
func Parse(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := value(dec)
	if err == nil {
		if _, err = dec.Token(); err == io.EOF {
			return v, nil
		} else if err == nil {
			err = errors.New("extra data after the document")
		}
	}
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, fmt.Errorf("invalid JSON: %w", err)
}

func value(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		list := []any{}
		for dec.More() {
			v, err := value(dec)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	o := New()
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return nil, err
		}
		v, err := value(dec)
		if err != nil {
			return nil, err
		}
		o.Set(k.(string), v)
	}
	_, err = dec.Token()
	return o, err
}

// Clone deep-copies a decoded value.
func Clone(v any) any {
	switch t := v.(type) {
	case *Obj:
		c := New()
		for _, k := range t.keys {
			c.Set(k, Clone(t.m[k]))
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, e := range t {
			c[i] = Clone(e)
		}
		return c
	}
	return v
}

// Equal compares decoded values the way a reader of the file would: object key
// order does not matter and 1 equals 1.0.
func Equal(a, b any) bool {
	switch x := a.(type) {
	case *Obj:
		y, ok := b.(*Obj)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for k, v := range x.m {
			w, ok := y.m[k]
			if !ok || !Equal(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		if x == y {
			return true
		}
		f, err1 := x.Float64()
		g, err2 := y.Float64()
		return err1 == nil && err2 == nil && f == g
	}
	return a == b
}
