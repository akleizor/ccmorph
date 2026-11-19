package router

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
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
}

var defaultKinds = map[string][]string{
	"writing":   {"gemini", "claude", "gpt"},
	"code":      {"claude", "gpt"},
	"design":    {"claude", "gpt"},
	"search":    {"grok", "gemini-flash", "qwen"},
	"vision":    {"gemini", "gemini-flash", "claude", "gpt"},
	"x":         {"grok"},
	"image_gen": {"gpt"},
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
	if c.Kinds == nil {
		c.Kinds = defaultKinds
	}
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

// withLock serializes ledger changes across the hooks and delegates of every session.
func withLock(fn func()) {
	_ = os.MkdirAll(routerDir(), 0o700)
	f, err := os.OpenFile(filepath.Join(routerDir(), "ledger.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err == nil {
		defer f.Close()
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	fn()
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
	found := false
	withLock(func() {
		b, err := os.ReadFile(ledgerPath())
		if err != nil {
			return
		}
		lines := bytes.SplitAfter(b, []byte("\n"))
		for i, line := range lines {
			var r Row
			if json.Unmarshal(line, &r) != nil || !r.matches(id) {
				continue
			}
			for k, v := range fields {
				r[k] = v
			}
			out, _ := json.Marshal(r)
			lines[i] = append(out, '\n')
			found = true
			break
		}
		if found {
			_ = paths.AtomicWrite(paths.WriteIn{Path: ledgerPath(), Temp: ".ledger-*", Data: bytes.Join(lines, nil), Mode: 0o600, Mkdir: 0o700})
		}
	})
	return found
}
