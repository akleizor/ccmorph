package ordjson

import (
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// IsDigits reports whether s is one or more ASCII digits.
func IsDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// IsNumber reports whether s parses as a floating-point number.
func IsNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

// StripBOM drops the byte-order mark an editor may have put on a JSON file.
func StripBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) }

// Truthy is Python's bool() over a value this package decoded: nil, false, "",
// zero, an empty array and an empty object are false, everything else true.
func Truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case []any:
		return len(t) > 0
	case *Obj:
		return t.Len() > 0
	}
	return true
}

// TruncRunes cuts s to at most n runes, leaving it untouched when it fits.
func TruncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
