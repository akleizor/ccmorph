package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/profile"
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
		l.err("usage: ccmorph context <profile> <model> [tokens|auto]")
		return 2
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'. Run: ccmorph list")
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
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"),
		append([]string{"model-window", "--context", name, model}, rest...)...)
	cmd.Env = append(os.Environ(), "CCMORPH_STATE_DIR="+l.dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return runRC(cmd)
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
			l.err("usage: ccmorph quota [profile] [--json]")
			return 2
		default:
			only = a
		}
	}
	names := l.confNames()
	if only != "" {
		if !l.exists(only) {
			l.err("no profile '" + only + "'. Run: ccmorph list")
			return 1
		}
		names = []string{only}
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
