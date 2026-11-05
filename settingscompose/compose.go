// Package settingscompose is the Go port of libexec/settings-compose: it writes a
// profile's native settings.json as canonical settings plus the profile's own
// layer, and folds Claude Code's own edits back into that layer. See the Python
// helper for the long-form contract; this port keeps its argv, files and exit
// codes (0 ok, 1 failed with a safe fallback, 2 usage, 3 refused: the settings
// generated last time do not carry the linked policy).
package settingscompose

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
)

type (
	value = any
	obj   = ordjson.Obj
)

const (
	layerFile   = "settings.profile.json"
	policyFile  = "settings.policy.json"
	baseFile    = "cache/settings.base.json"
	pendingFile = "cache/settings.base.pending.json"
	lockFile    = "cache/.settings.lock"
	backupsDir  = "backups"
	keepBackups = 10
	unsetKey    = "$ccmorphUnset"
	// A static hook: it prints this launch's advisory when the launch left one.
	adviseCommand = `[ -n "$CCMORPH_ADVISE" ] && printf "%s" "$CCMORPH_ADVISE" || true`
	// The first release read the advisory from a file; its entry may still sit in a profile's settings.
	adviseCommandV1 = `f="$CCMORPH_ADVISE_FILE"; [ -n "$f" ] && [ -f "$f" ] && cat "$f" || true`
)

var (
	ownedTop = []string{"model", "effortLevel", "availableModels"}
	ownedEnv = []string{"ANTHROPIC_MODEL", "CLAUDE_CODE_EFFORT_LEVEL"}
	// Deny rules only ever accumulate.
	permissionLists = [][]string{{"permissions", "deny"}}
)

// failure is a panic carrying why a projection failed; Main recovers it. A policy
// failure refuses the fallback (exit 3), any other one keeps the old settings (exit 1).
type failure struct {
	policy bool
	msg    string
}

func (f *failure) Error() string { return f.msg }

func osFail(err error) { panic(&failure{msg: err.Error()}) }
func valueFail(format string, a ...any) {
	panic(&failure{msg: fmt.Sprintf(format, a...)})
}
func policyFail(format string, a ...any) {
	panic(&failure{policy: true, msg: fmt.Sprintf(format, a...)})
}

// catch runs fn and returns the failure it raised, if any. A foreign panic (a
// settings file of an unexpected shape) is a failure too, never a crash of the launch.
func catch(fn func()) (out *failure) {
	defer func() {
		switch r := recover().(type) {
		case nil:
		case *failure:
			out = r
		default:
			out = &failure{msg: fmt.Sprint(r)}
		}
	}()
	fn()
	return nil
}

func stripBOM(b []byte) []byte { return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")) }

type missingT struct{}

var missing value = missingT{}

// Test hooks: the two reads whose failures the policy tests must provoke.
var (
	readFile     = os.ReadFile
	readPolicyFn = readPolicy
)

func parse(b []byte) value {
	v, err := ordjson.Parse(b)
	if err != nil {
		valueFail("%s", err)
	}
	return v
}

// pyPath normalises a path the way pathlib prints it: no empty or "." parts.
func pyPath(p string) string {
	abs := strings.HasPrefix(p, "/")
	var parts []string
	for _, s := range strings.Split(p, "/") {
		if s != "" && s != "." {
			parts = append(parts, s)
		}
	}
	out := strings.Join(parts, "/")
	switch {
	case abs:
		return "/" + out
	case out == "":
		return "."
	}
	return out
}

// join is pathlib's `dir / name`: no cleaning, so ".." stays where the user put it.
func join(dir string, names ...string) string {
	for _, n := range names {
		switch {
		case dir == ".":
			dir = n
		case strings.HasSuffix(dir, "/"):
			dir += n
		default:
			dir += "/" + n
		}
	}
	return dir
}

func asObj(v value, attr string) *obj {
	o, ok := v.(*obj)
	if !ok {
		valueFail("expected a JSON object (for %s), got %s", attr, typeName(v))
	}
	return o
}

func asList(v value) []any {
	l, ok := v.([]any)
	if !ok {
		valueFail("expected a JSON array, got %s", typeName(v))
	}
	return l
}

func truthy(v value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case json.Number:
		f, _ := t.Float64()
		return f != 0
	case []any:
		return len(t) > 0
	case *obj:
		return t.Len() > 0
	}
	return true
}

func load(path string) *obj {
	b, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ordjson.New()
		}
		osFail(err)
	}
	// Every settings file here is one a user's editor may have prefixed with a
	// BOM: read them all the way settings.json itself is read.
	o, ok := parse(stripBOM(b)).(*obj)
	if !ok {
		valueFail("%s: root is not a JSON object", path)
	}
	return o
}

func readPolicy(profileDir string) *obj {
	policy := join(profileDir, policyFile)
	b, err := readFile(policy)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Only an absent entry is optional, not a replaced link.
			if _, lerr := os.Lstat(policy); lerr != nil {
				if errors.Is(lerr, fs.ErrNotExist) {
					return ordjson.New()
				}
				policyFail("%s: %s", policy, lerr.Error())
			}
			policyFail("%s: missing file during policy read", policy)
		}
		policyFail("%s: %s", policy, err.Error())
	}
	v, perr := ordjson.Parse(stripBOM(b))
	if perr != nil {
		policyFail("%s: %s", policy, perr)
	}
	doc, ok := v.(*obj)
	if !ok {
		policyFail("%s: root is not a JSON object", policy)
	}
	doc.Delete(unsetKey) // tombstones belong to profile layers, not to a shared policy
	return doc
}

func canonicalFor(profileDir, canonicalPath string) *obj {
	doc := readPolicyFn(profileDir)
	base := load(canonicalPath)
	return merge(base, doc, nil)
}

func readBytes(path string) []byte {
	b, err := readFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		osFail(err)
	}
	if b == nil {
		b = []byte{}
	}
	return b
}

func encode(doc *obj) []byte {
	return append(ordjson.Marshal(doc, true), '\n')
}

func writeBytes(path string, data []byte) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		osFail(err)
	}
	f, err := os.CreateTemp(dir, ".settings.")
	if err != nil {
		osFail(err)
	}
	tmp := f.Name()
	fail := func(err error) {
		f.Close()
		os.Remove(tmp)
		osFail(err)
	}
	if _, err := f.Write(data); err != nil {
		fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		osFail(err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		osFail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		osFail(err)
	}
}

// settle finishes or discards a projection that was interrupted between its writes.
func settle(profileDir string) {
	pending := join(profileDir, pendingFile)
	data := readBytes(pending)
	if data == nil {
		return
	}
	if bytes.Equal(readBytes(join(profileDir, "settings.json")), data) {
		if err := os.Rename(pending, join(profileDir, baseFile)); err != nil {
			osFail(err)
		}
	} else if err := os.Remove(pending); err != nil {
		osFail(err)
	}
}

// saveBase writes the copy load() re-reads on every later launch, so it carries
// settings.json's content, never the BOM an editor prefixed the file with.
func saveBase(profileDir string, data []byte) {
	data = stripBOM(data)
	if cur := readBytes(join(profileDir, baseFile)); cur == nil || !bytes.Equal(cur, data) {
		writeBytes(join(profileDir, baseFile), data)
	}
}

func list(v value) (out []any) {
	if l, ok := v.([]any); ok {
		return l
	}
	return nil
}

func getObj(o *obj, k string) value {
	v, _ := o.Get(k)
	return v
}

func withoutInheritedHooks(layerIn, canonical *obj) *obj {
	layer := ordjson.Clone(layerIn).(*obj)
	hooksV := getObj(layer, "hooks")
	var hooks *obj
	if truthy(hooksV) {
		hooks = asObj(hooksV, "items")
	}
	if hooks != nil {
		canonHooks := ordjson.New()
		if h := getObj(canonical, "hooks"); truthy(h) {
			canonHooks = asObj(h, "get")
		}
		for _, event := range hooks.Keys() {
			entries := asList(getObj(hooks, event))
			type ident struct{ matcher, command value }
			var inherited []ident
			if raw, ok := canonHooks.Get(event); ok {
				for _, g := range asList(raw) {
					group := asObj(g, "get")
					for _, h := range asList(orEmptyList(group, "hooks")) {
						inherited = append(inherited, ident{orDefault(group, "matcher", ""), getObj(asObj(h, "get"), "command")})
					}
				}
			}
			has := func(i ident) bool {
				for _, x := range inherited {
					if ordjson.Equal(x.matcher, i.matcher) && ordjson.Equal(x.command, i.command) {
						return true
					}
				}
				return false
			}
			var kept []any
			for _, g := range entries {
				group := asObj(g, "get")
				var keep []any
				for _, h := range asList(orEmptyList(group, "hooks")) {
					hook := asObj(h, "get")
					if !has(ident{orDefault(group, "matcher", ""), getObj(hook, "command")}) {
						keep = append(keep, hook)
					}
				}
				if len(keep) > 0 {
					c := ordjson.Clone(group).(*obj)
					c.Set("hooks", keep)
					kept = append(kept, c)
				}
			}
			if len(kept) > 0 {
				hooks.Set(event, kept)
			} else {
				hooks.Delete(event)
			}
		}
	}
	if v, ok := layer.Get("hooks"); ok && !truthy(v) {
		layer.Delete("hooks")
	}
	return layer
}

func orEmptyList(o *obj, k string) value {
	if v, ok := o.Get(k); ok {
		return v
	}
	return []any{}
}

func orDefault(o *obj, k string, d value) value {
	if v, ok := o.Get(k); ok {
		return v
	}
	return d
}

func hasPath(paths [][]string, p []string) bool {
	for _, q := range paths {
		if pathEq(p, q) {
			return true
		}
	}
	return false
}

func pathEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hasPrefix(p, prefix []string) bool {
	return len(p) >= len(prefix) && pathEq(p[:len(prefix)], prefix)
}

// merge is a deep merge with special handling for hook and permission lists:
// lists under .hooks.* append, permissions.deny combines without duplicates,
// other lists replace. base and override are left untouched.
func merge(base, override *obj, path []string) *obj {
	result := ordjson.Clone(base).(*obj)
	mergeInto(result, override, path)
	return result
}

func mergeInto(result, override *obj, path []string) {
	for _, key := range override.Keys() {
		v, _ := override.Get(key)
		cur := append(append([]string(nil), path...), key)
		existing, _ := result.Get(key)
		vo, vIsObj := v.(*obj)
		eo, eIsObj := existing.(*obj)
		vl, vIsList := v.([]any)
		el, eIsList := existing.([]any)
		switch {
		case vIsObj && eIsObj:
			mergeInto(eo, vo, cur)
		case vIsList && eIsList:
			switch {
			case len(cur) >= 2 && cur[0] == "hooks":
				result.Set(key, append(append([]any(nil), el...), ordjson.Clone(vl).([]any)...))
			case hasPath(permissionLists, cur):
				out := append([]any(nil), el...)
				for _, item := range vl {
					found := false
					for _, e := range el {
						if ordjson.Equal(item, e) {
							found = true
							break
						}
					}
					if !found {
						out = append(out, ordjson.Clone(item))
					}
				}
				result.Set(key, out)
			default:
				result.Set(key, ordjson.Clone(v))
			}
		default:
			result.Set(key, ordjson.Clone(v))
		}
	}
}

// usageHookMark is the one command ccmorph owns in a profile's Stop list: the
// launch's own usage-row writer. A user's Stop hooks are never touched.
const usageHookMark = "ccmorph-core usage-row --profile "

// isUsageHook is an entry whose command is this profile's usage writer.
func isUsageHook(entry value) bool {
	e, ok := entry.(*obj)
	if !ok {
		return false
	}
	hooks, ok := getObj(e, "hooks").([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		if ho, ok := h.(*obj); ok {
			if c, ok := getObj(ho, "command").(string); ok && strings.Contains(c, usageHookMark) {
				return true
			}
		}
	}
	return false
}

// withoutUsage drops ccmorph's usage hook, so a fold never writes it into the
// user's layer and a compose that no longer asks for one removes the old one.
func withoutUsage(docIn *obj) *obj {
	doc := ordjson.Clone(docIn).(*obj)
	hooks, ok := getObj(doc, "hooks").(*obj)
	if !ok {
		return doc
	}
	entries, ok := getObj(hooks, "Stop").([]any)
	if !ok {
		return doc
	}
	var kept []any
	for _, e := range entries {
		if !isUsageHook(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		hooks.Set("Stop", kept)
	} else {
		hooks.Delete("Stop")
		if hooks.Len() == 0 {
			doc.Delete("hooks")
		}
	}
	return doc
}

// addUsage puts the launch's usage hook into Stop, after any hook the user set.
// withoutUsage copies, so the caller keeps the returned document.
func addUsage(doc *obj, command string) *obj {
	doc = withoutUsage(doc)
	if command == "" {
		return doc
	}
	hooks, ok := getObj(doc, "hooks").(*obj)
	if !ok {
		hooks = ordjson.New()
		doc.Set("hooks", hooks)
	}
	entries, _ := getObj(hooks, "Stop").([]any)
	hook := ordjson.New()
	hook.Set("type", "command")
	hook.Set("command", command)
	entry := ordjson.New()
	entry.Set("hooks", []any{hook})
	hooks.Set("Stop", append(entries, entry))
	return doc
}

func withoutAdvise(docIn *obj) *obj {
	doc := ordjson.Clone(docIn).(*obj)
	hooks, ok := getObj(doc, "hooks").(*obj)
	if !ok {
		return doc
	}
	entries, ok := getObj(hooks, "SessionStart").([]any)
	if !ok {
		return doc
	}
	var kept []any
	for _, e := range entries {
		if !isAdvise(e) {
			kept = append(kept, e)
		}
	}
	if len(kept) > 0 {
		hooks.Set("SessionStart", kept)
	} else {
		hooks.Delete("SessionStart")
		if hooks.Len() == 0 {
			doc.Delete("hooks")
		}
	}
	return doc
}

func isAdvise(entry value) bool {
	e, ok := entry.(*obj)
	if !ok {
		return false
	}
	hooks, ok := getObj(e, "hooks").([]any)
	if !ok {
		return false
	}
	for _, h := range hooks {
		if ho, ok := h.(*obj); ok {
			if c, ok := getObj(ho, "command").(string); ok && (c == adviseCommand || c == adviseCommandV1) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func owned(path []string, mapping map[string]bool) bool {
	if len(path) > 0 && path[0] == "hooks" {
		// Hooks flow one way, canonical -> profile; folding them back out of
		// settings.json would re-capture canonical hooks into the layer.
		return true
	}
	if len(path) == 1 {
		return contains(ownedTop, path[0])
	}
	if len(path) == 2 && path[0] == "env" {
		return contains(ownedEnv, path[1]) || mapping[path[1]]
	}
	if len(path) == 3 && path[0] == "modelSettings" {
		return path[2] == "effortLevel"
	}
	return false
}

func getPath(doc value, path []string) value {
	node := doc
	for _, k := range path {
		o, ok := node.(*obj)
		if !ok {
			return missing
		}
		v, ok := o.Get(k)
		if !ok {
			return missing
		}
		node = v
	}
	return node
}

func setPath(doc *obj, path []string, v value) {
	node := doc
	for _, k := range path[:len(path)-1] {
		next, ok := getObj(node, k).(*obj)
		if !ok {
			next = ordjson.New()
			node.Set(k, next)
		}
		node = next
	}
	node.Set(path[len(path)-1], ordjson.Clone(v))
}

func deletePath(doc *obj, path []string) bool {
	node := doc
	for _, k := range path[:len(path)-1] {
		next, ok := getObj(node, k).(*obj)
		if !ok {
			return false
		}
		node = next
	}
	if node.Has(path[len(path)-1]) {
		node.Delete(path[len(path)-1])
		return true
	}
	return false
}

func typeName(v value) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case *obj:
		return "object"
	}
	return "value"
}

// tombstones lists the layer's deletions. A malformed list fails the projection:
// null, booleans and numbers are refused, a string or object yields nothing.
func tombstones(layer *obj) [][]string {
	raw, ok := layer.Get(unsetKey)
	if !ok {
		return nil
	}
	items, isList := raw.([]any)
	if !isList {
		switch raw.(type) {
		case string, *obj:
			return nil
		}
		valueFail("%s must be an array, got %s", unsetKey, typeName(raw))
	}
	var out [][]string
next:
	for _, p := range items {
		keys, ok := p.([]any)
		if !ok || len(keys) == 0 {
			continue
		}
		path := make([]string, len(keys))
		for i, k := range keys {
			s, ok := k.(string)
			if !ok {
				continue next
			}
			path[i] = s
		}
		out = append(out, path)
	}
	return out
}

func withoutTombstones(layer *obj) *obj {
	out := ordjson.New()
	for _, k := range layer.Keys() {
		if k != unsetKey {
			v, _ := layer.Get(k)
			out.Set(k, v)
		}
	}
	return out
}

// applyTombstones deletes what the layer tombstones. Hooks are exempt: they only accumulate.
func applyTombstones(doc, layer *obj) {
	for _, p := range tombstones(layer) {
		if p[0] != "hooks" {
			deletePath(doc, p)
		}
	}
}

func setTombstones(layer *obj, paths [][]string) {
	sort.Slice(paths, func(i, j int) bool { return pathLess(paths[i], paths[j]) })
	var unique [][]string
	for _, p := range paths {
		if len(unique) == 0 || !pathEq(unique[len(unique)-1], p) {
			unique = append(unique, p)
		}
	}
	if len(unique) == 0 {
		layer.Delete(unsetKey)
		return
	}
	out := make([]any, len(unique))
	for i, p := range unique {
		l := make([]any, len(p))
		for j, k := range p {
			l[j] = k
		}
		out[i] = l
	}
	layer.Set(unsetKey, out)
}

func pathLess(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// fold carries edits made to settings.json since base into the profile layer.
func fold(base, current, layerIn, canonical *obj, mapping map[string]bool) *obj {
	layer := ordjson.Clone(layerIn).(*obj)
	unset := tombstones(layer)

	var walk func(b, c *obj, path []string)
	walk = func(b, c *obj, path []string) {
		keys := b.Keys()
		for _, k := range c.Keys() {
			if !b.Has(k) {
				keys = append(keys, k)
			}
		}
		for _, key := range keys {
			here := append(append([]string(nil), path...), key)
			if owned(here, mapping) {
				continue
			}
			before, after := value(missing), value(missing)
			if v, ok := b.Get(key); ok {
				before = v
			}
			if v, ok := c.Get(key); ok {
				after = v
			}
			if ordjson.Equal(before, after) {
				continue
			}
			bo, bok := before.(*obj)
			ao, aok := after.(*obj)
			switch {
			case bok && aok:
				walk(bo, ao, here)
			case after == missing:
				deletePath(layer, here)
				if getPath(canonical, here) != missing {
					unset = append(unset, here)
				}
			default:
				if al, ok := after.([]any); ok && hasPath(permissionLists, here) {
					// merge() adds canonical rules back; keep only the profile's own.
					canon, canonIsList := getPath(canonical, here).([]any)
					var own []any
					for _, r := range al {
						inCanon := false
						if canonIsList {
							for _, e := range canon {
								if ordjson.Equal(r, e) {
									inCanon = true
									break
								}
							}
						}
						if !inCanon {
							own = append(own, r)
						}
					}
					after = own
					if own == nil {
						after = []any{}
					}
				}
				setPath(layer, here, after)
				kept := unset[:0:0]
				for _, p := range unset {
					if !hasPrefix(p, here) {
						kept = append(kept, p)
					}
				}
				unset = kept
			}
		}
	}
	walk(withoutUsage(withoutAdvise(base)), withoutUsage(withoutAdvise(current)), nil)
	setTombstones(layer, unset)
	return layer
}

func acwValid(v value) bool {
	n, ok := v.(json.Number)
	if !ok {
		return false
	}
	f, err := n.Float64()
	return err == nil && f == math.Trunc(f) && f >= 100_000 && f <= 1_000_000
}

// machineKeys are settings the shared store makes machine-wide: a profile
// layer never overrides them (#185).
var machineKeys = []string{"cleanupPeriodDays"}

func build(canonical, layerIn, saved *obj, strip, mapping map[string]bool, acw *int64, usageHook string) *obj {
	layer := withoutInheritedHooks(layerIn, canonical)
	data := merge(canonical, withoutTombstones(layer), nil)
	applyTombstones(data, layer)
	data.Delete("model")
	data.Delete("effortLevel")
	if ms := getObj(data, "modelSettings"); truthy(ms) {
		for _, k := range asObj(ms, "values").Keys() {
			if row, ok := getObj(ms.(*obj), k).(*obj); ok {
				row.Delete("effortLevel")
			}
		}
	}
	if env, ok := getObj(data, "env").(*obj); ok {
		for _, k := range ownedEnv {
			env.Delete(k)
		}
		for k := range mapping {
			env.Delete(k)
		}
	}
	data = merge(data, saved, nil)
	// #185: session history is shared, so the value that decides how much of
	// it is pruned belongs to the machine alone; a profile layer holding the
	// old per-profile value would prune every profile's history.
	for _, k := range machineKeys {
		if v, ok := canonical.Get(k); ok {
			data.Set(k, ordjson.Clone(v))
		} else {
			data.Delete(k)
		}
	}
	if env, ok := getObj(data, "env").(*obj); ok {
		for k := range strip {
			env.Delete(k)
		}
		if env.Len() == 0 {
			data.Delete("env")
		}
	}
	if acw != nil && !acwValid(getObj(data, "autoCompactWindow")) {
		data.Set("autoCompactWindow", json.Number(strconv.FormatInt(*acw, 10)))
	}
	hv, ok := data.Get("hooks")
	if !ok {
		hv = ordjson.New()
		data.Set("hooks", hv)
	}
	if hooks, ok := hv.(*obj); ok {
		ev, ok := hooks.Get("SessionStart")
		if !ok {
			ev = []any{}
		}
		if entries, ok := ev.([]any); ok {
			hook := ordjson.New()
			hook.Set("type", "command")
			hook.Set("command", adviseCommand)
			entry := ordjson.New()
			entry.Set("hooks", []any{hook})
			hooks.Set("SessionStart", append(entries, entry))
		}
	}
	data = addUsage(data, usageHook)
	return data
}

func backup(profileDir string, content []byte) {
	folder := join(profileDir, backupsDir)
	if err := os.MkdirAll(folder, 0o777); err != nil {
		osFail(err)
	}
	now := time.Now()
	stamp := now.Format("20060102T150405") + fmt.Sprintf(".%06d", now.UnixNano()%1_000_000)
	target := join(folder, "settings."+stamp+".json")
	if err := os.WriteFile(target, content, 0o666); err != nil {
		osFail(err)
	}
	if err := os.Chmod(target, 0o600); err != nil {
		osFail(err)
	}
	old, _ := filepath.Glob(join(folder, "settings.*.json"))
	sort.Strings(old)
	if len(old) > keepBackups {
		for _, stale := range old[:len(old)-keepBackups] {
			os.Remove(stale)
		}
	}
}

func locked(profileDir string, fn func()) {
	path := join(profileDir, lockFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		osFail(err)
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		osFail(&fs.PathError{Op: "open", Path: path, Err: err})
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		osFail(err)
	}
	fn()
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// capture is the profile layer with every edit made to settings.json since it was generated.
func capture(profileDir string, current, canonical *obj, mapping map[string]bool) *obj {
	layerPath, basePath := join(profileDir, layerFile), join(profileDir, baseFile)
	if exists(basePath) || exists(layerPath) {
		layer := load(layerPath)
		if exists(basePath) && current != nil {
			layer = fold(load(basePath), current, layer, canonical, mapping)
		}
		return layer
	}
	// First run: the old settings.json WAS the profile layer.
	if current == nil {
		current = ordjson.New()
	}
	return withoutInheritedHooks(withoutUsage(withoutAdvise(current)), canonical)
}

func readCurrent(target string) ([]byte, *obj) {
	raw := readBytes(target)
	if raw == nil {
		return nil, nil
	}
	doc, ok := parse(stripBOM(raw)).(*obj)
	if !ok {
		valueFail("%s: root is not a JSON object", target)
	}
	return raw, doc
}

func carriesPolicy(profileDir string, policy *obj) (carried bool) {
	if policy.Len() == 0 {
		return true
	}
	// Everything that stops the comparison is "not carried": a launch must not start on a guess.
	if catch(func() { carried = carriesPolicyUnsafe(profileDir, policy) }) != nil {
		return false
	}
	return carried
}

func carriesPolicyUnsafe(profileDir string, policy *obj) bool {
	_, current := readCurrent(join(profileDir, "settings.json"))
	if current == nil {
		return false
	}
	layer := load(join(profileDir, layerFile))
	expected := merge(policy, withoutTombstones(layer), nil)
	applyTombstones(expected, layer)

	var held func(v value, here []string) bool
	held = func(v value, here []string) bool {
		if o, ok := v.(*obj); ok && o.Len() > 0 {
			for _, k := range o.Keys() {
				item, _ := o.Get(k)
				if !held(item, append(append([]string(nil), here...), k)) {
					return false
				}
			}
			return true
		}
		if vl, ok := v.([]any); ok && ((len(here) > 0 && here[0] == "hooks") || hasPath(permissionLists, here)) {
			// Hooks only ever accumulate: a policy's entry is carried when it is still somewhere in the list.
			generated, ok := getPath(current, here).([]any)
			if !ok {
				return false
			}
			for _, entry := range vl {
				found := false
				for _, g := range generated {
					if ordjson.Equal(entry, g) {
						found = true
						break
					}
				}
				if !found {
					return false
				}
			}
			return true
		}
		a, b := getPath(current, here), getPath(expected, here)
		if a == missing || b == missing {
			return a == b
		}
		return ordjson.Equal(a, b)
	}
	return held(policy, nil)
}

func refuseSymlinks(profileDir string) {
	for _, name := range []string{"settings.json", layerFile} {
		if fi, err := os.Lstat(join(profileDir, name)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			valueFail("profile settings must be regular files")
		}
	}
}

func saveLayer(profileDir string, layer *obj) {
	path := join(profileDir, layerFile)
	if !exists(path) || !ordjson.Equal(load(path), layer) {
		writeBytes(path, encode(layer))
	}
}

func foldOnly(profileDir, canonicalPath string, mapping map[string]bool) {
	refuseSymlinks(profileDir)
	locked(profileDir, func() {
		settle(profileDir)
		raw, current := readCurrent(join(profileDir, "settings.json"))
		saveLayer(profileDir, capture(profileDir, current, canonicalFor(profileDir, canonicalPath), mapping))
		// The saved copy now matches the layer, so the same edits are never folded twice.
		if raw != nil && exists(join(profileDir, baseFile)) {
			saveBase(profileDir, raw)
		}
	})
}

func materialize(profileDir, canonicalPath string, saved *obj, strip, mapping map[string]bool, acwArg *string, usageHook string) {
	target := join(profileDir, "settings.json")
	basePath, pending := join(profileDir, baseFile), join(profileDir, pendingFile)
	refuseSymlinks(profileDir)
	done := false
	locked(profileDir, func() {
		settle(profileDir)
		for i := 0; i < 3; i++ {
			currentBytes, current := readCurrent(target)
			canonical := canonicalFor(profileDir, canonicalPath)
			layer := capture(profileDir, current, canonical, mapping)
			var acw *int64
			if acwArg == nil {
				// Keep the budget derived last time; a set one in a layer wins anyway.
				if prev := getObj(load(basePath), "autoCompactWindow"); acwValid(prev) {
					f, _ := prev.(json.Number).Float64()
					v := int64(f)
					acw = &v
				}
			} else if *acwArg != "none" {
				n, _ := strconv.ParseInt(*acwArg, 10, 64)
				acw = &n
			}
			doc := build(canonical, layer, saved, strip, mapping, acw, usageHook)
			// Claude Code may be writing the same file; start over rather than
			// replace an edit that landed while this pass was reading.
			if !bytes.Equal(readBytes(target), currentBytes) || (readBytes(target) == nil) != (currentBytes == nil) {
				continue
			}
			saveLayer(profileDir, layer)
			cur := current
			if cur == nil {
				cur = ordjson.New()
			}
			if ordjson.Equal(doc, cur) && currentBytes != nil {
				saveBase(profileDir, currentBytes)
				done = true
				return
			}
			if currentBytes != nil {
				backup(profileDir, currentBytes)
			}
			data := encode(doc)
			writeBytes(pending, data)
			writeBytes(target, data)
			if err := os.Rename(pending, basePath); err != nil {
				osFail(err)
			}
			done = true
			return
		}
	})
	if !done {
		valueFail("%s kept changing while it was being regenerated", target)
	}
}

// fallbackOrRefuse is the exit code for a projection that failed after saying
// why. fold just fails; a launch may fall back to the settings generated last
// time, but only if they already enforce the linked policy.
func fallbackOrRefuse(command, profileDir string, stderr io.Writer) int {
	if command != "materialize" {
		return 1
	}
	kept := false
	_ = catch(func() { kept = carriesPolicy(profileDir, readPolicyFn(profileDir)) })
	if kept {
		return 1
	}
	fmt.Fprintf(stderr, "settings-compose: %s: the settings generated last time do not carry this policy\n",
		join(profileDir, policyFile))
	return 3
}

type options struct {
	command    string
	profileDir string
	canonical  string
	saved      string
	strip      []string
	mapping    []string
	acw        *string
	usageHook  string
}

func usage(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "usage: settings-compose [-h] {materialize,fold} ...")
	fmt.Fprintf(stderr, "settings-compose: error: %s\n", msg)
	return 2
}

// parseArgs is a small argparse: --opt value, --opt=value, unique prefixes.
func parseArgs(args []string) (*options, string) {
	if len(args) == 0 {
		return nil, "the following arguments are required: command"
	}
	o := &options{command: args[0], saved: "{}"}
	home, _ := os.UserHomeDir()
	o.canonical = pyPath(join(home, ".claude", "settings.json"))
	flags := map[string]bool{"--canonical": true, "--mapping": true, "--usage-hook": true}
	switch o.command {
	case "materialize":
		flags["--saved"], flags["--strip"], flags["--acw"] = true, true, true
	case "fold":
	default:
		return nil, fmt.Sprintf("argument command: invalid choice: '%s' (choose from 'materialize', 'fold')", o.command)
	}
	var positionals []string
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "--" {
			positionals = append(positionals, rest[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") || len(a) == 2 {
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				if a == "-h" {
					return nil, "help"
				}
				if _, err := strconv.ParseFloat(a, 64); err != nil {
					return nil, fmt.Sprintf("unrecognized arguments: %s", a)
				}
			}
			positionals = append(positionals, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		var match []string
		for f := range flags {
			if f == name {
				match = []string{f}
				break
			}
			if strings.HasPrefix(f, name) {
				match = append(match, f)
			}
		}
		if len(match) != 1 {
			if name == "--help" {
				return nil, "help"
			}
			return nil, fmt.Sprintf("unrecognized arguments: %s", a)
		}
		name = match[0]
		if !hasVal {
			if i+1 >= len(rest) || (strings.HasPrefix(rest[i+1], "-") && len(rest[i+1]) > 1 && !isNumber(rest[i+1])) {
				return nil, fmt.Sprintf("argument %s: expected one argument", name)
			}
			i++
			val = rest[i]
		}
		switch name {
		case "--canonical":
			o.canonical = pyPath(val)
		case "--saved":
			o.saved = val
		case "--strip":
			o.strip = append(o.strip, val)
		case "--mapping":
			o.mapping = append(o.mapping, val)
		case "--acw":
			v := val
			o.acw = &v
		case "--usage-hook":
			o.usageHook = val
		}
	}
	if len(positionals) < 1 {
		return nil, "the following arguments are required: profile_dir"
	}
	if len(positionals) > 1 {
		return nil, fmt.Sprintf("unrecognized arguments: %s", strings.Join(positionals[1:], " "))
	}
	o.profileDir = pyPath(positionals[0])
	return o, ""
}

func isNumber(s string) bool { _, err := strconv.ParseFloat(s, 64); return err == nil }

func set(items []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range items {
		m[s] = true
	}
	return m
}

// Main runs `settings-compose <materialize|fold> ...` and returns its exit code.
func Main(args []string, stderr io.Writer) int {
	o, bad := parseArgs(args)
	if bad == "help" {
		fmt.Fprintln(stderr, "usage: settings-compose [-h] {materialize,fold} ...")
		return 0
	}
	if bad != "" {
		return usage(stderr, bad)
	}
	fail := catch(func() {
		if o.command == "fold" {
			foldOnly(o.profileDir, o.canonical, set(o.mapping))
			return
		}
		savedV, err := ordjson.Parse([]byte(o.saved))
		if err != nil {
			valueFail("--saved: %s", err)
		}
		saved, ok := savedV.(*obj)
		if !ok {
			valueFail("--saved must be a JSON object")
		}
		if o.acw != nil && *o.acw != "none" && !isDigits(*o.acw) {
			valueFail("--acw must be a token count or none")
		}
		materialize(o.profileDir, o.canonical, saved, set(o.strip), set(o.mapping), o.acw, o.usageHook)
	})
	if fail == nil {
		return 0
	}
	fmt.Fprintf(stderr, "settings-compose: %s\n", fail.msg)
	if fail.policy {
		return 3
	}
	return fallbackOrRefuse(o.command, o.profileDir, stderr)
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// MergedView is what settings.json would hold for these layers, canon first: the
// merge plus every layer's tombstones, except a shared policy's. An unreadable or
// non-object layer is skipped. It is the one reading of the layers other readers share.
func MergedView(paths []string) *ordjson.Obj {
	data := ordjson.New()
	if catch(func() {
		for _, path := range paths {
			var layer *obj
			if catch(func() {
				b, err := readFile(path)
				if err != nil {
					osFail(err)
				}
				layer, _ = parse(b).(*obj)
			}) != nil || layer == nil {
				continue
			}
			data = merge(data, withoutTombstones(layer), nil)
			if filepath.Base(path) == policyFile {
				continue
			}
			applyTombstones(data, layer)
		}
	}) != nil {
		return ordjson.New() // a layer too malformed to merge reads as no settings at all
	}
	return data
}

// ACWValid reports whether v is an autoCompactWindow the engine's schema keeps.
func ACWValid(v any) bool { return acwValid(v) }
