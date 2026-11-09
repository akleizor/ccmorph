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

// effortList names the accepted levels as prose — "low, medium, high, xhigh or
// max" — for the refusal's one sentence.
func effortList() string {
	ls := profile.Efforts
	return strings.Join(ls[:len(ls)-1], ", ") + " or " + ls[len(ls)-1]
}

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
}

// ConfigStore runs the operation. The result is its text and its status: 0 done,
// 1 failed (reasons to errw), 10+n settings-compose's own status n.
func ConfigStore(a StoreArgs, errw io.Writer) (string, int) {
	op, materialize := a.Op, a.Materialize
	var out strings.Builder
	exit, projectedJSON, err := configStoreRun(a, &out, errw)
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
	if rc := settingscompose.Main(argv, errw); rc != 0 {
		return out.String(), 10 + rc
	}
	return out.String(), 0
}

func configStoreRun(a StoreArgs, out, errw io.Writer) (int, string, error) {
	op, key, sharedName, mappingKeys, materialize := a.Op, a.Key, a.Shared, a.Mapping, a.Materialize
	conf, settings := a.Conf, a.Settings
	confBytes, err := content(conf)
	if err != nil {
		return 0, "", err
	}
	settingsBytes, err := content(settings)
	if err != nil {
		return 0, "", err
	}
	originals := [2][]byte{confBytes, settingsBytes}
	if originals[0] == nil {
		return 0, "", fmt.Errorf("profile no longer exists")
	}
	if op == "snapshot" {
		fmt.Fprintln(out, fingerprint(originals[0], originals[1]))
		return 0, "", nil
	}
	doc := ordjson.New()
	if originals[1] != nil {
		v, err := ordjson.Parse(stripBOM(originals[1]))
		if err != nil {
			return 0, "", err
		}
		d, ok := v.(*ordjson.Obj)
		if !ok {
			return 0, "", fmt.Errorf("settings.json and its env must be objects")
		}
		doc = d
	}
	if _, err := objField(doc, "env", "settings.json and its env must be objects"); err != nil {
		return 0, "", err
	}

	confText, err := decodeUTF8(originals[0])
	if err != nil {
		return 0, "", err
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
		if name == "model" || name == "effort" || strings.HasPrefix(name, "effort.") || mapped[name] {
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
			return 0, "", err
		}
		if len(sb) > 0 {
			v, err := ordjson.Parse(stripBOM(sb))
			if err != nil {
				return 0, "", err
			}
			s, ok := v.(*ordjson.Obj)
			if !ok {
				return 0, "", fmt.Errorf("shared settings and its env must be objects")
			}
			shared = s
		}
		if _, err := objField(shared, "env", "shared settings and its env must be objects"); err != nil {
			return 0, "", err
		}
		native := dictMerge(shared, doc)
		rows, err := objField(native, "modelSettings", "modelSettings must be an object")
		if err != nil {
			return 0, "", err
		}
		env := asObj(getKey(native, "env"))
		if env == nil {
			env = ordjson.New()
		}
		chosen := getKey(env, "ANTHROPIC_MODEL")
		if !truthy(chosen) {
			chosen = getKey(native, "model")
		}
		if truthy(chosen) {
			setDefault("model", chosen)
		}
		if lvl := getKey(env, "CLAUDE_CODE_EFFORT_LEVEL"); truthy(lvl) {
			setDefault("effort", lvl)
		} else if lvl := getKey(native, "effortLevel"); truthy(lvl) {
			setDefault("effort", lvl)
		}
		for _, model := range rows.Keys() {
			rv, _ := rows.Get(model)
			if row := asObj(rv); row != nil && truthy(getKey(row, "effortLevel")) {
				setDefault("effort."+model, getKey(row, "effortLevel"))
			}
		}
		for _, name := range mappingKeys {
			if v := getKey(env, name); truthy(v) {
				setDefault("env."+name, v)
			}
		}
	}
	modelSettings := ordjson.New()
	for _, name := range defaults.Keys() {
		v, _ := defaults.Get(name)
		value, isStr := v.(string)
		if !isStr || strings.Contains(name, "=") || strings.ContainsAny(name+value, "\r\n") || hasSpace(name) {
			return 0, "", fmt.Errorf("model defaults must be single-line strings with valid keys")
		}
		if name == "effort" || strings.HasPrefix(name, "effort.") {
			if !slices.Contains(profile.Efforts, value) || name == "effort." {
				return 0, "", fmt.Errorf("invalid saved effort default")
			}
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
		if v, _ := defaults.Get(f[0]); truthy(v) {
			projected.Set(f[1], v)
		}
	}
	if op == "get" {
		v, _ := defaults.Get(key)
		if v == nil {
			v = ""
		}
		fmt.Fprintln(out, v)
		return 0, "", nil
	}
	if op == "model-settings" {
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
		if name == "model" && (strings.HasPrefix(value, "-") || strings.HasPrefix(value, "@") || value == "?" || hasSpace(value)) {
			return 0, "", fmt.Errorf("model must be a nonempty model id, not a flag or picker token")
		}
		if name == "effort" && !slices.Contains(profile.Efforts, value) {
			return 0, "", refuse("effort must be %s", effortList())
		}
		defaults.Set(name, value)
	}
	switch op {
	case "set", "configure", "seed-model-defaults", "prepare-launch", "migrate-native":
	default:
		return 0, "", fmt.Errorf("unsupported configuration operation")
	}
	if isSymlink(conf) || isSymlink(settings) {
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
	for _, line := range lines {
		k, _, _ := strings.Cut(line, "=")
		if !changes.Has(k) {
			kept = append(kept, line)
		}
	}
	for _, name := range changes.Keys() {
		if v, _ := changes.Get(name); truthy(v) {
			kept = append(kept, fmt.Sprintf("%s=%v", name, v))
		}
	}
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
		if originals[1] != nil {
			orig, _ := ordjson.Parse(stripBOM(originals[1]))
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
		if c, err := content(settings); err != nil || !sameBytes(c, originals[1]) {
			return errors.Join(err, errors.New("profile changed while configuring; nothing saved"))
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
