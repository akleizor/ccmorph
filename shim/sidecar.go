package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// --- usage sidecar (metadata only) -------------------------------------------

var (
	usageBuckets  = []string{"input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"}
	creationNames = []string{"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"}
)

// link and remove are the filesystem calls a test makes fail: a filesystem
// without hardlinks, a temp file that will not unlink.
var (
	link   = os.Link
	remove = os.Remove
)

// costLimit is the largest dollar amount a float still holds to the cent; a
// larger "cost" is no billed turn and would overflow a float total.
const costLimit = (1 << 53) / 100.0

type kv struct {
	key string
	raw []byte
}

// usageRow is an ordered set of whitelisted numeric buckets, values as written.
type usageRow []kv

func (u *usageRow) update(src usageRow) {
next:
	for _, n := range src {
		for i := range *u {
			if (*u)[i].key == n.key {
				(*u)[i].raw = n.raw
				continue next
			}
		}
		*u = append(*u, n)
	}
}

// finiteNumber: a JSON number (never a bool or string) that is a finite float.
func finiteNumber(raw []byte) (float64, bool) {
	if len(raw) == 0 || (raw[0] != '-' && (raw[0] < '0' || raw[0] > '9')) {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(raw), 64)
	return f, err == nil && !math.IsInf(f, 0) && !math.IsNaN(f)
}

func numbers(b []byte, ms []Member, keys []string) usageRow {
	var row usageRow
	for _, k := range keys {
		if sp, ok := lastMember(ms, k); ok {
			if _, fin := finiteNumber(b[sp.Start:sp.End]); fin {
				row = append(row, kv{k, append([]byte(nil), b[sp.Start:sp.End]...)})
			}
		}
	}
	return row
}

// bucketsOf keeps whitelisted numeric usage buckets only, plus the gateway's
// non-negative `cost` below costLimit. A key never seen billed, a non-number
// (service_tier is a string) and a bool all stay out, so nothing here can carry
// a prompt, a header or a credential.
func bucketsOf(b []byte, usage Span, ok bool) usageRow {
	if !ok || b[usage.Start] != '{' {
		return nil
	}
	ms, _ := Object(b, usage.Start)
	row := numbers(b, ms, usageBuckets)
	if sp, ok := lastMember(ms, "cost"); ok {
		if f, fin := finiteNumber(b[sp.Start:sp.End]); fin && f >= 0 && f < costLimit {
			row = append(row, kv{"cost", append([]byte(nil), b[sp.Start:sp.End]...)})
		}
	}
	if sp, ok := lastMember(ms, "cache_creation"); ok && b[sp.Start] == '{' {
		cms, _ := Object(b, sp.Start)
		if nested := numbers(b, cms, creationNames); len(nested) > 0 {
			row = append(row, kv{"cache_creation", nested.json()})
		}
	}
	return row
}

func (u usageRow) json() []byte {
	out := []byte{'{'}
	for i, n := range u {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, Quote(n.key)...)
		out = append(out, ':')
		out = append(out, n.raw...)
	}
	return append(out, '}')
}

// --- prompt-cache lineage (metadata only) -------------------------------------
//
// A session's transcript cannot prove that two adjacent calls sent the same
// cached prefix; the gateway can, because it holds the bytes it forwarded. A
// row for a /v1/messages call therefore carries the digests that settle where
// a warm cache miss came from: head_sha256 over model, tools and system (the
// prompt's stable head) with the request's tool_choice folded in, and lineage,
// one entry per cache breakpoint, as "position:ttl:digest" in prompt order —
// tools, then system, then messages. An explicit marker is named by its own
// block; the top-level marker's automatic breakpoint is named auto@<block>,
// the last block the endpoint can cache, and its digest covers the prompt
// through and including that block. The digest of an explicit marker covers
// every forwarded byte of the prompt before and including its block. An
// entry's name (position:ttl) is the call's cache scope there, so a reader of
// adjacent rows sees whether the scope moved before comparing digests: a
// moved name with the same text means the breakpoint moved, an unchanged name
// with a moved digest says the bytes diverged — with head_sha256 saying
// whether at the head. Matching names and matching digests
// across a zero-read call narrow the owner to what the digest does not cover:
// the endpoint, the upstream account behind the route, or a request field
// outside model, tool_choice, tools, system and messages. It is evidence, not
// proof, that nothing forwarded explains the miss (#993). Like route_id, every
// digest here is a domain-separated one-way digest: no prompt text, header or
// credential leaves the body into a row, and a marker's ttl is one of the
// values the API defines or a fixed "invalid", never the input.

// cacheTTL is a marker's effective lifetime as one of the values the API
// defines — "5m" (also the default when ttl is omitted) or "1h" — and a fixed
// "invalid" for anything else. The raw input is never kept: a gateway accepts
// or ignores what it does not know, and a row must not carry a request string.
func cacheTTL(b []byte, ms []Member) string {
	sp, ok := lastMember(ms, "ttl")
	if !ok {
		return "5m"
	}
	if v, ok := str(b, sp); ok && (v == "5m" || v == "1h") {
		return v
	}
	return "invalid"
}

// cacheable is whether the endpoint can put a cache breakpoint on this block:
// the API's documented list of blocks it will not cache is a thinking or
// redacted_thinking block, an empty text block, and an empty string. Keeping
// the rule in one place is what stops the automatic breakpoint's position and
// its digest from disagreeing. The list could not be re-read from the vendor
// docs here (no network), so a correction belongs in this function alone.
func cacheable(b []byte, blk Span) bool {
	if blk.End <= blk.Start || blk.Start >= len(b) {
		return false
	}
	switch b[blk.Start] {
	case '"':
		return Unquote(b[blk.Start:blk.End]) != ""
	case '{':
		ms, _ := Object(b, blk.Start)
		typ := ""
		if sp, ok := lastMember(ms, "type"); ok {
			typ, _ = str(b, sp)
		}
		switch typ {
		case "thinking", "redacted_thinking":
			return false
		case "text":
			if sp, ok := lastMember(ms, "text"); ok {
				if s, isStr := str(b, sp); isStr && s == "" {
					return false
				}
			}
		}
		return true
	}
	return false
}

// lineageOf digests the forwarded body, reusing the caller's parse d when the
// bytes are the version it holds (one scan per body version, #477). It returns
// "", "" unless the body is a JSON object: the digest of bytes no endpoint can
// parse is not a cache lineage. An object with no cache_control markers gets
// the head only — with no breakpoint, no warm read of it can be correlated.
func lineageOf(body []byte, d *doc) (head, lineage string) {
	var ms []Member
	if d != nil && bytes.Equal(d.b, body) {
		ms = d.ms
	} else {
		if len(body) == 0 || !validJSON(body) {
			return "", ""
		}
		i := skipWS(body, 0)
		if body[i] != '{' {
			return "", ""
		}
		ms, _ = Object(body, i)
	}
	h := sha256.New()
	h.Write([]byte("ccmorph-prompt-lineage-v1\x00"))
	feed := func(tag string, sp Span, ok bool) {
		h.Write([]byte(tag))
		if !ok {
			h.Write([]byte{0})
			return
		}
		h.Write([]byte{1})
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(sp.End-sp.Start))
		h.Write(l[:])
		h.Write(body[sp.Start:sp.End])
	}
	// snap freezes the running hash through the state marshaler sha256's Hash
	// carries: a few hundred bytes per breakpoint, and a call marks few.
	snap := func() []byte {
		s, err := h.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return nil
		}
		return s
	}
	// sumOf is the hex digest of a frozen state, "" when it cannot be read.
	sumOf := func(s []byte) string {
		if s == nil {
			return ""
		}
		c := sha256.New()
		if err := c.(encoding.BinaryUnmarshaler).UnmarshalBinary(s); err != nil {
			return ""
		}
		return hex.EncodeToString(c.Sum(nil))
	}
	digest := func() string { return sumOf(snap()) }
	var entries []string
	// markCC records the breakpoint a cache_control object makes at the
	// current end of the prompt; a marker that is not an object marks nothing.
	markCC := func(pos string, cc Span) {
		if body[cc.Start] != '{' {
			return
		}
		cms, _ := Object(body, cc.Start)
		if dg := digest(); dg != "" {
			entries = append(entries, pos+":"+cacheTTL(body, cms)+":"+dg)
		}
	}
	mark := func(pos string, blk Span) {
		if body[blk.Start] != '{' {
			return
		}
		bms, _ := Object(body, blk.Start)
		if cc, ok := lastMember(bms, "cache_control"); ok {
			markCC(pos, cc)
		}
	}
	// Automatic caching: a top-level cache_control marker puts the endpoint's
	// breakpoint on the last block it can cache. The walk that feeds the hash
	// also remembers that block's position and the hash frozen there, so the
	// entry's name and digest come from one snapshot and cannot disagree.
	autoCC, autoOK := lastMember(ms, "cache_control")
	autoOn := autoOK && autoCC.Start < len(body) && body[autoCC.Start] == '{'
	var autoPos string
	var autoState []byte
	cand := func(pos string, blk Span) {
		if !autoOn || !cacheable(body, blk) {
			return
		}
		if s := snap(); s != nil {
			autoPos, autoState = pos, s
		}
	}
	sp, ok := lastMember(ms, "model")
	feed("model", sp, ok)
	if tsp, ok := lastMember(ms, "tools"); ok && body[tsp.Start] == '[' {
		es, _ := Array(body, tsp.Start)
		for i, e := range es {
			feed("tool", e, true)
			mark("t"+strconv.Itoa(i), e)
			cand("t"+strconv.Itoa(i), e)
		}
	} else {
		feed("tools", tsp, ok)
	}
	if ssp, ok := lastMember(ms, "system"); ok && body[ssp.Start] == '[' {
		es, _ := Array(body, ssp.Start)
		for i, e := range es {
			feed("system", e, true)
			mark("s"+strconv.Itoa(i), e)
			cand("s"+strconv.Itoa(i), e)
		}
	} else {
		feed("system", ssp, ok)
		cand("s0", ssp)
	}
	// head_sha256 also folds in tool_choice: it is not prompt bytes, so it
	// stays out of every prefix digest, but the endpoint drops a cache entry
	// whose tool_choice changed. A moved head with unmoved breakpoint digests
	// then names that class instead of blaming the prompt bytes.
	head = digest()
	tc := sha256.New()
	tc.Write([]byte("ccmorph-prompt-head-v1\x00"))
	if sp, ok := lastMember(ms, "tool_choice"); ok {
		tc.Write([]byte{1})
		var l [8]byte
		binary.BigEndian.PutUint64(l[:], uint64(sp.End-sp.Start))
		tc.Write(l[:])
		tc.Write(body[sp.Start:sp.End])
	} else {
		tc.Write([]byte{0})
	}
	sum := sha256.Sum256(append(append([]byte("ccmorph-prompt-head-v1\x00"), head...), tc.Sum(nil)...))
	head = hex.EncodeToString(sum[:])
	if msp, ok := lastMember(ms, "messages"); ok && body[msp.Start] == '[' {
		es, _ := Array(body, msp.Start)
		for i, m := range es {
			if body[m.Start] != '{' {
				feed("msg", m, true)
				continue
			}
			mms, _ := Object(body, m.Start)
			rsp, ok := lastMember(mms, "role")
			feed("role", rsp, ok)
			csp, ok := lastMember(mms, "content")
			if !ok {
				feed("block", csp, false)
				continue
			}
			if body[csp.Start] == '[' {
				bs, _ := Array(body, csp.Start)
				for j, blk := range bs {
					feed("block", blk, true)
					mark("m"+strconv.Itoa(i)+"."+strconv.Itoa(j), blk)
					cand("m"+strconv.Itoa(i)+"."+strconv.Itoa(j), blk)
				}
				continue
			}
			feed("block", csp, true)
			cand("m"+strconv.Itoa(i)+".0", csp)
		}
	}
	// The automatic entry names the block its breakpoint landed on, so an
	// append that moves the breakpoint is a changed name, not only a new
	// digest. With no eligible block the name says so; the digest is then the
	// whole prompt, as the endpoint falls back to its own last block.
	if autoOn {
		if autoState == nil {
			autoPos, autoState = "none", snap()
		}
		ams, _ := Object(body, autoCC.Start)
		if dg := sumOf(autoState); dg != "" {
			entries = append(entries, "auto@"+autoPos+":"+cacheTTL(body, ams)+":"+dg)
		}
	}
	return head, strings.Join(entries, ",")
}

type record struct {
	responseID, turnID string
	requested          *string
	servedModel        *string
	retries            int
	masked             map[string]int
	head               string
	lineage            string
	task               *string
	session            *string
	parent             *string
	attempt            int
	responseModel      *string
	timestamp          string
	usage              usageRow
	complete           bool
}

// UsageSidecar writes one row per upstream response, keyed by the response id
// it answers with, in the day folder of the row's UTC stamp. A consumer joins
// a transcript's message id to <sha256(response_id)>.json. Each turn carries
// a random turn_id for row ownership; if a gateway recycles a response id,
// the later turn goes to
// <sha256(response_id.turn_id)>.json, and on to <...turn_id.N>.json for N=1..3,
// so even identical timestamps preserve both rows. A name is taken only when it
// is free or already holds this turn's own row, so a turn whose every name is
// taken loses its own row rather than destroy another's; the claim is atomic: a
// hardlink where supported, an exclusive create where not. No session, prompt,
// header or credential is written: the route is a domain-separated digest of the
// profile and the adapter's route identity, and the lineage digests are the
// same kind of digest (lineageOf holds the reasons). Rows are written at message_start
// and again on every usage update (tmp + rename, 0600), so a truncated stream
// leaves the last known partial row with complete:false. Every failure is
// swallowed: the relay owns the result.
type UsageSidecar struct {
	profile, routeID string
	tags             taskTags
	served           *string
	retries          int
	masked           map[string]int // distinct values masked in the request, by kind
	lineageHead      string         // head digest of the forwarded body (lineageOf)
	lineage          string         // its cache breakpoints and prefix digests
	prices           *priceBook     // nil: no upstream host to price against
}

func newSidecar(profile, identity string) *UsageSidecar {
	sum := sha256.Sum256([]byte("ccmorph-route-v1\x00" + profile + "\x00" + identity))
	return &UsageSidecar{profile: profile, routeID: hex.EncodeToString(sum[:]), tags: noTags}
}

// tagged is a view whose rows carry the request's task tags.
func (u *UsageSidecar) tagged(t taskTags) *UsageSidecar {
	v := *u
	v.tags = t
	return &v
}

func (u *UsageSidecar) start(id string, requested *string, model *string, usage usageRow) *record {
	return &record{
		responseID: id, turnID: randHex(16), requested: requested, servedModel: u.served,
		retries: u.retries, masked: u.masked, head: u.lineageHead, lineage: u.lineage,
		task: u.tags.task, session: u.tags.session, parent: u.tags.parent, attempt: u.tags.attempt, responseModel: model,
		timestamp: utcStamp(time.Now().UTC()), usage: usage,
	}
}

func jsonStr(s string) []byte {
	out := []byte{}
	(&canon{ascii: true}).str(&out, []byte(s))
	return out
}

func optStr(s *string) []byte {
	if s == nil {
		return []byte("null")
	}
	return jsonStr(*s)
}

func (u *UsageSidecar) row(r *record) []byte {
	out := []byte(`{"schema":1,"response_id":`)
	out = append(out, jsonStr(r.responseID)...)
	out = append(out, `,"turn_id":`...)
	out = append(out, jsonStr(r.turnID)...)
	out = append(out, `,"route_id":"`+u.routeID+`","profile":`...)
	out = append(out, jsonStr(u.profile)...)
	out = append(out, `,"requested_model":`...)
	out = append(out, optStr(r.requested)...)
	out = append(out, `,"served_model":`...)
	out = append(out, optStr(r.servedModel)...)
	out = append(out, `,"retries":`+strconv.Itoa(r.retries)...)
	if len(r.masked) > 0 {
		out = append(out, `,"masked":`+maskedJSON(r.masked)...)
	}
	if r.head != "" {
		out = append(out, `,"head_sha256":`...)
		out = append(out, jsonStr(r.head)...)
	}
	if r.lineage != "" {
		out = append(out, `,"lineage":`...)
		out = append(out, jsonStr(r.lineage)...)
	}
	out = append(out, `,"task":`...)
	out = append(out, optStr(r.task)...)
	if r.session != nil {
		out = append(out, `,"session":`...)
		out = append(out, optStr(r.session)...)
	}
	if r.parent != nil {
		out = append(out, `,"parent":`...)
		out = append(out, optStr(r.parent)...)
	}
	out = append(out, `,"attempt":`+strconv.Itoa(r.attempt)+`,"response_model":`...)
	out = append(out, optStr(r.responseModel)...)
	out = append(out, `,"timestamp":"`+r.timestamp+`","usage":`...)
	out = append(out, u.withCost(r).json()...)
	if r.complete {
		return append(out, `,"complete":true}`...)
	}
	return append(out, `,"complete":false}`...)
}

// withCost is the row's usage with estimated_cost computed from the provider's
// list price when the upstream billed none. `cost` stays what the gateway billed
// and nothing else, so a reader summing billed spend never adds a list price.
func (u *UsageSidecar) withCost(r *record) usageRow {
	if u.prices == nil || slices.ContainsFunc(r.usage, func(n kv) bool { return n.key == "cost" }) {
		return r.usage
	}
	model := ""
	if r.servedModel != nil {
		model = *r.servedModel
	} else if r.responseModel != nil {
		model = *r.responseModel
	}
	if model == "" {
		return r.usage
	}
	p, ok := u.prices.lookup(model)
	if !ok {
		return r.usage
	}
	c, ok := p.estimate(r.usage)
	if !ok {
		return r.usage
	}
	return append(slices.Clone(r.usage), kv{"estimated_cost", []byte(strconv.FormatFloat(c, 'f', -1, 64))})
}

// usageBase is the store every usage row lives in, sharded by UTC day.
func usageBase() string { return filepath.Join(paths.StateDir(), "usage") }

// usageDay is the UTC day folder a row stamped ts belongs to. A stamp that
// will not parse is today: the sidecar stamps what it writes now, and a row
// carries its own timestamp for a reader to filter on. The name is
// paths.DayLayout's, the one spelling the store's writer and readers share.
func usageDay(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t = time.Now()
	}
	return t.UTC().Format(paths.DayLayout)
}

// rowDirs is where the row of a response stamped ts can be found, and the
// first entry is where its write claims: the stamp's own UTC day folder, then
// the neighbouring days (a response that spans a UTC midnight puts its earlier
// and later stamps one folder apart), then the flat store a build before the
// shards — or a move that could not rename — left rows in.
func rowDirs(ts string) []string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t = time.Now()
	}
	t = t.UTC()
	base := usageBase()
	return []string{
		filepath.Join(base, t.Format(paths.DayLayout)),
		filepath.Join(base, t.AddDate(0, 0, -1).Format(paths.DayLayout)),
		filepath.Join(base, t.AddDate(0, 0, 1).Format(paths.DayLayout)),
		base,
	}
}

// owns is true only when the row on disk at path carries turn: the one thing
// that makes replacing that name safe.
func owns(path, turn string) bool {
	data, err := paths.ReadRegularFile(path)
	if err != nil {
		return false
	}
	d := parseDoc(data)
	if d == nil {
		return false
	}
	t, ok := d.str("turn_id")
	return ok && t == turn
}

// claim places the row in tmp at candidate atomically: a hardlink under a free
// name, an exclusive create where the filesystem has no hardlinks (both fail
// when the name is taken, which is the claim), or a replace when the name
// already holds this record's own earlier row. False only when the name belongs
// to another turn.
func claim(tmp, candidate, turn string) (bool, error) {
	err := link(tmp, candidate)
	switch {
	case err == nil:
		remove(tmp)
		return true, nil
	case errors.Is(err, fs.ErrExist):
		if !owns(candidate, turn) {
			return false, nil
		}
	default:
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
		} else if errors.Is(err, fs.ErrExist) {
			if !owns(candidate, turn) {
				return false, nil
			}
		} else {
			return false, err
		}
	}
	return true, os.Rename(tmp, candidate)
}

func (u *UsageSidecar) write(r *record) error {
	lock, err := lockClosedUsage(time.Now(), rowDirs(r.timestamp)[:1])
	if err != nil {
		return err
	}
	if lock != nil {
		defer lock.Close()
	}
	return u.writeLocked(r)
}

func (u *UsageSidecar) writeLocked(r *record) error {
	base := usageBase()
	dir := filepath.Join(base, usageDay(r.timestamp))
	if err := os.MkdirAll(filepath.Dir(base), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(base, 0o700); err != nil {
		if st, serr := os.Stat(base); !errors.Is(err, fs.ErrExist) || serr != nil || !st.IsDir() {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if st, serr := os.Stat(dir); !errors.Is(err, fs.ErrExist) || serr != nil || !st.IsDir() {
			return err
		}
	}
	primary, spills := rowNames(dir, r.responseID, r.turnID)
	archived, err := paths.ReadUsageArchive(dir + ".jsonl")
	if err != nil {
		// A damaged archive this run cannot read no longer holds every write
		// of the day: recorded once, the claim proceeds on the folder alone.
		// Readers merge the original filename once, so no row is double-counted.
		logScanOnce(err)
		archived = nil
	}
	// The claim asks the archive about this response's own names only: a map of
	// the whole day per written row made every row of a long transcript pay for
	// the day's size under the store's exclusive lock (#654). The archive's
	// names are strictly ordered (ReadUsageArchive verifies it).
	archivedRow := func(path string) ([]byte, bool) {
		name := filepath.Base(path)
		i := sort.Search(len(archived), func(i int) bool { return archived[i].Name >= name })
		if i < len(archived) && archived[i].Name == name {
			return archived[i].Data, true
		}
		return nil, false
	}
	f, err := os.CreateTemp(dir, "tmp*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(u.row(r))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	fail := func(err error) error { remove(tmp); return err }
	if werr != nil {
		return fail(werr)
	}
	// A spill this turn already owns stays its file even if the primary (or an
	// earlier spill) was removed; ownership comes from the disk row.
	var candidates []string
	for _, s := range spills {
		if data, _ := archivedRow(s); owns(s, r.turnID) || usageOwned(data, r.responseID, r.turnID) {
			candidates = append(candidates, s)
			break
		}
	}
	candidates = append(append(candidates, primary), spills...)
	for _, c := range candidates {
		if data, ok := archivedRow(c); ok && !usageOwned(data, r.responseID, r.turnID) {
			continue
		}
		ok, err := claim(tmp, c, r.turnID)
		if err != nil {
			return fail(err)
		}
		if ok {
			return nil
		}
	}
	// Every name belongs to another turn: drop this row, never theirs. The
	// sentinel is distinct because a directory failure can also wrap fs.ErrExist
	// (a file where the usage dir belongs), and a transcript reader must retry
	// that one while this one is permanent.
	return fail(errRowNameTaken)
}

// retireEarlierCopies removes this turn's own row from every directory but the
// one its write just claimed: a response whose stamp crossed a UTC day between
// two hook passes — or that a build before the shards wrote flat — would
// otherwise sit in two places, and a reader would count the response twice.
// Ownership is the turn_id the row on disk carries; another turn's row is
// never touched. Only the hook's rows move days (a relay's record keeps the
// stamp it was started with), so only the hook calls this. A spill a build
// before #633 spelled under the joined key's own hash is one of those copies
// too: it still carries this turn, so it is retired with the new-shape ones.
func retireEarlierCopies(dirs []string, responseID, turnID string) {
	for _, dir := range dirs[1:] {
		primary, spills := rowNames(dir, responseID, turnID)
		spills = append(spills, legacySpillNames(dir, responseID, turnID)...)
		for _, path := range append([]string{primary}, spills...) {
			if owns(path, turnID) {
				remove(path)
			}
		}
	}
}

// rowNames is where one response's row can live, in the sidecar's own claim
// order: the primary name its id owns, then the spills its turn owns when a
// gateway recycled that id. A writer and a reader of these rows share this one
// definition, so neither can disagree with the other about the store.
func rowNames(dir, responseID, turnID string) (primary string, spills []string) {
	primary = filepath.Join(dir, rowFileName(responseID))
	key := responseID + "." + turnID
	spills = []string{filepath.Join(dir, spillFileName(responseID, key))}
	for n := 1; n <= 3; n++ {
		spills = append(spills, filepath.Join(dir, spillFileName(responseID, key+"."+strconv.Itoa(n))))
	}
	return primary, spills
}

// legacySpillNames is where a build before #633 put a spill: the joined key's
// own hash, with the .N names the old claim tried after it. The new shape is
// the response id's hash followed by the key's, so the two are unrelated
// strings; only a reader that holds the row's own turn can name one, and
// migrateOldSpills re-files them under the new shape when it scans a directory.
func legacySpillNames(dir, responseID, turnID string) []string {
	key := responseID + "." + turnID
	names := []string{filepath.Join(dir, rowFileName(key))}
	for n := 1; n <= 3; n++ {
		names = append(names, filepath.Join(dir, rowFileName(key+"."+strconv.Itoa(n))))
	}
	return names
}

// errRowNameTaken is a row no name on disk can hold: every candidate belongs to
// another turn, so writing it again answers the same. It wraps fs.ErrExist,
// which the sidecar's own tests read as the shape of a lost row.
var errRowNameTaken = fmt.Errorf("every row name belongs to another turn: %w", fs.ErrExist)

// observe folds one parsed SSE event into rec and persists what is known.
// Never fails the relay.
func (u *UsageSidecar) observe(d *doc, rec *record, requested *string) *record {
	if d == nil {
		return rec
	}
	kind, _ := d.str("type")
	switch {
	case kind == "message_start":
		if sp, ok := d.last("message"); ok && d.b[sp.Start] == '{' {
			ms, _ := Object(d.b, sp.Start)
			if idSpan, ok := lastMember(ms, "id"); ok {
				if id, isStr := str(d.b, idSpan); isStr {
					var model *string
					if ms2, ok := lastMember(ms, "model"); ok {
						if m, isStr := str(d.b, ms2); isStr {
							model = &m
						}
					}
					us, hasUsage := lastMember(ms, "usage")
					rec = u.start(id, requested, model, bucketsOf(d.b, us, hasUsage))
					u.write(rec)
				}
			}
		}
	case rec != nil && kind == "message_delta":
		us, ok := d.last("usage")
		if usage := bucketsOf(d.b, us, ok); len(usage) > 0 {
			rec.usage.update(usage)
			u.write(rec)
		}
	case rec != nil && kind == "message_stop":
		us, ok := d.last("usage")
		rec.usage.update(bucketsOf(d.b, us, ok))
		rec.complete = true
		u.write(rec)
	}
	return rec
}

// response records a non-stream JSON message as one complete row.
func (u *UsageSidecar) response(data []byte, requested *string) {
	d := parseDoc(data)
	if d == nil {
		return
	}
	id, ok := d.str("id")
	if !ok {
		return
	}
	var model *string
	if m, ok := d.str("model"); ok {
		model = &m
	}
	us, hasUsage := d.last("usage")
	rec := u.start(id, requested, model, bucketsOf(d.b, us, hasUsage))
	rec.complete = true
	u.write(rec)
}
