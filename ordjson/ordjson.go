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
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
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
	switch t := v.(type) {
	case string:
		return quoteString(t), nil
	case *Obj:
		if t == nil {
			return []byte("null"), nil
		}
		return t.MarshalJSON()
	case []any:
		if t == nil {
			return []byte("null"), nil
		}
		var b bytes.Buffer
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			eb, err := encode(e)
			if err != nil {
				return nil, err
			}
			b.Write(eb)
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// quoteString writes s as a JSON string. A lone surrogate Parse kept as WTF-8
// (ED A0..BF 80..BF) is written back as the \uXXXX escape it came from, the
// form Node and Python write, because UTF-8 cannot spell it at all.
func quoteString(s string) []byte {
	var b bytes.Buffer
	b.WriteByte('"')
	plain := 0
	for i := 0; i < len(s); {
		if i+2 < len(s) && s[i] == 0xED && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			b.Write(plainInner(s[plain:i]))
			r := rune(s[i]&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
			fmt.Fprintf(&b, `\u%04x`, r)
			i += 3
			plain = i
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	b.Write(plainInner(s[plain:]))
	b.WriteByte('"')
	return b.Bytes()
}

// plainInner writes the inside of a JSON string, with no quotes.
func plainInner(s string) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil
	}
	out := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if len(out) >= 2 {
		out = out[1 : len(out)-1]
	}
	return out
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
	// A user's editor may have saved Latin-1 bytes. Decoding them would turn
	// each one into U+FFFD and the rewrite would write that back, so refuse
	// the file instead: it is never silently mutated.
	if !utf8.Valid(data) {
		return nil, errors.New("invalid JSON: invalid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	v, err := value(dec, data)
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

// value reads one decoded value. data and the token offsets let a string keep
// the literal it was read from: the standard decoder turns a lone surrogate
// escape into U+FFFD, which a rewrite would then write back into the file.
func value(dec *json.Decoder, data []byte) (any, error) {
	start := dec.InputOffset()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if _, ok := tok.(string); ok {
		return unquote(bytes.TrimLeft(data[start:dec.InputOffset()], " \t\r\n,:")), nil
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	if d == '[' {
		list := []any{}
		for dec.More() {
			v, err := value(dec, data)
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
		start := dec.InputOffset()
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		kraw := data[start:dec.InputOffset()]
		v, err := value(dec, data)
		if err != nil {
			return nil, err
		}
		o.Set(unquote(bytes.TrimLeft(kraw, " \t\r\n,:")), v)
	}
	_, err = dec.Token()
	return o, err
}

// Unescape decodes the escapes of the bytes between a JSON string literal's
// quotes: s is the inside of the literal, not the literal itself. A lone
// surrogate escape is kept as its 3-byte WTF-8 form, so no input is lost and
// the caller can tell it from text; a surrogate pair decodes to one rune; an
// escape without four hex digits decodes as U+0000; an escape cut off at the
// very end decodes to what precedes it. Parse and the shim's canon share this
// one walker (#524), so what one decodes the other quotes back.
func Unescape(s []byte) string {
	if bytes.IndexByte(s, '\\') < 0 {
		return string(s)
	}
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b = append(b, c)
			continue
		}
		i++
		if i >= len(s) {
			break
		}
		switch s[i] {
		case '"', '\\', '/':
			b = append(b, s[i])
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r := hex4(s[i+1:])
			i += 4
			if r >= 0xD800 && r <= 0xDBFF && i+6 < len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
				if r2 := hex4(s[i+3:]); r2 >= 0xDC00 && r2 <= 0xDFFF {
					b = utf8.AppendRune(b, utf16.DecodeRune(r, r2))
					i += 6
					continue
				}
			}
			if r >= 0xD800 && r <= 0xDFFF {
				b = append(b, byte(0xE0|(r>>12)), byte(0x80|((r>>6)&0x3F)), byte(0x80|(r&0x3F)))
			} else {
				b = utf8.AppendRune(b, r)
			}
		default:
			b = append(b, s[i])
		}
	}
	return string(b)
}

// unquote turns a string literal the decoder accepted back into a Go string,
// keeping a lone surrogate as its WTF-8 bytes. A token shorter than two bytes
// has no quotes to strip, so it is returned as it is.
func unquote(raw []byte) string {
	if len(raw) < 2 {
		return string(raw)
	}
	return Unescape(raw[1 : len(raw)-1])
}

func hex4(b []byte) rune {
	if len(b) < 4 {
		return 0
	}
	n, err := strconv.ParseUint(string(b[:4]), 16, 32)
	if err != nil {
		return 0
	}
	return rune(n)
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
// order does not matter, 1 equals 1.0, and distinct integers stay distinct.
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
		// A plain decimal compares exactly, so two integer literals above
		// 2^53 stay distinct. A literal with an exponent keeps the float
		// comparison: big.Rat would allocate by the exponent's size.
		if !strings.ContainsAny(string(x)+string(y), "eE") {
			xr, ok1 := new(big.Rat).SetString(string(x))
			yr, ok2 := new(big.Rat).SetString(string(y))
			return ok1 && ok2 && xr.Cmp(yr) == 0
		}
		f, err1 := x.Float64()
		g, err2 := y.Float64()
		return err1 == nil && err2 == nil && f == g
	}
	return a == b
}
