package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// GH-638: a legacy stem over MaxNameLen can still reach rm through
// profile.RemovableName, and rm reaches KeychainDelete. The gate that refuses
// it is profile's one name rule, so no keychain command may run. The fake
// security is also the proof the observation works: a name the gate accepts
// does reach it. Its log path is passed as data (calls.path beside the
// script), never substituted into the script source: an operator's TMPDIR may
// hold a space, and a substituted path would break the shell line (REVIEW.md,
// env is a tool).
func TestKeychainDeleteRefusesALongStemWithoutTouchingTheKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KeychainDelete only runs on darwin")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "security")
	body := "#!/bin/sh\ncalls=$(cat \"$(dirname \"$0\")/calls.path\") || exit 1\nprintf '%s\\n' \"$*\" >> \"$calls\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calls.path"), []byte(calls+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	long := strings.Repeat("a", profile.MaxNameLen+1)
	KeychainDelete(long)
	if data, err := os.ReadFile(calls); err == nil {
		t.Fatalf("KeychainDelete ran security for a %d-byte name: %s", len(long), data)
	}

	KeychainDelete("delete_probe")
	data, err := os.ReadFile(calls)
	if err != nil || len(data) == 0 {
		t.Fatalf("the fake security was not reached for a valid name (%v); the refusal above proves nothing", err)
	}
}

// securityStub is a stand-in for the macOS security CLI that answers the
// three things util/keychain.go and profile.KeychainGet ask of it, storing
// items in files instead of the machine's login keychain (GH-830):
//
//	security default-keychain       answers "a login keychain exists"
//	security -i                     runs add-generic-password scripts, one command per
//	                                line; the quoting is q()'s in util/keychain.go, and
//	                                POSIX shell double quotes unescape it identically,
//	                                which is what the eval below relies on. eval here
//	                                only ever sees a line this package's own code wrote
//	                                to the test's stub, never anything from outside.
//	security find-generic-password  prints the stored secret only with -w, exactly like
//	                                the real CLI — without -w it prints attributes and
//	                                no password, and a caller that dropped -w reads
//	                                metadata instead of a secret — or exits 44 (not
//	                                found), so nothing is invented
//
// The item directory is passed as data (items.path beside the script), never
// substituted into the script source: an operator's TMPDIR may hold a space.
const securityStub = `#!/bin/sh
me=$(dirname "$0")
ITEMS=$(cat "$me/items.path") || exit 1
[ -n "$ITEMS" ] || exit 1
case "$1" in
default-keychain)
	exit 0
;;
-i)
	while IFS= read -r line; do
		[ -n "$line" ] || continue
		eval "set -- $line" || exit 1
		[ "$1" = add-generic-password ] || exit 1
		svc="" acct="" sec=""
		while [ "$#" -gt 0 ]; do
			case "$1" in
			-s) svc="$2"; shift 2 ;;
			-a) acct="$2"; shift 2 ;;
			-w) sec="$2"; shift 2 ;;
			*) shift ;;
			esac
		done
		[ -n "$svc" ] && [ -n "$acct" ] || exit 1
		printf '%s' "$sec" > "$ITEMS/$svc.$acct"
	done
	exit 0
;;
find-generic-password)
	shift
	svc="" acct="" w=""
	while [ "$#" -gt 0 ]; do
		case "$1" in
		-s) svc="$2"; shift 2 ;;
		-a) acct="$2"; shift 2 ;;
		-w) w=1; shift ;;
		*) shift ;;
		esac
	done
	if [ -z "$svc" ] || [ -z "$acct" ]; then exit 1; fi
	if [ ! -f "$ITEMS/$svc.$acct" ]; then exit 44; fi
	if [ -n "$w" ]; then
		printf '%s\n' "$(cat "$ITEMS/$svc.$acct")"
	else
		printf 'class: "genp"\nattributes:\n'
	fi
	exit 0
;;
esac
exit 127
`

// keychainStub writes the security stub and returns its bin dir; the caller
// scopes it with t.Setenv, so what a test then reaches is this script, never
// the login keychain.
func keychainStub(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	items := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "items.path"), []byte(items+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(bin, "security")
	if err := os.WriteFile(script, []byte(securityStub), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// GH-830: this test used to run against the real login keychain under a
// profile name built from filepath.Base(t.TempDir()) — the constant "001"
// since Go 1.24 — so every run on one Mac shared the account
// ccmtest001.api_key and the loser of two concurrent runs took
// add-generic-password: returned -25299. It now drives the same production
// path (KeychainMigrate, keychainMove, keychainPut, profile.KeychainGet)
// through the stub above: runs share no identity and no file, and the flow
// needs no login keychain. It stays darwin-only because KeychainMigrate
// returns before the keychain off darwin; the round-trip below covers the
// stub's quoting and its -w rule there, where it can run.
func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KeychainMigrate returns early off darwin (util/keychain.go)")
	}
	t.Setenv("PATH", keychainStub(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	name := "ccmtest"
	conf := filepath.Join(dir, name+".conf")
	secret := `sk-"quoted"\back`
	os.WriteFile(conf, []byte("type=apikey\napi_key="+secret+"\n"), 0o600)
	if err := KeychainMigrate(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") || strings.Contains(string(data), "sk-") {
		t.Fatal(string(data))
	}
	if got, err := profile.KeychainGet("keychain:" + name + ".api_key"); err != nil || got != secret {
		t.Fatalf("got %q %v", got, err)
	}
}

// keychainPut and profile.KeychainGet carry no darwin gate of their own, so
// this round-trip — the same stub, the same secret, the same q() quoting a
// Mac's real security would parse — runs on the gate's Linux passes too.
// Without it, a stub or quoting bug would first show on a Mac, where no
// check here can see it. The stub must also answer like the real CLI: the
// password reaches stdout only through -w, so a caller that dropped -w from
// profile.KeychainGet reads metadata, not the secret.
func TestKeychainPutGetRoundTripsTheQuotedSecret(t *testing.T) {
	// The stub must survive an operator's TMPDIR carrying a space: its paths
	// are passed as data, never substituted into shell source (REVIEW.md:
	// env is a tool). Set before keychainStub, whose temp dirs then sit
	// inside the spaced path on every platform, Mac CI included.
	spaced := filepath.Join(t.TempDir(), "with spaces")
	if err := os.Mkdir(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spaced)
	t.Setenv("PATH", keychainStub(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	ref := keychainRef("stubprobe", "api_key")
	secret := `sk-"quoted"\back`
	if err := keychainPut(ref, secret); err != nil {
		t.Fatal(err)
	}
	got, err := profile.KeychainGet(ref)
	if err != nil || got != secret {
		t.Fatalf("got %q %v", got, err)
	}
	out, _ := exec.Command("security", "find-generic-password", "-s", profile.KeychainService,
		"-a", "stubprobe.api_key").Output()
	if strings.Contains(string(out), "sk-") {
		t.Fatalf("the stub printed the password without -w, so a KeychainGet that dropped -w could not fail: %q", out)
	}
	if _, err := profile.KeychainGet("keychain:nostored.api_key"); err == nil {
		t.Fatal("the stub answered for an account it never stored")
	}
}
