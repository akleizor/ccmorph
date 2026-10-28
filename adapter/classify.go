package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/shim"
)

const (
	thinkingProbeVersion = 2
	jobs                 = 8
	// How long to wait for another process's classification before falling back
	// to the cache. Must stay under the caller's readiness budget (600 s).
	lockWait = 120 * time.Second
)

// Row is one model's cached classification.
type Row struct {
	Model     string `json:"model"`
	Dialect   string `json:"dialect"`
	Note      string `json:"note"`
	Status    int    `json:"status"`
	Cap       *int   `json:"cap"`
	Nothink   bool   `json:"nothink"`
	IDRecycle bool   `json:"id_recycle"`
}

func (r Row) capped() bool { return r.Cap != nil && *r.Cap != 0 }

// rowKeys is every key a cached row must carry; a row from an older schema (or a
// truncated write) is dropped at load, which re-probes exactly that model.
var rowKeys = []string{"model", "dialect", "note", "cap", "nothink", "id_recycle", "status"}

// usableRow converts a cached row, or reports it unusable.
func usableRow(v any) (Row, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return Row{}, false
	}
	for _, k := range rowKeys {
		if _, has := m[k]; !has {
			return Row{}, false
		}
	}
	model, ok1 := m["model"].(string)
	dialect, ok2 := m["dialect"].(string)
	if !ok1 || !ok2 {
		return Row{}, false
	}
	r := Row{Model: model, Dialect: dialect, Nothink: truthy(m["nothink"]), IDRecycle: truthy(m["id_recycle"])}
	r.Note, _ = m["note"].(string)
	if f, ok := m["status"].(float64); ok {
		r.Status = int(f)
	}
	if f, ok := m["cap"].(float64); ok {
		n := int(f)
		r.Cap = &n
	}
	return r, true
}

func sortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Dialect != rows[j].Dialect {
			return rows[i].Dialect < rows[j].Dialect
		}
		return rows[i].Model < rows[j].Model
	})
}

// cachedRows is every well-formed cached row for this endpoint, or none. The
// stored endpoint is re-checked on every read, so a moved base_url or a hash
// collision falls through to a fresh probe.
func cachedRows(base, cache string) []Row {
	raw, err := os.ReadFile(cache)
	if err != nil {
		return nil
	}
	var blob map[string]any
	if json.Unmarshal(raw, &blob) != nil || blob["endpoint"] != base {
		return nil
	}
	rowsIn, ok := blob["rows"].([]any)
	if !ok && blob["rows"] != nil {
		return nil
	}
	// Rows written before the anthropic_path marker probed the wrong URL, so
	// every verdict from that era is unreliable.
	if blob["anthropic_path"] != "/v1/messages" {
		return nil
	}
	oldThinking := blob["thinking_probe"] != float64(thinkingProbeVersion)
	var rows []Row
	for _, v := range rowsIn {
		if r, ok := usableRow(v); ok && !(oldThinking && r.Nothink) {
			rows = append(rows, r)
		}
	}
	return rows
}

func cachedSelection(base, cache string, cached []Row, required string) []Row {
	var row *Row
	for i := range cached {
		if cached[i].Model == required {
			row = &cached[i]
			break
		}
	}
	if row == nil {
		return nil
	}
	if row.Dialect == "unusable" {
		note := row.Note
		if note == "" {
			note = "no usable dialect answered"
		}
		die("model %s is cached as unusable on %s (%s); choose another model. Cache: %s",
			shim.PyRepr(required), base, note, cache)
	}
	out := append([]Row(nil), cached...)
	sortRows(out)
	return out
}

// classify returns every model's row, probing the ones never seen on this
// endpoint. A selected-model launch takes the cached fast path before touching
// inventory or the lock. The whole load-probe-write is serialized on a per-cache
// lock, which is an optimisation and never a precondition: if it cannot be
// opened the work is done unlocked, because losing the launch costs the session.
func classify(base, key, cache, required string, auto bool) ([]Row, error) {
	if required != "" {
		if rows := cachedSelection(base, cache, cachedRows(base, cache), required); rows != nil {
			return rows, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(cache), 0o777); err != nil {
		logf("cache directory unavailable (%s); classifying without a lock", pyOSError(err))
		return classifyLocked(base, key, cache, required, auto)
	}
	lock, err := os.OpenFile(cache+".lock", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		logf("lock unavailable (%s); classifying without one -- a concurrent run may duplicate this sweep, which is slow but correct", pyOSError(err))
		return classifyLocked(base, key, cache, required, auto)
	}
	defer lock.Close()
	deadline := time.Now().Add(lockWait)
	announced := false
	for syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		if !time.Now().Before(deadline) {
			cached := cachedRows(base, cache)
			// A selected model waits for ITS OWN row, not merely for any cache.
			if required != "" {
				found := false
				for _, r := range cached {
					found = found || r.Model == required
				}
				if !found {
					die("another classification of %s has held the lock for %ds and %s is still not cached; retry after the active check finishes",
						base, int(lockWait.Seconds()), shim.PyRepr(required))
				}
			}
			if len(cached) > 0 {
				logf("another classification of %s still running after %ds; using %d cached row(s)",
					base, int(lockWait.Seconds()), len(cached))
				sortRows(cached)
				return cached, nil
			}
			die("another classification of %s has held the lock for %ds and nothing is cached yet; retry after the active check finishes",
				base, int(lockWait.Seconds()))
		}
		if !announced {
			logf("waiting for an in-flight classification of %s (up to %ds)", base, int(lockWait.Seconds()))
			announced = true
		}
		time.Sleep(250 * time.Millisecond)
	}
	return classifyLocked(base, key, cache, required, auto)
}

func classifyLocked(base, key, cache, required string, auto bool) ([]Row, error) {
	// Load the cache FIRST so an inventory failure can fall back to it.
	cached := cachedRows(base, cache)
	known := map[string]Row{}
	for _, r := range cached {
		known[r.Model] = r
	}
	// The launch we waited on may have just cached the selected model.
	if required != "" {
		if rows := cachedSelection(base, cache, cached, required); rows != nil {
			return rows, nil
		}
	}

	knownNames := func() []string {
		names := make([]string, 0, len(known))
		for m := range known {
			names = append(names, m)
		}
		sort.Strings(names)
		return names
	}
	// A selected model absent from the cache is classified ALONE: a full
	// inventory sweep buys nothing this launch needs. Every cached row is kept.
	var models []string
	if _, have := known[required]; required != "" && !have {
		logf("classifying only the requested model %s on %s (%d cached row(s) kept, no inventory scan)",
			shim.PyRepr(required), base, len(known))
		models = append(knownNames(), required)
	} else {
		// Inventory only discovers models NEW since the last launch; if it fails,
		// fall back to the cache rather than dying.
		var err error
		models, err = inventoryFn(base, key, nil)
		if err != nil {
			if len(known) == 0 {
				return nil, err // no cache to fall back on
			}
			logf("inventory failed (%v); falling back to %d cached model(s) for %s", err, len(known), base)
			models = knownNames()
		}
	}

	var fresh []string
	for _, m := range models {
		if _, ok := known[m]; !ok {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) > 0 {
		logf("probing %d new model(s) of %d on %s", len(fresh), len(models), base)
		if auto {
			logf("automatic compatibility checks prefer endpoint metadata; only the selected model may need bounded inference")
		} else {
			logf("Compatibility checks send inference requests and may incur charges; usage depends on the model and gateway.")
		}
		logf("Cache: %s", cache)

		one := func(model string) Row {
			if auto {
				dialect, note, status := dialectAutoFn(base, key, model)
				if dialect == "unusable" && model == required {
					logf("Checking compatibility for %s: up to four small inference requests (8/16 output tokens each); charges may apply.", shim.PyRepr(model))
					dialect, note, status = dialectOfFn(base, key, model, true)
				}
				return Row{Model: model, Dialect: dialect, Note: note, Status: status}
			}
			dialect, note, status := dialectOfFn(base, key, model, false)
			r := Row{Model: model, Dialect: dialect, Note: note, Status: status, Cap: outputCapFn(base, key, model, dialect)}
			if dialect == "anthropic" {
				r.Nothink = reasonsFn(base, key, model)
				r.IDRecycle = recyclesFn(base, key, model)
			}
			return r
		}
		if required != "" {
			row := one(required)
			known[row.Model] = row
		} else {
			results := make([]Row, len(fresh))
			sem := make(chan struct{}, jobs)
			var wg sync.WaitGroup
			for i, m := range fresh {
				wg.Add(1)
				sem <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					results[i] = one(m)
				}()
			}
			wg.Wait()
			for _, r := range results {
				known[r.Model] = r
			}
		}
	} else {
		logf("%d model(s) known for %s", len(models), base)
	}

	rows := make([]Row, 0, len(models))
	for _, m := range models {
		rows = append(rows, known[m])
	}
	sortRows(rows)

	// An indeterminate verdict (a 5xx or a transport error) says nothing about
	// the model, so it is not cached -- but it must not suppress the verdicts of
	// the models that answered cleanly either: omitting a shaky model re-probes
	// exactly it next launch.
	shaky := map[string]bool{}
	var shakyNames []string
	for _, r := range rows {
		if r.Dialect == "unusable" && (indeterminate(r.Status) || (auto && strings.HasPrefix(r.Note, noInference))) {
			shaky[r.Model] = true
			shakyNames = append(shakyNames, r.Model)
		}
	}
	if len(shakyNames) > 0 {
		sort.Strings(shakyNames)
		logf("not caching indeterminate verdict(s) for %s; caching %d settled",
			strings.Join(shakyNames, ", "), len(rows)-len(shaky))
	}
	// Write the union, not this key's slice: the cache is shared by every profile
	// on the endpoint, and a key with a narrower entitlement must not truncate a
	// wider key's rows. `known` IS that union and the flock has been held
	// throughout. The delete is not dead: it evicts a row an older version wrote
	// as a settled `unusable` 403.
	var keep []Row
	for m, r := range known {
		if !shaky[m] {
			keep = append(keep, r)
		}
	}
	if len(keep) > 0 {
		sortRows(keep)
		if err := writeCache(cache, base, keep); err != nil {
			logf("cache not written (%s)", pyOSError(err))
		}
	}

	// A requested model that just probed unusable is a named failure, not a
	// silent fall-through to whatever else is cached.
	if required != "" {
		for _, r := range rows {
			if r.Model == required && r.Dialect == "unusable" {
				note := r.Note
				if note == "" {
					note = "no usable dialect answered"
				}
				die("model %s is unusable on %s (%s)", shim.PyRepr(required), base, note)
			}
		}
	}
	return rows, nil
}

// writeCache is atomic: a truncate-in-place write that dies midway leaves JSON
// the next load cannot parse, silently discarding every classification.
func writeCache(cache, base string, rows []Row) error {
	if err := os.MkdirAll(filepath.Dir(cache), 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(cache), ".cls.")
	if err != nil {
		return err
	}
	name := tmp.Name()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Endpoint      string `json:"endpoint"`
		AnthropicPath string `json:"anthropic_path"`
		ThinkingProbe int    `json:"thinking_probe"`
		Rows          []Row  `json:"rows"`
	}{base, "/v1/messages", thinkingProbeVersion, rows})
	_, err = tmp.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o600)
	}
	if err == nil {
		err = os.Rename(name, cache)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}

// pyOSError spells an os error the way Python's OSError str does for the cases
// users read: "[Errno N] strerror".
func pyOSError(err error) string {
	var errno syscall.Errno
	for e := error(err); e != nil; {
		if n, ok := e.(syscall.Errno); ok {
			errno = n
			break
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			break
		}
		e = u.Unwrap()
	}
	if errno == 0 {
		return err.Error()
	}
	return fmt.Sprintf("[Errno %d] %s", int(errno), errno.Error())
}
