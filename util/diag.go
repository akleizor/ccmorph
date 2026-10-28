package util

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/modelwindow"
	"github.com/akleizor/ccmorph/pycompat"
	"github.com/akleizor/ccmorph/settingscompose"
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
	var m pycompat.Value
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
	if truthy(allowed) {
		switch t := allowed.(type) {
		case []any:
			entries = t
		case *pycompat.Obj:
			for _, k := range t.Keys() {
				entries = append(entries, k)
			}
		case string:
		default:
			return records
		}
	}
	var lines []string
	if e := pycompat.Catch(func() {
		ceilings := walls(profile, conf)
		pinned := digitsInt(maxOut)
		budget := digitsInt(strings.TrimSpace(budgetRaw))
		pctP := pctPtr(pct)
		for _, ev := range entries {
			entry, ok := ev.(string)
			if !ok {
				continue
			}
			var w int64
			switch {
			case !no1m && oneMTag.MatchString(entry):
				w = 1_000_000
				if budget != 0 {
					w = min(w, budget)
				}
			case strings.HasPrefix(entry, "claude-"):
				w = 200_000
				if budget != 0 {
					w = min(w, budget)
				}
			default:
				continue
			}
			usable := w - min(w, engineReserve(entry, pinned))
			trigger := engineTrigger(usable, pctP)
			bare := strings.SplitN(modelwindow.RealID(entry), "[", 2)[0]
			ceiling := ceilings[profile+":"+bare]
			if ceiling == 0 {
				ceiling = ceilings[bare]
			}
			if ceiling != 0 && trigger >= ceiling {
				lines = append(lines, fmt.Sprintf("%s holds %d but compaction starts at %d", bare, ceiling, trigger))
			}
		}
	}); e != nil {
		return records
	}
	if len(lines) > 0 {
		records = append(records, record{"unsafe", strings.TrimRight(strings.Join(lines, "\n"), "\n")})
	}
	return records
}

// launchDiagnostics answers every read-only question a launch asks of the merged
// settings layers and the profile's own files: `<key>... -- <layer>...`, with the
// launch's readings in _CCM_* env. Records are NUL-framed `<kind>\t<text>`.
func launchDiagnostics(args []string, _ io.Reader, out, _ io.Writer) int {
	split := len(args)
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	keys := args[:split]
	var paths []string
	if split < len(args) {
		paths = args[split+1:]
	}
	env := os.Getenv
	emit := func(fields ...string) {
		for _, f := range fields {
			if strings.ContainsRune(f, 0) {
				return
			}
		}
		io.WriteString(out, strings.Join(fields, "\t")+"\x00")
	}

	merged := asObj(getKey(settingscompose.MergedView(paths), "env"))
	if merged == nil {
		merged = pycompat.NewObj()
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

	pct := env("_CCM_ENV_PCT")
	runInvariant := env("_CCM_SETTINGS") != ""
	if env("_CCM_PCT_IN_ENV") != "1" {
		if pctSet {
			pct = pctRaw
		} else if p := env("_CCM_POLICY_PCT"); p != "" {
			pct = p
		} else if v, ok := policyPct(env("_CCM_POLICY")); !ok {
			runInvariant = false
		} else {
			emit("policy", fmt.Sprint(v))
			pct = fmt.Sprint(v)
		}
	}

	for _, k := range keys {
		if s, ok := getKey(merged, k).(string); ok && s != "" {
			emit("chosen", k)
		}
	}

	if hk := env("_CCM_HDR_KEY"); hk != "" {
		if s, ok := getKey(merged, hk).(string); ok {
			if s = strings.TrimRight(s, "\n"); s != "" {
				emit("userset", s)
			}
		}
	}

	if runInvariant {
		for _, r := range invariantRecords(env("_CCM_SETTINGS"), env("_CCM_PROFILE_CONF"), env("_CCM_PROFILE"),
			strings.TrimSpace(env("_CCM_MAXOUT")), env("_CCM_BUDGET"), env("_CCM_NO1M") != "", pct) {
			emit(r.kind, r.text)
		}
	}

	if env("_CCM_HAZARD") == "1" {
		pycompat.Catch(func() {
			conf := env("_CCM_PROFILE_CONF")
			var srcs []string
			anyFile := false
			for _, p := range paths {
				if pycompat.Strip(p) != "" {
					srcs = append(srcs, p)
					anyFile = anyFile || isFile(p)
				}
			}
			if !anyFile || !isFile(conf) {
				return
			}
			rows := hazardRows(hazardIn{
				profile: env("_CCM_PROFILE"), budget: env("_CCM_BUDGET"), ceiling: env("_CCM_CEILING"),
				sources: srcs, pctRaw: pct, rawOut: strings.TrimSpace(env("_CCM_MAXOUT")),
				no1m: env("_CCM_NO1M") != "", conf: conf,
				pctInEnv: env("_CCM_PCT_IN_ENV") == "1" || (!pctSet && pct != ""),
			})
			emit("hazard", hazardMessage(rows, env("_CCM_WITHHELD") == "1"))
		})
	}
	return 0
}

// hazardSetVerb prints the hazard rows from the _CCM_* environment.
func hazardSetVerb(_ []string, _ io.Reader, out, errw io.Writer) int {
	env := os.Getenv
	var srcs []string
	for _, p := range strings.Split(env("_CCM_SOURCES"), "\n") {
		if pycompat.Strip(p) != "" {
			srcs = append(srcs, p)
		}
	}
	var rows []string
	if e := pycompat.Catch(func() {
		rows = hazardRows(hazardIn{
			profile: env("_CCM_PROFILE"), budget: env("_CCM_BUDGET"), ceiling: env("_CCM_CEILING"),
			sources: srcs, pctRaw: env("_CCM_PCT"), rawOut: strings.TrimSpace(env("_CCM_MAXOUT")),
			no1m: env("_CCM_NO1M") != "", conf: env("_CCM_PROFILE_CONF"), pctInEnv: env("_CCM_PCT_IN_ENV") != "",
		})
	}); e != nil {
		fmt.Fprintln(errw, e.Msg)
		return 1
	}
	if len(rows) > 0 {
		fmt.Fprintln(out, strings.Join(rows, "\n"))
	}
	return 0
}

// hazardMsgVerb words the rows in _CCM_ROWS.
func hazardMsgVerb(_ []string, _ io.Reader, out, errw io.Writer) int {
	var rows []string
	for _, l := range strings.Split(os.Getenv("_CCM_ROWS"), "\n") {
		if pycompat.Strip(l) != "" {
			rows = append(rows, l)
		}
	}
	var msg string
	if e := pycompat.Catch(func() { msg = hazardMessage(rows, os.Getenv("_CCM_WITHHELD") == "1") }); e != nil {
		fmt.Fprintln(errw, e.Msg)
		return 1
	}
	if msg != "" {
		fmt.Fprintln(out, msg)
	}
	return 0
}

// withholdFatal removes the fatal models in _CCM_ROWS from the generated settings.json
// at _CCM_SETTINGS, never the one the session is launched on.
func withholdFatal(_ []string, _ io.Reader, _, _ io.Writer) int {
	path := os.Getenv("_CCM_SETTINGS")
	v, err := loadJSON(path)
	if err != nil {
		return 0
	}
	doc := asObj(v)
	if doc == nil {
		return 0
	}
	offered, ok := getKey(doc, "availableModels").([]any)
	if !ok || len(offered) == 0 {
		return 0
	}
	fatal := map[string]bool{}
	for _, line := range strings.Split(os.Getenv("_CCM_ROWS"), "\n") {
		if pycompat.Strip(line) == "" {
			continue
		}
		_, name, _ := strings.Cut(line, "\t")
		fatal[name] = true
	}
	if len(fatal) == 0 {
		return 0
	}
	launched := strings.SplitN(os.Getenv("_CCM_LAUNCHED"), "[", 2)[0]
	kept := []any{}
	for _, m := range offered {
		s, isStr := m.(string)
		if !isStr {
			kept = append(kept, m)
			continue
		}
		base := strings.SplitN(s, "[", 2)[0]
		if base == launched || !fatal[base] {
			kept = append(kept, m)
		}
	}
	if len(kept) == len(offered) {
		return 0
	}
	doc.Set("availableModels", kept)
	data := pycompat.Marshal(doc, pycompat.Format{Indent: 2, ASCII: true}) + "\n"
	atomicWrite(path, ".ovl.", []byte(data), 0o600, false)
	return 0
}
