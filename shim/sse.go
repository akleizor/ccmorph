package shim

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// Events after which the upstream sends NOTHING but keeps the socket open for
// reuse. The shim must end the client response at these, or the client waits
// forever for bytes that never come.
func terminalEvent(kind string) bool { return kind == "message_stop" || kind == "error" }

// The id-rewriting hot-path filter (PARSE) and the widening a capturing shim
// needs: the two events that carry the response id and its usage.
var (
	parseNeedles   = [][]byte{[]byte(`"tool_use"`), []byte(`"message_stop"`), []byte(`"error"`)}
	captureNeedles = append(append([][]byte{}, parseNeedles...), []byte(`"message_start"`), []byte(`"message_delta"`))
)

const asciiSpace = " \t\n\r\x0b\x0c"

// dataPayload splits an SSE line into its "data:" prefix (kept so the upstream's
// own spacing is reused), payload and terminator. ok is false for anything but
// a data line.
func dataPayload(line []byte) (prefix, payload, eol []byte, ok bool) {
	stripped := bytes.TrimRight(line, "\r\n")
	eol = line[len(stripped):]
	if len(eol) == 0 {
		eol = []byte("\n") // preserve the original terminator; default LF
	}
	if !bytes.HasPrefix(stripped, []byte("data:")) {
		return nil, nil, eol, false
	}
	payload = bytes.TrimLeft(stripped[5:], asciiSpace)
	return stripped[:len(stripped)-len(payload)], payload, eol, true
}

// thinkingStream is an SSE response minus malformed empty thinking: the streamed
// form of the JSON-path removal. A thinking block that starts with no text and no
// signature waits for its next data event. A delta releases it untouched (a
// healthy block that starts empty). Its own content_block_stop drops both events,
// and later blocks' indexes shift down so the client still sees 0..n-1. Anything
// else releases it as is. event:/id:/comment lines wait for their data line, so a
// dropped event leaves no orphan header. Nothing is read past a data line except
// while one block is held.
type thinkingStream struct {
	r            *bufio.Reader
	out          [][]byte // lines ready for the relay loop
	pending      [][]byte // non-data lines awaiting their data line
	held         *heldStart
	dropped      []int // upstream indexes of dropped blocks
	swallowBlank bool  // the blank line closing a dropped stop
	err          error
}

type heldStart struct {
	head  [][]byte
	line  []byte
	obj   *doc
	tail  [][]byte
	index int
}

// readline returns the next line; at the end of the stream or a read error it
// returns what is left first, then the error (io.EOF for a clean end).
func (t *thinkingStream) readline() ([]byte, error) {
	for len(t.out) == 0 {
		if t.err != nil {
			t.release()
			t.out = append(t.out, t.pending...)
			t.pending = nil
			if len(t.out) == 0 {
				return nil, t.err
			}
			break
		}
		line, err := t.r.ReadBytes('\n')
		if err != nil {
			t.err = err
		}
		if len(line) > 0 {
			t.feed(line)
		}
	}
	line := t.out[0]
	t.out = t.out[1:]
	return line, nil
}

func intField(d *doc, key string) (int, bool) {
	sp, ok := d.last(key)
	if !ok {
		return 0, false
	}
	raw := d.b[sp.Start:sp.End]
	if bytes.ContainsAny(raw, ".eE") {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw))
	return n, err == nil
}

func (t *thinkingStream) feed(line []byte) {
	stripped := bytes.TrimRight(line, "\r\n")
	if len(stripped) == 0 {
		switch {
		case t.swallowBlank:
			t.swallowBlank = false
			t.pending = nil
		case t.held != nil:
			t.held.tail = append(append(t.held.tail, t.pending...), line)
			t.pending = nil
		default:
			t.out = append(append(t.out, t.pending...), line)
			t.pending = nil
		}
		return
	}
	if !bytes.HasPrefix(stripped, []byte("data:")) {
		t.pending = append(t.pending, line)
		return
	}
	_, payload, _, _ := dataPayload(line)
	var obj *doc
	if t.held != nil || (len(t.dropped) > 0 && bytes.Contains(payload, []byte(`"index"`))) ||
		(bytes.Contains(payload, []byte(`"content_block_start"`)) && bytes.Contains(payload, []byte(`"thinking"`))) {
		obj = parseDoc(payload)
	}
	kind := ""
	if obj != nil {
		kind, _ = obj.str("type")
	}
	if t.held != nil {
		if idx, ok := intField(obj0(obj), "index"); obj != nil && kind == "content_block_stop" && ok && idx == t.held.index {
			t.dropped = append(t.dropped, t.held.index)
			t.held = nil
			t.pending = nil
			t.swallowBlank = true
			return
		}
		t.release()
	}
	if obj != nil && kind == "content_block_start" {
		if idx, ok := intField(obj, "index"); ok {
			if cb, ok := obj.last("content_block"); ok && malformedThinking(obj.b, cb) {
				t.held = &heldStart{head: t.pending, line: line, obj: obj, index: idx}
				t.pending = nil
				return
			}
		}
	}
	t.out = append(t.out, t.pending...)
	t.pending = nil
	t.out = append(t.out, t.reindex(line, obj))
}

func obj0(d *doc) *doc {
	if d == nil {
		return &doc{}
	}
	return d
}

func (t *thinkingStream) release() {
	if t.held == nil {
		return
	}
	h := t.held
	t.held = nil
	t.out = append(t.out, h.head...)
	t.out = append(t.out, t.reindex(h.line, h.obj))
	t.out = append(t.out, h.tail...)
}

func (t *thinkingStream) reindex(line []byte, obj *doc) []byte {
	if len(t.dropped) == 0 || obj == nil {
		return line
	}
	idx, ok := intField(obj, "index")
	if !ok {
		return line
	}
	shift := 0
	for _, d := range t.dropped {
		if d < idx {
			shift++
		}
	}
	if shift == 0 {
		return line
	}
	sp, _ := obj.last("index")
	payload, _ := (&canon{ascii: true}).encode(obj.b, []Edit{{Start: sp.Start, End: sp.End, Raw: []byte(strconv.Itoa(idx - shift))}})
	prefix, _, eol, _ := dataPayload(line)
	return append(append(append([]byte{}, prefix...), payload...), eol...)
}

// relayStream copies the SSE stream line by line, flushing each so streaming
// latency is unchanged. It returns when the stream ends (clean EOF or a
// terminal event), with the upstream read error that cut it short, or the
// client write error.
func (s *Server) relayStream(rq *request, t lineSource, sidecar *UsageSidecar, requested *string, answerAs string) (readErr, writeErr error) {
	var rec *record
	needles := parseNeedles
	if sidecar != nil || answerAs != "" {
		needles = captureNeedles
	}
	write := func(b []byte) error {
		if _, err := rq.w.Write(b); err != nil {
			return err
		}
		return rq.flush()
	}
	for {
		line, err := t.readline()
		if err != nil {
			// Upstream EOF, including a truncated stream with no message_stop.
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return err, nil
		}
		prefix, payload, eol, isData := dataPayload(line)
		if !isData || bytes.Equal(bytes.TrimRight(line, "\r\n"), []byte("data: [DONE]")) {
			if err := write(line); err != nil {
				return nil, err
			}
			continue
		}
		// Hot-path pre-filter: text deltas dominate streaming traffic and carry
		// neither tool_use ids nor message_stop. A substring scan skips the
		// parse for ~95%+ of events; any event holding a needle is parsed, even
		// if the match is in text content (a wasted parse, harmless).
		if !containsAny(payload, needles) {
			if err := write(line); err != nil {
				return nil, err
			}
			continue
		}
		if out, changed := s.rewrite(payload, answerAs, false); changed {
			// Rewrite the data line in place: same "data:" prefix, same
			// terminator. The blank line that closes the SSE event is a
			// separate line and still arrives on the next read.
			payload = out
			line = append(append(append(make([]byte, 0, len(prefix)+len(out)+len(eol)), prefix...), out...), eol...)
		}
		if err := write(line); err != nil {
			return nil, err
		}
		// After the client has the event, so capture never sits in the
		// stream's latency path. A stream that dies with no terminal event
		// leaves the row it last wrote, complete:false.
		d := parseDoc(payload)
		if sidecar != nil {
			rec = sidecar.observe(d, rec, requested)
		}
		// message_stop, and an error event, are terminal: upstreams keep the
		// connection alive and send NOTHING after them. Emit the blank line that
		// closes the event first, since returning means the client never sees
		// the upstream's own.
		if d != nil {
			if kind, _ := d.str("type"); terminalEvent(kind) {
				return nil, write(eol)
			}
		}
	}
}

func containsAny(b []byte, needles [][]byte) bool {
	for _, n := range needles {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

var _ = http.StatusOK
