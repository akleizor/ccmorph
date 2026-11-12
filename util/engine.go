package util

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/settingscompose"
)

// jsWS is the ECMAScript WhiteSpace + LineTerminator set, one rune each.
var jsWS = string([]rune{9, 11, 12, 32, 0xa0, 0xfeff, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 10, 13})

func trimJS(s string) string { return strings.Trim(s, jsWS) }

var (
	expForm = regexp.MustCompile(`^[+-]?([0-9]+(\.[0-9]*)?|\.[0-9]+)[eE][+-]?[0-9]+$`)
	intHead = regexp.MustCompile(`^[+-]?[0-9]+`)
	pctHead = regexp.MustCompile("^[" + jsWS + "]*([+-]?(?:[0-9]+\\.?[0-9]*|\\.[0-9]+)(?:[eE][+-]?[0-9]+)?)")
)

const intCap = 1_000_000_000_000_000_000 // 10**18, what zsh arithmetic can hold

// capDigits is min(int(digits), 10**18) for a run of ASCII digits.
func capDigits(d string) int64 {
	d = strings.TrimLeft(d, "0")
	if len(d) > 18 {
		return intCap
	}
	if d == "" {
		return 0
	}
	n, _ := strconv.ParseInt(d, 10, 64)
	if n > intCap {
		return intCap
	}
	return n
}

// groupedInt reads 100_000 / 100,000 / "100 000": 1-3 digits, then groups of three
// that all use the one separator.
func groupedInt(v string) (string, bool) {
	r := []rune(v)
	i := 0
	if i < len(r) && (r[i] == '+' || r[i] == '-') {
		i++
	}
	isD := func(c rune) bool { return c >= '0' && c <= '9' }
	start := i
	for i < len(r) && isD(r[i]) {
		i++
	}
	if n := i - start; n < 1 || n > 3 || i >= len(r) || !strings.ContainsRune("_,\u00a0\u202f ", r[i]) {
		return "", false
	}
	sep, digits := r[i], string(r[start:i])
	if (len(r)-i)%4 != 0 {
		return "", false
	}
	for ; i < len(r); i += 4 {
		if r[i] != sep || !isD(r[i+1]) || !isD(r[i+2]) || !isD(r[i+3]) {
			return "", false
		}
		digits += string(r[i+1 : i+4])
	}
	return digits, true
}

// EngineInt is the integer the engine reads out of an env var (Jd/yN), or ok=false
// when it rejects the value. Positive results are capped at 10**18.
func EngineInt(raw string) (int64, bool) {
	v := trimJS(raw)
	if len([]rune(v)) <= 32 {
		if expForm.MatchString(v) {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
				return 0, false
			}
			if n <= 0 {
				return 0, false
			}
			if n >= 1e18 {
				return intCap, true
			}
			return int64(n), true
		}
		if d, ok := groupedInt(v); ok {
			if strings.HasPrefix(v, "-") {
				return 0, false
			}
			n := capDigits(d)
			return n, n > 0
		}
	}
	m := intHead.FindString(v)
	if m == "" {
		return 0, false
	}
	if m[0] == '-' {
		return 0, false
	}
	n := capDigits(strings.TrimPrefix(m, "+"))
	return n, n > 0
}

// EngineBool is the engine's env boolean: "1", "true", "yes" or "on" after an
// ECMAScript trim and lowercasing. Anything else (off, no, 0, 2) is false.
func EngineBool(raw string) bool {
	switch strings.ToLower(trimJS(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// enginePct is the percentage the engine honours (parseFloat prefix, in (0, 100]).
func enginePct(raw string) (float64, bool) {
	m := pctHead.FindStringSubmatch(raw)
	if m == nil {
		return 0, false
	}
	p, err := strconv.ParseFloat(m[1], 64)
	if err != nil || math.IsNaN(p) || !(p > 0 && p <= 100) {
		return 0, false
	}
	return p, true
}

var modelOut = []struct {
	key        string
	def, upper int64
}{
	{"claude-3-opus", 4096, 4096}, {"claude-3-haiku", 4096, 4096}, {"claude-3-sonnet", 8192, 8192},
	{"claude-3-5-haiku", 8192, 8192}, {"claude-3-5-sonnet", 8192, 8192},
}

// engineReserve is RL(): min(effective max-output-tokens, 20000), resolved per model.
func engineReserve(modelID string, pinned int64) int64 {
	bare := strings.ToLower(strings.SplitN(modelID, "[", 2)[0])
	def, upper := int64(20_000), int64(20_000)
	for _, e := range modelOut {
		if strings.Contains(bare, e.key) {
			def, upper = e.def, e.upper
			break
		}
	}
	eff := def
	if pinned != 0 {
		eff = min(pinned, upper)
	}
	return min(eff, 20_000)
}

// engineTrigger is xWe(): floor(usable*(pct/100)), capped 13,000 below usable.
func engineTrigger(usable int64, pct *float64) int64 {
	n := usable - 13_000
	if pct == nil {
		return n
	}
	return min(int64(float64(usable)*(*pct/100)), n)
}

func pctPtr(raw string) *float64 {
	if p, ok := enginePct(raw); ok {
		return &p
	}
	return nil
}

// digitsInt is int(v) if v.isdigit() else 0.
func digitsInt(v string) int64 {
	if v == "" {
		return 0
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return intCap
	}
	return n
}

// walls is the profile's probed windows, read from the conf at path.
func walls(profile, conf string) map[string]int64 {
	old, had := os.LookupEnv("MODEL_WINDOW_PROFILE_CONF")
	os.Setenv("MODEL_WINDOW_PROFILE_CONF", conf)
	defer func() {
		if had {
			os.Setenv("MODEL_WINDOW_PROFILE_CONF", old)
		} else {
			os.Unsetenv("MODEL_WINDOW_PROFILE_CONF")
		}
	}()
	w, _ := ctxwindow.ProfileWindows(profile)
	return w
}

var oneMTag = regexp.MustCompile(`(?i)\[1m\]`)

// bareID is the id the window registry knows an offered model by: the disguise
// undone, the [1m] marker stripped. The registry holds real, bare ids (the
// listing writes them undisguised), so every wall lookup — and every consumer of
// the names those lookups produce — derives it this one way.
func bareID(modelID string) string {
	return strings.SplitN(ctxwindow.RealID(modelID), "[", 2)[0]
}

// modelSizing is the readings one offered model is sized from: the profile's
// probed walls, the session budget and window, the percentage and output pin in
// force, and the 1M kill switch.
type modelSizing struct {
	profile string
	walls   map[string]int64
	budget  int64
	window  int64
	pct     *float64
	pinned  int64
	no1m    bool
}

// engineModel is the engine's per-model answer for one offered id — the one walk
// the unsafe warning and the hazard set both make. zE() resolves a window per
// model on every call, so the family test reads the offered id exactly as the
// engine does: a [1m] tag anywhere in it (dropped by the kill switch) wins first,
// then a claude- prefix — which a disguised id wears, and which takes the
// hardcoded 200,000 and ignores the session window — and everything else takes
// the session window. The session budget caps every ceiling. The wall is the
// profile's probe for the real, bare id, 0 when none is known. considered marks
// the ids the engine sizes by its own family table; the rest it sizes at the
// session window, which the launch's own derivation already bounds — so the
// warning, which has no reading of the window, skips them while the advisory
// sizes them at it.
//
// The boundary both surfaces compare at: a wall AT the trigger is safe. The
// engine compacts at e >= trigger (YRt), so a model walled exactly there is
// compacted in time; only a wall under the trigger dies uncompacted.
func engineModel(sz modelSizing, entry string) (bare string, wall, trigger int64, considered bool) {
	bare = bareID(entry)
	wall = sz.walls[sz.profile+":"+bare]
	if wall == 0 {
		wall = sz.walls[bare]
	}
	var ceil int64
	switch {
	case !sz.no1m && oneMTag.MatchString(entry):
		ceil, considered = 1_000_000, true
	case strings.HasPrefix(entry, "claude-"):
		ceil, considered = 200_000, true
	default:
		ceil = sz.window
	}
	if sz.budget != 0 {
		ceil = min(ceil, sz.budget)
	}
	usable := ceil - min(ceil, engineReserve(entry, sz.pinned))
	return bare, wall, engineTrigger(usable, sz.pct), considered
}

type HazardIn struct {
	Profile, Budget, Ceiling string
	Sources                  []string
	PctRaw, RawOut           string
	No1m                     bool
	Conf                     string
	PctInEnv                 bool
}

// hazardRows is the whole calculation behind the switch advisory: the trigger,
// then one "<wall>\t<name>" row per model that dies before it compacts, largest
// wall first. Nothing to say is nil.
func hazardRows(in HazardIn) ([]string, error) {
	budget, ceiling := digitsInt(in.Budget), digitsInt(in.Ceiling)
	doc := settingscompose.MergedView(in.Sources)
	pctRaw := in.PctRaw
	if !in.PctInEnv {
		if env := asObj(getKey(doc, "env")); env != nil {
			if s, ok := getKey(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE").(string); ok {
				pctRaw = s
			}
		}
	}
	pct := pctPtr(pctRaw)
	var window int64
	switch {
	case budget != 0 && ceiling != 0:
		window = min(budget, ceiling)
	case budget != 0:
		window = budget
	case ceiling != 0:
		window = ceiling
	}
	if window == 0 {
		return nil, nil
	}
	var rawOut int64
	if in.RawOut != "" {
		n, err := strconv.ParseInt(in.RawOut, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid literal for int() with base 10: '%s'", in.RawOut)
		}
		rawOut = n
	}
	reserve := engineReserve("", rawOut)
	usable := window - min(window, reserve)
	trigger := engineTrigger(usable, pct)
	if trigger <= 0 {
		return nil, nil
	}
	offered, _ := getKey(doc, "availableModels").([]any)
	if len(offered) == 0 {
		return nil, nil
	}
	w := walls(in.Profile, in.Conf)
	sz := modelSizing{profile: in.Profile, walls: w, budget: budget, window: window, pct: pct, pinned: rawOut, no1m: in.No1m}
	aliases := map[string]bool{"opus": true, "sonnet": true, "haiku": true, "fable": true}
	// Deduped on (bare, trigger) rather than the ceiling it came from: the wall
	// is a function of the bare id and the verdict of (wall, trigger), so two
	// offered variants that agree on both — a bare/[1m] pair whose floors
	// collide — share one verdict and one row, which is named once below
	// regardless.
	type seenKey struct {
		bare    string
		trigger int64
	}
	seen := map[seenKey]bool{}
	named := map[string]bool{}
	type row struct {
		wall int64
		name string
	}
	var rows []row
	for _, mv := range offered {
		m, ok := mv.(string)
		if !ok {
			continue
		}
		bare, wall, triggerM, _ := engineModel(sz, m)
		if aliases[bare] {
			continue
		}
		if wall == 0 {
			continue
		}
		k := seenKey{bare, triggerM}
		if seen[k] {
			continue
		}
		seen[k] = true
		if wall < triggerM && !named[bare] {
			named[bare] = true
			rows = append(rows, row{wall, bare})
		}
	}
	if len(rows) == 0 {
		return nil, nil
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].wall != rows[j].wall {
			return rows[i].wall > rows[j].wall
		}
		return rows[i].name > rows[j].name
	})
	out := []string{strconv.FormatInt(trigger, 10)}
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%d\t%s", r.wall, r.name))
	}
	return out, nil
}

func getKey(o *ordjson.Obj, k string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(k)
	return v
}

func kTokens(n int64) string { return fmt.Sprintf("%dk", int64(math.RoundToEven(float64(n)/1000))) }

// HazardMessage words hazard rows (trigger first, largest wall first) for the advisory.
func HazardMessage(rows []string, withheld bool) string {
	if len(rows) < 2 {
		return ""
	}
	trigger, err := strconv.ParseInt(strings.TrimSpace(rows[0]), 10, 64)
	if err != nil {
		return ""
	}
	type nw struct {
		wall int64
		name string
	}
	var named []nw
	for _, line := range rows[1:] {
		wall, name, _ := strings.Cut(line, "\t")
		n, err := strconv.ParseInt(strings.TrimSpace(wall), 10, 64)
		if err != nil {
			return ""
		}
		named = append(named, nw{n, name})
	}
	var shown []string
	for i, n := range named {
		if i == 3 {
			break
		}
		shown = append(shown, fmt.Sprintf("%s limit %s", n.name, kTokens(n.wall)))
	}
	more := ""
	if len(named) > 3 {
		more = fmt.Sprintf(" +%d smaller", len(named)-3)
	}
	list := strings.Join(shown, ", ")
	if withheld {
		return fmt.Sprintf("compacts at %s; hidden from /model: %s%s. Start a new session to use them, or set CCMORPH_WITHHOLD_FATAL=0 to show them.", kTokens(trigger), list, more)
	}
	return fmt.Sprintf("compacts at %s; lower model limits: %s%s. Start a new session on a lower-limit model to size compaction for it. Set CCMORPH_WITHHOLD_FATAL=1 to hide these choices.", kTokens(trigger), list, more)
}

// PolicyPct is the fleet context policy's autoCompactAtPercent, or ok=false when the
// file is missing, malformed or outside the schema this version understands.
func PolicyPct(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := ordjson.Parse(b)
	if err != nil {
		return 0, false
	}
	doc := asObj(v)
	if doc == nil {
		return 0, false
	}
	val, has := doc.Get("autoCompactAtPercent")
	if !has {
		return 0, false
	}
	sv, _ := doc.Get("schemaVersion")
	n, isNum := val.(json.Number)
	if !ordjson.Equal(sv, json.Number("1")) || !isNum {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil || !(0 < i && i < 100) {
		return 0, false
	}
	return i, true
}

// EffectivePct is the compaction percentage in force: the profile conf's own
// value, else the one in the merged settings layers; false when none is honoured.
func EffectivePct(conf string, layers []string) (string, bool) {
	if conf != "" {
		if v, ok := enginePct(conf); ok {
			return fmtG(v), true
		}
	}
	env := asObj(getKey(settingscompose.MergedView(layers), "env"))
	raw, isStr := getKey(env, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE").(string)
	if !isStr {
		return "", false
	}
	if v, ok := enginePct(raw); ok {
		return fmtG(v), true
	}
	return "", false
}

// fmtG is Python's format(v, "g").
func fmtG(v float64) string { return strconv.FormatFloat(v, 'g', 6, 64) }

// clampN bounds an auto-compact budget to what the engine honours, [1e5, 1e6].
func clampN(n int64) int64 { return min(max(n, 100000), 1000000) }

// EffectiveACW is the budget the engine uses, in its order. The env value is
// parsed and CLAMPED; a saved autoCompactWindow arrives already schema-checked
// (out of range was dropped); the derived value fills the gap. "" when none applies.
func EffectiveACW(env, saved, derived string) string {
	if n, ok := EngineInt(env); ok && env != "" {
		return strconv.FormatInt(clampN(n), 10)
	} else if saved != "" && strings.Trim(saved, "0123456789") == "" {
		return saved
	}
	return derived
}

// ClampACW clamps a decimal budget, by length first so a twenty-digit value is the
// ceiling rather than an overflow. Anything not decimal comes back unchanged.
func ClampACW(v string) string {
	if v == "" || strings.Trim(v, "0123456789") != "" {
		return v
	}
	if len(strings.TrimLeft(v, "0")) > 7 {
		return "1000000"
	}
	n, _ := strconv.ParseInt(v, 10, 64)
	return strconv.FormatInt(clampN(n), 10)
}
