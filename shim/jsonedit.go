package shim

// Byte-level JSON editing. The shim never decodes a body into a map and
// re-serialises it: that would sort keys and renormalise numbers. Bodies are
// validated once, located by span, and either forwarded untouched or rewritten
// in one streaming pass (canon) that keeps key order.

import (
	"bytes"
	"encoding/json/jsontext"
	"strconv"
	"unicode/utf8"
)

// maxDepth mirrors the nesting Python's json module survives (its recursion
// limit): a payload deeper than this is passed through unchanged.
const maxDepth = 1000

// Span is a half-open byte range of one JSON value inside a body.
type Span struct{ Start, End int }

// Member is one member of a JSON object: its unescaped key and its value span.
type Member struct {
	Key   string
	Value Span
}

// Edit replaces the value at [Start,End) in one canonical pass. Exactly one of
// Raw (verbatim JSON), Text (a string value, escaped by the pass) or Remove (an
// array element, with its separator) applies.
type Edit struct {
	Start, End int
	Raw        []byte
	Text       []byte
	IsText     bool
	Remove     bool
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// scanString returns the index just past the string that opens at b[i].
func scanString(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(b)
}

// scanValue returns the index just past the value that starts at b[i].
func scanValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return scanString(b, i)
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				i = scanString(b, i) - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return len(b)
	}
	for i < len(b) {
		switch b[i] {
		case ',', ']', '}', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// Object lists the members of the object that opens at b[i] and returns the
// index past it. b must be valid JSON.
func Object(b []byte, i int) ([]Member, int) {
	var ms []Member
	i = skipWS(b, i+1)
	for i < len(b) && b[i] != '}' {
		ke := scanString(b, i)
		key := Unquote(b[i:ke])
		i = skipWS(b, ke)
		i = skipWS(b, i+1) // the colon
		ve := scanValue(b, i)
		ms = append(ms, Member{key, Span{i, ve}})
		i = skipWS(b, ve)
		if i < len(b) && b[i] == ',' {
			i = skipWS(b, i+1)
		}
	}
	return ms, i + 1
}

// Array lists the elements of the array that opens at b[i] and returns the
// index past it. b must be valid JSON.
func Array(b []byte, i int) ([]Span, int) {
	var es []Span
	i = skipWS(b, i+1)
	for i < len(b) && b[i] != ']' {
		ve := scanValue(b, i)
		es = append(es, Span{i, ve})
		i = skipWS(b, ve)
		if i < len(b) && b[i] == ',' {
			i = skipWS(b, i+1)
		}
	}
	return es, i + 1
}

// Unquote decodes the JSON string literal in raw (quotes included). A lone
// surrogate escape is kept as its 3-byte WTF-8 form, so no input is lost and
// the caller can tell it from text (see canon).
func Unquote(raw []byte) string {
	if len(raw) < 2 || raw[0] != '"' {
		return ""
	}
	s := raw[1 : len(raw)-1]
	if bytes.IndexByte(s, '\\') < 0 {
		return string(s)
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			out = append(out, c)
			continue
		}
		i++
		switch s[i] {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, _ := hex4(s[i+1:])
			i += 4
			if r >= 0xD800 && r < 0xDC00 && i+6 < len(s)+0 && s[i+1] == '\\' && s[i+2] == 'u' {
				if lo, ok := hex4(s[i+3:]); ok && lo >= 0xDC00 && lo < 0xE000 {
					r = 0x10000 + (r-0xD800)<<10 + (lo - 0xDC00)
					i += 6
				}
			}
			out = appendRune(out, r)
		default: // " \ /
			out = append(out, s[i])
		}
	}
	return string(out)
}

func hex4(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	v, err := strconv.ParseUint(string(s[:4]), 16, 32)
	return rune(v), err == nil
}

// appendRune is utf8.AppendRune except that a surrogate code unit is written
// as its 3-byte (WTF-8) form instead of U+FFFD.
func appendRune(out []byte, r rune) []byte {
	if r >= 0xD800 && r < 0xE000 {
		return append(out, 0xE0|byte(r>>12), 0x80|byte(r>>6)&0x3F, 0x80|byte(r)&0x3F)
	}
	return utf8.AppendRune(out, r)
}

// Quote is the JSON string literal for s, escaped minimally (UTF-8 kept).
func Quote(s string) []byte {
	q, _ := jsontext.AppendQuote(nil, s)
	return q
}

// Apply returns b with the Raw edits (ascending, non-overlapping) spliced in.
func Apply(b []byte, edits []Edit) []byte {
	out := make([]byte, 0, len(b))
	at := 0
	for _, e := range edits {
		out = append(out, b[at:e.Start]...)
		out = append(out, e.Raw...)
		at = e.End
	}
	return append(out, b[at:]...)
}

// validObject reports whether b is one JSON object Python's json.loads would
// accept: strict UTF-8 on the wire, duplicate names allowed, and lone surrogate
// escapes allowed.
func validJSON(b []byte) bool {
	return utf8.Valid(b) &&
		jsontext.Value(b).IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
}

// doc is a validated JSON object body with its top-level members located.
type doc struct {
	b  []byte
	ms []Member
}

// parseDoc is nil unless b is a valid JSON object.
func parseDoc(b []byte) *doc {
	if len(b) == 0 || !validJSON(b) {
		return nil
	}
	i := skipWS(b, 0)
	if b[i] != '{' {
		return nil
	}
	ms, _ := Object(b, i)
	return &doc{b, ms}
}

// last is the value span of the last member named key (JSON's duplicate rule).
func (d *doc) last(key string) (Span, bool) { return lastMember(d.ms, key) }

func lastMember(ms []Member, key string) (Span, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Key == key {
			return ms[i].Value, true
		}
	}
	return Span{}, false
}

func lastIndex(ms []Member, key string) int {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Key == key {
			return i
		}
	}
	return -1
}

// str is the unescaped string at span s of b, if it is one.
func str(b []byte, s Span) (string, bool) {
	if b[s.Start] != '"' {
		return "", false
	}
	return Unquote(b[s.Start:s.End]), true
}

func (d *doc) str(key string) (string, bool) {
	s, ok := d.last(key)
	if !ok {
		return "", false
	}
	return str(d.b, s)
}

// canon re-emits JSON the way Python's json.dumps does, with the edits applied:
// no whitespace (or ", " and ": " when spaced), strings escaped minimally, or
// as \uXXXX when ascii, member order untouched, numbers kept as written.
type canon struct {
	ascii, spaced bool
	lone          bool // a lone surrogate met while writing raw UTF-8
}

type frame struct {
	object    bool
	n         int
	expectKey bool
}

// encode returns the canonical form of src, or ok=false when src nests deeper
// than maxDepth or (when !ascii) holds a lone surrogate, which only the ascii
// form can carry.
func (c *canon) encode(src []byte, edits []Edit) (out []byte, ok bool) {
	out = make([]byte, 0, len(src)+len(src)/16+64)
	var stack []frame
	sep, colon := ",", ":"
	if c.spaced {
		sep, colon = ", ", ": "
	}
	e := 0
	done := func() { // a value ended
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	item := func() { // an array element or object key begins
		if n := len(stack); n > 0 {
			if stack[n-1].n > 0 {
				out = append(out, sep...)
			}
			stack[n-1].n++
		}
	}
	for i := 0; i < len(src); {
		ch := src[i]
		switch ch {
		case ' ', '\t', '\n', '\r', ',', ':':
			i++
			continue
		case '}', ']':
			out = append(out, ch)
			stack = stack[:len(stack)-1]
			i++
			done()
			continue
		}
		top := len(stack) - 1
		isKey := top >= 0 && stack[top].object && stack[top].expectKey
		if !isKey {
			if e < len(edits) && edits[e].Start == i {
				ed := edits[e]
				e++
				i = ed.End
				if ed.Remove {
					continue
				}
				if top >= 0 && !stack[top].object {
					item()
				}
				if ed.IsText {
					if !c.str(&out, ed.Text) {
						return nil, false
					}
				} else {
					out = append(out, ed.Raw...)
				}
				done()
				continue
			}
			if top >= 0 && !stack[top].object {
				item()
			}
		}
		switch ch {
		case '{', '[':
			if len(stack) >= maxDepth {
				return nil, false
			}
			out = append(out, ch)
			stack = append(stack, frame{object: ch == '{', expectKey: true})
			i++
		case '"':
			end := scanString(src, i)
			if isKey {
				item()
			}
			if !c.str(&out, unquoteBytes(src[i:end])) {
				return nil, false
			}
			if isKey {
				out = append(out, colon...)
				stack[top].expectKey = false
			} else {
				done()
			}
			i = end
		default:
			end := scanValue(src, i)
			out = append(out, src[i:end]...)
			i = end
			done()
		}
	}
	return out, true
}

// unquoteBytes is Unquote without the string copy when nothing is escaped.
func unquoteBytes(raw []byte) []byte {
	if bytes.IndexByte(raw, '\\') < 0 {
		return raw[1 : len(raw)-1]
	}
	return []byte(Unquote(raw))
}

// str writes the string whose unescaped (UTF-8 or WTF-8) text is s as a quoted
// literal; false when raw UTF-8 mode meets a lone surrogate.
func (c *canon) str(out *[]byte, s []byte) bool {
	o := append(*out, '"')
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == '"':
			o = append(o, '\\', '"')
		case b == '\\':
			o = append(o, '\\', '\\')
		case b == '\n':
			o = append(o, '\\', 'n')
		case b == '\r':
			o = append(o, '\\', 'r')
		case b == '\t':
			o = append(o, '\\', 't')
		case b == '\b':
			o = append(o, '\\', 'b')
		case b == '\f':
			o = append(o, '\\', 'f')
		case b < 0x20 || (b == 0x7f && c.ascii):
			o = append(o, '\\', 'u', '0', '0', hexd[b>>4], hexd[b&15])
		case b < 0x80:
			o = append(o, b)
		default:
			r, size := utf8.DecodeRune(s[i:])
			if r == utf8.RuneError && size <= 1 {
				// WTF-8 surrogate (ED A0..BF xx) or stray byte.
				if b == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 {
					r = rune(b&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
					size = 3
					if !c.ascii {
						c.lone = true
						return false
					}
				} else {
					size = 1
				}
			}
			switch {
			case !c.ascii:
				o = append(o, s[i:i+size]...)
			case r >= 0x10000:
				r -= 0x10000
				o = appendU(appendU(o, 0xD800+(r>>10)), 0xDC00+(r&0x3FF))
			default:
				o = appendU(o, r)
			}
			i += size
			continue
		}
		i++
	}
	*out = append(o, '"')
	return true
}

const hexd = "0123456789abcdef"

func appendU(o []byte, r rune) []byte {
	return append(o, '\\', 'u', hexd[r>>12&15], hexd[r>>8&15], hexd[r>>4&15], hexd[r&15])
}

// encodeBody is canon for a whole request body: raw UTF-8 first, falling back
// to the ascii form for a lone surrogate (which cannot be written as UTF-8).
func encodeBody(src []byte, edits []Edit, ascii bool) ([]byte, bool) {
	c := canon{ascii: ascii}
	out, ok := c.encode(src, edits)
	if !ok && c.lone {
		c = canon{ascii: true}
		out, ok = c.encode(src, edits)
	}
	return out, ok
}
