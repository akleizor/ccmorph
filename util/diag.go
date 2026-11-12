package util

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/util/paths"
)

var disguised = regexp.MustCompile(`^claude-.*-dd(?:-|\n?\z)`)

type record struct{ kind, text string }

// reverse reverses a string by character.
func reverse(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// invariantRecords is the two warning blocks, each keeping its own failure rules.
func invariantRecords(settings, conf, profile, maxOut, budgetRaw string, no1m bool, pct string) (records []record) {
	var m any
	if v, err := loadJSON(settings); err == nil {
		if o := asObj(v); o != nil {
			m = getKey(o, "model")
		}
	}
	if s, ok := m.(string); ok && disguised.MatchString(s) {
		dm := s
		if _, tail, found := strings.Cut(s, "-dd-"); found && tail != "" {
			dm = reverse(tail)
		}
		records = append(records, record{"disguised", strings.TrimRight(dm, "\n")})
	}

	v, err := loadJSON(settings)
	if err != nil {
		return records
	}
	root := asObj(v)
	if root == nil {
		return records
	}
	allowed := getKey(root, "availableModels")
	var entries []any
	if ordjson.Truthy(allowed) {
		switch t := allowed.(type) {
		case []any:
			entries = t
		case *ordjson.Obj:
			for _, k := range t.Keys() {
				entries = append(entries, k)
			}
		case string:
		default:
			return records
		}
	}
	var lines []string
	{
		sz := modelSizing{profile: profile, walls: walls(profile, conf),
			budget: digitsInt(strings.TrimSpace(budgetRaw)), pct: pctPtr(pct),
			pinned: digitsInt(maxOut), no1m: no1m}
		for _, ev := range entries {
			entry, ok := ev.(string)
			if !ok {
				continue
			}
			bare, ceiling, trigger, considered := engineModel(sz, entry)
			// Only the family-sized ids: the rest take the session window, which
			// the launch's own derivation bounds and this warning has no reading
			// of; the hazard set, which knows the window, sizes them at it.
			if !considered || ceiling == 0 {
				continue
			}
			if ceiling < trigger {
				lines = append(lines, fmt.Sprintf("%s holds %d but compaction starts at %d", bare, ceiling, trigger))
			}
		}
	}
	if len(lines) > 0 {
		records = append(records, record{"unsafe", strings.TrimRight(strings.Join(lines, "\n"), "\n")})
	}
	return records
}

// Record is one answer of LaunchDiagnostics: a kind and its text.
type Record struct{ Kind, Text string }

// DiagIn is the launch's readings; Keys are the env keys to report as chosen.
type DiagIn struct {
	Keys, Paths                                       []string
	Settings, ProfileConf, Profile                    string
	EnvPct, MaxOut, Budget, Policy, PolicyPct, HdrKey string
	Ceiling                                           string
	PctInEnv, No1m, Hazard, Withheld                  bool
}

// LaunchDiagnostics answers every read-only question a launch asks of the merged
// settings layers and the profile's own files.
func LaunchDiagnostics(in DiagIn) (recs []Record) {
	layerPaths := in.Paths
	emit := func(fields ...string) {
		for _, f := range fields {
			if strings.ContainsRune(f, 0) {
				return
			}
		}
		r := Record{Kind: fields[0]}
		if len(fields) > 1 {
			r.Text = fields[1]
		}
		recs = append(recs, r)
	}

	// A layer the merge cannot apply answers none of the env questions; the warnings
	// below read settings.json themselves and still run.
	merged := asObj(getKey(settingscompose.MergedView(layerPaths), "env"))
	if merged == nil {
		merged = ordjson.New()
	}
	const pctKey = "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"
	pctRaw, isStr := getKey(merged, pctKey).(string)
	pctSet := false
	if isStr {
		_, pctSet = enginePct(pctRaw)
	}
	if pctSet {
		emit("pct-set")
	}

	pct := in.EnvPct
	runInvariant := in.Settings != ""
	if !in.PctInEnv {
		if pctSet {
			pct = pctRaw
		} else if p := in.PolicyPct; p != "" {
			pct = p
		} else if v, ok := PolicyPct(in.Policy); !ok {
			runInvariant = false
		} else {
			emit("policy", fmt.Sprint(v))
			pct = fmt.Sprint(v)
		}
	}

	for _, k := range in.Keys {
		if s, ok := getKey(merged, k).(string); ok && s != "" {
			emit("chosen", k)
		}
	}

	if hk := in.HdrKey; hk != "" {
		if s, ok := getKey(merged, hk).(string); ok {
			if s = strings.TrimRight(s, "\n"); s != "" {
				emit("userset", s)
			}
		}
	}

	if runInvariant {
		for _, r := range invariantRecords(in.Settings, in.ProfileConf, in.Profile,
			strings.TrimSpace(in.MaxOut), in.Budget, in.No1m, pct) {
			emit(r.kind, r.text)
		}
	}

	if in.Hazard {
		func() {
			conf := in.ProfileConf
			var srcs []string
			anyFile := false
			for _, p := range layerPaths {
				if strings.TrimSpace(p) != "" {
					srcs = append(srcs, p)
					anyFile = anyFile || paths.IsFile(p)
				}
			}
			if !anyFile || !paths.IsFile(conf) {
				return
			}
			rows, err := hazardRows(HazardIn{
				Profile: in.Profile, Budget: in.Budget, Ceiling: in.Ceiling,
				Sources: srcs, PctRaw: pct, RawOut: strings.TrimSpace(in.MaxOut),
				No1m: in.No1m, Conf: conf,
				PctInEnv: in.PctInEnv || (!pctSet && pct != ""),
			})
			if err == nil {
				emit("hazard", HazardMessage(rows, in.Withheld))
			}
		}()
	}
	return recs
}

// HazardSet is the switch advisory's rows, or the error that ended the calculation.
func HazardSet(in HazardIn) ([]string, error) {
	return hazardRows(in)
}

// WithholdFatal removes the fatal models in rows from the generated settings.json
// at path, never the one the session is launched on.
func WithholdFatal(path string, rows []string, launched string) {
	v, err := loadJSON(path)
	if err != nil {
		return
	}
	doc := asObj(v)
	if doc == nil {
		return
	}
	offered, ok := getKey(doc, "availableModels").([]any)
	if !ok || len(offered) == 0 {
		return
	}
	fatal := map[string]bool{}
	for _, line := range rows {
		if strings.TrimSpace(line) == "" {
			continue
		}
		_, name, _ := strings.Cut(line, "\t")
		fatal[name] = true
	}
	if len(fatal) == 0 {
		return
	}
	// The rows name a model by its real, bare id, so the withhold derives the same
	// base: a disguised offered entry is removed by the real id its row carries,
	// and the launched model is kept however it is spelled.
	launched = bareID(launched)
	kept := []any{}
	for _, m := range offered {
		s, isStr := m.(string)
		if !isStr {
			kept = append(kept, m)
			continue
		}
		base := bareID(s)
		if base == launched || !fatal[base] {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(offered) {
		return
	}
	doc.Set("availableModels", kept)
	data := string(ordjson.Marshal(doc, true)) + "\n"
	AtomicWrite(path, ".ovl.", []byte(data), 0o600, false)
}
