package util

import (
	"errors"
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

// KeychainDelete removes the profile's keychain items (macOS only, best effort).
func KeychainDelete(name string) {
	if !ctxwindow.ValidProfileName(name) || runtime.GOOS != "darwin" {
		return
	}
	for _, key := range []string{"api_key", "token"} {
		exec.Command("security", "delete-generic-password", "-s", keychainService,
			"-a", strings.TrimPrefix(keychainRef(name, key), "keychain:")).Run()
	}
}

// KeychainRename moves a renamed profile's items and rewrites its conf references.
// The conf is already at <renamed>.conf.
func KeychainRename(state, old, renamed string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return keychainMove(filepath.Join(state, renamed+".conf"), renamed, func(key string) string {
		return keychainRef(old, key)
	})
}

// KeychainMigrate moves every plain api_key and token under state into the login
// keychain, macOS only. A profile whose secret cannot be stored keeps its plain
// value: nothing is lost.
func KeychainMigrate(state string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if exec.Command("security", "default-keychain").Run() != nil {
		return nil // no login keychain (a CI or scratch $HOME): the conf stays 0600
	}
	confs, _ := filepath.Glob(filepath.Join(state, "*.conf"))
	var errs []error
	for _, conf := range confs {
		name := strings.TrimSuffix(filepath.Base(conf), ".conf")
		if !ctxwindow.ValidProfileName(name) || isSymlink(conf) {
			continue
		}
		if err := keychainMove(conf, name, nil); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// keychainMove rewrites conf's api_key and token as keychain references for
// profile name. A plain value is stored first; a reference from (a rename) is
// copied. The conf is rewritten only after every item is stored.
func keychainMove(conf, name string, from func(string) string) error {
	data, err := os.ReadFile(conf)
	if err != nil {
		return nil
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
				return fmt.Errorf("ccmorph: %s: %s is missing from the keychain", name, value)
			}
		} else if from != nil {
			continue
		}
		if err := keychainPut(ref, secret); err != nil {
			return fmt.Errorf("ccmorph: %s keeps its plain %s: %v", name, key, err)
		}
		lines[i] = key + "=" + ref
		changed = true
	}
	if !changed {
		return nil
	}
	_, before := ctxwindow.ReadConf(conf)
	fi, err := os.Stat(conf)
	if err != nil {
		return fmt.Errorf("ccmorph: %s: %v", name, err)
	}
	if err := AtomicWrite(conf, ".keychain-tmp.", []byte(strings.Join(lines, "\n")), fi.Mode().Perm(), true); err != nil {
		return fmt.Errorf("ccmorph: %s: %v", name, err)
	}
	// The key is part of the route fingerprint; the same key under a new
	// spelling must not orphan the profile's discovered windows.
	_, after := ctxwindow.ReadConf(conf)
	ctxwindow.Restamp(conf, ctxwindow.RouteFingerprint(before), ctxwindow.RouteFingerprint(after))
	if from != nil {
		KeychainDelete(strings.TrimSuffix(strings.TrimPrefix(from("api_key"), "keychain:"), ".api_key"))
	}
	return nil
}

// profileRead: profile-read <state> <name> <secure 0|1> [<profile env> <mapping env>]
// prints the profile's fields as NUL-separated key/value pairs for _ccm_read,
// or the refusal on stdout with exit 1. Removed-key notices go to stderr.
// The two trailing operands are the env-key lists a v0.4.3 launcher still passes
// when it outlives an update (#137); the lists live in Go now (#130), so they
// are accepted and ignored.
func profileRead(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 3 && len(args) != 5 {
		return 2
	}
	fields, err := profile.Read(args[0], args[1], args[2] == "1",
		profile.Defaults(),
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

// ProfileEnv reads, validates and builds the profile's launch environment, one
// VAR=value per entry, with its type. A refusal is the error; removed-key notices
// go to notice.
func ProfileEnv(state, profiles, name string, notice func(string)) (typ string, env []string, err error) {
	f, err := profile.Read(state, name, true, profile.Defaults(), notice)
	if err == nil {
		err = profile.Validate(name, f)
	}
	if err == nil {
		env, err = profile.Env(name, profiles, f)
	}
	if err != nil {
		return "", nil, err
	}
	return f["type"], env, nil
}

// profileEnv is `util profile-env <state dir> <profiles dir> <name>`: ProfileEnv as
// text, one VAR=value per line after a first `type=<type>` line. A refusal goes to
// stdout with exit 1. A v0.4.3 launcher's two trailing env-key list operands are
// accepted and ignored (#137, tests/old_launcher_contract_test.py).
func profileEnv(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 3 && len(args) != 5 {
		return 2
	}
	typ, env, err := ProfileEnv(args[0], args[1], args[2], func(line string) { fmt.Fprintln(errw, line) })
	if err != nil {
		io.WriteString(out, err.Error())
		return 1
	}
	fmt.Fprintln(out, "type="+typ)
	for _, l := range env {
		fmt.Fprintln(out, l)
	}
	return 0
}
