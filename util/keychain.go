package util

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
)

// A credential held in the login keychain is written in the conf as
// "keychain:<name>.<key>": the generic password whose service is ccmorph and
// whose account is that reference. Nothing else in the conf changes.
const keychainService = profile.KeychainService

func keychainRef(profile, key string) string { return "keychain:" + profile + "." + key }

// keychainPut stores secret under ref. The secret goes through security's
// stdin script, never argv, so ps never shows it.
func keychainPut(ref, secret string) error {
	if strings.ContainsAny(secret, "\r\n") {
		return fmt.Errorf("a credential cannot contain CR/LF")
	}
	q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
	cmd := exec.Command("security", "-i")
	cmd.Stdin = strings.NewReader("add-generic-password -U -s " + keychainService + " -a " +
		q(strings.TrimPrefix(ref, "keychain:")) + " -w " + q(secret) + "\n")
	if out, err := cmd.CombinedOutput(); err != nil || len(strings.TrimSpace(string(out))) > 0 {
		return fmt.Errorf("security add-generic-password failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// keychainDelete: keychain-delete <profile> removes the profile's keychain items.
func keychainDelete(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 1 || !ctxwindow.ValidProfileName(args[0]) || runtime.GOOS != "darwin" {
		return 0
	}
	for _, key := range []string{"api_key", "token"} {
		exec.Command("security", "delete-generic-password", "-s", keychainService,
			"-a", strings.TrimPrefix(keychainRef(args[0], key), "keychain:")).Run()
	}
	return 0
}

// keychainRename: keychain-rename <old> <new> moves a renamed profile's items
// and rewrites its conf references. The conf is already at <new>.conf.
func keychainRename(args []string, _ io.Reader, _, errw io.Writer) int {
	if len(args) != 3 || runtime.GOOS != "darwin" {
		return 0
	}
	state, old, renamed := args[0], args[1], args[2]
	return keychainMove(filepath.Join(state, renamed+".conf"), renamed, func(key string) string {
		return keychainRef(old, key)
	}, errw)
}

// keychainMigrate: keychain-migrate <state-dir> moves every plain api_key and
// token into the login keychain, macOS only. A profile whose secret cannot be
// stored keeps its plain value: nothing is lost.
func keychainMigrate(args []string, _ io.Reader, _, errw io.Writer) int {
	if len(args) != 1 || runtime.GOOS != "darwin" {
		return 0
	}
	if exec.Command("security", "default-keychain").Run() != nil {
		return 0 // no login keychain (a CI or scratch $HOME): the conf stays 0600
	}
	confs, _ := filepath.Glob(filepath.Join(args[0], "*.conf"))
	rc := 0
	for _, conf := range confs {
		name := strings.TrimSuffix(filepath.Base(conf), ".conf")
		if !ctxwindow.ValidProfileName(name) || isSymlink(conf) {
			continue
		}
		if keychainMove(conf, name, nil, errw) != 0 {
			rc = 1
		}
	}
	return rc
}

// keychainMove rewrites conf's api_key and token as keychain references for
// profile name. A plain value is stored first; a reference from (a rename) is
// copied. The conf is rewritten only after every item is stored.
func keychainMove(conf, name string, from func(string) string, errw io.Writer) int {
	data, err := os.ReadFile(conf)
	if err != nil {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "api_key" && key != "token") || value == "" {
			continue
		}
		ref := keychainRef(name, key)
		secret := value
		if strings.HasPrefix(value, "keychain:") {
			if from == nil || value != from(key) {
				continue
			}
			if secret, err = profile.KeychainGet(value); err != nil {
				fmt.Fprintf(errw, "ccmorph: %s: %s is missing from the keychain\n", name, value)
				return 1
			}
		} else if from != nil {
			continue
		}
		if err := keychainPut(ref, secret); err != nil {
			fmt.Fprintf(errw, "ccmorph: %s keeps its plain %s: %v\n", name, key, err)
			return 1
		}
		lines[i] = key + "=" + ref
		changed = true
	}
	if !changed {
		return 0
	}
	_, before := ctxwindow.ReadConf(conf)
	fi, err := os.Stat(conf)
	if err != nil {
		return 1
	}
	tmp := conf + ".keychain-tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), fi.Mode().Perm()); err != nil {
		return 1
	}
	if err := os.Rename(tmp, conf); err != nil {
		os.Remove(tmp)
		return 1
	}
	// The key is part of the route fingerprint; the same key under a new
	// spelling must not orphan the profile's discovered windows.
	_, after := ctxwindow.ReadConf(conf)
	ctxwindow.Restamp(conf, ctxwindow.RouteFingerprint(before), ctxwindow.RouteFingerprint(after))
	if from != nil {
		keychainDelete([]string{strings.TrimSuffix(strings.TrimPrefix(from("api_key"), "keychain:"), ".api_key")}, nil, nil, nil)
	}
	return 0
}

// profileRead: profile-read <state> <name> <secure 0|1> <profile-env,...> <mapping-env,...>
// prints the profile's fields as NUL-separated key/value pairs for _ccm_read,
// or the refusal on stdout with exit 1. Removed-key notices go to stderr.
func profileRead(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 5 {
		return 2
	}
	set := func(csv string) map[string]bool {
		m := map[string]bool{}
		for _, k := range strings.Split(csv, ",") {
			m[k] = true
		}
		return m
	}
	fields, err := profile.Read(args[0], args[1], args[2] == "1",
		profile.Lists{ProfileEnv: set(args[3]), MappingEnv: set(args[4])},
		func(line string) { fmt.Fprintln(errw, line) })
	if err != nil {
		io.WriteString(out, err.Error())
		return 1
	}
	var b strings.Builder
	for k, v := range fields {
		b.WriteString(k + "\x00" + v + "\x00")
	}
	io.WriteString(out, b.String()+".") // $(…) strips trailing NULs; the dot keeps the last empty value
	return 0
}

// profileValidate is `util profile-validate <name>` over NUL-separated key/value
// pairs on stdin; the refusal goes to stdout with exit 1.
func profileValidate(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	parts := strings.Split(string(readAll(in)), "\x00")
	f := map[string]string{}
	for i := 0; i+1 < len(parts); i += 2 {
		f[parts[i]] = parts[i+1]
	}
	if err := profile.Validate(args[0], f); err != nil {
		io.WriteString(out, err.Error())
		return 1
	}
	return 0
}

func profileValidateURL(args []string, _ io.Reader, out, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	if err := profile.ValidateBaseURL(args[0], args[1]); err != nil {
		io.WriteString(out, err.Error())
		return 1
	}
	return 0
}

// profileEnv is `util profile-env <state dir> <profiles dir> <name> <profile env
// csv> <mapping env csv>`: read, validate and print the profile's launch
// environment, one VAR=value per line, after a first `type=<type>` line. A refusal
// goes to stdout with exit 1.
func profileEnv(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 5 {
		return 2
	}
	set := func(csv string) map[string]bool {
		m := map[string]bool{}
		for _, k := range strings.Split(csv, ",") {
			m[k] = true
		}
		return m
	}
	name := args[2]
	f, err := profile.Read(args[0], name, true,
		profile.Lists{ProfileEnv: set(args[3]), MappingEnv: set(args[4])},
		func(line string) { fmt.Fprintln(errw, line) })
	if err == nil {
		err = profile.Validate(name, f)
	}
	var env []string
	if err == nil {
		env, err = profile.Env(name, args[1], f)
	}
	if err != nil {
		io.WriteString(out, err.Error())
		return 1
	}
	fmt.Fprintln(out, "type="+f["type"])
	for _, l := range env {
		fmt.Fprintln(out, l)
	}
	return 0
}
