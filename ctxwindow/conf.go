// Package ctxwindow is a model's context window: finding it and storing it. It
// backs `ccmorph-core model-window` (WindowMain), which resolves and stores
// windows in <profile>.conf, and `ccmorph-core context-probe` (ProbeMain), which
// discovers them. Manual context_window.<model> entries beat context_discovered
// metadata bound to the profile's route. Resolvers and setters send no network
// request; discovery reads gateway-declared limits from /v1/models, falls back
// to the models.dev catalogue, and runs paid probes only when the operator passes
// --allow-inference for one profile and model.
package ctxwindow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/pycompat"
)

const (
	// CCDefaultWindow is Claude Code's own window for a model nothing sized.
	CCDefaultWindow = 200_000
	// UnknownBudget is the compaction budget for a routed model nothing could
	// size: Claude Code's 200,000 sits above real walls, and compacting early on a
	// big model is recoverable.
	UnknownBudget = 128_000
	oneM          = 1_000_000
)

// DeclaredFields are the listing fields a gateway may use to state a window.
var DeclaredFields = []string{"max_input_tokens", "max_context_length", "context_length", "context_window"}

var routeFields = []string{"type", "base_url", "provider", "adapter", "api_key", "token"}

var (
	profileName = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	// Env keys that choose which model answers, not where requests go.
	modelChoiceEnv = regexp.MustCompile(`^(?:env\.ANTHROPIC_(?:DEFAULT_[A-Z]+_MODEL(?:_NAME|_DESCRIPTION)?|SMALL_FAST_MODEL|MODEL))$`)
)

// ValidProfileName reports whether s may name a profile.
func ValidProfileName(s string) bool { return profileName.MatchString(s) }

// StateDir is where ccmorph keeps profiles and the registry.
func StateDir() string {
	if v := os.Getenv("CCMORPH_STATE_DIR"); v != "" {
		return pycompat.Path(v)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = home + "/.config"
	}
	return pycompat.Join(pycompat.Path(base), "ccmorph")
}

// ProfilePath is the conf file for a profile (MODEL_WINDOW_PROFILE_CONF overrides).
func ProfilePath(profile string) string {
	if !ValidProfileName(profile) {
		pycompat.RaiseValue("invalid profile name")
	}
	if v := os.Getenv("MODEL_WINDOW_PROFILE_CONF"); v != "" {
		return pycompat.Path(v)
	}
	return pycompat.Join(StateDir(), profile+".conf")
}

func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// ReadConf returns the conf text and its key=value fields.
func ReadConf(path string) (string, map[string]string) {
	if isSymlink(path) {
		pycompat.RaiseOSMsg("profile configuration must not be a symlink")
	}
	text := pycompat.ReadText(path)
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.Trim(line, " \t\r"), "=")
		if ok && !strings.HasPrefix(key, "#") {
			fields[key] = value
		}
	}
	// Machine-written windows live beside the profile; a conf that still carries
	// the old inline record wins until the next save moves it out.
	if _, inline := fields["context_discovered"]; !inline {
		if data, err := os.ReadFile(DiscoveredPath(path)); err == nil {
			fields["context_discovered"] = string(data)
		}
	}
	return text, fields
}

// DiscoveredPath is where a conf's discovered windows are kept:
// profiles/<name>/models.json beside it, so rm and mv carry it with the profile.
func DiscoveredPath(conf string) string {
	dir, name := filepath.Split(conf)
	return filepath.Join(dir, "profiles", strings.TrimSuffix(name, ".conf"), "models.json")
}

// lockPath is the conf's lock file, kept out of the state root's listing.
func lockPath(conf string) string {
	dir, name := filepath.Split(conf)
	if err := os.MkdirAll(filepath.Join(dir, ".locks"), 0o700); err != nil {
		pycompat.RaiseOS(err)
	}
	return filepath.Join(dir, ".locks", strings.TrimSuffix(name, ".conf")+".lock")
}

// RouteFingerprint hashes the fields that decide where requests go.
func RouteFingerprint(fields map[string]string) string {
	route := pycompat.NewObj()
	for _, k := range routeFields {
		route.Set(k, fields[k])
	}
	for k, v := range fields {
		if strings.HasPrefix(k, "env.ANTHROPIC_") && !modelChoiceEnv.MatchString(k) {
			route.Set(k, v)
		}
	}
	sum := sha256.Sum256([]byte(pycompat.Marshal(route, pycompat.Format{SortKeys: true, ASCII: true})))
	return hex.EncodeToString(sum[:])
}

// Identity pins a conf file's inode, mtime and content so a writer can tell
// the file changed under it.
type Identity struct {
	dev, ino    uint64
	mtimeNs     int64
	sum, models [32]byte
}

func identityOf(f *os.File) (*Identity, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	id := &Identity{mtimeNs: fi.ModTime().UnixNano(), sum: sha256.Sum256(data)}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), uint64(st.Ino) //nolint: portable across the unix targets
	}
	return id, nil
}

// ProfileIdentity is nil for a symlink or an unreadable conf. It also pins the
// profile's models.json, so a writer sees a discovery saved since it read.
func ProfileIdentity(profile string) *Identity {
	path := ProfilePath(profile)
	if isSymlink(path) {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	id, err := identityOf(f)
	if err != nil {
		return nil
	}
	if data, err := os.ReadFile(DiscoveredPath(path)); err == nil {
		id.models = sha256.Sum256(data)
	}
	return id
}

// Positive is positive(value) for a string: up to ten ASCII digits, above zero.
func Positive(value string) (int64, bool) {
	if value == "" || len(value) > 10 {
		return 0, false
	}
	var n int64
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, n > 0
}

// RealID undoes the translator disguise: claude-<cover>-dd-<reversed id> -> the true id.
func RealID(modelID string) string {
	head, tail, ok := strings.Cut(modelID, "-dd-")
	if ok && strings.HasPrefix(head, "claude-") && tail != "" {
		r := []rune(tail)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		return string(r)
	}
	return modelID
}

var oneMSuffix = regexp.MustCompile(`(?i)\[1m\]$`)

// IsExcluded reports whether excluded_models names this model, directly or
// through the tier alias it maps.
func IsExcluded(fields map[string]string, modelID string) bool {
	excluded := map[string]bool{}
	for _, m := range strings.Split(fields["excluded_models"], ",") {
		if m != "" {
			excluded[m] = true
		}
	}
	bare := oneMSuffix.ReplaceAllString(RealID(modelID), "")
	mapped, ok := fields["env.ANTHROPIC_DEFAULT_"+strings.ToUpper(bare)+"_MODEL"]
	if !ok {
		mapped = bare
	}
	return excluded[bare] || excluded[oneMSuffix.ReplaceAllString(mapped, "")]
}

// ValidModel rejects empty ids and ids with whitespace or "=".
func ValidModel(model string) bool {
	if model == "" {
		return false
	}
	for _, c := range model {
		if pycompat.IsSpace(c) || c == '=' {
			return false
		}
	}
	return true
}

func discoveredDoc(fields map[string]string) *pycompat.Obj {
	text, ok := fields["context_discovered"]
	if !ok {
		text = "{}"
	}
	v, err := pycompat.Parse([]byte(text))
	if err != nil {
		if _, isSyntax := err.(*pycompat.SyntaxError); isSyntax {
			return nil
		}
		panic(err)
	}
	doc, ok := v.(*pycompat.Obj)
	if !ok {
		return nil
	}
	if route, _ := doc.Get("route"); route != RouteFingerprint(fields) {
		return nil
	}
	return doc
}

func posInt(v pycompat.Value) (int64, bool) {
	n, ok := v.(pycompat.Num)
	if !ok {
		return 0, false
	}
	i, ok := n.Int64()
	return i, ok && i > 0 // shortcut: a window past int64 is dropped, none is real
}

func subObj(doc *pycompat.Obj, key string) *pycompat.Obj {
	if doc == nil {
		return nil
	}
	v, _ := doc.Get(key)
	o, _ := v.(*pycompat.Obj)
	return o
}

// DiscoveredValues is the route-bound window table, in stored order.
func DiscoveredValues(fields map[string]string) *pycompat.OMap[int64] {
	out := pycompat.NewOMap[int64]()
	models := subObj(discoveredDoc(fields), "models")
	if models == nil {
		return out
	}
	for _, k := range models.Keys() {
		v, _ := models.Get(k)
		if n, ok := posInt(v); ok && ValidModel(k) {
			out.Set(k, n)
		}
	}
	return out
}

// DiscoveredSources is the provenance of windows filled by something other
// than a live probe or declared listing.
func DiscoveredSources(fields map[string]string) *pycompat.OMap[*pycompat.Obj] {
	out := pycompat.NewOMap[*pycompat.Obj]()
	sources := subObj(discoveredDoc(fields), "sources")
	if sources == nil {
		return out
	}
	for _, k := range sources.Keys() {
		v, _ := sources.Get(k)
		entry, ok := v.(*pycompat.Obj)
		if !ok || !ValidModel(k) {
			continue
		}
		if s, _ := entry.Get("source"); isString(s) {
			out.Set(k, entry)
		}
	}
	return out
}

func isString(v pycompat.Value) bool { _, ok := v.(string); return ok }

// DiscoveredMisses are models no lookup could size, with the day it last tried.
func DiscoveredMisses(fields map[string]string) *pycompat.OMap[string] {
	out := pycompat.NewOMap[string]()
	misses := subObj(discoveredDoc(fields), "misses")
	if misses == nil {
		return out
	}
	for _, k := range misses.Keys() {
		v, _ := misses.Get(k)
		if day, ok := v.(string); ok && ValidModel(k) {
			out.Set(k, day)
		}
	}
	return out
}

// ManualValues are the user's context_window.<model> pins.
func ManualValues(fields map[string]string) map[string]int64 {
	const prefix = "context_window."
	out := map[string]int64{}
	for k, v := range fields {
		if !strings.HasPrefix(k, prefix) || !ValidModel(k[len(prefix):]) {
			continue
		}
		if n, ok := Positive(v); ok {
			out[k[len(prefix):]] = n
		}
	}
	return out
}

// MappedTarget is the id the shim serves for bare, or bare when nothing rewrites
// it: the map's target is final, so it sizes the session.
func MappedTarget(fields map[string]string, bare string) string {
	base := strings.TrimRight(fields["base_url"], "/")
	if base == "" || base == "https://api.anthropic.com" {
		return bare
	}
	text := fields["model_map"]
	if text == "" {
		text = "{}"
	}
	v, err := pycompat.Parse([]byte(text))
	if err != nil {
		if _, ok := err.(*pycompat.SyntaxError); ok {
			return bare
		}
		panic(err)
	}
	mapping, ok := v.(*pycompat.Obj)
	if !ok {
		return bare
	}
	target, _ := mapping.Get(bare)
	s, ok := target.(string)
	if !ok || s == "" {
		return bare
	}
	return strings.TrimSuffix(s, "[1m]")
}

// ProfileWindows is the profile's discovered and manual windows, manual winning.
func ProfileWindows(profile string) map[string]int64 {
	var fields map[string]string
	if e := pycompat.Catch(func() { _, fields = ReadConf(ProfilePath(profile)) }); e != nil {
		if e.Kind == pycompat.OSError {
			return map[string]int64{}
		}
		panic(e)
	}
	out := map[string]int64{}
	d := DiscoveredValues(fields)
	for _, k := range d.Keys() {
		out[k], _ = d.Get(k)
	}
	for k, v := range ManualValues(fields) {
		out[k] = v
	}
	return out
}

// ProfileLock runs fn holding the profile's lock; a caller that already holds
// it (CCMORPH_PROFILE_LOCK_HELD) passes straight through.
func ProfileLock(profile string, fn func()) { ProfileLockCtx(context.Background(), profile, fn) }

// ProfileLockCtx is ProfileLock for a caller with a deadline: it stops waiting
// at ctx.Done and reports whether fn ran. A lock held by another ccmorph must
// not make a run outlive the deadline that asked it to write.
func ProfileLockCtx(ctx context.Context, profile string, fn func()) bool {
	path := ProfilePath(profile)
	for _, held := range strings.Split(os.Getenv("CCMORPH_PROFILE_LOCK_HELD"), ":") {
		if held == path {
			fn()
			return true
		}
	}
	// The lock holds no configuration data; its inode stays stable across remove/recreate.
	f, err := os.OpenFile(lockPath(path), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		pycompat.RaiseOS(err)
	}
	defer f.Close()
	if err := flockCtx(ctx, f); err != nil {
		if ctx.Err() != nil {
			return false
		}
		pycompat.RaiseOS(err)
	}
	fn()
	return true
}

// flockCtx takes an exclusive flock, polling only when the caller has a
// deadline to honour: without one it blocks exactly as before.
func flockCtx(ctx context.Context, f *os.File) error {
	if ctx.Done() == nil {
		return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if err != syscall.EWOULDBLOCK {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Canonical layout of a <profile>.conf: connection, model choice, behaviour,
// context, then machine-written state. A comment block travels with the key
// line below it.
var confSections = [][]string{
	{"type", "provider", "email", "token", "base_url", "api_key", "adapter", "tag"},
	{"model", "model_map", "effort", "effort.*",
		"env.ANTHROPIC_DEFAULT_FABLE_MODEL", "env.ANTHROPIC_DEFAULT_OPUS_MODEL*",
		"env.ANTHROPIC_DEFAULT_SONNET_MODEL", "env.ANTHROPIC_DEFAULT_HAIKU_MODEL",
		"env.ANTHROPIC_SMALL_FAST_MODEL", "env.CLAUDE_CODE_SUBAGENT_MODEL", "excluded_models"},
	{"private", "env.*"},
	{"window_sources", "context_window.*"},
	{"model_defaults", "context_discovered"},
}

type rank struct {
	section, position int
	key               string
}

func confRank(key string) rank {
	for s, patterns := range confSections {
		for p, pattern := range patterns {
			if pycompat.Fnmatch(key, pattern) {
				r := rank{section: s, position: p}
				if strings.Contains(pattern, "*") {
					r.key = key
				}
				return r
			}
		}
	}
	return rank{section: len(confSections)}
}

func (a rank) less(b rank) bool {
	if a.section != b.section {
		return a.section < b.section
	}
	if a.position != b.position {
		return a.position < b.position
	}
	return a.key < b.key
}

// CanonicalConf lays out conf text in canonical order.
func CanonicalConf(text string) string {
	type entry struct {
		rank  rank
		lines []string
	}
	var entries []entry
	var pending []string
	for _, line := range strings.Split(text, "\n") {
		bare := strings.Trim(line, " \t\r")
		if bare == "" {
			continue
		}
		if strings.HasPrefix(bare, "#") {
			pending = append(pending, line)
			continue
		}
		key, _, _ := strings.Cut(bare, "=")
		entries = append(entries, entry{confRank(key), append(pending, line)})
		pending = nil
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].rank.less(entries[j].rank) })
	var out []string
	last := -1
	for _, e := range entries {
		if last >= 0 && e.rank.section != last {
			out = append(out, "")
		}
		out = append(out, e.lines...)
		last = e.rank.section
	}
	if len(pending) > 0 {
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, pending...)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// Field is one key to write; a nil Value removes the key.
type Field struct {
	Key   string
	Value *string
}

func str(s string) *string { return &s }

// ReplaceFields rewrites path with the given fields and lays it out canonically.
// With expected, it gives up (false) if the file changed since.
func ReplaceFields(path string, fields []Field, expected *Identity) bool {
	text, _ := ReadConf(path)
	fi, err := os.Stat(path)
	if err != nil {
		pycompat.RaiseOS(err)
	}
	mode := fi.Mode().Perm() | fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky)
	names := map[string]bool{}
	for _, f := range fields {
		names[f.Key] = true
	}
	var rows []string
	for _, line := range strings.Split(text, "\n") {
		key, _, _ := strings.Cut(strings.Trim(line, " \t\r"), "=")
		if !names[key] {
			rows = append(rows, line)
		}
	}
	updated := strings.Join(rows, "\n")
	for _, f := range fields {
		if f.Value != nil {
			if updated != "" && !strings.HasSuffix(updated, "\n") {
				updated += "\n"
			}
			updated += f.Key + "=" + *f.Value + "\n"
		}
	}
	updated = CanonicalConf(updated)
	if updated == text {
		return true
	}
	dir := path[:strings.LastIndex(path, "/")+1]
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".context-")
	if err != nil {
		pycompat.RaiseOS(err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		pycompat.RaiseOS(err)
	}
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		pycompat.RaiseOS(err)
	}
	if err := tmp.Close(); err != nil {
		pycompat.RaiseOS(err)
	}
	if expected != nil {
		// Also protects against non-locking editors before the atomic replace.
		cur, err := os.Open(path)
		if err != nil {
			pycompat.RaiseOS(err)
		}
		now, err := identityOf(cur)
		cur.Close()
		if err != nil {
			pycompat.RaiseOS(err)
		}
		now.models = expected.models // ReplaceFields guards the conf only
		if *now != *expected {
			return false
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		pycompat.RaiseOS(err)
	}
	return true
}

// SaveDiscovered merges values (and their provenance and misses) into the
// profile's context_discovered record. sources tags entries that came from
// something other than a live probe or declared listing; a value written
// without a tag retires any catalogue tag the model carried. It returns false
// when the conf changed since expected was read.
func SaveDiscovered(profile string, values *pycompat.OMap[int64], expected *Identity, overwrite bool,
	sources *pycompat.OMap[*pycompat.Obj], misses *pycompat.OMap[string]) bool {
	return SaveDiscoveredCtx(context.Background(), profile, values, expected, overwrite, sources, misses)
}

// SaveDiscoveredCtx is SaveDiscovered under a caller's deadline: when the
// profile lock is still held elsewhere at ctx.Done it writes nothing and
// returns false.
func SaveDiscoveredCtx(ctx context.Context, profile string, values *pycompat.OMap[int64], expected *Identity, overwrite bool,
	sources *pycompat.OMap[*pycompat.Obj], misses *pycompat.OMap[string]) (saved bool) {
	if expected == nil {
		return false
	}
	if !ProfileLockCtx(ctx, profile, func() {
		if got := ProfileIdentity(profile); got == nil || *got != *expected {
			return
		}
		path := ProfilePath(profile)
		_, fields := ReadConf(path)
		merged := DiscoveredValues(fields)
		mergedSources := DiscoveredSources(fields)
		valid := pycompat.NewOMap[int64]()
		for _, m := range values.Keys() {
			if v, _ := values.Get(m); ValidModel(m) && v > 0 {
				valid.Set(m, v)
			}
		}
		tags := pycompat.NewOMap[*pycompat.Obj]()
		if sources != nil {
			for _, m := range sources.Keys() {
				if valid.Has(m) {
					t, _ := sources.Get(m)
					tags.Set(m, t)
				}
			}
		}
		if overwrite {
			merged = pycompat.Merge(merged, valid)
			mergedSources = pycompat.Merge(mergedSources, tags)
			for _, m := range valid.Keys() {
				if !tags.Has(m) {
					mergedSources.Delete(m) // a fresh unsourced write is a real value
				}
			}
		} else {
			merged = pycompat.Merge(valid, merged)
			mergedSources = pycompat.Merge(tags, mergedSources)
		}
		kept := pycompat.NewOMap[*pycompat.Obj]()
		for _, m := range mergedSources.Keys() {
			if merged.Has(m) {
				t, _ := mergedSources.Get(m)
				kept.Set(m, t)
			}
		}
		allMisses := DiscoveredMisses(fields)
		if misses != nil {
			allMisses = pycompat.Merge(allMisses, misses)
		}
		keptMisses := pycompat.NewOMap[string]()
		for _, m := range allMisses.Keys() {
			if !merged.Has(m) { // any value for the model retires its miss
				d, _ := allMisses.Get(m)
				keptMisses.Set(m, d)
			}
		}
		record := pycompat.NewObj()
		record.Set("route", RouteFingerprint(fields))
		models := pycompat.NewObj()
		for _, m := range merged.Keys() {
			v, _ := merged.Get(m)
			models.Set(m, pycompat.Int(v))
		}
		record.Set("models", models)
		if kept.Len() > 0 {
			o := pycompat.NewObj()
			for _, m := range kept.Keys() {
				t, _ := kept.Get(m)
				o.Set(m, t)
			}
			record.Set("sources", o)
		}
		if keptMisses.Len() > 0 {
			o := pycompat.NewObj()
			for _, m := range keptMisses.Keys() {
				d, _ := keptMisses.Get(m)
				o.Set(m, d)
			}
			record.Set("misses", o)
		}
		saved = writeDiscovered(path, record, expected)
	}) {
		return false
	}
	return saved
}

// writeDiscovered stores record as indented JSON in DiscoveredPath and drops any
// inline context_discovered line the conf still carries. The caller holds the
// profile lock and has checked expected.
func writeDiscovered(conf string, record *pycompat.Obj, expected *Identity) bool {
	text, _ := ReadConf(conf)
	inline := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "context_discovered=") {
			inline = true
		}
	}
	target := DiscoveredPath(conf)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		pycompat.RaiseOS(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".models-")
	if err != nil {
		pycompat.RaiseOS(err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(pycompat.Marshal(record, pycompat.Format{Indent: 2, ASCII: true}) + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		pycompat.RaiseOS(err)
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		pycompat.RaiseOS(err)
	}
	return !inline || ReplaceFields(conf, []Field{{Key: "context_discovered"}}, expected)
}

// Restamp rebinds conf's models.json from route old to route new, for a conf
// rewrite that changed only how the route is spelled.
func Restamp(conf, old, new string) {
	path := DiscoveredPath(conf)
	data, err := os.ReadFile(path)
	if err != nil || old == new {
		return
	}
	v, err := pycompat.Parse(data)
	doc, ok := v.(*pycompat.Obj)
	if err != nil || !ok {
		return
	}
	if route, _ := doc.Get("route"); route != old {
		return
	}
	doc.Set("route", new)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, []byte(pycompat.Marshal(doc, pycompat.Format{Indent: 2, ASCII: true})+"\n"), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// ContextValue is the manual window for model, or "auto".
func ContextValue(profile, model string) string {
	if !ValidModel(model) {
		pycompat.RaiseValue("context model id is invalid")
	}
	_, fields := ReadConf(ProfilePath(profile))
	if v, ok := ManualValues(fields)[model]; ok {
		return itoa(v)
	}
	return "auto"
}

func itoa(n int64) string { return pycompat.Marshal(pycompat.Int(n), pycompat.Format{}) }

// SetContext pins model's window, or clears the pin with "auto".
func SetContext(profile, model, value string) {
	if !ValidModel(model) {
		pycompat.RaiseValue("context model id is invalid")
	}
	parsed, ok := Positive(value)
	if value != "auto" && !ok {
		pycompat.RaiseValue("context tokens must be a positive integer (at most 10 digits) or auto")
	}
	ProfileLock(profile, func() {
		expected := ProfileIdentity(profile)
		path := ProfilePath(profile)
		_, fields := ReadConf(path)
		switch fields["type"] {
		case "apikey", "proxy", "provider":
		default:
			pycompat.RaiseValue("context overrides apply to routed profiles")
		}
		f := Field{Key: "context_window." + model}
		if ok {
			f.Value = str(itoa(parsed))
		}
		if !ReplaceFields(path, []Field{f}, expected) {
			pycompat.RaiseOSMsg("profile changed while setting context; retry")
		}
	})
}
