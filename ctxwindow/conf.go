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

	"github.com/akleizor/ccmorph/util/paths"
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
	return paths.ProfileRoot()
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

// ReadConf returns the conf text and its key=value fields.
func ReadConf(path string) (string, map[string]string, error) {
	text, err := confText(path)
	if err != nil {
		return "", nil, err
	}
	fields := parseConf(text)
	// Machine-written windows live beside the profile; a conf that still carries
	// the old inline record wins until the next save moves it out.
	if _, inline := fields["context_discovered"]; !inline {
		if data, err := os.ReadFile(DiscoveredPath(path)); err == nil {
			fields["context_discovered"] = string(data)
		}
	}
	return text, fields, nil
}

// confText is a conf's text alone: no symlink, readable, valid UTF-8. Callers
// that need no sidecar merge read this instead of ReadConf.
func confText(path string) (string, error) {
	if paths.IsSymlink(path) {
		return "", errors.New("profile configuration must not be a symlink")
	}
	return readText(path)
}

// readText is a file's text with "\r\n" and "\r" read as "\n".
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return textOf(b, path)
}

// textOf is bytes as a conf's text with "\r\n" and "\r" read as "\n".
var textOf = func(b []byte, path string) (string, error) {
	if !utf8.Valid(b) {
		return "", fmt.Errorf("%s is not valid UTF-8", path)
	}
	text := string(b)
	if strings.Contains(text, "\r") {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	}
	return text, nil
}

// parseConf is conf text as key=value fields.
func parseConf(text string) map[string]string {
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(strings.Trim(line, " \t\r"), "=")
		if ok && !strings.HasPrefix(key, "#") {
			fields[key] = value
		}
	}
	return fields
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

func identityOf(f *os.File) (*Identity, []byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, nil, err
	}
	id := &Identity{mtimeNs: fi.ModTime().UnixNano(), sum: sha256.Sum256(data)}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		id.dev, id.ino = uint64(st.Dev), uint64(st.Ino) //nolint: portable across the unix targets
	}
	return id, data, nil
}

// Snapshot is one read of a profile: the conf's identity, text and fields, with
// the sidecar hashed into the identity and its record merged into the fields.
// A caller that needs all three builds this instead of reading the files again.
type Snapshot struct {
	ID     *Identity
	Text   string
	Fields map[string]string
}

// profileSnapshot reads a profile's conf and sidecar once. A symlink or an
// unreadable conf is (nil, ReadConf's error); text the conf cannot carry (invalid
// UTF-8) still pins the identity, but is an error, like ReadConf.
func profileSnapshot(profile string) (*Snapshot, error) {
	path, err := ProfilePath(profile)
	if err != nil {
		return nil, err
	}
	return snapshotOf(path)
}

// snapshotOf is profileSnapshot over a conf path the caller already holds, as
// confScan hands out: the same read, without the profile-name lookup.
func snapshotOf(path string) (*Snapshot, error) {
	if paths.IsSymlink(path) {
		return nil, errors.New("profile configuration must not be a symlink")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	id, data, err := identityOf(f)
	if err != nil {
		return nil, err
	}
	// The sidecar is hashed whatever the conf's text turns out to be, so the
	// identity always pins both files.
	var sidecar []byte
	if b, serr := os.ReadFile(DiscoveredPath(path)); serr == nil {
		sidecar = b
		id.models = sha256.Sum256(sidecar)
	}
	text, terr := textOf(data, path)
	if terr != nil {
		return &Snapshot{ID: id}, terr
	}
	fields := parseConf(text)
	if _, inline := fields["context_discovered"]; !inline && sidecar != nil {
		fields["context_discovered"] = string(sidecar)
	}
	return &Snapshot{ID: id, Text: text, Fields: fields}, nil
}

// ProfileIdentity is nil for a symlink or an unreadable conf. It also pins the
// profile's models.json, so a writer sees a discovery saved since it read.
func ProfileIdentity(profile string) *Identity {
	snap, _ := profileSnapshot(profile)
	if snap == nil {
		return nil
	}
	return snap.ID
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

// ParseTokens is Positive with the k/m scale people type: "100k" and "2m" are
// the token counts they mean, resolved to plain digits. The scaled value keeps
// Positive's ten-digit bound.
func ParseTokens(value string) (int64, bool) {
	if n, ok := Positive(value); ok {
		return n, true
	}
	if len(value) < 2 {
		return 0, false
	}
	var scale int64
	switch value[len(value)-1] {
	case 'k', 'K':
		scale = 1_000
	case 'm', 'M':
		scale = 1_000_000
	default:
		return 0, false
	}
	n, ok := Positive(value[:len(value)-1])
	if !ok || n > (9_999_999_999/scale) {
		return 0, false
	}
	return n * scale, true
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
	return valuesOf(discoveredDoc(fields))
}

// valuesOf is the route-bound window table from an already-parsed doc.
func valuesOf(doc obj) map[string]int64 {
	out := map[string]int64{}
	models := subObj(doc, "models")
	if models == nil {
		return out
	}
	for _, k := range slices.Sorted(maps.Keys(models)) {
		v := models[k]
		if n, ok := asPosInt(v); ok && ValidModel(k) {
			out[k] = n
		}
	}
	return out
}

// DiscoveredSources is the provenance of windows filled by something other
// than a live probe or declared listing.
func DiscoveredSources(fields map[string]string) map[string]obj {
	return sourcesOf(discoveredDoc(fields))
}

// sourcesOf is the provenance table from an already-parsed doc.
func sourcesOf(doc obj) map[string]obj {
	out := map[string]obj{}
	sources := subObj(doc, "sources")
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
	return missesOf(discoveredDoc(fields))
}

// missesOf is the miss table from an already-parsed doc.
func missesOf(doc obj) map[string]string {
	out := map[string]string{}
	misses := subObj(doc, "misses")
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
	return mappedTarget(fields, parseModelMap(fields), bare)
}

// parseModelMap is the conf's model_map as a JSON object; nil when absent or
// not one, which mappedTarget reads as "no mapping".
func parseModelMap(fields map[string]string) obj {
	text := fields["model_map"]
	if text == "" {
		text = "{}"
	}
	v, err := parseJSON([]byte(text))
	if err != nil {
		return nil
	}
	mapping, _ := v.(obj)
	return mapping
}

// mappedTarget is MappedTarget over an already-parsed mapping.
func mappedTarget(fields map[string]string, mapping obj, bare string) string {
	base := strings.TrimRight(fields["base_url"], "/")
	if base == "" || base == "https://api.anthropic.com" {
		return bare
	}
	if mapping == nil {
		return bare
	}
	target := mapping[bare]
	s, ok := target.(string)
	if !ok || s == "" {
		return bare
	}
	return strings.TrimSuffix(s, "[1m]")
}

// RouteKnown is whether the route's discovered table carries model, as itself
// or through a glob pattern, mapped to its target first. It is the judgement
// `ccmorph context`'s save form makes for a provider, whose catalogue is not
// readable with the profile's own credentials — the probe that fills this
// table is the evidence the route offers.
func RouteKnown(fields map[string]string, model string) bool {
	target := MappedTarget(fields, model)
	discovered := DiscoveredValues(fields)
	if _, ok := discovered[target]; ok {
		return true
	}
	for _, p := range slices.Sorted(maps.Keys(discovered)) {
		if glob(target, p) {
			return true
		}
	}
	return false
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
	text, err := confText(path)
	if err != nil {
		return false, err
	}
	return replaceFields(path, text, fields, expected)
}

// errConfChanged is the sentinel replaceFields' guard returns when the conf
// changed under the rewrite: the write gives up with no error, and the caller
// learns the file was not its to replace.
var errConfChanged = errors.New("conf changed since it was read")

// replaceFields is ReplaceFields over conf text the caller already read; it
// reads the conf's mode and re-checks its identity, nothing else.
func replaceFields(path, text string, fields []Field, expected *Identity) (bool, error) {
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
	recheck := func() error {
		if expected == nil {
			return nil
		}
		// Also protects against non-locking editors before the atomic replace.
		cur, err := os.Open(path)
		if err != nil {
			return err
		}
		now, _, err := identityOf(cur)
		cur.Close()
		if err != nil {
			return err
		}
		now.models = expected.models // ReplaceFields guards the conf only
		if *now != *expected {
			return errConfChanged
		}
		return nil
	}
	if err := paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".context-", Data: []byte(updated), Mode: mode, Guard: recheck}); err != nil {
		if errors.Is(err, errConfChanged) {
			return false, nil
		}
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
		// One read of the profile carries the identity to check, the fields to
		// merge and the text to rewrite (GH-485).
		snap, serr := profileSnapshot(profile)
		if snap == nil || *snap.ID != *expected {
			return nil
		}
		if serr != nil {
			return serr
		}
		path, err := ProfilePath(profile)
		if err != nil {
			return err
		}
		fields := snap.Fields
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
		saved, err = writeDiscovered(path, snap.Text, record, expected)
		return err
	})
	if err != nil || !ran {
		return false, err
	}
	return saved, nil
}

// writeDiscovered stores record as indented JSON in DiscoveredPath and drops any
// inline context_discovered line the conf still carries. The caller holds the
// profile lock, has checked expected, and passes the conf text it read.
func writeDiscovered(conf, text string, record obj, expected *Identity) (bool, error) {
	inline := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "context_discovered=") {
			inline = true
		}
	}
	target := DiscoveredPath(conf)
	b, _ := json.MarshalIndent(record, "", "  ")
	// NoChmod: the record keeps the temp's own mode, as this write always did.
	if err := paths.AtomicWrite(paths.WriteIn{Path: target, Temp: ".models-", Data: append(b, '\n'), NoChmod: true, Mkdir: 0o700}); err != nil {
		return false, err
	}
	if !inline {
		return true, nil
	}
	return replaceFields(conf, text, []Field{{Key: "context_discovered"}}, expected)
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
	// NoChmod: the record keeps the temp's own mode, as this write always did.
	_ = paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".models-", Data: append(b, '\n'), NoChmod: true})
}

// SetContext pins model's window, or clears the pin with "auto".
func SetContext(profile, model, value string) error {
	if !ValidModel(model) {
		return errors.New("context model id is invalid")
	}
	parsed, ok := ParseTokens(value)
	if value != "auto" && !ok {
		return errors.New("context tokens must be a positive integer of at most 10 digits, with an optional k or m suffix (for example 100000, 100k or 2m), or auto")
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
