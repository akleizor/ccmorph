package shim

// Secret masking, a per-profile opt-in (CCMORPH_MASK=1). Secrets and passwords
// in what Claude Code sends go upstream as placeholders such as
// {{API_KEY_k3v9x2mqa7bc}}, and every placeholder in the reply is put back, in
// text and in tool input, streamed or not: the session and the commands it runs
// keep the real values while the provider never sees them.
//
// A placeholder is a keyed hash of its value, so the same value reads the same
// in every turn and the prompt cache holds. Values are held in memory only, as
// requests bring them; the history every request carries brings them again
// after a restart. A value once found is masked wherever it appears, which is
// what keeps a password the agent typed into a tool from leaking on the next
// turn. Thinking is neither masked nor put back: it reaches the client with its
// placeholders, so a replayed block still matches the signature it was made for.

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// minMasked is the shortest value masked. shortcut: a shorter secret (a 4-digit
// PIN) goes as written; masking it would also hit every number that contains it.
const minMasked = 6

type maskRule struct {
	kind string
	re   *regexp.Regexp // the value is group 1 when the pattern has one
	ok   func(string) bool
}

var (
	// identifierRE is a code reference, not a value: token = self.token,
	// password: config.password, api_key = os.environ.
	identifierRE = regexp.MustCompile(`^[A-Za-z_.\-\[\]]+$`)
	digitsRE     = regexp.MustCompile(`^[0-9.]+$`)
)

// assigned is a value worth masking after "<name>=" or "<name>:".
func assigned(v string) bool {
	if strings.ContainsAny(v[:1], "$%{<*([@#&|") || strings.Contains(v, "{{") || strings.Contains(v, "(") {
		return false
	}
	return !identifierRE.MatchString(v) && !digitsRE.MatchString(v)
}

func notTemplate(v string) bool {
	return !strings.ContainsAny(v[:1], "${<*%") && !strings.Contains(v, "{{")
}

var maskRules = []maskRule{
	{"PRIVATE_KEY", regexp.MustCompile(`-----BEGIN[A-Z ]*PRIVATE KEY-----[\s\S]*?-----END[A-Z ]*PRIVATE KEY-----`), nil},
	{"API_KEY", regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{20,}|sk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{32,}|` +
		`gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{40,}|glpat-[A-Za-z0-9_-]{20,}|` +
		`(?:AKIA|ASIA)[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|xox[abposr]-[A-Za-z0-9-]{10,}|` +
		`(?:sk|rk)_live_[A-Za-z0-9]{20,}|hf_[A-Za-z0-9]{30,}|npm_[A-Za-z0-9]{36}|ops_[A-Za-z0-9_-]{40,})`), nil},
	{"TOKEN", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), nil},
	{"TOKEN", regexp.MustCompile(`(?i)\bbearer\s+([A-Za-z0-9._~+/=-]{20,})`), notTemplate},
	// user:password@ in a URL or connection string
	{"PASSWORD", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^\s:/@'"]+:([^\s@/'"]+)@`), notTemplate},
	{"PASSWORD", regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:password|passwd|pwd)[A-Za-z0-9_.-]*["']?\s*[:=]\s*["']?([^\s"'` + "`" + `,;]{6,})`), assigned},
	{"SECRET", regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:secret|token|api[_-]?key|access[_-]?key|private[_-]?key)[A-Za-z0-9_.-]*["']?\s*[:=]\s*["']?([^\s"'` + "`" + `,;]{12,})`), assigned},
}

// fetchRE is a command whose whole output is a password: one simple command
// (no pipe, list or redirect) from a password manager. Its one-line output is
// masked as written.
var fetchRE = regexp.MustCompile(`^\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*(?:` +
	`op\s+read\b|op\s+item\s+get\b[^;&|<>]*--(?:fields|reveal)\b|` +
	`security\s+find-(?:generic|internet)-password\b[^;&|<>]*\s-w\b|` +
	`bw\s+get\s+(?:password|totp)\b|(?:go)?pass\s+show\b)[^;&|<>]*$`)

var placeholderRE = regexp.MustCompile(`\{\{([A-Z_]+)_([a-z2-7]{12})\}\}`)

// placeholderPrefixRE is a text end that may be the start of a placeholder a
// stream split across two events.
var placeholderPrefixRE = regexp.MustCompile(`\{(?:\{(?:[A-Z_]*(?:_[a-z2-7]{0,12}\}?)?)?)?$`)

type masker struct {
	key      []byte
	commands *regexp.Regexp // the profile's own password commands, beside fetchRE

	mu     sync.Mutex
	values map[string]string // placeholder -> value
	known  map[string]string // value -> placeholder
}

func newMasker(key []byte, commands string) (*masker, error) {
	m := &masker{key: key, values: map[string]string{}, known: map[string]string{}}
	if commands = strings.TrimSpace(commands); commands != "" {
		re, err := regexp.Compile(commands)
		if err != nil {
			return nil, fmt.Errorf("CCMORPH_MASK_COMMANDS: %w", err)
		}
		m.commands = re
	}
	return m, nil
}

// maskKey is the install's placeholder key, made on first use: placeholders
// stay the same across relaunches, so a resumed session's cache still holds.
func maskKey() ([]byte, error) {
	dir := stateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "mask.key")
	if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		key := make([]byte, 32)
		randRead(key)
		_, werr := f.Write(key)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return nil, werr
		}
		return key, nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s is not a masking key; delete it to make a new one", path)
	}
	return key, nil
}

func (m *masker) learn(value, kind string) {
	if len(value) < minMasked {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.known[value]; ok {
		return
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(value))
	tag := strings.ToLower(base32.StdEncoding.EncodeToString(mac.Sum(nil)))[:12]
	ph := "{{" + kind + "_" + tag + "}}"
	m.values[ph] = value
	m.known[value] = ph
}

func (m *masker) find(s string) {
	for _, r := range maskRules {
		for _, loc := range r.re.FindAllStringSubmatchIndex(s, -1) {
			a, b := loc[0], loc[1]
			if len(loc) >= 4 && loc[2] >= 0 {
				a, b = loc[2], loc[3]
			}
			if v := s[a:b]; r.ok == nil || r.ok(v) {
				m.learn(v, r.kind)
			}
		}
	}
}

// replacer swaps every known value for its placeholder, longest value first so
// a value inside a longer one never splits it.
func (m *masker) replacer() *strings.Replacer {
	m.mu.Lock()
	defer m.mu.Unlock()
	vals := make([]string, 0, len(m.known))
	for v := range m.known {
		vals = append(vals, v)
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, m.known[v])
	}
	return strings.NewReplacer(pairs...)
}

// target is one string of a request that goes through masking; fetched is the
// output of a password command, masked whole.
type target struct {
	sp      Span
	s       string
	fetched bool
}

// requestTargets are the strings of a Messages request that carry what the
// session read or wrote: system text, message text, tool input and tool
// results. Ids, signatures, thinking, images and documents are left alone.
// ok is false for a body too deep to read.
type requestTargets struct {
	b       []byte
	m       *masker
	fetches map[string]bool // tool_use ids whose command prints a password
	out     []target
	tooDeep bool
}

func (t *requestTargets) addString(sp Span, fetched bool) {
	if s, ok := str(t.b, sp); ok && s != "" {
		t.out = append(t.out, target{sp, s, fetched})
	}
}

// leaves adds every string under the value at sp (tool input).
func (t *requestTargets) leaves(sp Span, depth int) {
	if depth > maxDepth {
		t.tooDeep = true
		return
	}
	switch t.b[sp.Start] {
	case '"':
		t.addString(sp, false)
	case '{':
		ms, _ := Object(t.b, sp.Start)
		for _, mb := range ms {
			t.leaves(mb.Value, depth+1)
		}
	case '[':
		es, _ := Array(t.b, sp.Start)
		for _, e := range es {
			t.leaves(e, depth+1)
		}
	}
}

// content adds the text of a content value: a string or a list of blocks.
func (t *requestTargets) content(sp Span, fetched bool, depth int) {
	if depth > maxDepth {
		t.tooDeep = true
		return
	}
	switch t.b[sp.Start] {
	case '"':
		t.addString(sp, fetched)
	case '[':
		es, _ := Array(t.b, sp.Start)
		for _, e := range es {
			if t.b[e.Start] == '{' {
				t.block(e, fetched, depth+1)
			}
		}
	}
}

func (t *requestTargets) block(sp Span, fetched bool, depth int) {
	ms, _ := Object(t.b, sp.Start)
	typ := ""
	if v, ok := lastMember(ms, "type"); ok {
		typ, _ = str(t.b, v)
	}
	switch typ {
	case "text":
		if v, ok := lastMember(ms, "text"); ok {
			t.addString(v, fetched)
		}
	case "tool_use", "server_tool_use":
		if v, ok := lastMember(ms, "input"); ok {
			t.leaves(v, depth+1)
		}
	case "tool_result":
		v, ok := lastMember(ms, "content")
		if !ok {
			return
		}
		id := ""
		if idv, ok := lastMember(ms, "tool_use_id"); ok {
			id, _ = str(t.b, idv)
		}
		// a failed fetch prints an error, not a password
		failed := false
		if e, ok := lastMember(ms, "is_error"); ok {
			failed = string(t.b[e.Start:e.End]) == "true"
		}
		t.content(v, t.fetches[id] && !failed, depth+1)
	}
}

// commandOf is a tool_use block's input.command, if it has one.
func commandOf(b []byte, sp Span) (id, command string) {
	ms, _ := Object(b, sp.Start)
	if v, ok := lastMember(ms, "type"); !ok {
		return "", ""
	} else if typ, _ := str(b, v); typ != "tool_use" {
		return "", ""
	}
	if v, ok := lastMember(ms, "id"); ok {
		id, _ = str(b, v)
	}
	in, ok := lastMember(ms, "input")
	if !ok || b[in.Start] != '{' {
		return id, ""
	}
	ims, _ := Object(b, in.Start)
	if v, ok := lastMember(ims, "command"); ok {
		command, _ = str(b, v)
	}
	return id, command
}

func (m *masker) fetches(command string) bool {
	return fetchRE.MatchString(command) || (m.commands != nil && m.commands.MatchString(command))
}

// maskRequest is body with every secret in it replaced by its placeholder, and
// how many distinct values of each kind it masked. ok is false when the body
// cannot be read: the caller then sends nothing.
func (m *masker) maskRequest(body []byte) (out []byte, masked map[string]int, ok bool) {
	d := parseDoc(body)
	if d == nil {
		return nil, nil, false
	}
	t := &requestTargets{b: body, m: m, fetches: map[string]bool{}}
	msgs, hasMsgs := d.last("messages")
	var turns []Span
	if hasMsgs && body[msgs.Start] == '[' {
		turns, _ = Array(body, msgs.Start)
	}
	for _, turn := range turns {
		if body[turn.Start] != '{' {
			continue
		}
		tms, _ := Object(body, turn.Start)
		if c, ok := lastMember(tms, "content"); ok && body[c.Start] == '[' {
			blocks, _ := Array(body, c.Start)
			for _, bl := range blocks {
				if body[bl.Start] != '{' {
					continue
				}
				if id, cmd := commandOf(body, bl); id != "" && cmd != "" && m.fetches(cmd) {
					t.fetches[id] = true
				}
			}
		}
	}
	if sys, ok := d.last("system"); ok {
		t.content(sys, false, 1)
	}
	for _, turn := range turns {
		if body[turn.Start] == '{' {
			tms, _ := Object(body, turn.Start)
			if c, ok := lastMember(tms, "content"); ok {
				t.content(c, false, 2)
			}
		}
	}
	if t.tooDeep {
		return nil, nil, false
	}
	for _, tg := range t.out {
		if v := strings.TrimSpace(tg.s); tg.fetched && v != "" && !strings.Contains(v, "\n") {
			m.learn(v, "PASSWORD")
		}
		m.find(tg.s)
	}
	r := m.replacer()
	var edits []Edit
	seen := map[string]string{}
	for _, tg := range t.out {
		ns := r.Replace(tg.s)
		if ns == tg.s {
			continue
		}
		edits = append(edits, Edit{Start: tg.sp.Start, End: tg.sp.End, IsText: true, Text: []byte(ns)})
		for _, g := range placeholderRE.FindAllStringSubmatch(ns, -1) {
			seen[g[0]] = g[1]
		}
	}
	if len(edits) == 0 {
		return body, nil, true
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].Start < edits[j].Start })
	out, ok = encodeBody(body, edits, false)
	if !ok {
		return nil, nil, false
	}
	masked = map[string]int{}
	for _, kind := range seen {
		masked[kind]++
	}
	return out, masked, true
}

// restore puts each known placeholder in s back; quoted is for text that sits
// inside a JSON string (a streamed tool input), where the value goes escaped.
func (m *masker) restore(s string, quoted bool) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return placeholderRE.ReplaceAllStringFunc(s, func(ph string) string {
		v, ok := m.values[ph]
		if !ok {
			return ph
		}
		if quoted {
			q := Quote(v)
			return string(q[1 : len(q)-1])
		}
		return v
	})
}

// restoreBody is a JSON reply with its placeholders put back, outside thinking.
func (m *masker) restoreBody(b []byte, spaced bool) []byte {
	if !strings.Contains(string(b), "{{") || !validJSON(b) {
		return b
	}
	var edits []Edit
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if depth > maxDepth {
			return
		}
		switch b[i] {
		case '"':
			end := scanString(b, i)
			if s, _ := str(b, Span{i, end}); strings.Contains(s, "{{") {
				if ns := m.restore(s, false); ns != s {
					edits = append(edits, Edit{Start: i, End: end, IsText: true, Text: []byte(ns)})
				}
			}
		case '{':
			ms, _ := Object(b, i)
			if v, ok := lastMember(ms, "type"); ok {
				if typ, _ := str(b, v); typ == "thinking" || typ == "redacted_thinking" {
					return
				}
			}
			for _, mb := range ms {
				walk(mb.Value.Start, depth+1)
			}
		case '[':
			es, _ := Array(b, i)
			for _, e := range es {
				walk(e.Start, depth+1)
			}
		}
	}
	walk(skipWS(b, 0), 0)
	if len(edits) == 0 {
		return b
	}
	out, ok := (&canon{ascii: true, spaced: spaced}).encode(b, edits)
	if !ok {
		return b
	}
	return out
}

// maskedJSON is the usage row's "masked" member: distinct values by kind.
func maskedJSON(masked map[string]int) string {
	kinds := make([]string, 0, len(masked))
	for k := range masked {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range kinds {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"` + k + `":` + strconv.Itoa(masked[k]))
	}
	sb.WriteByte('}')
	return sb.String()
}

// --- streamed replies -------------------------------------------------------------

type lineSource interface {
	readline() ([]byte, error)
}

// unmaskStream is an SSE reply with its placeholders put back in text and tool
// input deltas. It works an event at a time; a delta whose text ends in what may
// be the start of a placeholder is held and joined to the next delta of its
// block, so a placeholder split across two events is put back whole. Nothing is
// added or dropped: the text the client assembles is the reply's, restored.
type unmaskStream struct {
	src   lineSource
	m     *masker
	out   [][]byte
	event [][]byte
	held  *heldDelta
	err   error
}

type heldDelta struct {
	lines       [][]byte
	index       int
	field, text string
	quoted      bool
}

func (u *unmaskStream) readline() ([]byte, error) {
	for len(u.out) == 0 {
		if u.err != nil {
			u.flush()
			u.out = append(u.out, u.event...)
			u.event = nil
			if len(u.out) == 0 {
				return nil, u.err
			}
			break
		}
		line, err := u.src.readline()
		if err != nil {
			u.err = err
			continue
		}
		u.event = append(u.event, line)
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			ev := u.event
			u.event = nil
			u.process(ev)
		}
	}
	line := u.out[0]
	u.out = u.out[1:]
	return line, nil
}

// delta is the event's text delta: its index, field and text.
func deltaOf(ev [][]byte) (line int, d *doc, index int, field, text string, ok bool) {
	for i, l := range ev {
		_, payload, _, isData := dataPayload(l)
		if !isData || !strings.Contains(string(payload), `"content_block_delta"`) {
			continue
		}
		d = parseDoc(payload)
		if d == nil {
			return 0, nil, 0, "", "", false
		}
		if typ, _ := d.str("type"); typ != "content_block_delta" {
			return 0, nil, 0, "", "", false
		}
		index, ok = intField(d, "index")
		ds, has := d.last("delta")
		if !ok || !has || d.b[ds.Start] != '{' {
			return 0, nil, 0, "", "", false
		}
		dms, _ := Object(d.b, ds.Start)
		for _, f := range []string{"text", "partial_json"} {
			if v, has := lastMember(dms, f); has {
				if s, isStr := str(d.b, v); isStr {
					return i, d, index, f, s, true
				}
			}
		}
		return 0, nil, 0, "", "", false
	}
	return 0, nil, 0, "", "", false
}

func (u *unmaskStream) process(ev [][]byte) {
	at, d, index, field, text, ok := deltaOf(ev)
	if !ok {
		u.flush()
		u.out = append(u.out, ev...)
		return
	}
	if h := u.held; h != nil {
		u.held = nil
		if h.index == index && h.field == field {
			text = h.text + text
		} else {
			u.emit(h.lines, h.text, h.field, h.quoted, -1, nil)
		}
	}
	quoted := field == "partial_json"
	if placeholderPrefixRE.MatchString(text) {
		u.held = &heldDelta{lines: ev, index: index, field: field, text: text, quoted: quoted}
		return
	}
	u.emit(ev, text, field, quoted, at, d)
}

func (u *unmaskStream) flush() {
	if h := u.held; h != nil {
		u.held = nil
		u.emit(h.lines, h.text, h.field, h.quoted, -1, nil)
	}
}

// emit writes the event with its delta's text set to text, restored.
func (u *unmaskStream) emit(ev [][]byte, text, field string, quoted bool, at int, d *doc) {
	if d == nil {
		at, d, _, _, _, _ = deltaOf(ev)
	}
	ds, _ := d.last("delta")
	dms, _ := Object(d.b, ds.Start)
	sp, _ := lastMember(dms, field)
	orig, _ := str(d.b, sp)
	text = u.m.restore(text, quoted)
	if text == orig {
		u.out = append(u.out, ev...)
		return
	}
	payload, ok := (&canon{ascii: true}).encode(d.b, []Edit{{Start: sp.Start, End: sp.End, IsText: true, Text: []byte(text)}})
	if !ok {
		u.out = append(u.out, ev...)
		return
	}
	prefix, _, eol, _ := dataPayload(ev[at])
	lines := append([][]byte{}, ev...)
	lines[at] = append(append(append([]byte{}, prefix...), payload...), eol...)
	u.out = append(u.out, lines...)
}
