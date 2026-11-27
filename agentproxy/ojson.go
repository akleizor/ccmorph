package agentproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// obj is a JSON object that keeps its keys in the order it was read: a body the
// proxy rewrites goes on in the order the session wrote it, so the endpoint
// sees the prompt it would have seen without the proxy. Values are *obj, []any,
// string, json.Number, bool or nil.
type obj struct {
	keys []string
	m    map[string]any
}

func newObj() *obj { return &obj{m: map[string]any{}} }

func (o *obj) get(k string) (any, bool) {
	v, ok := o.m[k]
	return v, ok
}

func (o *obj) str(k string) (string, bool) {
	s, ok := o.m[k].(string)
	return s, ok
}

func (o *obj) obj(k string) *obj {
	c, _ := o.m[k].(*obj)
	return c
}

func (o *obj) list(k string) []any {
	l, _ := o.m[k].([]any)
	return l
}

// set puts v under k, last when k is new.
func (o *obj) set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

// del removes k; false when it was not there.
func (o *obj) del(k string) bool {
	if _, ok := o.m[k]; !ok {
		return false
	}
	delete(o.m, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

// clone is a shallow copy: a block changed in place without changing its source.
func (o *obj) clone() *obj {
	c := &obj{keys: append([]string(nil), o.keys...), m: make(map[string]any, len(o.m))}
	for k, v := range o.m {
		c.m[k] = v
	}
	return c
}

// parseJSON reads one JSON value and nothing after it.
func parseJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after the JSON value")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	if d == '{' {
		o := newObj()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(kt.(string), v)
		}
		_, err := dec.Token()
		return o, err
	}
	l := []any{}
	for dec.More() {
		v, err := decodeValue(dec)
		if err != nil {
			return nil, err
		}
		l = append(l, v)
	}
	_, err = dec.Token()
	return l, err
}

// encodeJSON is v as compact UTF-8 JSON, keys in their order.
func encodeJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := writeValue(&buf, enc, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeValue(buf *bytes.Buffer, enc *json.Encoder, v any) error {
	switch x := v.(type) {
	case *obj:
		buf.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeScalar(buf, enc, k); err != nil {
				return err
			}
			buf.WriteByte(':')
			if err := writeValue(buf, enc, x.m[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, enc, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case json.Number:
		buf.WriteString(x.String())
	default:
		return writeScalar(buf, enc, x)
	}
	return nil
}

// writeScalar encodes a string, bool, nil or number through enc, which writes
// into buf; the newline Encode adds is taken back.
func writeScalar(buf *bytes.Buffer, enc *json.Encoder, v any) error {
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Truncate(buf.Len() - 1)
	return nil
}
