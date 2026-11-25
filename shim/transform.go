package shim

import (
	"encoding/hex"
	"encoding/json/jsontext"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"sync/atomic"
)

// --- response side: tool_use ids, empty thinking, model identity -------------

// walker finds the edits one response body needs: every tool_use id made
// unique, malformed empty thinking blocks removed, and the message's model
// renamed to the Anthropic id the client asked under. It never builds a tree.
type walker struct {
	b         []byte
	s         *Server
	answerAs  string
	edits     []Edit
	tooDeep   bool
	keepIDs   bool
	dropEmpty bool
}

const (
	modeNone   = iota
	modeTop    // decide: this object, or its "message" when it is a message_start
	modeTarget // this object is the message to rename
)

func (w *walker) walk(i, depth int, noDrop bool, mode int, filter bool) int {
	if depth > maxDepth {
		w.tooDeep = true
		return scanValue(w.b, i)
	}
	switch w.b[i] {
	case '{':
		return w.object(i, depth, noDrop, mode, filter)
	case '[':
		return w.array(i, depth, noDrop, filter)
	}
	return scanValue(w.b, i)
}

func (w *walker) object(i, depth int, noDrop bool, mode int, _ bool) int {
	ms, end := Object(w.b, i)
	typ := ""
	if sp, ok := LastMember(ms, "type"); ok {
		typ, _ = SpanStr(w.b, sp)
	}
	toolUse := typ == "tool_use"
	idIdx, contentIdx, msgIdx, modelIdx := -1, -1, -1, -1
	if toolUse && !w.keepIDs {
		if k := lastIndex(ms, "id"); k >= 0 && w.b[ms[k].Value.Start] == '"' {
			idIdx = k
		}
	}
	if !noDrop && w.dropEmpty {
		contentIdx = lastIndex(ms, "content")
	}
	apply := mode == modeTarget || (mode == modeTop && typ != "message_start")
	if mode == modeTop && typ == "message_start" {
		msgIdx = lastIndex(ms, "message")
	}
	if apply && w.answerAs != "" && typ == "message" {
		if k := lastIndex(ms, "model"); k >= 0 {
			if m, ok := SpanStr(w.b, ms[k].Value); ok && m != w.answerAs {
				modelIdx = k
			}
		}
	}
	for k, m := range ms {
		switch k {
		case idIdx:
			w.edits = append(w.edits, Edit{Start: m.Value.Start, End: m.Value.End, IsText: true, Text: []byte(w.s.uniqueID())})
		case modelIdx:
			w.edits = append(w.edits, Edit{Start: m.Value.Start, End: m.Value.End, IsText: true, Text: []byte(w.answerAs)})
		default:
			mode := modeNone
			if k == msgIdx {
				mode = modeTarget
			}
			w.walk(m.Value.Start, depth+1, noDrop || (toolUse && m.Key == "input"), mode, k == contentIdx)
		}
	}
	return end
}

func (w *walker) array(i, depth int, noDrop, filter bool) int {
	es, end := Array(w.b, i)
	for _, e := range es {
		if filter && malformedThinking(w.b, e) {
			w.edits = append(w.edits, Edit{Start: e.Start, End: e.End, Remove: true})
			continue
		}
		w.walk(e.Start, depth+1, noDrop, modeNone, false)
	}
	return end
}

// malformedThinking is a thinking block with no text and no signature. A signed
// empty block is a valid omitted-display block the client replays unchanged.
func malformedThinking(b []byte, sp Span) bool {
	if b[sp.Start] != '{' {
		return false
	}
	ms, _ := Object(b, sp.Start)
	t, ok := LastMember(ms, "type")
	if !ok {
		return false
	}
	if typ, isStr := SpanStr(b, t); !isStr || typ != "thinking" {
		return false
	}
	if th, ok := LastMember(ms, "thinking"); ok && !falsy(b, th) {
		return false
	}
	if sg, ok := LastMember(ms, "signature"); ok {
		if s, isStr := SpanStr(b, sg); isStr && s != "" {
			return false
		}
	}
	return true
}

// falsy is Python truthiness for a JSON value.
func falsy(b []byte, sp Span) bool {
	v := b[sp.Start:sp.End]
	switch v[0] {
	case 'n', 'f':
		return true
	case 't':
		return false
	case '"':
		return len(v) == 2
	case '[', '{':
		return skipWS(v, 1) == len(v)-1
	}
	f, err := strconv.ParseFloat(string(v), 64)
	return err == nil && f == 0
}

// rewrite returns b with the response edits applied, or b itself, byte for byte,
// when there are none (or b is not JSON, or nests too deeply to be handled).
func (s *Server) rewrite(b []byte, answerAs string, spaced bool) ([]byte, bool) {
	if len(b) == 0 || !validJSON(b) {
		return b, false
	}
	w := &walker{b: b, s: s, answerAs: answerAs, keepIDs: s.PreserveIDs, dropEmpty: true}
	w.walk(skipWS(b, 0), 0, false, modeTop, false)
	if w.tooDeep || len(w.edits) == 0 {
		return b, false
	}
	c := canon{ascii: true, spaced: spaced}
	out, ok := c.encode(b, w.edits)
	if !ok {
		return b, false
	}
	return out, true
}

var idCounter atomic.Uint64

// uniqueID mints toolu_shim_<per-process-random>_<n>. The random half is
// load-bearing: a transcript outlives the shim, so a bare counter would re-mint
// ids already in it and manufacture the duplicate-id corruption this shim exists
// to prevent.
func (s *Server) uniqueID() string {
	return idPrefix + strconv.FormatUint(idCounter.Add(1), 10)
}

// --- gateway error envelope ---------------------------------------------------

var sdkBytes = regexp.MustCompile(`SdkBytes\(bytes=0x([0-9a-fA-F]+)\)`)

// unwrapGatewayError is an Anthropic-shaped error for a hex-wrapped gateway
// error ({"detailMsg":"... SdkBytes(bytes=0x7b22...)"}), else nil.
func unwrapGatewayError(body []byte) []byte {
	d := parseDoc(body)
	if d == nil {
		return nil
	}
	detail, _ := d.str("detailMsg")
	m := sdkBytes.FindStringSubmatch(detail)
	if m == nil {
		return nil
	}
	raw, err := hex.DecodeString(m[1])
	if err != nil {
		return nil
	}
	inner := parseDoc(raw)
	if inner == nil {
		return nil
	}
	msg, ok := inner.str("message")
	if !ok {
		return nil
	}
	return errorJSON("invalid_request_error", msg)
}

// geminiMissingSignature is Gemini's own refusal of a tool turn whose
// functionCall came back without its thought_signature: the Gemini API and
// Vertex both begin the message with this sentence (Vertex relays it behind
// "Google Vertex bad request: "). Anchored at the start of the upstream's
// message, so a 400 that merely quotes the sentence — an invalid tool enum
// value carrying the phrase, say — is not mistaken for it.
var geminiMissingSignature = regexp.MustCompile(`^(?:Google Vertex bad request: )?Function call is missing a thought_signature in functionCall parts\.`)

// upstreamMessage is the error message of a JSON error body:
// {"error":{"message":...}} (Vertex, the Gemini API) or
// {"type":"error","error":{"message":...}} (Anthropic-shaped), else "".
func upstreamMessage(body []byte) string {
	d := parseDoc(body)
	if d == nil {
		return ""
	}
	sp, ok := d.last("error")
	if !ok || body[sp.Start] != '{' {
		return ""
	}
	e := docAt(body, sp)
	if msg, ok := e.str("message"); ok {
		return msg
	}
	if sp, ok := e.last("error"); ok && body[sp.Start] == '{' {
		msg, _ := docAt(body, sp).str("message")
		return msg
	}
	return ""
}

// docAt is the doc of the object at span sp.
func docAt(b []byte, sp Span) *doc {
	ms, _ := Object(b, sp.Start)
	return &doc{b: b, ms: ms}
}

// thoughtSignatureRefusal is a one-line Anthropic error for a 400 whose message
// is Gemini's missing-signature refusal, else body unchanged. The raw Vertex
// text runs to a paragraph, names a tool position, and says nothing to do.
// Reading the upstream's words is deliberate here and only here: this is a
// diagnostic, not a repair, so the structural rule above is untouched. The line
// names the hop that can drop the signature: behind the translator, a
// cliproxyapi older than akleizor/CLIProxyAPI v8.0.76-akleizor.4; on a direct
// route, the gateway, since the shim relays the history unchanged.
func thoughtSignatureRefusal(body []byte, translator bool) []byte {
	if !geminiMissingSignature.MatchString(upstreamMessage(body)) {
		return body
	}
	if translator {
		fmt.Fprintln(os.Stderr, "shim: upstream refused a tool call for a missing thought_signature; the translator did not send it back")
		return errorJSON("invalid_request_error",
			"Gemini refused the tool call because its thought_signature was not sent back: the translator sends it back only from "+
				"cliproxyapi 8.0.76-akleizor.4; if cliproxyapi -h shows an older version, run brew install akleizor/tap/cliproxyapi (macOS arm64)")
	}
	fmt.Fprintln(os.Stderr, "shim: upstream refused a tool call for a missing thought_signature; this route has no translator")
	return errorJSON("invalid_request_error",
		"Gemini refused the tool call because its thought_signature was not sent back: this route has no translator and "+
			"the shim relays the history unchanged, so the gateway did not carry it")
}

// errorJSON is json.dumps({"type":"error","error":{"type":t,"message":m}}) with
// Python's default separators.
func errorJSON(kind, message string) []byte {
	c := canon{ascii: true}
	out := []byte(`{"type": "error", "error": {"type": "` + kind + `", "message": `)
	c.str(&out, []byte(message))
	return append(out, '}', '}')
}

// --- request side --------------------------------------------------------------

// requestedModel is the doc's model id, spelled as sent ([1m] and all); a nil
// doc (the bytes are not a JSON object) has none.
func requestedModel(d *doc) (string, bool) {
	if d == nil {
		return "", false
	}
	return d.str("model")
}

// withModel is the body re-serialised with its model replaced.
func withModel(d *doc, model string) []byte {
	var edits []Edit
	for _, m := range d.ms {
		if m.Key == "model" {
			edits = append(edits, Edit{Start: m.Value.Start, End: m.Value.End, IsText: true, Text: []byte(model)})
		}
	}
	out, ok := (&canon{ascii: true}).encode(d.b, edits)
	if !ok {
		return d.b
	}
	return out
}

// userIDs locates metadata.user_id and the JSON object inside it.
func userIDs(d *doc) (inner []byte, user Span, ok bool) {
	meta, found := d.last("metadata")
	if !found || d.b[meta.Start] != '{' {
		return nil, Span{}, false
	}
	ms, _ := Object(d.b, meta.Start)
	user, found = LastMember(ms, "user_id")
	if !found || d.b[user.Start] != '"' {
		return nil, Span{}, false
	}
	inner = []byte(Unquote(d.b[user.Start:user.End]))
	// Not utf8.Valid: a lone surrogate arrives here as its WTF-8 form.
	if !jsontext.Value(inner).IsValid(jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true)) ||
		inner[skipWS(inner, 0)] != '{' {
		return nil, Span{}, false
	}
	return inner, user, true
}

// rememberSession captures the session id from metadata.user_id, keeping the
// first non-empty one. It also returns the object inside user_id, that
// object's members, and the user_id span — the pieces a caller that edits
// user_id needs — so swapDevice reads the same one pass instead of its own
// copy of the capture. ok is false when the body has no such object.
func rememberSession(d *doc, session *string) (inner []byte, ms []Member, user Span, ok bool) {
	inner, user, ok = userIDs(d)
	if !ok {
		return nil, nil, Span{}, false
	}
	ms, _ = Object(inner, skipWS(inner, 0))
	if sp, found := LastMember(ms, "session_id"); found {
		if sid, isStr := SpanStr(inner, sp); isStr && *session == "" {
			*session = sid
		}
	}
	return inner, ms, user, true
}

// swapDevice is the edit that replaces metadata.user_id.device_id (user_id is a
// JSON string) with this session's random one; also remembers the session id.
func (s *Server) swapDevice(d *doc, session *string) (Edit, bool) {
	inner, ms, user, ok := rememberSession(d, session)
	if !ok {
		return Edit{}, false
	}
	dev, ok := LastMember(ms, "device_id")
	if !ok || inner[dev.Start] != '"' {
		return Edit{}, false
	}
	edit := Edit{Start: dev.Start, End: dev.End, IsText: true, Text: []byte(s.deviceID(*session))}
	// The nested JSON keeps raw UTF-8; a lone surrogate keeps it escaped inside
	// the nested string, since escaping only the outer body would break it.
	text, ok := encodeBody(inner, []Edit{edit}, false)
	if !ok {
		return Edit{}, false
	}
	return Edit{Start: user.Start, End: user.End, IsText: true, Text: text}, true
}

func (s *Server) deviceID(session string) string {
	s.devicesMu.Lock()
	defer s.devicesMu.Unlock()
	if id, ok := s.devices[session]; ok {
		return id
	}
	buf := make([]byte, 32)
	randRead(buf)
	id := hex.EncodeToString(buf)
	s.devices[session] = id
	return id
}

// cleanRequest runs the profile's repair hook (if any) and the device-id swap
// (when swap) in one pass, over the doc the caller parsed. Not a JSON object,
// too deep, or nothing changed means the original bytes go upstream; changed
// bytes keep CJK as UTF-8 unless a lone surrogate forces the ascii form. The
// doc that comes back belongs to the bytes that come back.
func (s *Server) cleanRequest(body []byte, d *doc, swap bool, session *string) ([]byte, *doc) {
	if d == nil {
		return body, d
	}
	orig, origD := body, d
	changed := false
	if s.Hooks.RepairRequest != nil {
		if nb, ok := s.callRepair(body); ok {
			// The hook's answer is parsed here, once — unless it is the bytes it got.
			if nd := reparse(nb, d); nd != nil {
				body, d, changed = nb, nd, true
			}
		}
	}
	var edits []Edit
	if swap {
		if e, ok := s.swapDevice(d, session); ok {
			edits = append(edits, e)
		}
	} else if s.Hooks.OutboundHeaders != nil {
		rememberSession(d, session)
	}
	if !changed && len(edits) == 0 {
		return orig, origD
	}
	out, ok := encodeBody(body, edits, false)
	if !ok {
		return orig, origD
	}
	return out, reparse(out, d, origD)
}

// callRepair shields the request from a panicking hook: it leaves the bytes
// unchanged, as a raising Python hook did.
func (s *Server) callRepair(body []byte) (out []byte, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	return s.Hooks.RepairRequest(body)
}
