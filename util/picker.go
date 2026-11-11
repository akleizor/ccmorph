package util

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// iterItems is what Python's `for x in value` yields for a decoded JSON value.
func iterItems(v any) ([]any, bool) {
	switch t := v.(type) {
	case []any:
		return t, true
	case string:
		var out []any
		for _, c := range t {
			out = append(out, string(c))
		}
		return out, true
	case *ordjson.Obj:
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
func rewriteJSON(path string, mutate func(doc *ordjson.Obj) bool) {
	doc := ordjson.New()
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		v, perr := ordjson.Parse(b)
		o := asObj(v)
		if perr != nil || o == nil {
			return
		}
		doc = o
	case !os.IsNotExist(err):
		return
	}
	if mutate(doc) {
		AtomicWrite(path, ".proj.", ordjson.Marshal(doc, true), 0o600, false)
	}
}

// PickerIn is what the project picker needs: the profile's home dir, conf, its own
// model, the row tag, the profile name, its settings layer, the launch model and
// the endpoint's /v1/models listing.
type PickerIn struct {
	Home, Conf, Own, Tag, Profile, Layer, Launch, ModelsJSON string
}

// ProjectPicker writes the profile's own /model rows into its .claude.json and the
// allowlist into its settings layer. False when it could not; reasons go to errw.
func ProjectPicker(in PickerIn, errw io.Writer) bool {
	home, contextConf, own, tag, layer, launch := in.Home, in.Conf, in.Own, in.Tag, in.Layer, in.Launch
	tagText := strings.TrimSpace(tag)
	if tagText == "" {
		tagText = "Gateway"
	}
	confText := ""
	fields := map[string]string{}
	confExists := paths.Exists(contextConf)
	if confExists {
		var err error
		if confText, fields, err = ctxwindow.ReadConf(contextConf); err != nil {
			fmt.Fprintln(errw, err)
			return false
		}
	}
	excluded := map[string]bool{}
	for _, m := range strings.Split(fields["excluded_models"], ",") {
		if m != "" {
			excluded[m] = true
		}
	}
	modelMap := ordjson.New()
	if mv, err := ordjson.Parse([]byte(orDefault(fields["model_map"], "{}"))); err == nil {
		if o := asObj(mv); o != nil {
			modelMap = o
		}
	}

	var served []any
	fresh := true
	if pv, err := ordjson.Parse([]byte(in.ModelsJSON)); err == nil && asObj(pv) != nil {
		data := getKey(asObj(pv), "data")
		if !truthy(data) {
			data = []any{}
		}
		items, ok := iterItems(data)
		if !ok {
			fmt.Fprintln(errw, "TypeError: object is not iterable")
			return false
		}
		served = items
	} else {
		if len(excluded) == 0 {
			return true
		}
		v, lerr := loadJSON(filepath.Join(home, ".claude.json"))
		if lerr != nil {
			fmt.Fprintln(errw, lerr.Error())
			return false
		}
		cached := []any{}
		if o := asObj(v); o != nil {
			if c, has := o.Get("additionalModelOptionsCache"); has {
				items, ok := iterItems(c)
				if !ok {
					fmt.Fprintln(errw, "TypeError: object is not iterable")
					return false
				}
				cached = items
			}
		} else {
			fmt.Fprintln(errw, "AttributeError: object has no attribute 'get'")
			return false
		}
		for _, r := range cached {
			if value, ok := getKey(asObj(r), "value").(string); ok {
				row := ordjson.New()
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
	// The conf is written before the picker runs, so everything the derived
	// send-id check reads is constant: read and parse it once, before the loop.
	known := false
	if confExists {
		known = paths.Exists(ctxwindow.DiscoveredPath(contextConf))
		for _, line := range strings.Split(confText, "\n") {
			if strings.HasPrefix(line, "context_window.") || strings.HasPrefix(line, "context_discovered=") {
				known = true
				break
			}
		}
	}
	windowInputs := ctxwindow.NewInputs(fields)
	windowInputs.Preload()
	derivedSendID := func(id string) string {
		if !known {
			return ""
		}
		rec := windowInputs.Resolve(id)
		if rec.Unprobed {
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
		row := ordjson.New()
		row.Set("value", real)
		row.Set("label", real)
		row.Set("description", tagText)
		rows = append(rows, row)
		values = append(values, real)
	}
	if len(rows) == 0 {
		if len(excluded) > 0 {
			fmt.Fprintln(errw, "ccmorph: no cached or served picker models remain after excluded_models; revise the list or restore gateway access")
			return false
		}
		return true
	}

	rewriteJSON(filepath.Join(home, ".claude.json"), func(doc *ordjson.Obj) bool {
		if cur, has := doc.Get("additionalModelOptionsCache"); has && ordjson.Equal(cur, rows) {
			return false
		}
		doc.Set("additionalModelOptionsCache", rows)
		return true
	})

	rewriteJSON(layer, func(doc *ordjson.Obj) bool {
		beforeModels, _ := doc.Get("availableModels")
		beforeModel, _ := doc.Get("model")
		changed := func() bool {
			am, _ := doc.Get("availableModels")
			md, _ := doc.Get("model")
			return !ordjson.Equal(am, beforeModels) || !ordjson.Equal(md, beforeModel)
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
				return false // an allowlist that is not a list is left as it is
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
	return true
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

// LaunchWindow is the one-run window facts a launch needs, six fields: the launched
// model's record (4 fields), the route ceiling, the saved auto-compact budget; every
// fact falls through on its own. It reads the profile through the environment's
// state dir and MODEL_WINDOW_PROFILE_CONF, as the resolver does.
func LaunchWindow(profile, model string, sources []string) []string {
	fields := make([]string, 6)
	doc := mergedView(sources)
	if model != "" {
		copy(fields[0:4], ctxwindow.Resolve(profile, model).Fields())
	}
	values, _ := ctxwindow.ProfileWindows(profile)
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
	if v, has := doc.Get("autoCompactWindow"); has && acwValid(v) {
		fields[5] = acwString(v)
	}
	return fields
}
