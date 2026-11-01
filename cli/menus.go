package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		if len(types) > 0 && !contains(types, typ) {
			continue
		}
		kind, detail := typ, ""
		switch typ {
		case "sub":
			kind, detail = "Claude account", f["email"]
		case "token":
			kind = "Claude token"
		case "provider":
			kind = providerLabel(f["provider"])
			detail = l.accountModel(n)
			if detail == "" {
				detail = "Browser login"
			}
		case "apikey":
			kind = "API key"
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
		if len(types) > 0 {
			l.err("no compatible profiles for this command. Run: ccmorph add <profile>")
		} else {
			l.err("no saved profiles. Run: ccmorph add <profile>")
		}
		return "", false
	}
	v, rc := uiSelect("search", "Profile", strings.Join(rows, "\n"), "all")
	return v, rc == 0 && v != ""
}

const nativeAliases = "fable\tClaude Code Fable alias\nopus\tClaude Code Opus alias\nsonnet\tClaude Code Sonnet alias\nhaiku\tClaude Code Haiku alias"
const effortRows = "low\tFastest, least thorough\nmedium\tBalanced\nhigh\tMore reasoning\nxhigh\tMost reasoning, slowest"

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
			out, rc := l.util(nil, listing+"\n", "gw-models", l.conf(name), "picker")
			if rc != 0 {
				l.err("'" + name + "' endpoint returned no usable model list")
				return "", false
			}
			return out, true
		case "sub", "token":
			return nativeAliases, true
		case "provider":
			out, rc := l.util(nil, "", "saved-provider-models", filepath.Join(l.profileDirs, name, ".claude.json"))
			return out, rc == 0
		}
		return "", true
	case "effort":
		return effortRows, true
	}
	l.err("no picker for '" + slot + "'")
	return "", false
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
			label := strings.ToUpper(value[:1]) + strings.ToLower(value[1:])
			if value == "xhigh" {
				label = "Extra high"
			}
			row = value + "\t" + label + "\t" + detail
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
		say("Use: ccmorph set " + name + " " + slot + " <" + slot + ">")
		return "", false
	}
	return v, v != ""
}

func say(msg string) { fmt.Fprintln(os.Stderr, msg) }
