package util

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/pycompat"
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

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// migrateProfileHome moves a legacy ~/.claude-profiles into the state root under a lock.
func migrateProfileHome(args []string, _ io.Reader, _, errw io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	legacy, target := args[0], args[1]
	e := pycompat.Catch(func() {
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o777); err != nil {
			pycompat.RaiseOS(err)
		}
		if isSymlink(parent) {
			pycompat.RaiseValue("refusing symlinked state root")
		}
		fd, err := syscall.Open(filepath.Join(parent, ".profile-home-migration.lock"),
			syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			pycompat.RaiseOS(&os.PathError{Op: "open", Path: filepath.Join(parent, ".profile-home-migration.lock"), Err: err})
		}
		defer syscall.Close(fd)
		deadline := time.Now().Add(10 * time.Second)
		for {
			err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil {
				break
			}
			if err != syscall.EWOULDBLOCK {
				pycompat.RaiseOS(err)
			}
			if !time.Now().Before(deadline) {
				pycompat.RaiseValue("another profile migration is still running; retry shortly")
			}
			time.Sleep(50 * time.Millisecond)
		}
		switch {
		case isSymlink(legacy):
			if resolveLink(legacy) != resolveLink(target) || !isDir(target) || isSymlink(target) {
				pycompat.RaiseValue("legacy profile link points elsewhere; nothing moved")
			}
		case exists(legacy):
			if !isDir(legacy) || exists(target) || isSymlink(target) {
				pycompat.RaiseValue("both profile locations exist or are not directories; nothing moved")
			}
			// Rename on the same filesystem: never copy credentials or merge homes.
			if err := rename(legacy, target); err != nil {
				pycompat.RaiseOS(err)
			}
			if err := symlink(target, legacy); err != nil {
				if !exists(legacy) && !isSymlink(legacy) {
					rename(target, legacy)
				}
				pycompat.RaiseOS(err)
			}
			fmt.Fprintf(errw, "ccmorph: profile storage moved to %s; legacy path retained as a compatibility link\n", target)
		}
	})
	if e != nil {
		fmt.Fprintf(errw, "ccmorph: profile directory migration incomplete: %s\n", e.Msg)
		return 1
	}
	return 0
}

// normalizePluginIndex repairs plugin index paths made stale by the shared-plugins topology.
func normalizePluginIndex(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	home, profileRoot := pycompat.Path(args[0]), pycompat.Path(args[1])
	plugins := filepath.Join(home, ".claude", "plugins")
	legacyRoot := filepath.Join(home, ".claude-profiles")
	canonical := func(v pycompat.Value, kind string) (string, bool) {
		s, ok := v.(string)
		if !ok || !filepath.IsAbs(s) {
			return "", false
		}
		p := pycompat.Path(s)
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
			return target, exists(target)
		}
		return "", false
	}
	write := func(path string, data pycompat.Value) error {
		fi, err := os.Stat(path)
		if err != nil {
			return err
		}
		return atomicWrite(path, "."+filepath.Base(path)+".",
			[]byte(pycompat.Marshal(data, pycompat.Format{Indent: 2, ASCII: true})+"\n"), fi.Mode().Perm(), true)
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
					return 1
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
						return 1
					}
				}
			}
		}
	}
	return 0
}

var modelID = regexp.MustCompile(`^[A-Za-z0-9._:\[\]()@/-]+$`)

// validModelMap: a JSON object of unique model ids to model ids, no key carrying [1m].
func validModelMap(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 1 || !strings.HasPrefix(strings.TrimLeftFunc(args[0], pycompat.IsSpace), "{") {
		return 1
	}
	v, err := pycompat.Parse([]byte(args[0]))
	if err != nil {
		return 1
	}
	obj := asObj(v)
	if obj == nil {
		return 1
	}
	// A duplicate key collapses in the parsed object, so count the raw keys apart.
	if rawKeyCount(args[0]) != obj.Len() {
		return 1
	}
	for _, k := range obj.Keys() {
		val, _ := obj.Get(k)
		s, ok := val.(string)
		if !ok || !modelID.MatchString(k) || !modelID.MatchString(s) || strings.HasSuffix(strings.ToLower(k), "[1m]") {
			return 1
		}
	}
	return 0
}

// rawKeyCount counts the members of the top-level object in already-validated JSON.
func rawKeyCount(s string) int {
	depth, n := 0, 0
	inStr, esc, sawValue := false, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			if depth == 1 && !sawValue {
				n++
				sawValue = true
			}
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ',':
			if depth == 1 {
				sawValue = false
			}
		}
	}
	return n
}

// settingsValid exits 1 unless every existing file named is a JSON object.
func settingsValid(args []string, _ io.Reader, _, _ io.Writer) int {
	for _, p := range args {
		v, err := loadJSON(p)
		if err != nil {
			if pycompat.IsNotExist(err) {
				continue
			}
			return 1
		}
		if asObj(v) == nil {
			return 1
		}
	}
	return 0
}

// modelsCount prints the length of data in a model listing on stdin, or exits 1.
func modelsCount(_ []string, in io.Reader, out, _ io.Writer) int {
	v, err := pycompat.Parse(readAll(in))
	if err != nil {
		return 1
	}
	list, ok := getKey(asObj(v), "data").([]any)
	if !ok {
		return 1
	}
	fmt.Fprintln(out, len(list))
	return 0
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

// runningAdapter prints "<pid>\t<holder>:<name>,..." for the profile's running adapter,
// nothing when there is none, and __UNKNOWN__ when the process table cannot be read.
// The holders are the spawned children worth checking (cliproxyapi; a
// CCMORPH_SHIM_HOOKS build, any child but ccmorph-core) and, when no hooks child exists,
// the adapter process itself as "shim": the stock shim is served in-process, so
// its listener is the adapter's own.
func runningAdapter(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,command=").Output()
	if err != nil {
		fmt.Fprintln(out, "__UNKNOWN__")
		return 0
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
		if name, ok := adapterProfile(p.args); !ok || name != args[0] {
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
		fmt.Fprintf(out, "%d\t%s\n", p.pid, strings.Join(children, ","))
		return 0
	}
	return 0
}

// listenerOf prints the first TCP listen address of a pid, __UNKNOWN__ when lsof cannot say.
func listenerOf(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-p", args[0], "-iTCP", "-sTCP:LISTEN").Output()
	if err != nil {
		fmt.Fprintln(out, "__UNKNOWN__")
		return 0
	}
	rows := strings.Split(string(raw), "\n")
	for _, row := range rows[min(1, len(rows)):] {
		if f := strings.Fields(row); len(f) > 0 {
			fmt.Fprintln(out, f[max(len(f)-2, 0)])
			break
		}
	}
	return 0
}

// listing returns the entries of payload["data"], ok=false when the payload cannot be read like Python would.
func listing(payload pycompat.Value) ([]any, bool) {
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
	case string, *pycompat.Obj:
		return nil, true
	}
	return nil, false
}

// chatModel reports the id of a listing row that the endpoint does not deny for chat.
func chatModel(m pycompat.Value) (string, bool) {
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

// modelsFilteredJSON: models-filtered-json <conf>: the listing on stdin with only the
// chat-capable, non-excluded rows left.
func modelsFilteredJSON(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	var fields map[string]string
	if pycompat.Catch(func() { fields = confFields(args[0]) }) != nil {
		return 1
	}
	payload, err := pycompat.Parse(readAll(in))
	if err != nil {
		return 1
	}
	entries, ok := listing(payload)
	if !ok {
		return 1
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
		case []any, *pycompat.Obj:
			return 1 // unhashable: Python's `in` raises
		case string:
			if allowed[id] {
				kept = append(kept, row)
			}
		}
	}
	asObj(payload).Set("data", kept)
	fmt.Fprintln(out, jsonOut(payload))
	return 0
}

// pyStr is str() of a decoded scalar.
func pyStr(v pycompat.Value) string {
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
	case pycompat.Num:
		return t.Lit
	}
	return jsonOut(v)
}

// windowLabel: "<window> <unprobed 0|1> <label>" from a resolver record on stdin.
func windowLabel(_ []string, in io.Reader, out, _ io.Writer) int {
	v, err := pycompat.Parse(readAll(in))
	d := asObj(v)
	if err != nil || d == nil {
		return 1
	}
	window := ""
	if w := getKey(d, "window"); truthy(w) {
		window = pyStr(w)
	}
	unprobed := 0
	if truthy(getKey(d, "unprobed")) {
		unprobed = 1
	}
	label := "probed"
	switch src, _ := getKey(d, "source").(string); src {
	case "conf-discovered-catalogue", "catalogue-bundled":
		label = "catalogue-estimate"
	case "conf-manual":
		label = "manual"
	}
	fmt.Fprintf(out, "%s %d %s\n", window, unprobed, label)
	return 0
}

// effortRows prints the per-model effort overrides of a projected model-settings document.
func effortRows(_ []string, in io.Reader, out, _ io.Writer) int {
	v, err := pycompat.Parse(readAll(in))
	d := asObj(v)
	if err != nil || d == nil {
		return 1
	}
	rows := asObj(getKey(d, "modelSettings"))
	if rows == nil {
		return 0
	}
	keys := rows.Keys()
	sort.Strings(keys)
	for _, k := range keys {
		rv, _ := rows.Get(k)
		fmt.Fprintf(out, "  effort:%s %s  (profile override)\n", k, pyStr(getKey(asObj(rv), "effortLevel")))
	}
	return 0
}

// savedProviderModels prints "<value>\tSaved provider model" for the picker rows saved in a provider's .claude.json.
func savedProviderModels(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	var rows []any
	if v, err := loadJSON(args[0]); err == nil {
		rows, _ = getKey(asObj(v), "additionalModelOptionsCache").([]any)
	}
	for _, r := range rows {
		if value, ok := getKey(asObj(r), "value").(string); ok && value != "" && !hasSpace(value) {
			fmt.Fprintf(out, "%s\tSaved provider model\n", value)
		}
	}
	return 0
}

// unwireStatusline drops a statusLine command ccmorph itself once wired (_CCM_SL_OWNED).
func unwireStatusline(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	owned := map[string]bool{}
	for _, p := range strings.Split(os.Getenv("_CCM_SL_OWNED"), "\n") {
		if p != "" {
			owned[p] = true
		}
	}
	v, err := loadJSON(args[0])
	if err != nil {
		return 0
	}
	doc := asObj(v)
	if doc == nil {
		return 0
	}
	cmd, _ := getKey(asObj(getKey(doc, "statusLine")), "command").(string)
	if !owned[cmd] || cmd == "" {
		return 0
	}
	doc.Delete("statusLine")
	if atomicWrite(args[0], ".sl.", []byte(pycompat.Marshal(doc, pycompat.Format{Indent: 2, ASCII: true})+"\n"), 0o600, false) != nil {
		return 1
	}
	return 0
}

var commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)

// transcript reads a session transcript: one JSON record per line, unreadable lines skipped.
func transcript(path string) (recs []*pycompat.Obj, ok bool) {
	var text string
	if pycompat.Catch(func() { text = pycompat.ReadText(path) }) != nil {
		return nil, false
	}
	for _, line := range strings.Split(text, "\n") {
		v, err := pycompat.Parse([]byte(line))
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

func notSidechain(r *pycompat.Obj) bool { return getKey(r, "isSidechain") != true }

// resumeModel prints the model a transcript's session last used: its last /model command, else its last assistant turn.
func resumeModel(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	recs, ok := transcript(args[0])
	if !ok {
		return 1
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
		io.WriteString(out, commandModel)
	} else {
		io.WriteString(out, assistantModel)
	}
	return 0
}

// sessionState prints "<model>\t<effort>" from the last assistant turn that recorded both.
func sessionState(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	recs, ok := transcript(args[0])
	if !ok {
		return 1
	}
	var model, effort string
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
		return 1
	}
	fmt.Fprintf(out, "%s\t%s", model, effort)
	return 0
}

// adviseJSON wraps _CCM_ADVISE as the SessionStart hook's systemMessage.
func adviseJSON(_ []string, _ io.Reader, out, errw io.Writer) int {
	msg, ok := os.LookupEnv("_CCM_ADVISE")
	if !ok {
		fmt.Fprintln(errw, "KeyError: '_CCM_ADVISE'")
		return 1
	}
	o := pycompat.NewObj()
	o.Set("systemMessage", msg)
	fmt.Fprintln(out, jsonOut(o))
	return 0
}

var schemeHead = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)

// redactURL prints scheme://host[:port]/path, dropping userinfo, query and fragment.
func redactURL(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 1
	}
	// urlsplit's own cleanup: leading C0 controls and spaces, and every tab or newline.
	u := strings.TrimLeftFunc(args[0], func(r rune) bool { return r <= ' ' })
	u = strings.NewReplacer("\t", "", "\r", "", "\n", "").Replace(u)
	scheme := ""
	if m := schemeHead.FindStringSubmatch(u); m != nil {
		scheme = strings.ToLower(m[1])
		u = u[len(m[0]):]
	}
	netloc := ""
	if rest, ok := strings.CutPrefix(u, "//"); ok {
		end := strings.IndexAny(rest, "/?#")
		if end < 0 {
			end = len(rest)
		}
		netloc, u = rest[:end], rest[end:]
	}
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	// No host was found, so an @ is userinfo that would print whole.
	if netloc == "" && strings.Contains(u, "@") {
		return 1
	}
	host := netloc
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	port := ""
	hostOnly := host
	if strings.HasPrefix(host, "[") {
		if j := strings.Index(host, "]"); j >= 0 {
			hostOnly, port = host[1:j], strings.TrimPrefix(host[j+1:], ":")
		}
	} else if i := strings.LastIndex(host, ":"); i >= 0 {
		hostOnly, port = host[:i], host[i+1:]
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 0 || n > 65535 {
			return 1
		}
		if n == 0 {
			port = ""
		}
	}
	hostOnly = strings.ToLower(hostOnly)
	if strings.Contains(hostOnly, ":") {
		hostOnly = "[" + hostOnly + "]"
	}
	if port != "" {
		hostOnly += ":" + port
	}
	path := strings.TrimRight(u, "/")
	res := path
	if hostOnly != "" || (scheme != "" && netloc != "") {
		res = "//" + hostOnly + path
	}
	if scheme != "" {
		res = scheme + ":" + res
	}
	fmt.Fprintln(out, res)
	return 0
}

// gwModels: gw-models <conf> <picker|catalogue>: "<id>\t<owner>" rows from the listing on stdin.
// Exit 1 for an unreadable listing, 2 for one with no usable model.
func gwModels(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	fields := map[string]string{}
	if args[1] == "picker" {
		if pycompat.Catch(func() { fields = confFields(args[0]) }) != nil {
			return 1
		}
	}
	payload, err := pycompat.Parse(readAll(in))
	if err != nil {
		return 1
	}
	entries, ok := listing(payload)
	if !ok {
		return 1
	}
	type row struct{ id, owner string }
	var rows []row
	for _, m := range entries {
		id, ok := chatModel(m)
		if !ok || isExcluded(fields, id) {
			continue
		}
		owner := "?"
		if ov := getKey(asObj(m), "owned_by"); truthy(ov) {
			owner = pyStr(ov)
		}
		rows = append(rows, row{id, owner})
	}
	if len(rows) == 0 {
		return 2
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].id != rows[j].id {
			return rows[i].id < rows[j].id
		}
		return rows[i].owner < rows[j].owner
	})
	for _, r := range rows {
		fmt.Fprintf(out, "%s\t%s\n", r.id, r.owner)
	}
	return 0
}

// modelKnown: model-known <want> <model_map json>, listing in _CCM_MODELS_JSON.
// 0 served, 1 not served, 2 cannot tell.
func modelKnown(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	payload, err := pycompat.Parse([]byte(os.Getenv("_CCM_MODELS_JSON")))
	if err != nil {
		return 2
	}
	// A reply that is not the object a /v1/models listing is cannot be walked for
	// the model either: ignorance, not absence, so the carried model survives.
	if asObj(payload) == nil {
		return 2
	}
	mapJSON := args[1]
	if mapJSON == "" {
		mapJSON = "{}"
	}
	mv, err := pycompat.Parse([]byte(mapJSON))
	modelMap := asObj(mv)
	if err != nil || modelMap == nil {
		return 1
	}
	want := args[0]
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
