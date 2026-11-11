package util

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// Test seams: the two calls whose failures the migration tests must provoke.
var (
	rename  = os.Rename
	symlink = os.Symlink
)

// resolveLink is Path.resolve(): symlinks followed, a missing tail kept.
func resolveLink(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// MigrateProfileHome moves a legacy ~/.claude-profiles into the state root under a
// lock. moved reports a move, which the caller announces; the error is the
// message to show.
func MigrateProfileHome(legacy, target string) (moved bool, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("ccmorph: profile directory migration incomplete: %v", err)
		}
	}()
	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return false, err
	}
	if paths.IsSymlink(parent) {
		return false, errors.New("refusing symlinked state root")
	}
	lock := filepath.Join(parent, ".profile-home-migration.lock")
	fd, err := syscall.Open(lock, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return false, &os.PathError{Op: "open", Path: lock, Err: err}
	}
	defer syscall.Close(fd)
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return false, err
		}
		if !time.Now().Before(deadline) {
			return false, errors.New("another profile migration is still running; retry shortly")
		}
		time.Sleep(50 * time.Millisecond)
	}
	switch {
	case paths.IsSymlink(legacy):
		if resolveLink(legacy) != resolveLink(target) || !paths.IsDir(target) || paths.IsSymlink(target) {
			return false, errors.New("legacy profile link points elsewhere; nothing moved")
		}
	case paths.Exists(legacy):
		if !paths.IsDir(legacy) || paths.Exists(target) || paths.IsSymlink(target) {
			return false, errors.New("both profile locations exist or are not directories; nothing moved")
		}
		// Rename on the same filesystem: never copy credentials or merge homes.
		if err := rename(legacy, target); err != nil {
			return false, err
		}
		if err := symlink(target, legacy); err != nil {
			if !paths.Exists(legacy) && !paths.IsSymlink(legacy) {
				rename(target, legacy)
			}
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// NormalizePluginIndex repairs plugin index paths made stale by the shared-plugins
// topology; false when a rewrite failed.
func NormalizePluginIndex(homeDir, profileDir string) bool {
	home, profileRoot := filepath.Clean(homeDir), filepath.Clean(profileDir)
	plugins := filepath.Join(home, ".claude", "plugins")
	legacyRoot := filepath.Join(home, ".claude-profiles")
	canonical := func(v any, kind string) (string, bool) {
		s, ok := v.(string)
		if !ok || !filepath.IsAbs(s) {
			return "", false
		}
		p := filepath.Clean(s)
		for _, root := range []string{legacyRoot, profileRoot} {
			rest, ok := strings.CutPrefix(p, strings.TrimRight(root, "/")+"/")
			if !ok {
				continue
			}
			parts := strings.Split(rest, "/")
			if len(parts) < 3 || parts[1] != "plugins" || parts[2] != kind {
				return "", false
			}
			target := filepath.Join(append([]string{plugins}, parts[2:]...)...)
			return target, paths.Exists(target)
		}
		return "", false
	}
	write := func(path string, data any) error {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		return AtomicWrite(path, "."+filepath.Base(path)+".",
			[]byte(string(ordjson.Marshal(data, true))+"\n"), fi.Mode().Perm(), true)
	}
	known := filepath.Join(plugins, "known_marketplaces.json")
	if v, err := loadJSON(known); err == nil {
		if o := asObj(v); o != nil {
			changed := false
			for _, k := range o.Keys() {
				rv, _ := o.Get(k)
				rec := asObj(rv)
				if rec == nil {
					continue
				}
				loc, _ := rec.Get("installLocation")
				if t, ok := canonical(loc, "marketplaces"); ok && loc != t {
					rec.Set("installLocation", t)
					changed = true
				}
			}
			if changed {
				if err := write(known, o); err != nil {
					return false
				}
			}
		}
	}
	installed := filepath.Join(plugins, "installed_plugins.json")
	if v, err := loadJSON(installed); err == nil {
		if root := asObj(v); root != nil {
			if recs := asObj(getKey(root, "plugins")); recs != nil {
				changed := false
				for _, k := range recs.Keys() {
					vv, _ := recs.Get(k)
					versions, _ := vv.([]any)
					for _, rv := range versions {
						rec := asObj(rv)
						if rec == nil {
							continue
						}
						loc, _ := rec.Get("installPath")
						if t, ok := canonical(loc, "cache"); ok && loc != t {
							rec.Set("installPath", t)
							changed = true
						}
					}
				}
				if changed {
					if err := write(installed, root); err != nil {
						return false
					}
				}
			}
		}
	}
	return true
}

// normalizePluginIndexVerb is `util normalize-plugin-index <home> <profiles dir>`,
// the one verb install.sh runs.
func normalizePluginIndexVerb(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	if !NormalizePluginIndex(args[0], args[1]) {
		return 1
	}
	return 0
}

// SettingsValid reports whether every existing file named is a JSON object.
func SettingsValid(paths ...string) bool {
	for _, p := range paths {
		v, err := loadJSON(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false
		}
		if asObj(v) == nil {
			return false
		}
	}
	return true
}

// modelsCount is the length of data in a model listing; false when it has none.
func modelsCount(body string) (int, bool) {
	v, err := ordjson.Parse([]byte(body))
	if err != nil {
		return 0, false
	}
	list, ok := getKey(asObj(v), "data").([]any)
	return len(list), ok
}

// shellSplit is shlex.split for a ps command column.
func shellSplit(s string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	have := false
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		case c == '\\':
			if i+1 >= len(r) {
				return nil, false
			}
			i++
			cur.WriteRune(r[i])
			have = true
		case c == '\'':
			j := i + 1
			for j < len(r) && r[j] != '\'' {
				j++
			}
			if j >= len(r) {
				return nil, false
			}
			cur.WriteString(string(r[i+1 : j]))
			i, have = j, true
		case c == '"':
			j := i + 1
			closed := false
			for j < len(r) {
				if r[j] == '\\' && j+1 < len(r) && (r[j+1] == '"' || r[j+1] == '\\') {
					cur.WriteRune(r[j+1])
					j += 2
					continue
				}
				if r[j] == '"' {
					closed = true
					break
				}
				cur.WriteRune(r[j])
				j++
			}
			if !closed {
				return nil, false
			}
			i, have = j, true
		default:
			cur.WriteRune(c)
			have = true
		}
	}
	if have {
		out = append(out, cur.String())
	}
	return out, true
}

var pythonName = regexp.MustCompile(`(?i)^python(?:\d+(?:\.\d+)*)?$`)

func execIndex(args []string) int {
	if len(args) == 0 {
		return -1
	}
	if pythonName.MatchString(filepath.Base(args[0])) {
		if len(args) > 1 {
			return 1
		}
		return -1
	}
	return 0
}

// runningAdapter is "<pid>\t<holder>:<name>,..." for the profile's running adapter,
// "" when there is none, and __UNKNOWN__ when the process table cannot be read.
// The holders are the spawned children worth checking (cliproxyapi; a
// CCMORPH_SHIM_HOOKS build, any child but ccmorph-core) and, when no hooks child exists,
// the adapter process itself as "shim": the stock shim is served in-process, so
// its listener is the adapter's own.
func runningAdapter(profile string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		return "__UNKNOWN__"
	}
	type proc struct {
		pid, ppid int
		args      []string
	}
	var procs []proc
	for _, row := range strings.Split(string(raw), "\n") {
		f := strings.Fields(row)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		rest := strings.TrimLeft(row, " \t")
		for k := 0; k < 2; k++ {
			rest = strings.TrimLeft(rest[strings.IndexAny(rest, " \t"):], " \t")
		}
		a, ok := shellSplit(rest)
		if !ok {
			continue
		}
		procs = append(procs, proc{pid, ppid, a})
	}
	adapterProfile := func(a []string) (string, bool) {
		i := execIndex(a)
		if i < 0 || i+1 >= len(a) {
			return "", false
		}
		if filepath.Base(a[i]) == "ccmorph-core" && a[i+1] == "adapter" {
			if i+2 < len(a) {
				return a[i+2], true
			}
			return "", false
		}
		if filepath.Base(a[i]) == "ccmorph-adapter" {
			return a[i+1], true
		}
		return "", false
	}
	for _, p := range procs {
		if name, ok := adapterProfile(p.args); !ok || name != profile {
			continue
		}
		var children []string
		shimChild := false
		for _, c := range procs {
			if c.ppid != p.pid || len(c.args) == 0 {
				continue
			}
			if ci := execIndex(c.args); ci >= 0 {
				if base := filepath.Base(c.args[ci]); base != "ccmorph-core" {
					children = append(children, fmt.Sprintf("%d:%s", c.pid, base))
					shimChild = shimChild || base != "cliproxyapi"
				}
			}
		}
		// The stock shim is served inside the adapter process, not spawned (only
		// a CCMORPH_SHIM_HOOKS build runs as a child): with no such
		// child, the adapter's own pid is the listener doctor must be told to
		// check — the port a live session's base_url points at.
		if !shimChild {
			children = append(children, fmt.Sprintf("%d:shim", p.pid))
		}
		return fmt.Sprintf("%d\t%s", p.pid, strings.Join(children, ","))
	}
	return ""
}

// listenerOf is the first TCP listen address of a pid, __UNKNOWN__ when lsof cannot say.
func listenerOf(pid string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-p", pid, "-iTCP", "-sTCP:LISTEN").Output()
	if err != nil {
		return "__UNKNOWN__"
	}
	rows := strings.Split(string(raw), "\n")
	for _, row := range rows[min(1, len(rows)):] {
		if f := strings.Fields(row); len(f) > 0 {
			return f[max(len(f)-2, 0)]
		}
	}
	return ""
}

// listing returns the entries of payload["data"], ok=false when the payload cannot be read like Python would.
func listing(payload any) ([]any, bool) {
	o := asObj(payload)
	if o == nil {
		return nil, false
	}
	d, has := o.Get("data")
	if !has {
		return nil, true
	}
	switch t := d.(type) {
	case []any:
		return t, true
	case string, *ordjson.Obj:
		return nil, true
	}
	return nil, false
}

// chatModel reports the id of a listing row that the endpoint does not deny for chat.
func chatModel(m any) (string, bool) {
	row := asObj(m)
	if row == nil {
		return "", false
	}
	if caps := asObj(getKey(row, "capabilities")); caps != nil {
		if v, ok := caps.Get("chat_completion"); ok && v == false {
			return "", false
		}
	}
	id, ok := getKey(row, "id").(string)
	return id, ok && id != ""
}

func isExcluded(fields map[string]string, id string) bool { return ctxwindow.IsExcluded(fields, id) }

// ModelsFilteredJSON is the listing with only the chat-capable, non-excluded rows
// left, as JSON; false when the conf or listing cannot be read.
func ModelsFilteredJSON(conf, body string) (string, bool) {
	fields, err := readConf(conf)
	if err != nil {
		return "", false
	}
	payload, err := ordjson.Parse([]byte(body))
	if err != nil {
		return "", false
	}
	entries, ok := listing(payload)
	if !ok {
		return "", false
	}
	allowed := map[string]bool{}
	for _, m := range entries {
		if id, ok := chatModel(m); ok && !isExcluded(fields, id) {
			allowed[id] = true
		}
	}
	kept := []any{}
	for _, row := range entries {
		o := asObj(row)
		if o == nil {
			continue
		}
		switch id := getKey(o, "id").(type) {
		case []any, *ordjson.Obj:
			return "", false // unhashable: Python's `in` raises
		case string:
			if allowed[id] {
				kept = append(kept, row)
			}
		}
	}
	asObj(payload).Set("data", kept)
	return jsonOut(payload), true
}

// pyStr is str() of a decoded scalar.
func pyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case json.Number:
		return string(t)
	}
	return jsonOut(v)
}

// WindowLabel reads a model-window resolver record (JSON): the window, whether it
// is unprobed, and how it was learned.
func WindowLabel(record string) (window string, unprobed bool, label string, ok bool) {
	v, err := ordjson.Parse([]byte(record))
	d := asObj(v)
	if err != nil || d == nil {
		return "", false, "", false
	}
	if w := getKey(d, "window"); truthy(w) {
		window = pyStr(w)
	}
	label = "probed"
	switch src, _ := getKey(d, "source").(string); src {
	case "conf-discovered-catalogue", "catalogue-bundled":
		label = "catalogue-estimate"
	case "conf-manual":
		label = "manual"
	}
	return window, truthy(getKey(d, "unprobed")), label, true
}

// EffortRow is one model's effort override.
type EffortRow struct{ Model, Effort string }

// EffortRows are the per-model effort overrides of a projected model-settings
// document (JSON), by model name.
func EffortRows(doc string) []EffortRow {
	v, err := ordjson.Parse([]byte(doc))
	d := asObj(v)
	if err != nil || d == nil {
		return nil
	}
	rows := asObj(getKey(d, "modelSettings"))
	if rows == nil {
		return nil
	}
	keys := rows.Keys()
	sort.Strings(keys)
	var out []EffortRow
	for _, k := range keys {
		rv, _ := rows.Get(k)
		out = append(out, EffortRow{k, pyStr(getKey(asObj(rv), "effortLevel"))})
	}
	return out
}

// SavedProviderModels are the picker values saved in a provider's .claude.json.
func SavedProviderModels(claudeJSON string) (values []string) {
	var rows []any
	if v, err := loadJSON(claudeJSON); err == nil {
		rows, _ = getKey(asObj(v), "additionalModelOptionsCache").([]any)
	}
	for _, r := range rows {
		if value, ok := getKey(asObj(r), "value").(string); ok && value != "" && !hasSpace(value) {
			values = append(values, value)
		}
	}
	return values
}

// UnwireStatusline drops the statusLine of the settings file at path when its
// command is one ccmorph itself once wired (one of owned).
func UnwireStatusline(path string, owned ...string) error {
	v, err := loadJSON(path)
	if err != nil {
		return nil
	}
	doc := asObj(v)
	if doc == nil {
		return nil
	}
	cmd, _ := getKey(asObj(getKey(doc, "statusLine")), "command").(string)
	if cmd == "" || !slices.Contains(owned, cmd) {
		return nil
	}
	doc.Delete("statusLine")
	return AtomicWrite(path, ".sl.", []byte(string(ordjson.Marshal(doc, true))+"\n"), 0o600, false)
}

var commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)

// transcript reads a session transcript: one JSON record per line, unreadable lines skipped.
func transcript(path string) (recs []*ordjson.Obj, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(b) {
		return nil, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		v, err := ordjson.Parse([]byte(line))
		if err != nil {
			continue
		}
		o := asObj(v)
		if o == nil {
			return nil, false // record.get on a non-object crashes the python program
		}
		recs = append(recs, o)
	}
	return recs, true
}

func notSidechain(r *ordjson.Obj) bool { return getKey(r, "isSidechain") != true }

// ResumeModel is the model a transcript's session last used: its last /model
// command, else its last assistant turn. ok is false for an unreadable transcript.
func ResumeModel(path string) (model string, ok bool) {
	recs, ok := transcript(path)
	if !ok {
		return "", false
	}
	var commandModel, assistantModel string
	sawCommand := false
	for _, r := range recs {
		msg := asObj(getKey(r, "message"))
		if getKey(r, "type") == "assistant" && notSidechain(r) && msg != nil && getKey(msg, "role") == "assistant" {
			if m, ok := getKey(msg, "model").(string); ok && m != "" && m != "<synthetic>" {
				assistantModel = m
			}
		}
		if getKey(r, "type") != "user" || !notSidechain(r) || msg == nil || getKey(msg, "role") != "user" {
			continue
		}
		content, ok := getKey(msg, "content").(string)
		if !ok || !strings.Contains(content, "<command-name>/model</command-name>") {
			continue
		}
		sawCommand = true
		value := ""
		if m := commandArgs.FindStringSubmatch(content); m != nil {
			value = m[1]
		}
		if value != "" && !strings.ContainsAny(value, "\r\n") {
			commandModel = value
		} else {
			commandModel = ""
		}
	}
	if sawCommand {
		return commandModel, true
	}
	return assistantModel, true
}

// SessionState is the model and effort of the last assistant turn that recorded both.
func SessionState(path string) (model, effort string, ok bool) {
	recs, ok := transcript(path)
	if !ok {
		return "", "", false
	}
	for _, r := range recs {
		msg := asObj(getKey(r, "message"))
		if getKey(r, "type") != "assistant" || !notSidechain(r) || msg == nil || getKey(msg, "role") != "assistant" {
			continue
		}
		m, e := getKey(msg, "model"), getKey(r, "effort")
		if m == nil || m == "" || m == "<synthetic>" || e == nil || e == "" {
			continue
		}
		model, effort = pyStr(m), pyStr(e)
	}
	if model == "" || effort == "" || strings.ContainsAny(model+effort, "\t\r\n") {
		return "", "", false
	}
	return model, effort, true
}

// HasConversation reports whether a transcript holds a conversation Claude Code
// can resume. Claude Code refuses an id whose transcript carries no record with
// the identity it resumes by -- which is every session that has yet to see its
// first message, the file still empty or not written (#288).
func HasConversation(path string) bool {
	recs, ok := transcript(path)
	if !ok {
		return false
	}
	for _, r := range recs {
		if s, ok := getKey(r, "sessionId").(string); ok && s != "" {
			if u, ok := getKey(r, "uuid").(string); ok && u != "" {
				if ts, ok := getKey(r, "timestamp").(string); ok && ts != "" {
					return true
				}
			}
		}
	}
	return false
}

// Advise wraps msg as the SessionStart hook's systemMessage.
func Advise(msg string) string {
	o := ordjson.New()
	o.Set("systemMessage", msg)
	return jsonOut(o)
}

// ModelRow is one model of a gateway listing and its label: the owner when the
// listing names one, else the display name.
type ModelRow struct{ ID, Label string }

// GwModels are the chat models of the listing, sorted; with picker the profile
// conf's excluded models are left out. False for an unreadable listing or one with
// no usable model.
func GwModels(conf string, picker bool, body string) ([]ModelRow, bool) {
	fields := map[string]string{}
	if picker {
		var err error
		if fields, err = readConf(conf); err != nil {
			return nil, false
		}
	}
	payload, err := ordjson.Parse([]byte(body))
	if err != nil {
		return nil, false
	}
	entries, ok := listing(payload)
	if !ok {
		return nil, false
	}
	var rows []ModelRow
	for _, m := range entries {
		id, ok := chatModel(m)
		if !ok || isExcluded(fields, id) {
			continue
		}
		label := ""
		if v := getKey(asObj(m), "owned_by"); truthy(v) {
			label = pyStr(v)
		} else if v := getKey(asObj(m), "display_name"); truthy(v) {
			label = pyStr(v)
		}
		rows = append(rows, ModelRow{id, label})
	}
	if len(rows) == 0 {
		return nil, false
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ID != rows[j].ID {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].Label < rows[j].Label
	})
	return rows, true
}

// modelInListing is model-known over a listing body: 0 served, 1 not, 2 cannot tell.
func modelInListing(listing, want, mapJSON string) int {
	payload, err := ordjson.Parse([]byte(listing))
	if err != nil {
		return 2
	}
	// A reply that is not the object a /v1/models listing is cannot be walked for
	// the model either: ignorance, not absence, so the carried model survives.
	if asObj(payload) == nil {
		return 2
	}
	if mapJSON == "" {
		mapJSON = "{}"
	}
	mv, err := ordjson.Parse([]byte(mapJSON))
	modelMap := asObj(mv)
	if err != nil || modelMap == nil {
		return 1
	}
	if mapped, has := modelMap.Get(want); has && mapped != nil {
		s, ok := mapped.(string)
		if !ok {
			return 1
		}
		want = strings.TrimSuffix(s, "[1m]")
	}
	uncertain := false
	// A listing that does not list cannot prove a model absent: keep the carried one.
	data, hasData := asObj(payload).Get("data")
	entries, isList := data.([]any)
	if hasData && !isList {
		return 2
	}
	for _, m := range entries {
		row := asObj(m)
		if row == nil {
			uncertain = true
			continue
		}
		id, ok := getKey(row, "id").(string)
		if !ok || id == "" {
			uncertain = true
			continue
		}
		if id != want {
			continue
		}
		if caps := asObj(getKey(row, "capabilities")); caps != nil {
			if v, ok := caps.Get("chat_completion"); ok && v == false {
				return 1
			}
		}
		return 0
	}
	if uncertain || getKey(asObj(payload), "has_more") == true {
		return 2
	}
	return 1
}

// modelIDShape is whether v can be a model id at all: the store's own rule for a
// saved model, so a flag or picker token keeps its dedicated refusal instead of
// being misdescribed as "not served".
func modelIDShape(v string) bool {
	return v != "" && v != "?" && !strings.HasPrefix(v, "-") && !strings.HasPrefix(v, "@") && !hasSpace(v)
}

// ServedRefusal is the one message set, run and doctor print when a requested
// model id is not served by a reachable listing (#267): the id as the person
// spelled it, the closest listed ids, and every chat-capable id the listing
// serves. who names the route -- a profile name, or "this gateway". ok is false
// when the listing cannot adjudicate the id -- it is served or mapped to a
// served target, it is a tier alias, it is not shaped like a model id, or the
// listing is unreadable or paged -- so the caller proceeds as before.
func ServedRefusal(body, want, mapJSON, who string) (string, bool) {
	if !modelIDShape(want) {
		return "", false
	}
	orig := want
	if strings.HasSuffix(strings.ToLower(want), "[1m]") {
		want = want[:len(want)-4]
	}
	switch want {
	case "", "opus", "sonnet", "haiku", "fable":
		return "", false
	}
	// A readable, complete listing proves absence even when it serves nothing; a
	// malformed or paged one cannot (modelInListing answers 2 for those).
	if _, ok := modelsCount(body); !ok {
		return "", false
	}
	if modelInListing(body, want, mapJSON) != 1 {
		return "", false
	}
	rows, _ := GwModels("", false, body)
	var ids []string
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		ids = append(ids, r.ID)
	}
	msg := "'" + orig + "' is not served by " + who
	if closest := ctxwindow.ClosestMatches(want, ids, 3); len(closest) > 0 {
		msg += " (closest: " + strings.Join(closest, ", ") + ")"
	}
	served := "(none)"
	if len(ids) > 0 {
		served = strings.Join(ids, ", ")
	}
	msg += "; served: " + served
	return msg, true
}
