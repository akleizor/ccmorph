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
// dropped event leaves no orphan header; an event with no data payload yet (an
// event: line, an empty or bare data line) waits for the blank line, which
// decides whether the whole event is empty and goes with it. Nothing is read
// past a data line except while one block is held or such an event awaits its
// blank.
type thinkingStream struct {
	r            *bufio.Reader
	buf          []byte   // the start of a line whose end has not arrived yet
	cr           bool     // the last read ended on a bare CR whose line already went on
	out          [][]byte // lines ready for the relay loop
	pending      [][]byte // lines awaiting the event's data line or decision
	held         *heldStart
	dropped      []int // upstream indexes of dropped blocks
	swallowBlank bool  // the blank line closing a dropped stop
	hadData      bool  // this event already relayed a data line with a payload
	emptyData    bool  // this event names a type or has an empty data field, and no payload yet: undecided
	err          error
}

type heldStart struct {
	head  [][]byte
	line  []byte
	obj   *doc
	tail  [][]byte
	index int
}

// fill takes one read: whatever the upstream has sent so far, never waiting
// past it, and feeds every whole line in it. SSE ends a line at CRLF, LF or
// a bare CR. A line whose CR is the last byte the upstream has sent goes on
// at once, so a bare-CR terminal event ends the reply without waiting for
// the next byte. An LF opening the next read is then the second half of that
// CRLF: SSE reads the CR alone as the same line end, so the LF is dropped,
// never fed as a blank line that would end the event early; every event the
// client reads is unchanged. The start of a line whose end has not arrived
// waits in buf. A read error is kept and ends the stream after the bytes
// read with it.
func (t *thinkingStream) fill() {
	start := len(t.buf)
	buf := make([]byte, start+max(4096, start))
	copy(buf, t.buf)
	n, err := t.r.Read(buf[start:])
	if err != nil {
		t.err = err
	}
	buf = buf[:start+n]
	// A CR that ends the read may be the first half of a CRLF whose LF the
	// reader already holds: take the next byte while it costs no read.
	for n > 0 && buf[len(buf)-1] == '\r' && t.r.Buffered() > 0 {
		c, _ := t.r.ReadByte()
		buf = append(buf, c)
	}
	if t.cr && n > 0 {
		// cr is set only when its line ended the read, so buf held nothing
		// before this read: buf[0] is the read's first byte.
		t.cr = false
		if buf[0] == '\n' {
			buf = buf[1:]
		}
	}
	for {
		i := bytes.IndexAny(buf, "\r\n")
		if i < 0 {
			break
		}
		end := i + 1
		if buf[i] == '\r' {
			if end == len(buf) {
				t.cr = true
			} else if buf[end] == '\n' {
				end++
			}
		}
		// Capped at its own length, so appending to a line never writes
		// over the next.
		t.feed(buf[:end:end])
		buf = buf[end:]
	}
	t.buf = buf
}

// readline returns the next line; at the end of the stream or a read error it
// returns what is left first, then the error (io.EOF for a clean end).
func (t *thinkingStream) readline() ([]byte, error) {
	for len(t.out) == 0 {
		if t.err != nil {
			if len(t.buf) > 0 {
				// The stream's last line, ended by the stream itself.
				t.feed(t.buf)
				t.buf = nil
			}
			t.release()
			if !t.emptyData {
				// A stream cut mid empty-event dies with it: relaying its
				// header and empty data line without the deciding blank
				// would hand the client the unparsable event anyway.
				t.out = append(t.out, t.pending...)
			}
			t.pending = nil
			if len(t.out) == 0 {
				return nil, t.err
			}
			break
		}
		t.fill()
	}
	line := t.out[0]
	t.out = t.out[1:]
	return line, nil
}

// inHand takes every whole line already read that readline has not returned,
// in stream order: the decided lines, then those still waiting on their
// event's data line or blank line. cut is the start of a line whose
// terminator has not arrived. It is handed over apart and never relayed as
// read: SSE ignores an unterminated line, and ending it would make a cut-off
// field ("event: message_st") a complete, wrong one. It never reads the
// upstream.
func (t *thinkingStream) inHand() (lines [][]byte, cut []byte) {
	lines, cut = append(t.out, t.pending...), t.buf
	t.out, t.pending, t.buf = nil, nil, nil
	return lines, cut
}

// terminalName is the event line that closes a terminal event of kind when
// the reply ends at its data line while the upstream has sent only the start
// of a later line (cut, no terminator yet). If cut is the start of the event's
// own name ("eve", "event: message_st", "event:message_stop"), that name is
// written whole, in the upstream's spelling, so the client reads the event
// under the name the upstream was sending; the name comes from the data's
// type, never from the cut bytes. Any other cut line gives nil: it is dropped,
// as SSE drops an unterminated line.
func terminalName(cut []byte, kind string, eol []byte) []byte {
	if len(cut) == 0 {
		return nil
	}
	for _, name := range []string{"event: " + kind, "event:" + kind} {
		if bytes.HasPrefix([]byte(name), cut) {
			return append([]byte(name), eol...)
		}
	}
	return nil
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
		case t.emptyData:
			// The event ended with no data payload (only empty data lines, or
			// none at all). No client
			// can parse "" as JSON (issue #689: llmgate heartbeats arrived as
			// "event: content_block_delta" + "data:" and Claude Code died on
			// the unparsable chunk), so the event carries nothing and the
			// relay drops it whole: pending holds its header lines and its
			// empty data line, and this blank line goes with them, so the
			// bytes read exactly as a stream that never emitted it. The
			// filter authors no empty data line itself — its one self-written
			// event carries JSON — so it is the upstream gateway's, and the
			// verbatim relay is what delivered it. Empty events are never
			// terminal (a terminal event's data is JSON), and a held thinking
			// start keeps waiting for its next real data event.
			t.pending = nil
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
		t.hadData, t.emptyData = false, false
		return
	}
	if t.swallowBlank {
		// Every line up to the blank belongs to the dropped stop event: an
		// empty data: continuation relayed here would land, orphaned, at the
		// head of the next event.
		return
	}
	bareData := bytes.Equal(stripped, []byte("data")) // SSE: a field with no colon has an empty value
	if !bareData && !bytes.HasPrefix(stripped, []byte("data:")) {
		if !t.hadData && (bytes.HasPrefix(stripped, []byte("event:")) || bytes.Equal(stripped, []byte("event"))) {
			// A named event is empty until a payload line arrives: Claude
			// Code's SDK dispatches "event: X" with no data line as data ""
			// and fails the same JSON parse.
			t.emptyData = true
		}
		t.pending = append(t.pending, line)
		return
	}
	var payload []byte
	if !bareData {
		_, payload, _, _ = dataPayload(line)
	}
	if len(payload) == 0 && !t.hadData {
		// Possibly an empty event: the blank line decides, so buffer this
		// line with the header lines instead of relaying it. If a data line
		// with a payload follows before the blank, the event is not empty —
		// SSE joins data lines with newlines, so this line only adds JSON
		// whitespace around the payload — and everything releases in order.
		t.pending = append(t.pending, line)
		t.emptyData = true
		return
	}
	// A payload line makes the event real; an empty line after one is just a
	// continuation of it (valid trailing whitespace) and relays as any data
	// line below — swallowing either would merge the event into the next.
	t.hadData, t.emptyData = true, false
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
		if obj != nil && kind == "content_block_stop" {
			if idx, ok := intField(obj, "index"); ok && idx == t.held.index {
				t.dropped = append(t.dropped, t.held.index)
				t.held = nil
				t.pending = nil
				t.swallowBlank = true
				return
			}
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
		// connection alive and send NOTHING after them, so the reply ends here
		// and never reads the upstream again. The rest of this event already
		// read (a field after its data line, the blank line closing it)
		// relays as is, so the event reaches the client whole. If its blank
		// line has not arrived, it is written for it, after the event's own
		// name when the upstream was cut off sending that (terminalName).
		if d != nil {
			if kind, _ := d.str("type"); terminalEvent(kind) {
				lines, cut := t.inHand()
				for _, rest := range lines {
					if err := write(rest); err != nil {
						return nil, err
					}
					if len(bytes.TrimRight(rest, "\r\n")) == 0 {
						return nil, nil
					}
				}
				if name := terminalName(cut, kind, eol); name != nil {
					if err := write(name); err != nil {
						return nil, err
					}
				}
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
