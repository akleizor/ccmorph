package util

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/profile"
)

// GH-638: a legacy stem over MaxNameLen can still reach rm through
// profile.RemovableName, and rm reaches KeychainDelete. The gate that refuses
// it is profile's one name rule, so no keychain command may run. The fake
// security is also the proof the observation works: a name the gate accepts
// does reach it. The fake logs each argv it received to <state>/calls, so the
// assertion reads the exact command, not a shell-quoted line.
func TestKeychainDeleteRefusesALongStemWithoutTouchingTheKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KeychainDelete only runs on darwin")
	}
	state := stubSecurity(t)
	calls := filepath.Join(state, "calls")

	long := strings.Repeat("a", profile.MaxNameLen+1)
	KeychainDelete(long)
	if data, err := os.ReadFile(calls); err == nil {
		t.Fatalf("KeychainDelete ran security for a %d-byte name: %s", len(long), data)
	}

	KeychainDelete("delete_probe")
	data, err := os.ReadFile(calls)
	want := "delete-generic-password -s ccmorph -a delete_probe.api_key\n" +
		"delete-generic-password -s ccmorph -a delete_probe.token\n"
	if err != nil || string(data) != want {
		t.Fatalf("security got %q %v, want %q; the refusal above proves nothing", data, err, want)
	}
}

// GH-830: this test used to run against the real login keychain under a
// profile name built from filepath.Base(t.TempDir()) — the constant "001"
// since Go 1.24 — so every run on one Mac shared the account
// ccmtest001.api_key and the loser of two concurrent runs took
// add-generic-password: returned -25299. It now drives the same production
// path (KeychainMigrate, keychainMove, keychainPut, profile.KeychainGet)
// through the Go fake security: runs share no identity and no file, and the
// flow needs no login keychain. It stays darwin-only because KeychainMigrate
// returns before the keychain off darwin; the round-trip below covers the
// fake's quoting and its -w rule there, where it can run.
func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KeychainMigrate returns early off darwin (util/keychain.go)")
	}
	stubSecurity(t)
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

// GH-848: the keychain walk scanned the state root with the same unescaped
// join as the router (see router/profiles_test.go for the cause); a root
// holding `[` made the pattern bad and the walk silently migrated nothing.
// The walk runs against the Go fake stubSecurity installs on PATH, which keeps
// every item under its own state directory, so the check works wherever the
// tests run — and the stored items name the profile each write was for, which
// is what proves the walk saw exactly a and b: c.conf links to a plaintext
// conf kept OUTSIDE the scanned root, so a walk that visited the link would
// find a plain secret to store under c's own name, and its atomic rewrite
// would replace the link itself.
func TestKeychainWalkMigratesEveryConfUnderABracketedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state[cd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state := stubSecurity(t)

	linkTarget := filepath.Join(t.TempDir(), "target.conf")
	if err := os.WriteFile(linkTarget, []byte("type=apikey\napi_key=sk-plain-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "bad-name"} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"),
			[]byte("type=apikey\napi_key=sk-plain-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(linkTarget, filepath.Join(root, "c.conf")); err != nil {
		t.Fatal(err)
	}

	if err := keychainWalk(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		data, err := os.ReadFile(filepath.Join(root, name+".conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") {
			t.Fatalf("%s.conf was not migrated under a root holding `[`: %s", name, data)
		}
	}

	// The keychain saw exactly the a and b writes, once each: one store per
	// migrated profile, and no c or bad-name — those confs were never walked.
	// The add log, not the final store, because -U makes a second add
	// overwrite the first: main counted the stdin commands and this count
	// holds the same line, so a duplicate write is caught again.
	if adds, want := fakeSecurityAdds(t, state), []string{"ccmorph\ta.api_key", "ccmorph\tb.api_key"}; fmt.Sprint(adds) != fmt.Sprint(want) {
		t.Fatalf("the walk issued %d adds %q, want exactly %q; it migrated nothing, a skipped conf, or wrote one twice", len(adds), adds, want)
	}
	stored := fakeSecurityStored(t, state)
	if len(stored) != 2 || stored["a.api_key"] != "sk-plain-a" || stored["b.api_key"] != "sk-plain-b" {
		t.Fatalf("the walk stored %v, want exactly a and b's api_key; the walk migrated nothing or a skipped conf", stored)
	}

	// The skipped confs kept their bytes untouched: bad-name still plain,
	// and c.conf still a link to its plain target outside the root.
	if data, err := os.ReadFile(filepath.Join(root, "bad-name.conf")); err != nil ||
		!strings.Contains(string(data), "api_key=sk-plain-bad-name") {
		t.Fatalf("bad-name.conf holds no valid profile yet was rewritten: %s (%v)", data, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "c.conf")); err != nil ||
		fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("c.conf was replaced as a file, so the walk visited the link: %v", err)
	}
	if data, err := os.ReadFile(linkTarget); err != nil ||
		!strings.Contains(string(data), "api_key=sk-plain-c") {
		t.Fatalf("the link's target was rewritten, so the walk visited c.conf: %s (%v)", data, err)
	}
}

// keychainPut and profile.KeychainGet carry no darwin gate of their own, so
// this round-trip — the same fake, the same secret, the same q() quoting a
// Mac's real security would parse — runs on the gate's Linux passes too.
// Without it, a fake or quoting bug would first show on a Mac, where no
// check here can see it. The fake must also answer like the real CLI: the
// password reaches stdout only through -w, so a caller that dropped -w from
// profile.KeychainGet reads metadata, not the secret; and an existing item is
// an error unless -U is given.
func TestKeychainPutGetRoundTripsTheQuotedSecret(t *testing.T) {
	// The fake must survive an operator's TMPDIR carrying a space: its state
	// and the binary copy sit under that path, never substituted into shell
	// source (REVIEW.md: env is a tool). Set before stubSecurity, whose temp
	// dirs then sit inside the spaced path on every platform, Mac CI included.
	spaced := filepath.Join(t.TempDir(), "with spaces")
	if err := os.Mkdir(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", spaced)
	state := stubSecurity(t)
	secrets := []string{
		`sk-"quoted"\back`,
		"sk-$HOME",
		"sk-`id`",
		"sk-$(id)",
		"sk-'single'",
		"sk- two  spaces",
		"sk-\\",
		"sk-\"",
	}
	for i, secret := range secrets {
		ref := keychainRef(fmt.Sprintf("stubprobe%d", i), "api_key")
		if err := keychainPut(ref, secret); err != nil {
			t.Fatalf("put %q: %v", secret, err)
		}
		got, err := profile.KeychainGet(ref)
		if err != nil || got != secret {
			t.Fatalf("put %q: got %q %v", secret, got, err)
		}
	}
	// -U is delete-then-create; without it the fake answers -25299 for the
	// existing item, as the real CLI does.
	ref := keychainRef("stubprobe0", "api_key")
	if err := keychainPut(ref, "sk-second"); err != nil {
		t.Fatalf("second put to stubprobe0: %v", err)
	}
	if got, err := profile.KeychainGet(ref); err != nil || got != "sk-second" {
		t.Fatalf("re-put: got %q %v", got, err)
	}
	out, _ := exec.Command("security", "find-generic-password", "-s", profile.KeychainService,
		"-a", "stubprobe0.api_key").Output()
	if strings.Contains(string(out), "sk-") {
		t.Fatalf("the fake printed the password without -w, so a KeychainGet that dropped -w could not fail: %q", out)
	}
	if calls, err := os.ReadFile(filepath.Join(state, "calls")); err == nil && strings.Contains(string(calls), "sk-") {
		t.Fatalf("a secret reached security's argv: %q", calls)
	}
	if adds, err := os.ReadFile(filepath.Join(state, "adds")); err != nil || strings.Contains(string(adds), "sk-") {
		t.Fatalf("the add log holds a secret or is unreadable (%v): %q; it records the service and account only", err, adds)
	}
	if _, err := profile.KeychainGet("keychain:nostored.api_key"); err == nil {
		t.Fatal("the fake answered for an account it never stored")
	}
}

// GH-830: the keychain tests must not share the machine's login keychain, and
// no test may depend on a host tool to keep them apart. security is replaced by
// this test binary itself: stubSecurity copies the binary beside a state
// directory and puts that one directory on PATH, and TestMain (in
// sharedbridge_test.go) dispatches it to fakeSecurity. There is no shell, no
// eval and no dirname/cat: a TMPDIR with a space or a shadowed host tool cannot
// change what runs.
const fakeSecurityEnv = "CCMORPH_TEST_FAKE_SECURITY"

// stubSecurity installs the fake security for this test and returns its state
// directory (items and the argv log). PATH holds only the fake's directory, so
// nothing here can resolve the operator's real security.
func stubSecurity(t *testing.T) string {
	t.Helper()
	state, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	core := coreCopy(t)
	bin, err := filepath.Abs(filepath.Dir(core))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(core, filepath.Join(bin, "security")); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeSecurityEnv, state)
	t.Setenv("PATH", bin)
	return state
}

// fakeSecurity answers the three commands util/keychain.go and
// profile.KeychainGet ask of the real CLI, storing items in files under state:
//
//	default-keychain               answers "a login keychain exists"
//	-i                             runs add-generic-password scripts, one command
//	                               per line, parsed by securityWords
//	find-generic-password -w       prints the stored secret; without -w it
//	                               prints attributes and no password, so a
//	                               caller that dropped -w reads metadata
//	delete-generic-password        removes an item
//
// A missing item exits 44, like the real CLI, so nothing is invented. Every
// argv is appended to state/calls.
func fakeSecurity(state string, args []string, in io.Reader, out, errw io.Writer) int {
	log, err := os.OpenFile(filepath.Join(state, "calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(errw, "fake security: %v\n", err)
		return 1
	}
	fmt.Fprintln(log, strings.Join(args, " "))
	log.Close()
	if len(args) == 0 {
		return 2
	}
	switch args[0] {
	case "default-keychain":
		fmt.Fprintf(out, "    %q\n", filepath.Join(state, "login.keychain-db"))
		return 0
	case "-i":
		return fakeSecurityCommands(state, in, errw)
	case "add-generic-password":
		fmt.Fprintln(errw, "fake security: add-generic-password on argv puts the secret in ps; keychainPut sends it through -i (INTENT.md promise 5)")
		return 2
	case "find-generic-password":
		svc, acct, withSecret, err := parseFakeLookup(args[1:])
		if err != nil {
			fmt.Fprintf(errw, "fake security: %v\n", err)
			return 2
		}
		secret, err := os.ReadFile(fakeSecurityItem(state, svc, acct))
		if err != nil {
			fmt.Fprintln(errw, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
			return 44
		}
		if withSecret {
			fmt.Fprintln(out, string(secret))
			return 0
		}
		fmt.Fprintf(out, "keychain: %q\nclass: \"genp\"\nattributes:\n    \"acct\"<blob>=%q\n    \"svce\"<blob>=%q\n",
			filepath.Join(state, "login.keychain-db"), acct, svc)
		return 0
	case "delete-generic-password":
		svc, acct, _, err := parseFakeLookup(args[1:])
		if err != nil {
			fmt.Fprintf(errw, "fake security: %v\n", err)
			return 2
		}
		if err := os.Remove(fakeSecurityItem(state, svc, acct)); err != nil {
			fmt.Fprintln(errw, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
			return 44
		}
		fmt.Fprintln(out, "password has been deleted.")
		return 0
	}
	fmt.Fprintf(errw, "fake security: unknown command %s\n", args[0])
	return 2
}

// fakeSecurityCommands runs the add-generic-password lines keychainPut writes
// to `security -i`.
func fakeSecurityCommands(state string, in io.Reader, errw io.Writer) int {
	r := bufio.NewReader(in)
	for {
		line, err := r.ReadString('\n')
		if trimmed := strings.TrimRight(line, "\r\n"); strings.TrimSpace(trimmed) != "" {
			words, werr := securityWords(trimmed)
			if werr != nil {
				fmt.Fprintf(errw, "fake security: %v\n", werr)
				return 1
			}
			if len(words) == 0 || words[0] != "add-generic-password" {
				fmt.Fprintf(errw, "fake security: -i takes add-generic-password, got %v\n", words)
				return 1
			}
			if code := fakeSecurityAdd(state, words[1:], errw); code != 0 {
				return code
			}
		}
		if errors.Is(err, io.EOF) {
			return 0
		}
		if err != nil {
			fmt.Fprintf(errw, "fake security: %v\n", err)
			return 1
		}
	}
}

// fakeSecurityAdd stores one item. Like the real CLI, an existing item is an
// error unless -U is given: -U is delete-then-create, so dropping -U from
// keychainPut must fail the tests. Every parsed add is recorded in
// <state>/adds — the service and account only, never the secret — so a test
// can count the writes a run issued: -U makes a duplicate overwrite its item,
// which the final store alone cannot show.
func fakeSecurityAdd(state string, words []string, errw io.Writer) int {
	svc, acct, secret := "", "", ""
	hasSecret, update := false, false
	for i := 0; i < len(words); i++ {
		switch words[i] {
		case "-s", "-a", "-w":
			if i+1 >= len(words) {
				fmt.Fprintf(errw, "fake security: %s needs a value\n", words[i])
				return 2
			}
			switch words[i] {
			case "-s":
				svc = words[i+1]
			case "-a":
				acct = words[i+1]
			case "-w":
				secret, hasSecret = words[i+1], true
			}
			i++
		case "-U":
			update = true
		}
	}
	if svc == "" || acct == "" || !hasSecret {
		fmt.Fprintln(errw, "fake security: add-generic-password needs -s, -a and -w")
		return 2
	}
	log, err := os.OpenFile(filepath.Join(state, "adds"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(errw, "fake security: %v\n", err)
		return 1
	}
	fmt.Fprintf(log, "%s\t%s\n", svc, acct)
	log.Close()
	item := fakeSecurityItem(state, svc, acct)
	if _, err := os.Stat(item); err == nil && !update {
		fmt.Fprintln(errw, "security: SecKeychainItemCreateFromContent (<default>): The specified item already exists in the keychain.")
		return 45
	}
	if err := os.WriteFile(item, []byte(secret), 0o600); err != nil {
		fmt.Fprintf(errw, "fake security: %v\n", err)
		return 1
	}
	return 0
}

// parseFakeLookup reads the -s and -a values and whether -w was given; -w is a
// flag here, so it takes no value.
func parseFakeLookup(words []string) (svc, acct string, withSecret bool, err error) {
	for i := 0; i < len(words); i++ {
		switch words[i] {
		case "-s", "-a":
			if i+1 >= len(words) {
				return "", "", false, fmt.Errorf("%s needs a value", words[i])
			}
			if words[i] == "-s" {
				svc = words[i+1]
			} else {
				acct = words[i+1]
			}
			i++
		case "-w":
			withSecret = true
		}
	}
	if svc == "" || acct == "" {
		return "", "", false, errors.New("this command needs -s and -a")
	}
	return svc, acct, withSecret, nil
}

// fakeSecurityItem is the file backing one generic password; the item name is
// hex so a service or account carrying a slash stays one file.
func fakeSecurityItem(state, service, account string) string {
	return filepath.Join(state, "item-"+hex.EncodeToString([]byte(service+"\x00"+account)))
}

// fakeSecurityAdds reads the add log under state as "service\taccount" lines,
// one per parsed add-generic-password, in the order the fake received them.
// The secret is never part of a line: a test that counts writes must not need
// the value, and the value must not reach a file a test reads casually.
func fakeSecurityAdds(t *testing.T, state string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "adds"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// fakeSecurityStored reads what the fake holds under state as account ->
// secret. A test asserts what security was asked to store from the store
// itself: argv never carries a secret (INTENT.md promise 5), and the -i
// script only reaches the fake on stdin.
func fakeSecurityStored(t *testing.T, state string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "item-") {
			continue
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(name, "item-"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		service, account, ok := strings.Cut(string(raw), "\x00")
		if !ok || service != "ccmorph" {
			t.Fatalf("%s names service %q, want ccmorph", name, service)
		}
		secret, err := os.ReadFile(filepath.Join(state, name))
		if err != nil {
			t.Fatal(err)
		}
		out[account] = string(secret)
	}
	return out
}

// securityWords splits one add-generic-password line the way keychainPut's q()
// writes it: words split on spaces and tabs, `"` starts and ends a quoted run
// inside a word, and inside quotes a backslash makes the next byte literal.
// security is not a shell, so `$` and backticks stay literal. An empty quoted
// word is a word; an unterminated quote or a trailing backslash is an error.
func securityWords(line string) ([]string, error) {
	var words []string
	var cur strings.Builder
	inWord, inQuote := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case inQuote:
			switch c {
			case '\\':
				if i+1 >= len(line) {
					return nil, errors.New("trailing backslash in a quoted word")
				}
				i++
				cur.WriteByte(line[i])
			case '"':
				inQuote = false
			default:
				cur.WriteByte(c)
			}
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '"':
			inQuote = true
			inWord = true
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inQuote {
		return nil, errors.New("unterminated quote")
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}

// TestSecurityWordsReadsWhatKeychainPutWrites pins the parser keychainPut's
// script passes through.
func TestSecurityWordsReadsWhatKeychainPutWrites(t *testing.T) {
	got, err := securityWords(`add-generic-password -U -s ccmorph -a "p.api_key" -w "sk-\"quoted\"\\back"`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"add-generic-password", "-U", "-s", "ccmorph", "-a", "p.api_key", "-w", `sk-"quoted"\back`}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("got %q, want %q", got, want)
	}
	for line, want := range map[string]string{
		`-w "sk-$HOME"`:  "sk-$HOME",
		"-w \"sk-`id`\"": "sk-`id`",
		`-w "a  b"`:      "a  b",
		`-w ""`:          "",
	} {
		words, err := securityWords(line)
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		if w := words[len(words)-1]; w != want {
			t.Errorf("%s: last word %q, want %q", line, w, want)
		}
	}
	for _, line := range []string{`-w "open`, `-w "x\`} {
		if _, err := securityWords(line); err == nil {
			t.Errorf("%s parsed; want an error", line)
		}
	}
}

// TestStubSecurityIsTheOnlySecurityOnPath proves the fake is what a test then
// reaches: PATH holds one directory, security resolves there, and it invents
// nothing.
func TestStubSecurityIsTheOnlySecurityOnPath(t *testing.T) {
	state := stubSecurity(t)
	path := os.Getenv("PATH")
	if !filepath.IsAbs(path) || strings.ContainsRune(path, os.PathListSeparator) {
		t.Fatalf("PATH is %q; want one absolute directory", path)
	}
	found, err := exec.LookPath("security")
	if err != nil || found != filepath.Join(path, "security") {
		t.Fatalf("security resolves to %q %v, want the fake in %q", found, err, path)
	}
	if _, err := os.Stat(filepath.Join(state, "calls")); err == nil {
		t.Fatal("stubSecurity ran security itself")
	}
	if err := exec.Command("security", "default-keychain").Run(); err != nil {
		t.Fatalf("default-keychain: %v", err)
	}
	if calls, err := os.ReadFile(filepath.Join(state, "calls")); err != nil || string(calls) != "default-keychain\n" {
		t.Fatalf("calls %q %v", calls, err)
	}
	if err := exec.Command("security", "dump-keychain").Run(); err == nil {
		t.Error("the fake answered unknown command dump-keychain")
	}
	if err := exec.Command("security", "add-generic-password", "-s", "ccmorph", "-a", "x", "-w", "sk-argv").Run(); err == nil {
		t.Error("the fake accepted add-generic-password on argv")
	}
}

// GH-830: before the fake, every run of the migrate test shared the login
// keychain account ccmtest001.api_key, so two concurrent runs raced. The
// fake removes the shared item, and this pair proves the property directly:
// two processes put their own secret to the same fixed account, wait until
// both have put, and each reads back its own. A store shared between them
// makes one child read the other's secret and fails.
func TestKeychainRunsInTwoProcessesShareNoItem(t *testing.T) {
	barrier := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	secrets := []string{"child-a", "child-b"}
	var out [2]bytes.Buffer
	cmds := make([]*exec.Cmd, len(secrets))
	for i, secret := range secrets {
		cmds[i] = exec.CommandContext(ctx, os.Args[0],
			"-test.run=^TestKeychainChildHoldsItsOwnItem$", "-test.count=1", "-test.v")
		cmds[i].Env = append(os.Environ(),
			"CCMORPH_TEST_KEYCHAIN_PARENT="+strconv.Itoa(os.Getpid()),
			"CCMORPH_TEST_KEYCHAIN_BARRIER="+barrier,
			"CCMORPH_TEST_KEYCHAIN_SECRET="+secret,
		)
		cmds[i].Stdout = &out[i]
		cmds[i].Stderr = &out[i]
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Errorf("child %s: %v\n%s", secrets[i], err, out[i].String())
			continue
		}
		if !strings.Contains(out[i].String(), "--- PASS: TestKeychainChildHoldsItsOwnItem") {
			t.Errorf("child %s did not pass its own readback:\n%s", secrets[i], out[i].String())
		}
	}
}

// TestKeychainChildHoldsItsOwnItem is the child half of
// TestKeychainRunsInTwoProcessesShareNoItem and skips in every normal run:
// both env variables and this process's real parent pid must agree.
func TestKeychainChildHoldsItsOwnItem(t *testing.T) {
	if os.Getenv("CCMORPH_TEST_KEYCHAIN_PARENT") != strconv.Itoa(os.Getppid()) {
		t.Skip("runs only as a child of TestKeychainRunsInTwoProcessesShareNoItem")
	}
	barrier := os.Getenv("CCMORPH_TEST_KEYCHAIN_BARRIER")
	secret := os.Getenv("CCMORPH_TEST_KEYCHAIN_SECRET")
	if barrier == "" || secret == "" {
		t.Fatal("the child env is incomplete")
	}
	stubSecurity(t)
	// The account every run shared before #830: the fake gives each process
	// its own item, so the fixed name is no longer a shared identity.
	ref := keychainRef("ccmtest001", "api_key")
	if err := keychainPut(ref, secret); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(barrier, secret+".ready"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(barrier)
		if err == nil && len(entries) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the barrier never held two entries (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	got, err := profile.KeychainGet(ref)
	if err != nil || got != secret {
		t.Fatalf("got %q %v, want %q: the two processes share one item", got, err, secret)
	}
}
