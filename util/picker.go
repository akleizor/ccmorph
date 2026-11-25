package util

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/settingscompose"
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

// rewriteJSON is the picker's read, mutate, atomic replace of one file in profile
// name's home — its .claude.json or its settings layer — through the one locked
// rewrite every writer of those files shares (#861). Any failure, and a holder
// of the profile's live lock that outlasts the bounded wait, leaves the file as
// it was: the rows the file already hold stand until a launch can write them.
func rewriteJSON(profiles, name, path string, mutate func(doc *ordjson.Obj) bool) error {
	return rewriteProfileJSON(profiles, name, path, func(doc *ordjson.Obj) (bool, error) {
		return mutate(doc), nil
	}, func(path string, doc *ordjson.Obj) error {
		return AtomicWrite(path, ".proj.", ordjson.Marshal(doc, true), 0o600, false)
	})
}

// PickerIn is what the project picker needs: the profile's home dir, conf, its own
// model, the row tag, the profile name, its settings layer, the launch model and
// the endpoint's /v1/models listing. A listing that did not arrive usable is read
// as exactly that: the gateway's state, never the profile's config error (#723).
type PickerIn struct {
	Home, Conf, Own, Tag, Profile, Layer, Launch, ModelsJSON string
}

// PickerResult is how a project-picker pass ends. RCGatewayDown is the exit
// class of the route-side one: EX_TEMPFAIL — the condition may clear, retry
// later — so a caller can tell a gateway outage from its own crash (#723).
type PickerResult int

const (
	PickerDone      PickerResult = iota // rows projected, or there was nothing to project
	PickerRefused                       // the pass failed; the reason was printed to errw
	PickerUnreached                     // no listing arrived and the cache judged nothing: the caller words the route failure
)

const RCGatewayDown = 75

// ProjectPicker writes the profile's own /model rows into its .claude.json and the
// allowlist into its settings layer. Reasons for a refusal go to errw; a pass the
// gateway's silence made undecidable returns PickerUnreached worded by no one,
// because the route — not this profile — is what failed (#723).
func ProjectPicker(in PickerIn, errw io.Writer) PickerResult {
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
			return PickerRefused
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

	// A reply body that is not a listing — a 200 error object, data absent,
	// null or not a list, a body that does not parse — is no listing arrived,
	// exactly as silence is: the cache is judged and the caller words the
	// route. Only a real data list, empty or not, is the endpoint's own answer
	// (#747, the shared shape rule of #558 and #695).
	var served []any
	fresh := false
	if rows, verdict := ctxwindow.ListingShapeBody(in.ModelsJSON); verdict != ctxwindow.ListingNotAListing {
		served, fresh = rows, true
	}
	if !fresh {
		if len(excluded) == 0 {
			return PickerDone
		}
		v, lerr := loadJSON(filepath.Join(home, ".claude.json"))
		if lerr != nil {
			fmt.Fprintln(errw, lerr.Error())
			return PickerRefused
		}
		cached := []any{}
		if o := asObj(v); o != nil {
			if c, has := o.Get("additionalModelOptionsCache"); has {
				items, ok := iterItems(c)
				if !ok {
					fmt.Fprintln(errw, "TypeError: object is not iterable")
					return PickerRefused
				}
				cached = items
			}
		} else {
			fmt.Fprintln(errw, "AttributeError: object has no attribute 'get'")
			return PickerRefused
		}
		for _, r := range cached {
			if value, ok := getKey(asObj(r), "value").(string); ok {
				row := ordjson.New()
				row.Set("id", value)
				served = append(served, row)
			}
		}
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
		if DeniedForChat(o) {
			continue
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
			if !fresh {
				// The listing never arrived usable and the cache holds nothing to
				// fall back on: whether the route would answer past the exclusions
				// is unknowable until it answers (#723). This is the gateway's
				// state, not the profile's config — errw stays silent here so the
				// caller can word the route it could not reach.
				return PickerUnreached
			}
			fmt.Fprintln(errw, "ccmorph: "+ExclusionsRefusal(in.Profile, contextConf))
			return PickerRefused
		}
		return PickerDone
	}

	// The profile is the home itself, so the lock is the one ApproveKey and a
	// session of this profile take for the same files (#861).
	profiles, name := filepath.Dir(home), filepath.Base(home)
	rewriteJSON(profiles, name, filepath.Join(home, ".claude.json"), func(doc *ordjson.Obj) bool {
		if cur, has := doc.Get("additionalModelOptionsCache"); has && ordjson.Equal(cur, rows) {
			return false
		}
		doc.Set("additionalModelOptionsCache", rows)
		return true
	})

	rewriteJSON(profiles, name, layer, func(doc *ordjson.Obj) bool {
		beforeModels, _ := doc.Get("availableModels")
		beforeModel, _ := doc.Get("model")
		changed := func() bool {
			am, _ := doc.Get("availableModels")
			md, _ := doc.Get("model")
			return !ordjson.Equal(am, beforeModels) || !ordjson.Equal(md, beforeModel)
		}
		stored, _ := doc.Get("model")
		if s, ok := stored.(string); ok && disguised.MatchString(s) {
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
		if ordjson.Truthy(src) {
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
			if keepLaunch && !hasString(current, launch) {
				current = append(current, launch)
			}
			if current == nil {
				current = []any{}
			}
			doc.Set("availableModels", current)
			return changed()
		}
		var fam []string
		for _, mv := range current {
			if m, ok := mv.(string); ok && IsTier(m) {
				fam = append(fam, m)
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
			if (isFam || (strings.HasPrefix(m, "claude-") && !covered(m))) && !disguised.MatchString(m) {
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
	return PickerDone
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
	doc := settingscompose.MergedView(sources)
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
		if strings.HasPrefix(m, "claude-") || IsTier(m) {
			continue
		}
		if w := values[m]; w > best {
			best = w
		}
	}
	if best > 0 {
		fields[4] = fmt.Sprint(best)
	}
	if v, has := doc.Get("autoCompactWindow"); has && settingscompose.ACWValid(v) {
		fields[5] = acwString(v)
	}
	return fields
}
