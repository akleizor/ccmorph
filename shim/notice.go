package shim

import (
	"bytes"
	"strconv"
)

// A routed turn tells the user which model answers it before the answer is
// used (INTENT promise 4). The shim reaches the client through the reply, so
// the notice is one text block at the head of the turn's first answer — after
// any thinking block, because a reply that starts with thinking must keep it
// there. The same switch is logged on stderr and lands in the usage row's
// served_model, so `ccmorph-core shim report` names it too.

// noticeNeedles are the events that can decide where the notice goes.
var noticeNeedles = [][]byte{
	[]byte(`"content_block_start"`),
	[]byte(`"message_delta"`),
	[]byte(`"message_stop"`),
	[]byte(`"error"`),
}

// noticeStream inserts the notice block into an SSE reply before its first
// block that is not thinking, then shifts every later block up one index so the
// client still sees 0..n-1. A stream that errors first passes through as is.
type noticeStream struct {
	src     lineSource
	text    string
	out     [][]byte
	pending [][]byte // event:/id:/comment lines awaiting their data line
	at      int      // the index the notice took; -1 until placed
	last    int      // highest content_block index the upstream sent
	done    bool     // an error event came first: leave the stream alone
	err     error
}

func (t *noticeStream) readline() ([]byte, error) {
	for len(t.out) == 0 {
		if t.err != nil {
			t.out = append(t.out, t.pending...)
			t.pending = nil
			if len(t.out) == 0 {
				return nil, t.err
			}
			break
		}
		line, err := t.src.readline()
		if err != nil {
			t.err = err
			continue
		}
		t.feed(line)
	}
	line := t.out[0]
	t.out = t.out[1:]
	return line, nil
}

func (t *noticeStream) feed(line []byte) {
	stripped := bytes.TrimRight(line, "\r\n")
	if len(stripped) == 0 {
		// The blank line closes the event whose lines are pending.
		t.out = append(append(t.out, t.pending...), line)
		t.pending = nil
		return
	}
	if !bytes.HasPrefix(stripped, []byte("data:")) {
		// An event's header waits for its data line, so a placement never
		// splits an event.
		t.pending = append(t.pending, line)
		return
	}
	_, payload, _, _ := dataPayload(line)
	if t.at < 0 && !t.done {
		if at, ok := t.place(payload); ok {
			t.at = at
			// The notice is a whole event of its own, so it goes before the
			// pending header lines: those belong to the event this data line ends.
			t.out = append(t.out, t.noticeEvents()...)
		}
	}
	t.out = append(t.out, t.pending...)
	t.pending = nil
	t.out = append(t.out, t.reindex(line, payload))
}

// place decides whether this data line is where the notice goes, and at which
// index. It also tracks the highest block index and gives up on an error event.
func (t *noticeStream) place(payload []byte) (int, bool) {
	if !containsAny(payload, noticeNeedles) {
		return 0, false
	}
	d := parseDoc(payload)
	if d == nil {
		return 0, false
	}
	kind, _ := d.str("type")
	switch kind {
	case "content_block_start":
		idx, ok := intField(d, "index")
		if !ok {
			return 0, false
		}
		if cb, ok := d.last("content_block"); ok && d.b[cb.Start] == '{' {
			typ := ""
			if ms, _ := Object(d.b, cb.Start); ms != nil {
				if sp, ok := lastMember(ms, "type"); ok {
					typ, _ = str(d.b, sp)
				}
			}
			if typ == "thinking" || typ == "redacted_thinking" {
				t.last = max(t.last, idx)
				return 0, false
			}
		}
		t.last = max(t.last, idx)
		return idx, true
	case "message_delta", "message_stop":
		return t.last + 1, true
	case "error":
		t.done = true
	}
	return 0, false
}

// reindex shifts a data line's index when the notice sits at or before it.
func (t *noticeStream) reindex(line, payload []byte) []byte {
	if t.at < 0 || !bytes.Contains(payload, []byte(`"index"`)) {
		return line
	}
	d := parseDoc(payload)
	if d == nil {
		return line
	}
	idx, ok := intField(d, "index")
	if !ok || idx < t.at {
		return line
	}
	return setIndex(line, d, idx+1)
}

// noticeEvents is one complete text block carrying the notice.
func (t *noticeStream) noticeEvents() [][]byte {
	index := strconv.Itoa(t.at)
	text := string(jsonStr(t.text))
	return [][]byte{
		[]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":" + index +
			",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"),
		[]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":" + index +
			",\"delta\":{\"type\":\"text_delta\",\"text\":" + text + "}}\n\n"),
		[]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":" + index + "}\n\n"),
	}
}

// setIndex rewrites a data line's index field to n, keeping the prefix and terminator.
func setIndex(line []byte, d *doc, n int) []byte {
	sp, ok := d.last("index")
	if !ok {
		return line
	}
	payload, ok := (&canon{ascii: true}).encode(d.b, []Edit{{Start: sp.Start, End: sp.End, Raw: []byte(strconv.Itoa(n))}})
	if !ok {
		return line
	}
	prefix, _, eol, _ := dataPayload(line)
	return append(append(append([]byte{}, prefix...), payload...), eol...)
}

// insertNotice puts the notice block at the head of a non-stream reply's
// content, after any thinking block, and returns the body unchanged when it
// has no content array to edit.
func insertNotice(body []byte, text string) ([]byte, bool) {
	d := parseDoc(body)
	if d == nil {
		return body, false
	}
	sp, ok := d.last("content")
	if !ok || d.b[sp.Start] != '[' {
		return body, false
	}
	items, _ := Array(d.b, sp.Start)
	block := []byte(`{"type":"text","text":` + string(jsonStr(text)) + `}`)
	if len(items) == 0 {
		return Apply(body, []Edit{{Start: sp.Start + 1, End: sp.Start + 1, Raw: block}}), true
	}
	at := -1
	for i, it := range items {
		typ := ""
		if d.b[it.Start] == '{' {
			if ms, _ := Object(d.b, it.Start); ms != nil {
				if tsp, ok := lastMember(ms, "type"); ok {
					typ, _ = str(d.b, tsp)
				}
			}
		}
		if typ != "thinking" && typ != "redacted_thinking" {
			at = i
			break
		}
	}
	if at < 0 {
		end := items[len(items)-1].End
		return Apply(body, []Edit{{Start: end, End: end, Raw: append([]byte(","), block...)}}), true
	}
	start := items[at].Start
	return Apply(body, []Edit{{Start: start, End: start, Raw: append(append([]byte{}, block...), ',')}}), true
}
