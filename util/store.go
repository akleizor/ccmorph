package util

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/modelwindow"
	"github.com/akleizor/ccmorph/pycompat"
	"github.com/akleizor/ccmorph/settingscompose"
)

// stateFile is one file read as bytes; nil means it does not exist.
func content(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		if pycompat.IsNotExist(err) {
			return nil
		}
		pycompat.RaiseOS(err)
	}
	if b == nil {
		b = []byte{}
	}
	return b
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

func decodeUTF8(b []byte) string {
	if !utf8.Valid(b) {
		_, err := pycompat.Parse(b)
		pycompat.RaiseValue("%s", err.Error())
	}
	return string(b)
}

// dictMerge is the store's recursive dict merge: objects merge, anything else replaces.
func dictMerge(base, override *pycompat.Obj) *pycompat.Obj {
	result := pycompat.NewObj()
	for _, k := range base.Keys() {
		v, _ := base.Get(k)
		result.Set(k, v)
	}
	for _, k := range override.Keys() {
		v, _ := override.Get(k)
		cur, _ := result.Get(k)
		co, cok := cur.(*pycompat.Obj)
		vo, vok := v.(*pycompat.Obj)
		if cok && vok {
			result.Set(k, dictMerge(co, vo))
		} else {
			result.Set(k, v)
		}
	}
	return result
}

func objField(o *pycompat.Obj, key string, msg string) *pycompat.Obj {
	v, has := o.Get(key)
	if !has {
		return pycompat.NewObj()
	}
	r, ok := v.(*pycompat.Obj)
	if !ok {
		pycompat.RaiseValue("%s", msg)
	}
	return r
}

func hasSpace(s string) bool {
	for _, c := range s {
		if pycompat.IsSpace(c) {
			return true
		}
	}
	return false
}

// replaceFile is the commit step of a save; a test seam for a failing replacement.
var replaceFile = os.Rename

func configStore(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) < 5 {
		fmt.Fprintln(errw, "usage: util config-store <op> <conf> <settings> <key> <shared> [mapping keys...]")
		return 2
	}
	op, confName, settingsName, key, sharedName := args[0], args[1], args[2], args[3], args[4]
	mappingKeys := args[5:]
	materialize := os.Getenv("_CCM_MATERIALIZE")
	var projectedJSON string
	exit := 0
	e := pycompat.Catch(func() {
		exit, projectedJSON = configStoreRun(op, confName, settingsName, key, sharedName, mappingKeys, materialize, out, errw)
	})
	if e != nil {
		switch e.Kind {
		case pycompat.OSError, pycompat.ValueError:
			fmt.Fprintf(errw, "ccmorph: could not save/read configuration: %s\n", e.Msg)
		default:
			fmt.Fprintf(errw, "%s: %s\n", e.Class, e.Msg)
		}
		return 1
	}
	if exit != 0 || materialize == "" || op != "prepare-launch" {
		return exit
	}
	// settings-compose's own statuses, shifted past the store's: exit 1 still means
	// "the store failed, do not start".
	argv := append(strings.Split(materialize, "\n"), "--saved", projectedJSON)
	if rc := settingscompose.Main(argv, errw); rc != 0 {
		return 10 + rc
	}
	return 0
}

func configStoreRun(op, confName, settingsName, key, sharedName string, mappingKeys []string, materialize string, out, errw io.Writer) (int, string) {
	conf, settings := confName, settingsName
	originals := [2][]byte{content(conf), content(settings)}
	if originals[0] == nil {
		pycompat.RaiseValue("profile no longer exists")
	}
	if op == "snapshot" {
		fmt.Fprintln(out, fingerprint(originals[0], originals[1]))
		return 0, ""
	}
	doc := pycompat.NewObj()
	if originals[1] != nil {
		v, err := pycompat.Parse(pycompat.StripBOM(originals[1]))
		if err != nil {
			pycompat.RaiseValue("%s", err.Error())
		}
		d, ok := v.(*pycompat.Obj)
		if !ok {
			pycompat.RaiseValue("settings.json and its env must be objects")
		}
		doc = d
	}
	objField(doc, "env", "settings.json and its env must be objects")

	lines := strings.Split(decodeUTF8(originals[0]), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	fields := pycompat.NewOMap[string]()
	for _, line := range lines {
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
			fields.Set(k, v)
		}
	}
	mapped := map[string]bool{}
	for _, k := range mappingKeys {
		mapped["env."+k] = true
	}
	defaults := pycompat.NewOMap[pycompat.Value]()
	for _, name := range fields.Keys() {
		if name == "model" || name == "effort" || strings.HasPrefix(name, "effort.") || mapped[name] {
			v, _ := fields.Get(name)
			defaults.Set(name, v)
		}
	}
	setDefault := func(name string, v pycompat.Value) {
		if !defaults.Has(name) {
			defaults.Set(name, v)
		}
	}
	if md, _ := fields.Get("model_defaults"); md != "1" {
		shared := pycompat.NewObj()
		if sb := content(sharedName); len(sb) > 0 {
			v, err := pycompat.Parse(pycompat.StripBOM(sb))
			if err != nil {
				pycompat.RaiseValue("%s", err.Error())
			}
			s, ok := v.(*pycompat.Obj)
			if !ok {
				pycompat.RaiseValue("shared settings and its env must be objects")
			}
			shared = s
		}
		objField(shared, "env", "shared settings and its env must be objects")
		native := dictMerge(shared, doc)
		rows := objField(native, "modelSettings", "modelSettings must be an object")
		env := asObj(getKey(native, "env"))
		if env == nil {
			env = pycompat.NewObj()
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
	modelSettings := pycompat.NewObj()
	for _, name := range defaults.Keys() {
		v, _ := defaults.Get(name)
		value, isStr := v.(string)
		if !isStr || strings.Contains(name, "=") || strings.ContainsAny(name+value, "\r\n") || hasSpace(name) {
			pycompat.RaiseValue("model defaults must be single-line strings with valid keys")
		}
		if name == "effort" || strings.HasPrefix(name, "effort.") {
			switch value {
			case "low", "medium", "high", "xhigh", "max":
			default:
				pycompat.RaiseValue("invalid saved effort default")
			}
			if name == "effort." {
				pycompat.RaiseValue("invalid saved effort default")
			}
		}
		if strings.HasPrefix(name, "effort.") {
			row := pycompat.NewObj()
			row.Set("effortLevel", value)
			modelSettings.Set(name[7:], row)
		}
	}
	projEnv := pycompat.NewObj()
	for _, name := range defaults.Keys() {
		if strings.HasPrefix(name, "env.") {
			v, _ := defaults.Get(name)
			projEnv.Set(name[4:], v)
		}
	}
	projected := pycompat.NewObj()
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
		return 0, ""
	}
	if op == "model-settings" {
		fmt.Fprintln(out, jsonOut(projected))
		return 0, ""
	}
	if expected := os.Getenv("_CCM_CONFIG_SNAPSHOT"); expected != "" && expected != fingerprint(originals[0], originals[1]) {
		pycompat.RaiseValue("profile changed while configuring; nothing saved")
	}
	for _, name := range []string{"model", "effort"} {
		value := os.Getenv("_CCM_CONFIG_" + strings.ToUpper(name))
		if value == "" {
			continue
		}
		if name == "model" && (strings.HasPrefix(value, "-") || strings.HasPrefix(value, "@") || value == "?" || hasSpace(value)) {
			pycompat.RaiseValue("model must be a nonempty model id, not a flag or picker token")
		}
		if name == "effort" && value != "low" && value != "medium" && value != "high" && value != "xhigh" {
			pycompat.RaiseValue("effort must be low, medium, high or xhigh")
		}
		defaults.Set(name, value)
	}
	switch op {
	case "set", "configure", "seed-model-defaults", "prepare-launch", "migrate-native":
	default:
		pycompat.RaiseValue("unsupported configuration operation")
	}
	if isSymlink(conf) || isSymlink(settings) {
		pycompat.RaiseValue("model defaults require profile-local files")
	}
	changes := defaults.Copy()
	changes.Set("model_defaults", "1")
	if op == "configure" {
		must := func(k string) string {
			v, ok := os.LookupEnv(k)
			if !ok {
				pycompat.Crash("KeyError", "'%s'", k)
			}
			return v
		}
		switch must("_CCM_CONFIG_TYPE") {
		case "apikey":
			changes.Set("base_url", must("_CCM_CONFIG_ENDPOINT"))
			changes.Set("api_key", must("_CCM_CONFIG_CREDENTIAL"))
		case "token":
			changes.Set("token", must("_CCM_CONFIG_CREDENTIAL"))
		}
	}
	for _, name := range changes.Keys() {
		if v, _ := changes.Get(name); strings.ContainsAny(fmt.Sprint(v), "\r\n") {
			pycompat.RaiseValue("configuration values cannot contain CR/LF")
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
	proposed := []byte(modelwindow.CanonicalConf(strings.Join(kept, "\n")))

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
		rows := objField(doc, "modelSettings", "modelSettings must be an object")
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
			orig, _ := pycompat.Parse(pycompat.StripBOM(originals[1]))
			if !pycompat.Equal(doc, orig) {
				targets = append(targets, target{settings, originals[1], []byte(pycompat.Marshal(doc, pycompat.Format{Indent: 2}) + "\n")})
			}
		}
	}

	type staged struct {
		target
		temp string
	}
	var pending []staged
	var applied []target
	stage := func(path string, data []byte) string {
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			pycompat.RaiseOS(err)
		}
		f, err := os.CreateTemp(filepath.Dir(path), ".configure.*")
		if err != nil {
			pycompat.RaiseOS(err)
		}
		_, werr := f.Write(data)
		f.Close()
		if werr != nil {
			os.Remove(f.Name())
			pycompat.RaiseOS(werr)
		}
		return f.Name()
	}
	cleanup := func() {
		for _, p := range pending {
			os.Remove(p.temp)
		}
	}
	func() {
		defer func() {
			r := recover()
			if r == nil {
				cleanup()
				return
			}
			for i := len(applied) - 1; i >= 0; i-- {
				a := applied[i]
				if !sameBytes(content(a.path), a.after) {
					fmt.Fprintf(errw, "ccmorph: %s changed externally; left untouched during rollback\n", filepath.Base(a.path))
					continue
				}
				if a.before == nil {
					os.Remove(a.path)
				} else {
					tmp := stage(a.path, a.before)
					err := os.Rename(tmp, a.path)
					os.Remove(tmp)
					if err != nil {
						cleanup()
						pycompat.RaiseOS(err)
					}
				}
			}
			cleanup()
			panic(r)
		}()
		for _, t := range targets {
			pending = append(pending, staged{t, stage(t.path, t.after)})
		}
		if !sameBytes(content(conf), originals[0]) || !sameBytes(content(settings), originals[1]) {
			pycompat.RaiseValue("profile changed while configuring; nothing saved")
		}
		for _, p := range pending {
			if !sameBytes(content(p.path), p.before) {
				pycompat.RaiseValue("%s changed while saving", filepath.Base(p.path))
			}
			if err := replaceFile(p.temp, p.path); err != nil {
				pycompat.RaiseOS(err)
			}
			applied = append(applied, p.target)
		}
	}()
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
	return 0, jsonOut(projected)
}
