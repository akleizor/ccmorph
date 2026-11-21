package paths

import (
	"bytes"
	"encoding/json"
)

// ArchiveCopySuffix names the internal key a reader gives the archived copy
// of a row when a live row of the same file name carries a different
// identity: the two are not one turn, so the folder copy keeps the file name
// and the archive copy rides alongside under this suffix. The byte cannot
// appear in a file name, so the key cannot collide with one (#654).
const ArchiveCopySuffix = "\x00archive"

// SameNamedRows is the store's one merge rule, shared by all three readers —
// quota's tallies, libexec/ccmorph-core's report, and share/usage-export.
// Two rows that share a file name are one turn, and the newer one wins, only
// when their identity agrees: a non-empty response_id both hold, a turn_id
// that must match whenever it is there, and an empty turn only when both
// rows carry equal string timestamps. A member that is not a string — the
// NaN/Infinity tokens Python's json.dump wrote for a bad cost included — is
// not a match, and a duplicated member follows json.Unmarshal's last-wins
// reading, which is what every reader keys on. The export mirrors this rule
// in Python where its receipt merge reads.
func SameNamedRows(a, b []byte) bool {
	if bytes.Equal(a, b) {
		return true
	}
	var ra, rb rowIdentity
	if json.Unmarshal(NonFiniteToNull(a), &ra) != nil || json.Unmarshal(NonFiniteToNull(b), &rb) != nil {
		return false
	}
	if ra.ID == "" || ra.ID != rb.ID {
		return false
	}
	ta, taOK := rawString(ra.Turn)
	tb, tbOK := rawString(rb.Turn)
	if (len(ra.Turn) > 0 && !taOK) || (len(rb.Turn) > 0 && !tbOK) || ta != tb {
		return false
	}
	if ta == "" {
		sa, saOK := rawString(ra.Stamp)
		sb, sbOK := rawString(rb.Stamp)
		return saOK && sbOK && sa == sb
	}
	return true
}

type rowIdentity struct {
	ID    string          `json:"response_id"`
	Turn  json.RawMessage `json:"turn_id"`
	Stamp json.RawMessage `json:"timestamp"`
}

// rawString reads a top-level member as a string: absent, null, or any other
// JSON kind is not one.
func rawString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// NonFiniteToNull rewrites the NaN/Infinity/-Infinity tokens Python's
// json.dump wrote for a bad cost into null. encoding/json refuses the tokens
// outright, so a reader that refused them would drop rows whose damage is
// only a price — while Go's own ParseFloat happily re-produces them. String
// bodies are scanned past, so no stored text is ever rewritten.
func NonFiniteToNull(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] == '"' {
			e := scanJSONString(b, i)
			out = append(out, b[i:e]...)
			i = e
			continue
		}
		matched := false
		for _, tok := range []string{"-Infinity", "Infinity", "NaN"} {
			if bytes.HasPrefix(b[i:], []byte(tok)) {
				out = append(out, "null"...)
				i += len(tok)
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, b[i])
			i++
		}
	}
	return out
}

// scanJSONString returns the index just past the string that opens at b[i].
func scanJSONString(b []byte, i int) int {
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
