// Package pycompat holds the pieces of Python's behaviour that ccmorph's file
// formats depend on: an order-preserving JSON model that decodes like json.loads
// and encodes like json.dumps, and Python-style OSError wording. The Go ports of
// the Python helpers (settings-compose, model-window, refresh-model-registry) share it
// so their files stay byte-identical with what the Python helpers wrote.
package pycompat

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Value is nil, bool, string, Num, []any or *Obj.
type Value = any

// Num is a JSON number kept as text so big integers and float spelling survive.
// Lit is already normalised the way Python would print it.
type Num struct {
	Lit   string
	Float bool
}

// Int builds an integer Num.
func Int(n int64) Num { return Num{Lit: strconv.FormatInt(n, 10)} }

// Int64 returns the value when it is an integer that fits int64.
func (n Num) Int64() (int64, bool) {
	if n.Float {
		return 0, false
	}
	v, err := strconv.ParseInt(n.Lit, 10, 64)
	return v, err == nil
}

// F64 is the number as a float64.
func (n Num) F64() float64 {
	switch n.Lit {
	case "NaN":
		return math.NaN()
	case "Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(n.Lit, 64)
	return f
}

// Obj is a JSON object that keeps insertion order, like a Python dict.
type Obj struct {
	keys []string
	m    map[string]Value
}

// NewObj returns an empty object.
func NewObj() *Obj { return &Obj{m: map[string]Value{}} }

// Get returns the value and whether the key is present.
func (o *Obj) Get(k string) (Value, bool) { v, ok := o.m[k]; return v, ok }

// Has reports whether k is present.
func (o *Obj) Has(k string) bool { _, ok := o.m[k]; return ok }

// Set stores v; a new key goes last, an existing key keeps its place.
func (o *Obj) Set(k string, v Value) {
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
			break
		}
	}
}

// Keys returns a copy of the keys in order.
func (o *Obj) Keys() []string { return append([]string(nil), o.keys...) }

// Len is the number of keys.
func (o *Obj) Len() int { return len(o.keys) }

// Clone deep-copies a value.
func Clone(v Value) Value {
	switch t := v.(type) {
	case *Obj:
		c := NewObj()
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

// Equal is Python's ==: dicts ignore order, and True == 1.
func Equal(a, b Value) bool {
	if x, ok := a.(bool); ok {
		a = boolNum(x)
	}
	if x, ok := b.(bool); ok {
		b = boolNum(x)
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case Num:
		y, ok := b.(Num)
		if !ok {
			return false
		}
		if !x.Float && !y.Float {
			return x.Lit == y.Lit
		}
		return x.F64() == y.F64()
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
	case *Obj:
		y, ok := b.(*Obj)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for _, k := range x.keys {
			w, ok := y.m[k]
			if !ok || !Equal(x.m[k], w) {
				return false
			}
		}
		return true
	}
	return false
}

func boolNum(b bool) Num {
	if b {
		return Int(1)
	}
	return Int(0)
}

// MaxDepth stands in for Python's recursion limit while decoding.
const MaxDepth = 1000

// DepthError is Python's RecursionError while decoding or walking a value.
type DepthError struct{}

func (DepthError) Error() string {
	return "maximum recursion depth exceeded while decoding a JSON document"
}

// SyntaxError is json.JSONDecodeError (a ValueError); Msg carries Python's wording.
type SyntaxError struct{ Msg string }

func (e *SyntaxError) Error() string { return e.Msg }

// StripBOM drops a leading UTF-8 BOM, as json.loads does for bytes (never for str).
func StripBOM(data []byte) []byte { return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) }

// Parse decodes data like json.loads: invalid UTF-8 and bad syntax are
// *SyntaxError, nesting past MaxDepth is DepthError.
func Parse(data []byte) (Value, error) {
	if !utf8.Valid(data) {
		pos := 0
		for pos < len(data) {
			r, size := utf8.DecodeRune(data[pos:])
			if r == utf8.RuneError && size == 1 {
				break
			}
			pos += size
		}
		return nil, &SyntaxError{fmt.Sprintf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", data[pos], utf8.RuneCount(data[:pos]))}
	}
	p := &parser{s: string(data)}
	p.ws()
	v := p.value(0)
	if p.err != nil {
		return nil, p.err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, p.fail("Extra data", p.i)
	}
	return v, nil
}

type parser struct {
	s   string
	i   int
	err error
}

func (p *parser) fail(msg string, at int) error {
	if p.err != nil {
		return p.err
	}
	runes := utf8.RuneCountInString(p.s[:at])
	line := strings.Count(p.s[:at], "\n") + 1
	col := runes + 1
	if nl := strings.LastIndex(p.s[:at], "\n"); nl >= 0 {
		col = utf8.RuneCountInString(p.s[nl+1:at]) + 1
	}
	p.err = &SyntaxError{fmt.Sprintf("%s: line %d column %d (char %d)", msg, line, col, runes)}
	return p.err
}

func (p *parser) ws() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *parser) value(depth int) Value {
	if p.err != nil {
		return nil
	}
	if p.i >= len(p.s) {
		p.fail("Expecting value", p.i)
		return nil
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case strings.HasPrefix(p.s[p.i:], "null"):
		p.i += 4
		return nil
	case strings.HasPrefix(p.s[p.i:], "true"):
		p.i += 4
		return true
	case strings.HasPrefix(p.s[p.i:], "false"):
		p.i += 5
		return false
	case strings.HasPrefix(p.s[p.i:], "NaN"):
		p.i += 3
		return Num{Lit: "NaN", Float: true}
	case strings.HasPrefix(p.s[p.i:], "Infinity"):
		p.i += 8
		return Num{Lit: "Infinity", Float: true}
	case strings.HasPrefix(p.s[p.i:], "-Infinity"):
		p.i += 9
		return Num{Lit: "-Infinity", Float: true}
	}
	return p.number()
}

func (p *parser) number() Value {
	start, i := p.i, p.i
	if i < len(p.s) && p.s[i] == '-' {
		i++
	}
	digits := func() int {
		n := 0
		for i < len(p.s) && p.s[i] >= '0' && p.s[i] <= '9' {
			i++
			n++
		}
		return n
	}
	if i < len(p.s) && p.s[i] == '0' {
		i++
	} else if digits() == 0 {
		p.fail("Expecting value", start)
		return nil
	}
	float := false
	if i+1 < len(p.s) && p.s[i] == '.' && p.s[i+1] >= '0' && p.s[i+1] <= '9' {
		i++
		digits()
		float = true
	}
	if i < len(p.s) && (p.s[i] == 'e' || p.s[i] == 'E') {
		j := i + 1
		if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
			j++
		}
		if j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			i = j
			digits()
			float = true
		}
	}
	lit := p.s[start:i]
	p.i = i
	if !float {
		if lit == "-0" {
			lit = "0"
		}
		return Num{Lit: lit}
	}
	f, _ := strconv.ParseFloat(lit, 64) // overflow gives +-Inf, as Python's float() does
	return Num{Lit: FloatRepr(f), Float: true}
}

func (p *parser) str() string {
	start := p.i
	p.i++
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			p.fail("Unterminated string starting at", start)
			return ""
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String()
		case c < 0x20:
			p.fail("Invalid control character at", p.i)
			return ""
		case c == '\\':
			if p.i+1 >= len(p.s) {
				p.fail("Unterminated string starting at", start)
				return ""
			}
			e := p.s[p.i+1]
			p.i += 2
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, ok := p.hex4()
				if !ok {
					p.fail("Invalid \\uXXXX escape", p.i-2)
					return ""
				}
				if r >= 0xD800 && r < 0xDC00 && strings.HasPrefix(p.s[p.i:], "\\u") {
					save := p.i
					p.i += 2
					if r2, ok := p.hex4(); ok && r2 >= 0xDC00 && r2 < 0xE000 {
						r = 0x10000 + (r-0xD800)<<10 + (r2 - 0xDC00)
					} else {
						p.i = save
					}
				}
				if r >= 0xD800 && r < 0xE000 {
					// Python keeps a lone surrogate as a code point. Go strings
					// cannot, so keep its WTF-8 bytes: Marshal writes the same
					// \uXXXX escape back and the user's file stays unchanged.
					b.Write([]byte{byte(0xE0 | r>>12), byte(0x80 | (r>>6)&0x3F), byte(0x80 | r&0x3F)})
				} else {
					b.WriteRune(r)
				}
			default:
				p.fail("Invalid \\escape", p.i-2)
				return ""
			}
		default:
			b.WriteByte(c)
			p.i++
		}
	}
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	n, err := strconv.ParseUint(p.s[p.i:p.i+4], 16, 32)
	if err != nil || strings.ContainsAny(p.s[p.i:p.i+4], "+-_") {
		return 0, false
	}
	p.i += 4
	return rune(n), true
}

func (p *parser) array(depth int) Value {
	if depth >= MaxDepth {
		p.err = DepthError{}
		return nil
	}
	p.i++
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out
	}
	for {
		p.ws()
		v := p.value(depth + 1)
		if p.err != nil {
			return nil
		}
		out = append(out, v)
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == ']' {
			p.i++
			return out
		}
		if p.i >= len(p.s) || p.s[p.i] != ',' {
			p.fail("Expecting ',' delimiter", p.i)
			return nil
		}
		p.i++
	}
}

func (p *parser) object(depth int) Value {
	if depth >= MaxDepth {
		p.err = DepthError{}
		return nil
	}
	p.i++
	out := NewObj()
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			p.fail("Expecting property name enclosed in double quotes", p.i)
			return nil
		}
		k := p.str()
		if p.err != nil {
			return nil
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			p.fail("Expecting ':' delimiter", p.i)
			return nil
		}
		p.i++
		p.ws()
		v := p.value(depth + 1)
		if p.err != nil {
			return nil
		}
		out.Set(k, v)
		p.ws()
		if p.i < len(p.s) && p.s[p.i] == '}' {
			p.i++
			return out
		}
		if p.i >= len(p.s) || p.s[p.i] != ',' {
			p.fail("Expecting ',' delimiter", p.i)
			return nil
		}
		p.i++
	}
}

// FloatRepr prints a float the way Python's repr does.
func FloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	s := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde+XX
	sign := ""
	if s[0] == '-' {
		sign, s = "-", s[1:]
	}
	mant, expS, _ := strings.Cut(s, "e")
	exp, _ := strconv.Atoi(expS)
	digits := strings.Replace(mant, ".", "", 1)
	if exp < -4 || exp >= 16 {
		m := digits[:1]
		if len(digits) > 1 {
			m += "." + digits[1:]
		}
		es := "+"
		if exp < 0 {
			es, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, m, es, exp)
	}
	if exp < 0 {
		return sign + "0." + strings.Repeat("0", -exp-1) + digits
	}
	if len(digits) <= exp+1 {
		return sign + digits + strings.Repeat("0", exp+1-len(digits)) + ".0"
	}
	return sign + digits[:exp+1] + "." + digits[exp+1:]
}

// Format selects json.dumps options.
type Format struct {
	Indent   int  // >0 pretty-prints with that many spaces
	Compact  bool // separators (",", ":") instead of (", ", ": ")
	ASCII    bool // ensure_ascii=True
	SortKeys bool
}

// Marshal encodes like json.dumps with the given options.
func Marshal(v Value, f Format) string {
	var b strings.Builder
	f.write(&b, v, 0)
	return b.String()
}

func (f Format) write(b *strings.Builder, v Value, level int) {
	itemSep, keySep := ", ", ": "
	if f.Compact {
		itemSep, keySep = ",", ":"
	}
	if f.Indent > 0 {
		itemSep = ","
	}
	nl := func(l int) {
		b.WriteString("\n")
		b.WriteString(strings.Repeat(" ", f.Indent*l))
	}
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case string:
		f.str(b, t)
	case Num:
		b.WriteString(t.Lit)
	case []any:
		if len(t) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[")
		for i, e := range t {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if f.Indent > 0 {
				nl(level + 1)
			}
			f.write(b, e, level+1)
		}
		if f.Indent > 0 {
			nl(level)
		}
		b.WriteString("]")
	case *Obj:
		if t.Len() == 0 {
			b.WriteString("{}")
			return
		}
		keys := t.Keys()
		if f.SortKeys {
			sort.Strings(keys)
		}
		b.WriteString("{")
		for i, k := range keys {
			if i > 0 {
				b.WriteString(itemSep)
			}
			if f.Indent > 0 {
				nl(level + 1)
			}
			f.str(b, k)
			b.WriteString(keySep)
			f.write(b, t.m[k], level+1)
		}
		if f.Indent > 0 {
			nl(level)
		}
		b.WriteString("}")
	default:
		panic(fmt.Sprintf("pycompat: cannot marshal %T", v))
	}
}

func (f Format) str(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); {
		// A lone surrogate Parse kept as WTF-8 (ED A0..BF 80..BF): Python holds
		// it as a code point and dumps the same \uXXXX escape, in both ASCII
		// and non-ASCII mode, because UTF-8 cannot spell it at all.
		if c := s[i]; c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			r := rune(c&0x0F)<<12 | rune(s[i+1]&0x3F)<<6 | rune(s[i+2]&0x3F)
			fmt.Fprintf(b, `\u%04x`, r)
			i += 3
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		i += n
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case r <= 0x7f || !f.ASCII:
			b.WriteRune(r)
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3ff))
		default:
			fmt.Fprintf(b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
}
