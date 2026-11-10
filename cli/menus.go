package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

// profileRows is _ccm_profile_rows: ID<TAB>kind<TAB>detail<TAB>marker per profile.
func (l *launcher) profileRows(active string, types []string) []string {
	var rows []string
	for _, n := range l.confNames() {
		f, ok := l.readRaw(n)
		if !ok {
			say("ccmorph: skipping invalid profile '" + n + "'")
			continue
		}
		typ := f["type"]
		if len(types) > 0 && !slices.Contains(types, typ) {
			continue
		}
		kind, detail := profile.KindLabel(typ, f["provider"]), ""
		switch typ {
		case "sub":
			detail = f["email"]
		case "token":
		case "provider":
			kind = providerLabel(f["provider"])
			detail = l.accountModel(n)
			if detail == "" {
				detail = "Browser login"
			}
		case "apikey":
			detail = f["base_url"]
			if detail == "" {
				detail = "api.anthropic.com"
			}
			if m := l.accountModel(n); m != "" {
				detail += " → " + m
			}
		}
		marker := ""
		if n == active {
			marker = "default"
		}
		rows = append(rows, n+"\t"+kind+"\t"+strings.ReplaceAll(detail, "\t", " ")+"\t"+marker)
	}
	return rows
}

// menuProfile is _ccm_menu_profile: the profile chooser, limited to types when given.
func (l *launcher) menuProfile(types ...string) (string, bool) {
	rows := l.profileRows(l.active(), types)
	if len(rows) == 0 {
		switch {
		case len(l.confNames()) == 0:
			l.err(noProfilesMsg)
		case len(types) > 0:
			l.err("no compatible profiles for this command. Run: ccmorph add <profile>")
		default:
			l.err("no saved profiles. Run: ccmorph add <profile>")
		}
		return "", false
	}
	v, rc := uiSelect("search", "Profile", strings.Join(rows, "\n"), "all")
	return v, rc == 0 && v != ""
}

// modelRows is the picker's text for gateway models: one
// "<id>\t<owner or display name>" per line.
func modelRows(rows []util.ModelRow) string {
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.ID + "\t" + r.Label
	}
	return strings.Join(lines, "\n")
}

const nativeAliases = "fable\tClaude Code Fable alias\nopus\tClaude Code Opus alias\nsonnet\tClaude Code Sonnet alias\nhaiku\tClaude Code Haiku alias"

// effortRows mirrors profile.Efforts, lowest to highest, one "<level>\t<description>"
// per line (#264).
const effortRows = "low\tFastest, least thorough\nmedium\tBalanced\nhigh\tMore reasoning\nxhigh\tEven more reasoning\nmax\tMost reasoning, slowest"

// valueRows is _ccm_value_rows. base and key, when set, preview an unsaved endpoint.
func (l *launcher) valueRows(slot, name string, preview ...string) (string, bool) {
	switch slot {
	case "model":
		f, ok := l.readRaw(name)
		if !ok {
			return "", false
		}
		switch f["type"] {
		case "apikey":
			listing, ok := l.modelsJSON(name, preview...)
			if !ok {
				return "", false
			}
			rows, ok := util.GwModels(l.conf(name), true, listing)
			if !ok {
				l.err("'" + name + "' endpoint returned no usable model list")
				return "", false
			}
			return modelRows(rows), true
		case "sub", "token":
			return nativeAliases, true
		case "provider":
			var rows []string
			for _, v := range util.SavedProviderModels(filepath.Join(l.profileDirs, name, ".claude.json")) {
				rows = append(rows, v+"\tSaved provider model")
			}
			return strings.Join(rows, "\n"), true
		}
		return "", true
	case "effort":
		return effortRows, true
	}
	l.err("no picker for '" + slot + "'")
	return "", false
}

// effortLabel is the picker's label for one effort level: the plain name,
// naming beside it the value the commands accept (#367).
func effortLabel(value string) string {
	label := strings.ToUpper(value[:1]) + strings.ToLower(value[1:])
	if value == "xhigh" {
		label = "Extra high (xhigh)"
	}
	return label
}

// chooseValue is _ccm_choose_value: effort is a fixed menu, models are searchable.
func chooseValue(slot, prompt, rows, display string) (string, int) {
	if slot != "effort" {
		return uiSelect("search", prompt, rows, display)
	}
	var labeled []string
	for _, row := range splitRows(rows) {
		value := rowID(row)
		if !strings.HasPrefix(value, "@") {
			detail := strings.TrimPrefix(row, value+"\t")
			if display == "labels" {
				if _, d, ok := strings.Cut(detail, "\t"); ok {
					detail = d
				}
			}
			row = value + "\t" + effortLabel(value) + "\t" + detail
		}
		labeled = append(labeled, row)
	}
	return uiSelect("native", prompt, strings.Join(labeled, "\n"), "labels")
}

func title(s string) string { return strings.ToUpper(s[:1]) + s[1:] }

// pickValue is _ccm_pick_value: choose a model or effort for a saved profile.
func (l *launcher) pickValue(slot, name string) (string, bool) {
	rows, ok := l.valueRows(slot, name)
	if !ok {
		return "", false
	}
	if rows == "" {
		l.err("No saved model list. Use: ccmorph config " + name + " to enter a custom ID.")
		return "", false
	}
	v, rc := chooseValue(slot, title(slot)+" — "+name, rows, "all")
	if rc != 0 {
		// A leave is answered by the picker's own Cancelled line; the usage
		// line belongs to the headless refusal, where no picker ever ran and
		// the caller needs the direct command spelled out.
		if rc == 2 {
			say("Use: ccmorph set " + name + " " + slot + " <" + slot + ">")
		}
		return "", false
	}
	return v, v != ""
}

// pickLaunchModel is the model picker of `run -i`: the saved model first, as
// the Keep row `set model -i` opens on, so Enter alone means the saved model
// (#377). The custom-ID escape stays with the commands that can save one; a
// profile with nothing saved opens the plain catalogue, titled so.
func (l *launcher) pickLaunchModel(name, saved string) (string, bool) {
	rows, ok := l.valueRows("model", name)
	if !ok {
		return "", false
	}
	if rows == "" {
		l.err("No saved model list. Use: ccmorph config " + name + " to enter a custom ID.")
		return "", false
	}
	heading := "Model — " + name
	if saved == "" {
		heading += " (none saved)"
	}
	choice, rc := chooseValue("model", heading, strings.Join(menuRows("model", saved, "", "", rows, saved != "", false), "\n"), "labels")
	if rc != 0 {
		// A leave is answered by the picker's own Cancelled line; the usage
		// line belongs to the headless refusal, where no picker ever ran and
		// the caller needs the direct command spelled out.
		if rc == 2 {
			say("Use: ccmorph set " + name + " model <model>")
		}
		return "", false
	}
	if choice == "@keep" {
		return saved, true
	}
	return choice, choice != ""
}

// defaultRows is the catalogue a default picker opens on, saying so when a
// model list cannot be read. base and key, when set, preview an unsaved endpoint.
func (l *launcher) defaultRows(slot, name, typ string, preview ...string) string {
	if slot == "model" && typ == "apikey" {
		say("Loading models from the selected endpoint…")
	}
	rows, _ := l.valueRows(slot, name, preview...)
	if slot == "model" && rows == "" {
		say("Model list unavailable. Keep the current default or enter a custom model ID.")
	}
	return rows
}

// menuRows is the list every model menu opens on: the current value first, as
// the Keep row, then the catalogue without it. keep offers that row even when
// nothing is set, as config can keep the unsaved state — worded as what
// keeping it means at launch (#381); custom adds the custom-ID escape, which
// belongs to the commands that can save one.
func menuRows(slot, current, typ, base, rows string, keep, custom bool) []string {
	menu := []string{}
	if keep {
		shown := current
		if shown == "" {
			shown = unsavedLabel(slot, typ, base)
		}
		menu = append(menu, "@keep\tKeep "+shown)
	}
	for _, row := range splitRows(rows) {
		value := rowID(row)
		if value == current {
			continue
		}
		menu = append(menu, value+"\t"+value+"\t"+strings.TrimPrefix(row, value+"\t"))
	}
	if custom && slot == "model" {
		menu = append(menu, "@custom\tEnter a custom model ID…")
	}
	return menu
}

// valueMenu is the menu every default picker opens: the current value first,
// the catalogue without it, and a custom-ID escape for models. keep offers the
// keep row even when nothing is set, as config can keep the unsaved state —
// worded as what keeping it means at launch (#381).
func (l *launcher) valueMenu(slot, name, current, typ, base, rows string, keep bool) (string, bool) {
	menu := menuRows(slot, current, typ, base, rows, keep, true)
	choice, rc := chooseValue(slot, "Default "+slot+" — "+name, strings.Join(menu, "\n"), "labels")
	if rc != 0 {
		return "", false
	}
	switch choice {
	case "@keep":
		return current, true
	case "@custom":
		answer, rc := uiInput("Custom model ID (Enter keeps current): ", false)
		if rc != 0 {
			return "", false
		}
		if answer == "" {
			answer = current
		}
		return answer, true
	}
	return choice, true
}

// pickModelDefault is the picker of `set <profile> model`: the same list, opened
// the same way, as the model step of config, with the saved model marked as the
// row to keep (#279).
func (l *launcher) pickModelDefault(name string) (string, bool) {
	f, ok := l.readRaw(name)
	if !ok {
		return "", false
	}
	current := l.accountModel(name)
	// A leave is answered by the picker's own Cancelled line (#280): no usage line here.
	return l.valueMenu("model", name, current, f["type"], f["base_url"], l.defaultRows("model", name, f["type"]), current != "")
}

func say(msg string) { fmt.Fprintln(os.Stderr, msg) }
