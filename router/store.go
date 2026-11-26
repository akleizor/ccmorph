package router

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// ProfileConfig is the user's say over one profile; every field is optional.
type ProfileConfig struct {
	Class    string `json:"class,omitempty"` // free, subscription, budget, api; read from the profile when unset
	Reserved bool   `json:"reserved,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
	// DisabledModels are never routed on this profile.
	DisabledModels []string `json:"disabled_models,omitempty"`
}

// Config is router.json beside the profiles. Missing keys take Defaults.
type Config struct {
	Enabled  bool                     `json:"enabled"`
	Decider  *Decider                 `json:"decider,omitempty"`
	Timeout  float64                  `json:"timeout,omitempty"`
	Kinds    map[string][]string      `json:"kinds,omitempty"`
	TierCap  map[string]string        `json:"tier_cap,omitempty"`
	Profiles map[string]ProfileConfig `json:"profiles,omitempty"`
	// AdvisorOnly are name fragments of models never routed, any version.
	AdvisorOnly    []string `json:"advisor_only,omitempty"`
	DisabledModels []string `json:"disabled_models,omitempty"`
	// Order is tried first among qualifying models; Only limits a tier to its models.
	Order  []string            `json:"order,omitempty"`
	Only   map[string][]string `json:"only,omitempty"`
	Labels map[string]Label    `json:"labels,omitempty"`
	// TimeValue is US dollars per minute of waiting for the shadow picker; 0 keeps TimeValue.
	TimeValue float64 `json:"time_value,omitempty"`
}

var defaultKinds = map[string][]string{
	"writing":    {"gemini", "claude", "gpt"},
	"writing_zh": {"deepseek", "gemini", "claude", "gpt"},
	"mechanical": {"qwen", "deepseek"},
	"code":       {"claude", "gpt"},
	"design":     {"claude", "gpt"},
	"search":     {"grok", "gemini-flash", "qwen"},
	"vision":     {"gemini", "gemini-flash", "claude", "gpt"},
	"x":          {"grok"},
	"image_gen":  {"gpt"},
}

func ConfigPath() string { return filepath.Join(paths.ProfileRoot(), "router.json") }

func routerDir() string { return filepath.Join(paths.StateDir(), "router") }

// LoadConfig reads router.json; a missing file is the default, which is off.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath())
	if err != nil && !os.IsNotExist(err) {
		return c, err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &c); err != nil {
			return c, err
		}
	}
	// A kind router.json names replaces that kind's default; a kind it omits
	// keeps it, except image_gen: its families are the only ones allowed to
	// generate (and bill) images, so a config that names kinds without it
	// keeps image generation off as before.
	_, named := c.Kinds["image_gen"]
	kinds := maps.Clone(defaultKinds)
	if c.Kinds != nil && !named {
		delete(kinds, "image_gen")
	}
	maps.Copy(kinds, c.Kinds)
	c.Kinds = kinds
	if c.TierCap == nil {
		// Size benchmarks rank coding and reasoning, not prose.
		c.TierCap = map[string]string{"writing": "mid"}
	}
	if c.Timeout <= 0 || math.IsNaN(c.Timeout) || math.IsInf(c.Timeout, 0) {
		c.Timeout = 3600
	}
	return c, nil
}

// Enabled reports whether the router is on, without failing a launch on a bad file.
func Enabled() bool {
	c, err := LoadConfig()
	return err == nil && c.Enabled
}

// SetEnabled flips only the enabled key, keeping every other key the user wrote.
func SetEnabled(on bool) error {
	raw := map[string]any{}
	if b, err := os.ReadFile(ConfigPath()); err == nil && len(b) > 0 {
		if err := json.Unmarshal(b, &raw); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	raw["enabled"] = on
	b, _ := json.MarshalIndent(raw, "", "  ")
	return paths.AtomicWrite(paths.WriteIn{Path: ConfigPath(), Temp: ".router-*", Data: append(b, '\n'), Mode: 0o600, Mkdir: 0o700})
}

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".router-*", Data: append(b, '\n'), Mode: 0o600, Mkdir: 0o700})
}

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// --- availability: a refused account or a broken model waits out its backoff ---

const modelBackoff = 15 * 60

func unavailablePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(routerDir(), "unavailable", hex.EncodeToString(sum[:])+".json")
}

// markUnavailable keeps the later reset: two sessions may block one profile at
// once, and a shorter block written last must not reopen it early.
func markUnavailable(key string, until float64) {
	withLock(func() {
		_ = writeJSON(unavailablePath(key), map[string]float64{"until": max(until, unavailableUntil(key))})
	})
}

func unavailableUntil(key string) float64 {
	var v struct{ Until float64 }
	if readJSON(unavailablePath(key), &v) && !math.IsNaN(v.Until) && !math.IsInf(v.Until, 0) {
		return v.Until
	}
	return 0
}

// --- ledger: one row per route; outcomes joined later by id, tool_use_id or agent_id ---

type Row map[string]any

func ledgerPath() string { return filepath.Join(routerDir(), "ledger.jsonl") }

const (
	// lockPoll is how often a writer retries ledger.lock.
	lockPoll = 10 * time.Millisecond
	// lockWait bounds the wait when no hook deadline owns the process.
	lockWait = 2 * time.Second
)

// ledgerDeadline is the process's budget for taking ledger.lock, as UnixNano;
// zero means lockWait from each call. armLock sets it once at the routing entry:
// a hook hands its own deadline, so every later write shares the time the hook
// has left; a dispatch hands none, so each write gets the bounded default.
var ledgerDeadline atomic.Int64

func armLock(deadline time.Time) {
	var d int64
	if !deadline.IsZero() {
		d = deadline.UnixNano()
	}
	ledgerDeadline.Store(d)
}

// withLock serializes ledger changes across the hooks and delegates of every
// session. The lock is taken without blocking and polled until this process's
// budget runs out; a write it cannot protect is refused rather than parked past
// the hook's deadline. A lock file that cannot be opened is the same refusal,
// never an unlocked write.
func withLock(fn func()) bool {
	if err := os.MkdirAll(routerDir(), 0o700); err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(routerDir(), "ledger.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	deadline := time.Now().Add(lockWait)
	if d := ledgerDeadline.Load(); d != 0 {
		deadline = time.Unix(0, d)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return false
		}
		// The budget is checked after every attempt, taken or not: a spent
		// budget refuses even a free lock, and the deferred Close drops it.
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		if err == nil {
			fn()
			return true
		}
		time.Sleep(min(lockPoll, left))
	}
}

func appendLine(path string, row Row) {
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(row)
	_, _ = f.Write(append(b, '\n'))
}

func ledgerAppend(row Row) { withLock(func() { appendLine(ledgerPath(), row) }) }

func outcomeAppend(row Row) { appendLine(filepath.Join(routerDir(), "outcomes.jsonl"), row) }

func readRows(path string) []Row {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []Row
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r Row
		if json.Unmarshal(sc.Bytes(), &r) == nil && r != nil {
			rows = append(rows, r)
		}
	}
	return rows
}

func (r Row) matches(id string) bool {
	return id != "" && (r["id"] == id || r["tool_use_id"] == id || r["agent_id"] == id)
}

// ledgerUpdate merges fields into the one row the id names, keeping every other line.
func ledgerUpdate(id string, fields Row) bool {
	found, _ := ledgerRewrite(func(r Row) bool { return r.matches(id) }, false, fields)
	return found
}

var errLedgerLocked = errors.New("the ledger stayed locked")

// ledgerRewrite merges fields into the first row match accepts, or the last
// when newest, keeping every other line. found is whether a row matched; err
// is a lock or write that failed, so nothing was recorded.
func ledgerRewrite(match func(Row) bool, newest bool, fields Row) (found bool, err error) {
	locked := withLock(func() {
		var b []byte
		if b, err = os.ReadFile(ledgerPath()); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
			return
		}
		lines := bytes.SplitAfter(b, []byte("\n"))
		at := -1
		for i := range lines {
			var r Row
			if json.Unmarshal(lines[i], &r) != nil || !match(r) {
				continue
			}
			at = i
			if !newest {
				break
			}
		}
		if at < 0 {
			return
		}
		var r Row
		_ = json.Unmarshal(lines[at], &r)
		for k, v := range fields {
			r[k] = v
		}
		out, _ := json.Marshal(r)
		lines[at] = append(out, '\n')
		found = true
		err = paths.AtomicWrite(paths.WriteIn{Path: ledgerPath(), Temp: ".ledger-*", Data: bytes.Join(lines, nil), Mode: 0o600, Mkdir: 0o700})
	})
	if !locked && err == nil {
		err = errLedgerLocked
	}
	return found, err
}
