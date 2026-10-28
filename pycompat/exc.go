package pycompat

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Kind classifies a raised Python exception the way the helpers' `except`
// clauses do: OSError and ValueError are expected failures, anything else is a crash.
type Kind int

const (
	OSError Kind = iota
	ValueError
	Other
)

// Exc is a Python-style exception carried by panic and recovered by Catch.
type Exc struct {
	Kind  Kind
	Class string // Other: the Python class name
	Msg   string
}

func (e *Exc) Error() string { return e.Msg }

// RaiseOS panics with an OSError worded like Python's.
func RaiseOS(err error) { panic(&Exc{Kind: OSError, Msg: OSErrorText(err)}) }

// RaiseOSMsg panics with an OSError of the given text (OSError("...")).
func RaiseOSMsg(format string, a ...any) {
	panic(&Exc{Kind: OSError, Msg: fmt.Sprintf(format, a...)})
}

// RaiseValue panics with a ValueError.
func RaiseValue(format string, a ...any) {
	panic(&Exc{Kind: ValueError, Msg: fmt.Sprintf(format, a...)})
}

// Crash panics with an exception the helpers do not expect.
func Crash(class, format string, a ...any) {
	panic(&Exc{Kind: Other, Class: class, Msg: fmt.Sprintf(format, a...)})
}

// Catch runs fn and returns what it raised, if anything.
func Catch(fn func()) (out *Exc) {
	defer func() {
		switch r := recover().(type) {
		case nil:
		case *Exc:
			out = r
		case DepthError:
			out = &Exc{Kind: Other, Class: "RecursionError", Msg: r.Error()}
		case *SyntaxError:
			out = &Exc{Kind: ValueError, Msg: r.Msg}
		case error:
			out = &Exc{Kind: Other, Class: "RuntimeError", Msg: r.Error()}
		default:
			out = &Exc{Kind: Other, Class: "RuntimeError", Msg: fmt.Sprint(r)}
		}
	}()
	fn()
	return nil
}

// ReadText is Path.read_text(encoding="utf-8"): invalid UTF-8 is a ValueError
// and "\r\n" / "\r" are read as "\n". Other failures are OSErrors.
func ReadText(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		RaiseOS(err)
	}
	if !utf8.Valid(b) {
		pos := 0
		for pos < len(b) {
			r, size := utf8.DecodeRune(b[pos:])
			if r == utf8.RuneError && size == 1 {
				break
			}
			pos += size
		}
		RaiseValue("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", b[pos], utf8.RuneCount(b[:pos]))
	}
	s := string(b)
	if strings.Contains(s, "\r") {
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	}
	return s
}

// IsNotExist reports a missing file (Python's FileNotFoundError).
func IsNotExist(err error) bool { return err != nil && errors.Is(err, fs.ErrNotExist) }

// IsSpace is str.isspace for one character.
func IsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Strip is str.strip() with no argument.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// SplitLines is str.splitlines().
func SplitLines(s string) []string {
	var out []string
	rs := []rune(s)
	cur := make([]rune, 0, 16)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch r {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		case '\r':
			if i+1 < len(rs) && rs[i+1] == '\n' {
				i++
			}
		default:
			cur = append(cur, r)
			continue
		}
		out = append(out, string(cur))
		cur = cur[:0]
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

// Path normalises a path the way pathlib prints it: no empty or "." parts, ".." kept.
func Path(p string) string {
	abs := strings.HasPrefix(p, "/")
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	out := strings.Join(parts, "/")
	switch {
	case abs:
		return "/" + out
	case out == "":
		return "."
	}
	return out
}

// Join is pathlib's `dir / name`.
func Join(dir string, names ...string) string {
	for _, n := range names {
		switch {
		case n == "":
		case strings.HasPrefix(n, "/"):
			dir = n
		case dir == ".":
			dir = n
		case strings.HasSuffix(dir, "/"):
			dir += n
		default:
			dir += "/" + n
		}
	}
	return dir
}

// OMap is an insertion-ordered string-keyed map, like a Python dict.
type OMap[V any] struct {
	keys []string
	m    map[string]V
}

// NewOMap returns an empty map.
func NewOMap[V any]() *OMap[V] { return &OMap[V]{m: map[string]V{}} }

// Get returns the value and whether it exists.
func (o *OMap[V]) Get(k string) (V, bool) { v, ok := o.m[k]; return v, ok }

// Has reports whether k exists.
func (o *OMap[V]) Has(k string) bool { _, ok := o.m[k]; return ok }

// Set stores v; a new key goes last, an existing key keeps its place.
func (o *OMap[V]) Set(k string, v V) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

// Delete removes k.
func (o *OMap[V]) Delete(k string) {
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

// Keys returns the keys in order.
func (o *OMap[V]) Keys() []string { return append([]string(nil), o.keys...) }

// Len is the number of keys.
func (o *OMap[V]) Len() int { return len(o.keys) }

// Copy is dict(o).
func (o *OMap[V]) Copy() *OMap[V] {
	c := NewOMap[V]()
	for _, k := range o.keys {
		c.Set(k, o.m[k])
	}
	return c
}

// Merge is {**a, **b}: a's keys first, b's values win, b's new keys last.
func Merge[V any](a, b *OMap[V]) *OMap[V] {
	out := a.Copy()
	for _, k := range b.keys {
		out.Set(k, b.m[k])
	}
	return out
}
