package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
)

// context is `ccmorph context <profile> <model> [tokens|auto]`.
func (l *launcher) context(args []string) int {
	name, model := "", ""
	if len(args) > 0 {
		name = args[0]
	}
	if len(args) > 1 {
		model = args[1]
	}
	if name == "" || name == "-i" || name == "--interactive" {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		l.err("usage: " + usageLine("context"))
		return 2
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	if model == "" || model == "-i" || model == "--interactive" {
		picked, ok := l.pickValue("model", name)
		if !ok {
			return 1
		}
		model = picked
	}
	rest := []string{}
	if len(args) > 2 {
		rest = args[2:]
	}
	if len(rest) > 1 {
		l.err("usage: ccmorph context <profile> <model> [tokens|auto]")
		return 2
	}
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"),
		append([]string{"model-window", "--context", name, model}, rest...)...)
	cmd.Env = append(os.Environ(), "CCMORPH_STATE_DIR="+l.dir)
	cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	if len(rest) > 0 {
		cmd.Stdout = os.Stdout
		rc := runRC(cmd)
		// The save is done; a model the route does not carry — its listing for a
		// gateway, the record its probe fills for a provider — is named after it
		// (#370), the way the read form names a model it cannot answer auto for
		// (#268). A listing that cannot be read says nothing, and the warning
		// never fails the save.
		if rc == 0 {
			_, cols := termSize()
			for _, line := range warnLines(l.unlistedWarning(name, model, rest[0]), cols) {
				fmt.Fprintln(os.Stderr, line)
			}
		}
		return rc
	}
	// The read form captures the core's answer: "auto" promises discovery will
	// size the model, so a model the route does not serve is named instead of
	// reading as auto (#268). A pin for an unlisted model still reads back, and
	// a route whose listing cannot be read keeps answering auto.
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 127
	}
	answer := strings.TrimRight(string(out), "\n")
	if answer == "auto" && l.listingKnown(name, model) == 1 {
		l.err(model + " is not served by " + name + ". Run: ccmorph models " + name)
		return 1
	}
	fmt.Println(answer)
	return 0
}

// unlistedWarning is what the set form says when the route does not carry the
// model just pinned (#370): the old check read the discovered table, which
// stays empty whenever nothing could be sized, so a model the gateway lists was
// warned about like a made-up name. "" says nothing: a non-numeric value pins
// no limit, a listing that cannot be read does not rule the model out, and a
// provider is asked through the record its probe fills, not the listing its own
// credentials cannot read.
func (l *launcher) unlistedWarning(name, model, value string) string {
	if _, numeric := ctxwindow.ParseTokens(value); !numeric {
		return ""
	}
	f, _ := l.readQuiet(name)
	if f["type"] == "provider" {
		_, merged, _ := ctxwindow.ReadConf(l.conf(name)) // carries the probe's record
		if ctxwindow.RouteKnown(merged, model) {
			return ""
		}
		return unlistedWarningText(model, name, "provider")
	}
	if !l.contextUnlisted(name, model, f) {
		return ""
	}
	return unlistedWarningText(model, name, f["type"])
}

// contextUnlisted is whether the route's own listing — the one `ccmorph models`
// prints — does not carry want (#370). It asks with the id as spelled, so a
// model the map rewrites still counts through its target; util.ModelServed
// keeps judging the target, because that is what a session runs. A key with no
// Base URL talks to api.anthropic.com, the listing `ccmorph models` reads
// there — not the launch check that calls every claude-* id served.
func (l *launcher) contextUnlisted(name, want string, f map[string]string) bool {
	if f == nil || profile.Validate(name, f) != nil {
		return false
	}
	base := f["base_url"]
	if f["type"] == "apikey" && base == "" {
		base = "https://api.anthropic.com"
	}
	return util.ContextServed(want, f["type"], base, f["model_map"], f["api_key"]) == util.ServedNo
}

// unlistedWarningText is the sentence itself. A provider's listing is not
// readable with the profile's own credentials, so its advice is the probe that
// records what the route serves.
func unlistedWarningText(model, name, typ string) string {
	advice := "ccmorph models " + name
	if typ == "provider" {
		advice = "ccmorph probe " + name
	}
	return "'" + model + "' is not listed by " + name + "; the limit is saved for when it appears. Check the listing with: " + advice
}

// warnLines is a warning as it prints at width columns: prefixed and wrapped on
// word boundaries, the way help pages wrap (#364), so a terminal never cuts
// inside a word (#370). An empty message prints nothing.
func warnLines(msg string, width int) []string {
	if msg == "" {
		return nil
	}
	return wrapWords("ccmorph: warning: "+msg, width)
}

// quota is `ccmorph quota [profile] [--json]`. Each secret reaches the core on
// stdin, never in argv.
func (l *launcher) quota(args []string) int {
	asJSON, only := false, ""
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-") || only != "":
			return l.extraArg("quota", a)
		default:
			only = a
		}
	}
	names := l.confNames()
	if only != "" {
		if !l.exists(only) {
			l.noProfile(only)
			return 1
		}
		names = []string{only}
	}
	// The core's stdin contract has no next step in it; the sentence is the
	// cli's to say. JSON keeps its machine shape either way.
	if len(names) == 0 && !asJSON {
		fmt.Println(noProfilesMsg)
		return 0
	}
	var in strings.Builder
	row := func(f ...string) { in.WriteString(strings.Join(f, "\t") + "\n") }
	for _, n := range names {
		f, ok := l.readQuiet(n)
		if !ok {
			continue
		}
		dir := filepath.Join(l.profileDirs, n)
		budget := f["monthly_budget_usd"]
		switch f["type"] {
		case "sub":
			row(n, "sub", dir, "")
		case "token":
			row(n, "token", dir, f["token"])
		case "provider":
			if auth, ok := l.authDirQuiet(n); ok && f["provider"] == "codex" {
				row(n, "codex", auth, "")
			} else {
				row(n, "gateway", "", budget)
			}
		case "apikey":
			row(n, "gateway", f["base_url"], budget, f["api_key"])
		default:
			row(n, "gateway", "", budget)
		}
	}
	flags := []string{}
	if asJSON {
		flags = append(flags, "--json")
	} else if isTTY(1) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" {
		flags = append(flags, "--color")
	}
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), append([]string{"quota"}, flags...)...)
	cmd.Stdin = strings.NewReader(in.String())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return runRC(cmd)
}

// authDirQuiet is `_ccm_provider_auth_dir 2>/dev/null`: no symlinked root or dir.
func (l *launcher) authDirQuiet(name string) (string, bool) {
	root := filepath.Join(l.dir, "auth")
	dir := filepath.Join(root, name)
	for _, p := range []string{root, dir} {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return dir, true
}

func runRC(cmd *exec.Cmd) int {
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return 127
	}
	return 0
}
