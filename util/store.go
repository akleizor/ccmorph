package util

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/util/paths"
)

// content is one file read as bytes; nil means it does not exist.
func content(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err == nil && b == nil {
		b = []byte{}
	}
	return b, err
}

func sameBytes(a, b []byte) bool {
	return (a == nil) == (b == nil) && string(a) == string(b)
}

func fingerprint(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		if p == nil {
			io.WriteString(h, "none;")
			continue
		}
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
		io.WriteString(h, ";")
	}
	return hex.EncodeToString(h.Sum(nil))
}

func decodeUTF8(b []byte) (string, error) {
	if !utf8.Valid(b) {
		return "", errors.New("configuration is not valid UTF-8")
	}
	return string(b), nil
}

// dictMerge is the store's recursive dict merge: objects merge, anything else replaces.
func dictMerge(base, override *ordjson.Obj) *ordjson.Obj {
	result := ordjson.New()
	for _, k := range base.Keys() {
		v, _ := base.Get(k)
		result.Set(k, v)
	}
	for _, k := range override.Keys() {
		v, _ := override.Get(k)
		cur, _ := result.Get(k)
		co, cok := cur.(*ordjson.Obj)
		vo, vok := v.(*ordjson.Obj)
		if cok && vok {
			result.Set(k, dictMerge(co, vo))
		} else {
			result.Set(k, v)
		}
	}
	return result
}

func objField(o *ordjson.Obj, key string, msg string) (*ordjson.Obj, error) {
	v, has := o.Get(key)
	if !has {
		return ordjson.New(), nil
	}
	r, ok := v.(*ordjson.Obj)
	if !ok {
		return nil, errors.New(msg)
	}
	return r, nil
}

// hasSpace is Python's str.isspace, which also counts U+001C-U+001F; ctxwindow
// refuses those in a model id, and a store that accepted one would write a
// value the context reader then silently drops.
func hasSpace(s string) bool {
	for _, c := range s {
		if unicode.IsSpace(c) || (c >= 0x1c && c <= 0x1f) {
			return true
		}
	}
	return false
}

// replaceFile is the commit step of a save; a test seam for a failing replacement.
var replaceFile = os.Rename

// refusal is a value the store was asked to save and will not: the fault is in
// the request, not in the configuration files, so it prints as its own sentence,
// never as a failed save or read (#264).
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

func refuse(format string, a ...any) error { return &refusal{fmt.Sprintf(format, a...)} }

// StoreChange is what a set or configure operation saves; a Snapshot, when set,
// must still match the profile's files. Type, Endpoint and Credential are read by
// configure only.
type StoreChange struct{ Model, Effort, Snapshot, Type, Endpoint, Credential string }

// StoreArgs is one operation of the profile's configuration store (get, snapshot,
// model-settings, seed-model-defaults, prepare-launch, set, configure) over its
// conf, settings layer and the shared canonical settings. A prepare-launch with a
// Materialize argv (newline-joined) runs settings-compose over it afterwards.
type StoreArgs struct {
	Op, Conf, Settings, Key, Shared string
	Mapping                         []string
	Materialize                     string
	Change                          StoreChange
	// Live is the descriptor this process holds for the profile's live lock,
	// nil when it holds none: a projection run inside a launch upgrades the
	// launch's own lock instead of waiting on it (#939).
	Live *os.File
}

// layerProfileDir is the profile dir a layer path names, "" when the path is
// not one of the three spellings a profile's layer has. A whole-file write of
// it derives the profile's live lock from this (#939).
func layerProfileDir(path string) string {
	switch filepath.Base(path) {
	case "settings.json", "settings.profile.json":
		return filepath.Dir(path)
	case "settings.base.json":
		if filepath.Base(filepath.Dir(path)) == "cache" {
			return filepath.Dir(filepath.Dir(path))
		}
	}
	return ""
}

// ConfigStore runs the operation. The result is its text and its status: 0 done,
// 1 failed (reasons to errw), 10+n settings-compose's own status n.
func ConfigStore(a StoreArgs, errw io.Writer) (string, int) {
	op, materialize := a.Op, a.Materialize
	var out strings.Builder
	// A prepare-launch reads the profile layer and may remove model and
	// effortLevel from it. That whole-file write holds the profile's live lock
	// exclusively from the read to the rename; under a live session the lock is
	// busy, so the layer is left as it was and only the conf saves (#939).
	layerWrite := true
	var release func()
	if op == "prepare-launch" {
		if dir := layerProfileDir(a.Settings); dir != "" {
			var why error
			release, why = liveExclusiveLock(filepath.Dir(dir), filepath.Base(dir), "")
			layerWrite = release != nil
			if release == nil && why != nil && !errors.Is(why, ErrProfileInUse) {
				fmt.Fprintf(errw, "ccmorph: %v; the settings layer was left as it was\n", why)
			}
		}
	}
	exit, projectedJSON, err := func() (int, string, error) {
		if release != nil {
			defer release()
		}
		return configStoreRun(a, layerWrite, &out, errw)
	}()
	if err != nil {
		var r *refusal
		if errors.As(err, &r) {
			fmt.Fprintf(errw, "ccmorph: %v\n", err)
		} else {
			fmt.Fprintf(errw, "ccmorph: could not save/read configuration: %v\n", err)
		}
		return out.String(), 1
	}
	if exit != 0 || materialize == "" || op != "prepare-launch" {
		return out.String(), exit
	}
	// settings-compose's own statuses, shifted past the store's: exit 1 still means
	// "the store failed, do not start".
	argv := append(strings.Split(materialize, "\n"), "--saved", projectedJSON)
	if rc := settingscompose.MainWithLive(argv, errw, a.Live); rc != 0 {
		return out.String(), 10 + rc
	}
	return out.String(), 0
}

// storeState is one profile's store as one read of its files leaves it: the
// bytes its conf and settings layer held, the conf's own lines, the settings
// object parsed from the layer, and the model defaults and launch projection
// parsed from them. Every store op — read or write — starts from this same
// read, so a command parses the files once (#478).
type storeState struct {
	originals                [2][]byte
	lines                    []string
	doc, defaults, projected *ordjson.Obj
}

// readStoreState is the store's one read of a profile: its conf and settings
// layer, and the shared canonical settings when the conf has not taken the
// shared defaults yet. Its errors are the store's own, worded for the sentence
// ConfigStore prints.
func readStoreState(a StoreArgs) (*storeState, error) {
	conf, settings, sharedName, mappingKeys := a.Conf, a.Settings, a.Shared, a.Mapping
	confBytes, err := content(conf)
	if err != nil {
		return nil, err
	}
	settingsBytes, err := content(settings)
	if err != nil {
		return nil, err
	}
	originals := [2][]byte{confBytes, settingsBytes}
	if originals[0] == nil {
		return nil, fmt.Errorf("profile no longer exists")
	}
	doc := ordjson.New()
	if originals[1] != nil {
		v, err := ordjson.Parse(ordjson.StripBOM(originals[1]))
		if err != nil {
			return nil, err
		}
		d, ok := v.(*ordjson.Obj)
		if !ok {
			return nil, fmt.Errorf("settings.json and its env must be objects")
		}
		doc = d
	}
	if _, err := objField(doc, "env", "settings.json and its env must be objects"); err != nil {
		return nil, err
	}

	confText, err := decodeUTF8(originals[0])
	if err != nil {
		return nil, err
	}
	lines := strings.Split(confText, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	fields := ordjson.New()
	for _, line := range lines {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			fields.Set(k, v)
		}
	}
	mapped := map[string]bool{}
	for _, k := range mappingKeys {
		mapped["env."+k] = true
	}
	defaults := ordjson.New()
	for _, name := range fields.Keys() {
		if name == "model" || name == "model_source" || name == "effort" || strings.HasPrefix(name, "effort.") || mapped[name] {
			v, _ := fields.Get(name)
			defaults.Set(name, v)
		}
	}
	setDefault := func(name string, v any) {
		if !defaults.Has(name) {
			defaults.Set(name, v)
		}
	}
	if md, _ := fields.Get("model_defaults"); md != "1" {
		shared := ordjson.New()
		sb, err := content(sharedName)
		if err != nil {
			return nil, err
		}
		if len(sb) > 0 {
			v, err := ordjson.Parse(ordjson.StripBOM(sb))
			if err != nil {
				return nil, err
			}
			s, ok := v.(*ordjson.Obj)
			if !ok {
				return nil, fmt.Errorf("shared settings and its env must be objects")
			}
			shared = s
		}
		if _, err := objField(shared, "env", "shared settings and its env must be objects"); err != nil {
			return nil, err
		}
		native := dictMerge(shared, doc)
		rows, err := objField(native, "modelSettings", "modelSettings must be an object")
		if err != nil {
			return nil, err
		}
		env := asObj(getKey(native, "env"))
		if env == nil {
			env = ordjson.New()
		}
		// GH-862: a model taken from the shared ~/.claude/settings.json, and not
		// from this profile's own layer, is marked so every screen can name its source.
		docEnv := asObj(getKey(doc, "env"))
		chosen, fromShared := getKey(env, "ANTHROPIC_MODEL"), false
		if ordjson.Truthy(chosen) {
			fromShared = docEnv == nil || !ordjson.Truthy(getKey(docEnv, "ANTHROPIC_MODEL"))
		} else {
			chosen = getKey(native, "model")
			fromShared = !ordjson.Truthy(getKey(doc, "model"))
		}
		if ordjson.Truthy(chosen) && !defaults.Has("model") {
			defaults.Set("model", chosen)
			if fromShared {
				defaults.Set("model_source", profile.ModelSourceSettings)
			}
		}
		if lvl := getKey(env, "CLAUDE_CODE_EFFORT_LEVEL"); ordjson.Truthy(lvl) {
			setDefault("effort", lvl)
		} else if lvl := getKey(native, "effortLevel"); ordjson.Truthy(lvl) {
			setDefault("effort", lvl)
		}
		for _, model := range rows.Keys() {
			rv, _ := rows.Get(model)
			if row := asObj(rv); row != nil && ordjson.Truthy(getKey(row, "effortLevel")) {
				setDefault("effort."+model, getKey(row, "effortLevel"))
			}
		}
		for _, name := range mappingKeys {
			if v := getKey(env, name); ordjson.Truthy(v) {
				setDefault("env."+name, v)
			}
		}
	}
	modelSettings := ordjson.New()
	for _, name := range defaults.Keys() {
		v, _ := defaults.Get(name)
		value, isStr := v.(string)
		if !isStr || strings.Contains(name, "=") || strings.ContainsAny(name+value, "\r\n") || hasSpace(name) {
			return nil, fmt.Errorf("model defaults must be single-line strings with valid keys")
		}
		if strings.HasPrefix(name, "effort.") {
			row := ordjson.New()
			row.Set("effortLevel", value)
			modelSettings.Set(name[7:], row)
		}
	}
	projEnv := ordjson.New()
	for _, name := range defaults.Keys() {
		if strings.HasPrefix(name, "env.") {
			v, _ := defaults.Get(name)
			projEnv.Set(name[4:], v)
		}
	}
	projected := ordjson.New()
	projected.Set("env", projEnv)
	projected.Set("modelSettings", modelSettings)
	for _, f := range [][2]string{{"model", "model"}, {"effort", "effortLevel"}} {
		if v, _ := defaults.Get(f[0]); ordjson.Truthy(v) {
			projected.Set(f[1], v)
		}
	}
	return &storeState{originals: originals, lines: lines, doc: doc, defaults: defaults, projected: projected}, nil
}

// checkSavedEfforts is the one effort rule over the defaults a store read or
// save produces: every level is a level profile.Efforts names, and the conf's
// own key is "effort", never the empty "effort." that names no model (#839).
// It runs after a save's change is applied, so a save that overwrites a value
// the files refuse is its own repair.
func checkSavedEfforts(conf string, defaults *ordjson.Obj) error {
	name := strings.TrimSuffix(filepath.Base(conf), ".conf")
	for _, k := range defaults.Keys() {
		if k != "effort" && !strings.HasPrefix(k, "effort.") {
			continue
		}
		v, _ := defaults.Get(k)
		s, _ := v.(string)
		if k == "effort." {
			return fmt.Errorf("invalid saved effort key 'effort.' in '%s': effort must be %s", name, profile.EffortList())
		}
		if !slices.Contains(profile.Efforts, s) {
			return profile.EffortError(name, s)
		}
	}
	return nil
}

// StoreRead is every read answer the store gives over one parse of a profile's
// files (#478): each model default by key, the settings a launch projects from
// them, and the fingerprint of the files as this read saw them — the snapshot a
// save must still match.
type StoreRead struct {
	Values     map[string]string
	Projection string
	Snapshot   string
}

// ConfigStoreRead is the store's read half in one parse: what the get,
// model-settings and snapshot operations answer, held together for a caller
// that serves a command's reads from one read of the files. Its error is the
// store's own; ConfigStore's sentence wraps it for the person.
func ConfigStoreRead(a StoreArgs) (StoreRead, error) {
	s, err := readStoreState(a)
	if err != nil {
		return StoreRead{}, err
	}
	if err := checkSavedEfforts(a.Conf, s.defaults); err != nil {
		return StoreRead{}, err
	}
	r := StoreRead{Values: map[string]string{}, Projection: jsonOut(s.projected), Snapshot: fingerprint(s.originals[0], s.originals[1])}
	for _, k := range s.defaults.Keys() {
		v, _ := s.defaults.Get(k)
		r.Values[k] = fmt.Sprint(v)
	}
	return r, nil
}

func configStoreRun(a StoreArgs, layerWrite bool, out, errw io.Writer) (int, string, error) {
	op, key, materialize, mappingKeys := a.Op, a.Key, a.Materialize, a.Mapping
	conf, settings := a.Conf, a.Settings
	if op == "snapshot" {
		// The snapshot answers before any parse: broken settings still snapshot,
		// the way a save still compares the bytes it started from.
		confBytes, err := content(conf)
		if err != nil {
			return 0, "", err
		}
		settingsBytes, err := content(settings)
		if err != nil {
			return 0, "", err
		}
		if confBytes == nil {
			return 0, "", fmt.Errorf("profile no longer exists")
		}
		fmt.Fprintln(out, fingerprint(confBytes, settingsBytes))
		return 0, "", nil
	}
	s, err := readStoreState(a)
	if err != nil {
		return 0, "", err
	}
	doc, defaults, projected, originals, lines := s.doc, s.defaults, s.projected, s.originals, s.lines
	if op == "get" {
		if err := checkSavedEfforts(conf, defaults); err != nil {
			return 0, "", err
		}
		v, _ := defaults.Get(key)
		if v == nil {
			v = ""
		}
		fmt.Fprintln(out, v)
		return 0, "", nil
	}
	if op == "model-settings" {
		if err := checkSavedEfforts(conf, defaults); err != nil {
			return 0, "", err
		}
		fmt.Fprintln(out, jsonOut(projected))
		return 0, "", nil
	}
	if expected := a.Change.Snapshot; expected != "" && expected != fingerprint(originals[0], originals[1]) {
		return 0, "", fmt.Errorf("profile changed while configuring; nothing saved")
	}
	for _, name := range []string{"model", "effort"} {
		value := map[string]string{"model": a.Change.Model, "effort": a.Change.Effort}[name]
		if value == "" {
			continue
		}
		if name == "model" && !modelIDShape(value) {
			return 0, "", refuse("model must be a nonempty model id, not a flag or picker token")
		}
		if name == "effort" && !slices.Contains(profile.Efforts, value) {
			return 0, "", refuse("effort must be %s", profile.EffortList())
		}
		defaults.Set(name, value)
		if name == "model" {
			defaults.Set("model_source", "") // a saved model is the person's own choice; the imported marker goes
		}
	}
	switch op {
	case "set", "configure", "seed-model-defaults", "prepare-launch", "migrate-native":
	default:
		return 0, "", fmt.Errorf("unsupported configuration operation")
	}
	// The save is judged on the defaults it will leave behind: the change above
	// has already replaced the value it overwrites, so a set that repairs an
	// invalid saved effort passes, while one that leaves it in place names it.
	if err := checkSavedEfforts(conf, defaults); err != nil {
		return 0, "", err
	}
	if paths.IsSymlink(conf) || paths.IsSymlink(settings) {
		return 0, "", fmt.Errorf("model defaults require profile-local files")
	}
	changes := ordjson.Clone(defaults).(*ordjson.Obj)
	changes.Set("model_defaults", "1")
	if op == "configure" {
		switch a.Change.Type {
		case "apikey":
			changes.Set("base_url", a.Change.Endpoint)
			changes.Set("api_key", a.Change.Credential)
		case "token":
			changes.Set("token", a.Change.Credential)
		}
	}
	for _, name := range changes.Keys() {
		if v, _ := changes.Get(name); strings.ContainsAny(fmt.Sprint(v), "\r\n") {
			return 0, "", fmt.Errorf("configuration values cannot contain CR/LF")
		}
	}
	var kept []string
	seen := map[string]bool{}
	written := map[string]bool{}
	for _, line := range lines {
		k, _, hasValue := strings.Cut(line, "=")
		if hasValue && !strings.HasPrefix(strings.TrimLeft(line, " \t\r"), "#") {
			if v, changed := changes.Get(k); changed {
				// Rewrite a key on its first line so its comment stays with it (#1031).
				if !written[k] && ordjson.Truthy(v) {
					kept = append(kept, fmt.Sprintf("%s=%v", k, v))
					written[k] = true
				}
				continue
			}
			// Identical lines are one fact; comments and differing lines stay.
			if seen[line] {
				continue
			}
			seen[line] = true
		}
		kept = append(kept, line)
	}
	// Put new keys before trailing comments so those comments stay at EOF.
	// CanonicalConf can now sort each key together with its own comments.
	at := len(kept)
	for at > 0 {
		bare := strings.Trim(kept[at-1], " \t\r")
		if bare != "" && !strings.HasPrefix(bare, "#") {
			break
		}
		at--
	}
	var added []string
	for _, name := range changes.Keys() {
		if v, _ := changes.Get(name); !written[name] && ordjson.Truthy(v) {
			added = append(added, fmt.Sprintf("%s=%v", name, v))
		}
	}
	kept = slices.Insert(kept, at, added...)
	proposed := []byte(ctxwindow.CanonicalConf(strings.Join(kept, "\n")))

	type target struct {
		path          string
		before, after []byte
	}
	var targets []target
	if !sameBytes(proposed, originals[0]) {
		targets = append(targets, target{conf, originals[0], proposed})
	}
	if op == "prepare-launch" {
		doc.Delete("model")
		doc.Delete("effortLevel")
		rows, err := objField(doc, "modelSettings", "modelSettings must be an object")
		if err != nil {
			return 0, "", err
		}
		for _, k := range rows.Keys() {
			rv, _ := rows.Get(k)
			if row := asObj(rv); row != nil {
				row.Delete("effortLevel")
			}
		}
		if env := asObj(getKey(doc, "env")); env != nil {
			for _, name := range append([]string{"ANTHROPIC_MODEL", "CLAUDE_CODE_EFFORT_LEVEL"}, mappingKeys...) {
				env.Delete(name)
			}
		}
		if layerWrite && originals[1] != nil {
			orig, _ := ordjson.Parse(ordjson.StripBOM(originals[1]))
			if !ordjson.Equal(doc, orig) {
				targets = append(targets, target{settings, originals[1], []byte(string(ordjson.Marshal(doc, true)) + "\n")})
			}
		}
	}

	type staged struct {
		target
		temp string
	}
	var pending []staged
	var applied []target
	stage := func(path string, data []byte) (string, error) {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return "", err
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".configure.*")
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		f.Close()
		if werr != nil {
			os.Remove(f.Name())
			return "", werr
		}
		return f.Name(), nil
	}
	cleanup := func() {
		for _, p := range pending {
			os.Remove(p.temp)
		}
	}
	commit := func() error {
		for _, t := range targets {
			temp, err := stage(t.path, t.after)
			if err != nil {
				return err
			}
			pending = append(pending, staged{t, temp})
		}
		if c, err := content(conf); err != nil || !sameBytes(c, originals[0]) {
			return errors.Join(err, errors.New("profile changed while configuring; nothing saved"))
		}
		if layerWrite {
			if c, err := content(settings); err != nil || !sameBytes(c, originals[1]) {
				return errors.Join(err, errors.New("profile changed while configuring; nothing saved"))
			}
		}
		for _, p := range pending {
			if c, err := content(p.path); err != nil || !sameBytes(c, p.before) {
				return errors.Join(err, fmt.Errorf("%s changed while saving", filepath.Base(p.path)))
			}
			if err := replaceFile(p.temp, p.path); err != nil {
				return err
			}
			applied = append(applied, p.target)
		}
		return nil
	}
	if cerr := commit(); cerr != nil {
		for i := len(applied) - 1; i >= 0; i-- {
			a := applied[i]
			if c, err := content(a.path); err != nil || !sameBytes(c, a.after) {
				fmt.Fprintf(errw, "ccmorph: %s changed externally; left untouched during rollback\n", filepath.Base(a.path))
				continue
			}
			if a.before == nil {
				os.Remove(a.path)
				continue
			}
			tmp, err := stage(a.path, a.before)
			if err == nil {
				err = os.Rename(tmp, a.path)
				os.Remove(tmp)
			}
			if err != nil {
				cleanup()
				return 0, "", err
			}
		}
		cleanup()
		return 0, "", cerr
	}
	cleanup()
	switch {
	case op == "prepare-launch":
		if materialize == "" {
			fmt.Fprintln(out, jsonOut(projected))
		}
	case op == "seed-model-defaults" && key == "model":
		v, _ := defaults.Get("model")
		if v == nil {
			v = ""
		}
		fmt.Fprintln(out, v)
	}
	return 0, jsonOut(projected), nil
}
