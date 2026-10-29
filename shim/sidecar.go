package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
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

type record struct {
	responseID, turnID string
	requested          *string
	servedModel        *string
	retries            int
	masked             map[string]int
	task               *string
	attempt            int
	responseModel      *string
	timestamp          string
	usage              usageRow
	complete           bool
}

// UsageSidecar writes one row per upstream response, keyed by the response id it
// answers with. A consumer joins a transcript's message id to
// <sha256(response_id)>.json. Each turn carries a random turn_id for row
// ownership; if a gateway recycles a response id, the later turn goes to
// <sha256(response_id.turn_id)>.json, and on to <...turn_id.N>.json for N=1..3,
// so even identical timestamps preserve both rows. A name is taken only when it
// is free or already holds this turn's own row, so a turn whose every name is
// taken loses its own row rather than destroy another's; the claim is atomic: a
// hardlink where supported, an exclusive create where not. No session, prompt,
// header or credential is written: the route is a domain-separated digest of the
// profile and the adapter's route identity. Rows are written at message_start
// and again on every usage update (tmp + rename, 0600), so a truncated stream
// leaves the last known partial row with complete:false. Every failure is
// swallowed: the relay owns the result.
type UsageSidecar struct {
	profile, routeID string
	tags             taskTags
	served           *string
	retries          int
	masked           map[string]int // distinct values masked in the request, by kind
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
	now := time.Now().UTC()
	ts := now.Format("2006-01-02T15:04:05")
	if us := now.Nanosecond() / 1000; us != 0 {
		ts += fmt.Sprintf(".%06d", us)
	}
	return &record{
		responseID: id, turnID: randHex(16), requested: requested, servedModel: u.served,
		retries: u.retries, masked: u.masked, task: u.tags.task, attempt: u.tags.attempt, responseModel: model,
		timestamp: ts + "+00:00", usage: usage,
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
	out = append(out, `,"task":`...)
	out = append(out, optStr(r.task)...)
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

func stateDir() string {
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return filepath.Join(root, "ccmorph")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccmorph")
}

// owns is true only when the row on disk at path carries turn: the one thing
// that makes replacing that name safe.
func owns(path, turn string) bool {
	data, err := os.ReadFile(path)
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
	dir := filepath.Join(stateDir(), "usage")
	if err := os.MkdirAll(filepath.Dir(dir), 0o777); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		if st, serr := os.Stat(dir); !errors.Is(err, fs.ErrExist) || serr != nil || !st.IsDir() {
			return err
		}
	}
	named := func(key string) string {
		sum := sha256.Sum256([]byte(key))
		return filepath.Join(dir, hex.EncodeToString(sum[:])+".json")
	}
	primary := named(r.responseID)
	key := r.responseID + "." + r.turnID
	spills := []string{named(key)}
	for n := 1; n <= 3; n++ {
		spills = append(spills, named(key+"."+strconv.Itoa(n)))
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
		if owns(s, r.turnID) {
			candidates = append(candidates, s)
			break
		}
	}
	candidates = append(append(candidates, primary), spills...)
	for _, c := range candidates {
		ok, err := claim(tmp, c, r.turnID)
		if err != nil {
			return fail(err)
		}
		if ok {
			return nil
		}
	}
	// Every name belongs to another turn: drop this row, never theirs.
	return fail(fs.ErrExist)
}

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
