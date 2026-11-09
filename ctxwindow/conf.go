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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
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
		return filepath.Clean(v)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = home + "/.config"
	}
	return filepath.Join(base, "ccmorph")
}

// ProfilePath is the conf file for a profile (MODEL_WINDOW_PROFILE_CONF overrides).
func ProfilePath(profile string) (string, error) {
	if !ValidProfileName(profile) {
		return "", errors.New("invalid profile name")
	}
	if v := os.Getenv("MODEL_WINDOW_PROFILE_CONF"); v != "" {
		return filepath.Clean(v), nil
	}
	return filepath.Join(StateDir(), profile+".conf"), nil
}

func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// ReadConf returns the conf text and its key=value fields.
func ReadConf(path string) (string, map[string]string, error) {
	if isSymlink(path) {
		return "", nil, errors.New("profile configuration must not be a symlink")
	}
	text, err := readText(path)
	if err != nil {
		return "", nil, err
	}
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
	return text, fields, nil
}

// readText is a file's text with "\r\n" and "\r" read as "\n".
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s is not valid UTF-8", path)
	}
	text := string(b)
	if strings.Contains(text, "\r") {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	}
	return text, nil
}

// DiscoveredPath is where a conf's discovered windows are kept:
// profiles/<name>/models.json beside it, so rm and mv carry it with the profile.
func DiscoveredPath(conf string) string {
	dir, name := filepath.Split(conf)
	return filepath.Join(dir, "profiles", strings.TrimSuffix(name, ".conf"), "models.json")
}

// lockName is the conf's lock file path, kept out of the state root's listing.
func lockName(conf string) string {
	dir, name := filepath.Split(conf)
	return filepath.Join(dir, ".locks", strings.TrimSuffix(name, ".conf")+".lock")
}

// lockPath is lockName with the .locks directory made first.
func lockPath(conf string) (string, error) {
	lock := lockName(conf)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		return "", err
	}
	return lock, nil
}

// heldLock reports whether this process already holds conf's lock: either the
// same conf path is named in CCMORPH_PROFILE_LOCK_HELD, or a named conf's lock
// file and this one are one file on disk (macOS ignores letter case, so
// work2.conf and Work2.conf can share .locks/work2.lock). Taking that file
// again would wait forever on ourselves.
func heldLock(conf string) bool {
	lock, err := os.Stat(lockName(conf))
	for _, held := range strings.Split(os.Getenv("CCMORPH_PROFILE_LOCK_HELD"), ":") {
		if held == "" {
			continue
		}
		if held == conf {
			return true
		}
		if err == nil {
			if other, err := os.Stat(lockName(held)); err == nil && os.SameFile(lock, other) {
				return true
			}
		}
	}
	return false
}

// RouteFingerprint hashes the fields that decide where requests go.
func RouteFingerprint(fields map[string]string) string {
	route := map[string]string{}
	for _, k := range routeFields {
		route[k] = fields[k]
	}
	for k, v := range fields {
		if strings.HasPrefix(k, "env.ANTHROPIC_") && !modelChoiceEnv.MatchString(k) {
			route[k] = v
		}
	}
	// The digest is stored beside every discovered window; these exact bytes keep
	// already-stored routes (and the probes that paid for them) valid.
	var doc strings.Builder
	doc.WriteByte('{')
	for i, k := range slices.Sorted(maps.Keys(route)) {
		if i > 0 {
			doc.WriteString(", ")
		}
		doc.WriteString(asciiJSON(k) + ": " + asciiJSON(route[k]))
	}
	doc.WriteByte('}')
	sum := sha256.Sum256([]byte(doc.String()))
	return hex.EncodeToString(sum[:])
}

// asciiJSON is a JSON string with every non-ASCII character escaped.
func asciiJSON(s string) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	var out strings.Builder
	for _, r := range strings.TrimSuffix(b.String(), "\n") {
		if r < 0x80 {
			out.WriteRune(r)
			continue
		}
		for _, u := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&out, "\\u%04x", u)
		}
	}
	return out.String()
}

// glob is fnmatch's `*` and `?`: a model id may hold "/" and "[1m]", which are plain characters here.
func glob(name, pattern string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return name == pattern
	}
	var re strings.Builder
	re.WriteString("(?s)^")
	for _, r := range pattern {
		switch r {
		case '*':
			re.WriteString(".*")
		case '?':
			re.WriteString(".")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	re.WriteString("$")
	return regexp.MustCompile(re.String()).MatchString(name)
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
	path, err := ProfilePath(profile)
	if err != nil || isSymlink(path) {
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
		if unicode.IsSpace(c) || (c >= 0x1c && c <= 0x1f) || c == '=' {
			return false
		}
	}
	return true
}

func discoveredDoc(fields map[string]string) obj {
	text, ok := fields["context_discovered"]
	if !ok {
		text = "{}"
	}
	v, err := parseJSON([]byte(text))
	if err != nil {
		return nil
	}
	doc, ok := v.(obj)
	if !ok {
		return nil
	}
	if route := doc["route"]; route != RouteFingerprint(fields) {
		return nil
	}
	return doc
}

func posInt(v any) (int64, bool) {
	i, ok := anyInt(v)
	return i, ok && i > 0 // shortcut: a window past int64 is dropped, none is real
}

func subObj(doc obj, key string) obj {
	if doc == nil {
		return nil
	}
	v := doc[key]
	o, _ := v.(obj)
	return o
}

// DiscoveredValues is the route-bound window table, in stored order.
func DiscoveredValues(fields map[string]string) map[string]int64 {
	out := map[string]int64{}
	models := subObj(discoveredDoc(fields), "models")
	if models == nil {
		return out
	}
	for _, k := range slices.Sorted(maps.Keys(models)) {
		v := models[k]
		if n, ok := posInt(v); ok && ValidModel(k) {
			out[k] = n
		}
	}
	return out
}

// DiscoveredSources is the provenance of windows filled by something other
// than a live probe or declared listing.
func DiscoveredSources(fields map[string]string) map[string]obj {
	out := map[string]obj{}
	sources := subObj(discoveredDoc(fields), "sources")
	if sources == nil {
		return out
	}
	for _, k := range slices.Sorted(maps.Keys(sources)) {
		v := sources[k]
		entry, ok := v.(obj)
		if !ok || !ValidModel(k) {
			continue
		}
		if s := entry["source"]; isString(s) {
			out[k] = entry
		}
	}
	return out
}

func isString(v any) bool { _, ok := v.(string); return ok }

// DiscoveredMisses are models no lookup could size, with the day it last tried.
func DiscoveredMisses(fields map[string]string) map[string]string {
	out := map[string]string{}
	misses := subObj(discoveredDoc(fields), "misses")
	if misses == nil {
		return out
	}
	for _, k := range slices.Sorted(maps.Keys(misses)) {
		v := misses[k]
		if day, ok := v.(string); ok && ValidModel(k) {
			out[k] = day
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
	v, err := parseJSON([]byte(text))
	if err != nil {
		return bare
	}
	mapping, ok := v.(obj)
	if !ok {
		return bare
	}
	target := mapping[bare]
	s, ok := target.(string)
	if !ok || s == "" {
		return bare
	}
	return strings.TrimSuffix(s, "[1m]")
}

// ProfileWindows is the profile's discovered and manual windows, manual winning.
func ProfileWindows(profile string) (map[string]int64, error) {
	path, err := ProfilePath(profile)
	if err != nil {
		return nil, err
	}
	_, fields, err := ReadConf(path)
	if err != nil { // an unreadable conf has no windows
		return map[string]int64{}, nil
	}
	out := DiscoveredValues(fields)
	maps.Copy(out, ManualValues(fields))
	return out, nil
}

// ProfileLock runs fn holding the profile's lock; a caller that already holds
// it (CCMORPH_PROFILE_LOCK_HELD) passes straight through.
func ProfileLock(profile string, fn func() error) error {
	_, err := ProfileLockCtx(context.Background(), profile, fn)
	return err
}

// ProfileLockCtx is ProfileLock for a caller with a deadline: it stops waiting
// at ctx.Done and reports whether fn ran. A lock held by another ccmorph must
// not make a run outlive the deadline that asked it to write.
func ProfileLockCtx(ctx context.Context, profile string, fn func() error) (bool, error) {
	path, err := ProfilePath(profile)
	if err != nil {
		return false, err
	}
	if heldLock(path) {
		return true, fn()
	}
	// The lock holds no configuration data; its inode stays stable across remove/recreate.
	lock, err := lockPath(path)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if err := flockCtx(ctx, f); err != nil {
		if ctx.Err() != nil {
			return false, nil
		}
		return false, err
	}
	return true, fn()
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
	{"private", "usage_route", "env.*"},
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
			if glob(key, pattern) {
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
func ReplaceFields(path string, fields []Field, expected *Identity) (bool, error) {
	text, _, err := ReadConf(path)
	if err != nil {
		return false, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
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
		return true, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".context-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.WriteString(updated); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if expected != nil {
		// Also protects against non-locking editors before the atomic replace.
		cur, err := os.Open(path)
		if err != nil {
			return false, err
		}
		now, err := identityOf(cur)
		cur.Close()
		if err != nil {
			return false, err
		}
		now.models = expected.models // ReplaceFields guards the conf only
		if *now != *expected {
			return false, nil
		}
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// SaveDiscovered merges values (and their provenance and misses) into the
// profile's context_discovered record. sources tags entries that came from
// something other than a live probe or declared listing; a value written
// without a tag retires any catalogue tag the model carried. It returns false
// when the conf changed since expected was read.
func SaveDiscovered(profile string, values map[string]int64, expected *Identity, overwrite bool,
	sources map[string]obj, misses map[string]string) (bool, error) {
	return SaveDiscoveredCtx(context.Background(), profile, values, expected, overwrite, sources, misses)
}

// SaveDiscoveredCtx is SaveDiscovered under a caller's deadline: when the
// profile lock is still held elsewhere at ctx.Done it writes nothing and
// returns false.
func SaveDiscoveredCtx(ctx context.Context, profile string, values map[string]int64, expected *Identity, overwrite bool,
	sources map[string]obj, misses map[string]string) (saved bool, err error) {
	if expected == nil {
		return false, nil
	}
	ran, err := ProfileLockCtx(ctx, profile, func() error {
		if got := ProfileIdentity(profile); got == nil || *got != *expected {
			return nil
		}
		path, err := ProfilePath(profile)
		if err != nil {
			return err
		}
		_, fields, err := ReadConf(path)
		if err != nil {
			return err
		}
		merged := DiscoveredValues(fields)
		mergedSources := DiscoveredSources(fields)
		valid := map[string]int64{}
		for m, v := range values {
			if ValidModel(m) && v > 0 {
				valid[m] = v
			}
		}
		tags := map[string]obj{}
		for m, t := range sources {
			if _, ok := valid[m]; ok {
				tags[m] = t
			}
		}
		if overwrite {
			maps.Copy(merged, valid)
			maps.Copy(mergedSources, tags)
			for m := range valid {
				if _, ok := tags[m]; !ok {
					delete(mergedSources, m) // a fresh unsourced write is a real value
				}
			}
		} else {
			maps.Copy(valid, merged) // a stored value outranks a new one
			merged = valid
			maps.Copy(tags, mergedSources)
			mergedSources = tags
		}
		record := obj{"route": RouteFingerprint(fields), "models": merged}
		kept := obj{}
		for m, t := range mergedSources {
			if _, ok := merged[m]; ok {
				kept[m] = t
			}
		}
		if len(kept) > 0 {
			record["sources"] = kept
		}
		keptMisses := obj{}
		allMisses := DiscoveredMisses(fields)
		maps.Copy(allMisses, misses)
		for m, d := range allMisses {
			if _, ok := merged[m]; !ok { // any value for the model retires its miss
				keptMisses[m] = d
			}
		}
		if len(keptMisses) > 0 {
			record["misses"] = keptMisses
		}
		saved, err = writeDiscovered(path, record, expected)
		return err
	})
	if err != nil || !ran {
		return false, err
	}
	return saved, nil
}

// writeDiscovered stores record as indented JSON in DiscoveredPath and drops any
// inline context_discovered line the conf still carries. The caller holds the
// profile lock and has checked expected.
func writeDiscovered(conf string, record obj, expected *Identity) (bool, error) {
	text, _, err := ReadConf(conf)
	if err != nil {
		return false, err
	}
	inline := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "context_discovered=") {
			inline = true
		}
	}
	target := DiscoveredPath(conf)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".models-")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	b, _ := json.MarshalIndent(record, "", "  ")
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return false, err
	}
	if !inline {
		return true, nil
	}
	return ReplaceFields(conf, []Field{{Key: "context_discovered"}}, expected)
}

// Restamp rebinds conf's models.json from route old to route new, for a conf
// rewrite that changed only how the route is spelled.
func Restamp(conf, old, new string) {
	path := DiscoveredPath(conf)
	data, err := os.ReadFile(path)
	if err != nil || old == new {
		return
	}
	var doc map[string]any
	if json.Unmarshal(data, &doc) != nil || doc["route"] != old {
		return
	}
	doc["route"] = new
	b, _ := json.MarshalIndent(doc, "", "  ")
	tmp := path + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o600) == nil {
		os.Rename(tmp, path)
	}
}

// ContextValue is the manual window for model, or "auto".
func ContextValue(profile, model string) (string, error) {
	if !ValidModel(model) {
		return "", errors.New("context model id is invalid")
	}
	path, err := ProfilePath(profile)
	if err != nil {
		return "", err
	}
	_, fields, err := ReadConf(path)
	if err != nil {
		return "", err
	}
	if v, ok := ManualValues(fields)[model]; ok {
		return strconv.FormatInt(v, 10), nil
	}
	return "auto", nil
}

// SetContext pins model's window, or clears the pin with "auto".
func SetContext(profile, model, value string) error {
	if !ValidModel(model) {
		return errors.New("context model id is invalid")
	}
	parsed, ok := Positive(value)
	if value != "auto" && !ok {
		return errors.New("context tokens must be a positive integer (at most 10 digits) or auto")
	}
	return ProfileLock(profile, func() error {
		expected := ProfileIdentity(profile)
		path, err := ProfilePath(profile)
		if err != nil {
			return err
		}
		_, fields, err := ReadConf(path)
		if err != nil {
			return err
		}
		switch fields["type"] {
		case "apikey", "proxy", "provider":
		default:
			return errors.New("context overrides apply to routed profiles")
		}
		f := Field{Key: "context_window." + model}
		if ok {
			f.Value = str(strconv.FormatInt(parsed, 10))
		}
		replaced, err := ReplaceFields(path, []Field{f}, expected)
		if err == nil && !replaced {
			err = errors.New("profile changed while setting context; retry")
		}
		return err
	})
}
