package util

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/pycompat"
)

// iterItems is what Python's `for x in value` yields for a decoded JSON value.
func iterItems(v pycompat.Value) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case string:
		var out []any
		for _, c := range t {
			out = append(out, string(c))
		}
		return out, true
	case *pycompat.Obj:
		var out []any
		for _, k := range t.Keys() {
			out = append(out, k)
		}
		return out, true
	}
	return nil, false
}

var cloaked = regexp.MustCompile(`^claude-.*-dd(?:-|\n?\z)`)

var families = []string{"opus", "sonnet", "haiku", "fable"}

func inList(list []any, s string) bool {
	for _, e := range list {
		if x, ok := e.(string); ok && x == s {
			return true
		}
	}
	return false
}

// rewriteJSON is the picker's single read, mutate, atomic replace of one settings file.
// Any failure leaves the file as it was.
func rewriteJSON(path string, mutate func(doc *pycompat.Obj) bool) {
	pycompat.Catch(func() {
		doc := pycompat.NewObj()
		b, err := os.ReadFile(path)
		switch {
		case err == nil:
			v, perr := pycompat.Parse(b)
			if perr != nil {
				return
			}
			o := asObj(v)
			if o == nil {
				return
			}
			doc = o
		case !pycompat.IsNotExist(err):
			return
		}
		if !mutate(doc) {
			return
		}
		atomicWrite(path, ".proj.", []byte(pycompat.Marshal(doc, pycompat.Format{Indent: 2, ASCII: true})), 0o600, false)
	})
}

// projectPicker writes the profile's own /model rows into its .claude.json and the
// allowlist into its settings layer. Args: home conf own tag profile layer launch;
// the endpoint's listing is in _CCM_MODELS_JSON.
func projectPicker(args []string, _ io.Reader, _, errw io.Writer) int {
	if len(args) != 7 {
		return 2
	}
	home, contextConf, own, tag, profile, layer, launch := args[0], args[1], args[2], args[3], args[4], args[5], args[6]
	tagText := pycompat.Strip(tag)
	if tagText == "" {
		tagText = "Gateway"
	}
	var fields map[string]string
	if e := pycompat.Catch(func() {
		fields = map[string]string{}
		if exists(contextConf) {
			fields = confFields(contextConf)
		}
	}); e != nil {
		fmt.Fprintln(errw, e.Msg)
		return 1
	}
	excluded := map[string]bool{}
	for _, m := range strings.Split(fields["excluded_models"], ",") {
		if m != "" {
			excluded[m] = true
		}
	}
	modelMap := pycompat.NewObj()
	if mv, err := pycompat.Parse([]byte(orDefault(fields["model_map"], "{}"))); err == nil {
		if o := asObj(mv); o != nil {
			modelMap = o
		}
	}

	var served []any
	fresh := true
	if pv, err := pycompat.Parse([]byte(os.Getenv("_CCM_MODELS_JSON"))); err == nil && asObj(pv) != nil {
		data := getKey(asObj(pv), "data")
		if !truthy(data) {
			data = []any{}
		}
		items, ok := iterItems(data)
		if !ok {
			fmt.Fprintln(errw, "TypeError: object is not iterable")
			return 1
		}
		served = items
	} else {
		if len(excluded) == 0 {
			return 0
		}
		v, lerr := loadJSON(filepath.Join(home, ".claude.json"))
		if lerr != nil {
			fmt.Fprintln(errw, lerr.Error())
			return 1
		}
		cached := []any{}
		if o := asObj(v); o != nil {
			if c, has := o.Get("additionalModelOptionsCache"); has {
				items, ok := iterItems(c)
				if !ok {
					fmt.Fprintln(errw, "TypeError: object is not iterable")
					return 1
				}
				cached = items
			}
		} else {
			fmt.Fprintln(errw, "AttributeError: object has no attribute 'get'")
			return 1
		}
		for _, r := range cached {
			if value, ok := getKey(asObj(r), "value").(string); ok {
				row := pycompat.NewObj()
				row.Set("id", value)
				served = append(served, row)
			}
		}
		fresh = false
	}

	isExcl := func(id string) bool { return ctxwindow.IsExcluded(fields, id) }
	mappedAlias := func(id string) bool {
		if id == "" {
			return false
		}
		bare := strings.TrimSuffix(id, "[1m]")
		target, ok := getKey(modelMap, bare).(string)
		return ok && target != "" && !isExcl(target)
	}
	derivedSendID := func(id string) string {
		if !exists(contextConf) {
			return ""
		}
		text := ""
		if pycompat.Catch(func() { text = pycompat.ReadText(contextConf) }) != nil {
			return ""
		}
		known := false
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "context_window.") || strings.HasPrefix(line, "context_discovered=") {
				known = true
				break
			}
		}
		if !known {
			return ""
		}
		saved, had := os.LookupEnv("CLAUDE_CODE_MAX_CONTEXT_TOKENS")
		os.Unsetenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS")
		os.Setenv("MODEL_WINDOW_PROFILE_CONF", contextConf)
		if had {
			defer os.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", saved)
		}
		var rec ctxwindow.Record
		if pycompat.Catch(func() { rec = ctxwindow.Resolve(profile, id) }) != nil || rec.Unprobed {
			return ""
		}
		if rec.SendID != id {
			return rec.SendID
		}
		return ""
	}

	var rows []any
	var values []string
	for _, m := range served {
		o := asObj(m)
		if o == nil {
			continue
		}
		mid, ok := getKey(o, "id").(string)
		if !ok || mid == "" || isExcl(mid) {
			continue
		}
		if caps := asObj(getKey(o, "capabilities")); caps != nil {
			if v, has := caps.Get("chat_completion"); has && v == false {
				continue
			}
		}
		real := ctxwindow.RealID(mid)
		if real == mid && strings.HasPrefix(mid, "claude-") {
			if send := derivedSendID(mid); send != "" {
				real = send
			}
		}
		row := pycompat.NewObj()
		row.Set("value", real)
		row.Set("label", real)
		row.Set("description", tagText)
		rows = append(rows, row)
		values = append(values, real)
	}
	if len(rows) == 0 {
		if len(excluded) > 0 {
			fmt.Fprintln(errw, "ccmorph: no cached or served picker models remain after excluded_models; revise the list or restore gateway access")
			return 1
		}
		return 0
	}

	rewriteJSON(filepath.Join(home, ".claude.json"), func(doc *pycompat.Obj) bool {
		if cur, has := doc.Get("additionalModelOptionsCache"); has && pycompat.Equal(cur, rows) {
			return false
		}
		doc.Set("additionalModelOptionsCache", rows)
		return true
	})

	rewriteJSON(layer, func(doc *pycompat.Obj) bool {
		beforeModels, _ := doc.Get("availableModels")
		beforeModel, _ := doc.Get("model")
		changed := func() bool {
			am, _ := doc.Get("availableModels")
			md, _ := doc.Get("model")
			return !pycompat.Equal(am, beforeModels) || !pycompat.Equal(md, beforeModel)
		}
		stored, _ := doc.Get("model")
		if s, ok := stored.(string); ok && cloaked.MatchString(s) {
			undisguised := ctxwindow.RealID(s)
			for _, v := range values {
				if v == undisguised {
					stored = undisguised
					doc.Set("model", undisguised)
					break
				}
			}
		}
		var current []any
		src, _ := doc.Get("availableModels")
		if truthy(src) {
			items, ok := iterItems(src)
			if !ok {
				pycompat.Crash("TypeError", "object is not iterable")
			}
			for _, m := range items {
				if s, ok := m.(string); ok && !isExcl(s) {
					current = append(current, s)
				}
			}
		}
		keepLaunch := mappedAlias(launch) && !isExcl(launch)
		if !fresh {
			if keepLaunch && !inList(current, launch) {
				current = append(current, launch)
			}
			if current == nil {
				current = []any{}
			}
			doc.Set("availableModels", current)
			return changed()
		}
		var fam []string
		for _, m := range current {
			for _, f := range families {
				if m == f {
					fam = append(fam, f)
				}
			}
		}
		covered := func(model string) bool {
			for _, f := range fam {
				if strings.HasPrefix(model, "claude-"+f+"-") {
					return true
				}
			}
			return false
		}
		var kept []any
		for _, mv := range current {
			m := mv.(string)
			isFam := false
			for _, f := range fam {
				isFam = isFam || m == f
			}
			if (isFam || (strings.HasPrefix(m, "claude-") && !covered(m))) && !cloaked.MatchString(m) {
				kept = append(kept, m)
			}
		}
		inProjected := func(s string) bool {
			for _, v := range values {
				if v == s {
					return true
				}
			}
			return false
		}
		for _, keepV := range []any{own, stored} {
			if k, ok := keepV.(string); ok && k != "" && !inProjected(k) && !isExcl(k) && !covered(k) {
				kept = append(kept, k)
			}
		}
		if keepLaunch && !inProjected(launch) {
			kept = append(kept, launch)
		}
		seen := map[string]bool{}
		merged := []any{}
		for _, s := range append(kept, toAny(values)...) {
			if !seen[s.(string)] {
				seen[s.(string)] = true
				merged = append(merged, s)
			}
		}
		doc.Set("availableModels", merged)
		return changed()
	})
	return 0
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// launchWindow prints the one-run window facts a launch needs, tab-separated:
// the launched model's record (4 fields), the route ceiling, the saved auto-compact
// budget. Args: profile model layer...; every fact falls through on its own.
func launchWindow(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) < 2 {
		return 2
	}
	profile, model, sources := args[0], args[1], args[2:]
	fields := make([]string, 6)
	doc := (*pycompat.Obj)(nil)
	pycompat.Catch(func() { doc = mergedView(sources) })

	if model != "" {
		pycompat.Catch(func() {
			copy(fields[0:4], ctxwindow.Resolve(profile, model).Fields())
		})
	}
	pycompat.Catch(func() {
		values := ctxwindow.ProfileWindows(profile)
		var best int64
		models, _ := getKey(doc, "availableModels").([]any)
		for _, mv := range models {
			m, ok := mv.(string)
			if !ok {
				continue
			}
			m = strings.SplitN(m, "[", 2)[0]
			if strings.HasPrefix(m, "claude-") || m == "opus" || m == "sonnet" || m == "haiku" || m == "fable" {
				continue
			}
			if w := values[m]; w > best {
				best = w
			}
		}
		if best > 0 {
			fields[4] = fmt.Sprint(best)
		}
	})
	pycompat.Catch(func() {
		if doc == nil {
			pycompat.Crash("AttributeError", "no view")
		}
		v, has := doc.Get("autoCompactWindow")
		if has && acwValid(v) {
			fields[5] = acwString(v)
		}
	})
	fmt.Fprintln(out, strings.Join(fields, "\t"))
	return 0
}
