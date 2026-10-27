package shim

import (
	"encoding/hex"
	"encoding/json/jsontext"
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
	if sp, ok := lastMember(ms, "type"); ok {
		typ, _ = str(w.b, sp)
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
			if m, ok := str(w.b, ms[k].Value); ok && m != w.answerAs {
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
	t, ok := lastMember(ms, "type")
	if !ok {
		return false
	}
	if typ, isStr := str(b, t); !isStr || typ != "thinking" {
		return false
	}
	if th, ok := lastMember(ms, "thinking"); ok && !falsy(b, th) {
		return false
	}
	if sg, ok := lastMember(ms, "signature"); ok {
		if s, isStr := str(b, sg); isStr && s != "" {
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
	out, changed := s.rewriteEdits(b, answerAs, spaced)
	return out, changed
}

func (s *Server) rewriteEdits(b []byte, answerAs string, spaced bool) ([]byte, bool) {
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

// errorJSON is json.dumps({"type":"error","error":{"type":t,"message":m}}) with
// Python's default separators.
func errorJSON(kind, message string) []byte {
	c := canon{ascii: true}
	out := []byte(`{"type": "error", "error": {"type": "` + kind + `", "message": `)
	c.str(&out, []byte(message))
	return append(out, '}', '}')
}

// --- request side --------------------------------------------------------------

// requestedModel is the body's model id, spelled as sent ([1m] and all).
func requestedModel(body []byte) (string, bool) {
	d := parseDoc(body)
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
	user, found = lastMember(ms, "user_id")
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
// first non-empty one.
func rememberSession(d *doc, session *string) {
	inner, _, ok := userIDs(d)
	if !ok {
		return
	}
	ms, _ := Object(inner, skipWS(inner, 0))
	if sp, ok := lastMember(ms, "session_id"); ok {
		if sid, isStr := str(inner, sp); isStr && *session == "" {
			*session = sid
		}
	}
}

// swapDevice is the edit that replaces metadata.user_id.device_id (user_id is a
// JSON string) with this session's random one; also remembers the session id.
func (s *Server) swapDevice(d *doc, session *string) (Edit, bool) {
	inner, user, ok := userIDs(d)
	if !ok {
		return Edit{}, false
	}
	ms, _ := Object(inner, skipWS(inner, 0))
	if sp, ok := lastMember(ms, "session_id"); ok {
		if sid, isStr := str(inner, sp); isStr && *session == "" {
			*session = sid
		}
	}
	dev, ok := lastMember(ms, "device_id")
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
// (when swap) in one pass. Not a JSON object, too deep, or nothing changed means
// the original bytes go upstream; changed bytes keep CJK as UTF-8 unless a lone
// surrogate forces the ascii form.
func (s *Server) cleanRequest(body []byte, swap bool, session *string) []byte {
	orig := body
	d := parseDoc(body)
	if d == nil {
		return orig
	}
	changed := false
	if s.Hooks.RepairRequest != nil {
		if nb, ok := s.callRepair(body); ok {
			if nd := parseDoc(nb); nd != nil {
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
		return orig
	}
	out, ok := encodeBody(body, edits, false)
	if !ok {
		return orig
	}
	return out
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
