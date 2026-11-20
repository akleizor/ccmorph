package util

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDoctorEmptySubscriptionFromKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain storage is macOS only")
	}
	old := readSigninKeychain
	t.Cleanup(func() { readSigninKeychain = old })
	readSigninKeychain = func(string, string) ([]byte, error) {
		return []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":""}}`), nil
	}
	rows, failed, _ := DoctorRoute("test", "sub", "", "", "", nil, "", t.TempDir(), false)
	if !failed || len(rows) != 1 || rows[0].Kind != "FAIL" || rows[0].Msg != "credentials: signed out of Claude; sign in again: ccmorph login test" || rows[0].Fix != "sign in again: ccmorph login test" {
		t.Fatalf("unexpected doctor result: failed=%v rows=%+v", failed, rows)
	}
}

func TestDoctorRefreshOnlySubscriptionFromKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain storage is macOS only")
	}
	old := readSigninKeychain
	t.Cleanup(func() { readSigninKeychain = old })
	readSigninKeychain = func(string, string) ([]byte, error) {
		return []byte(`{"claudeAiOauth":{"accessToken":"","refreshToken":"refresh"}}`), nil
	}
	rows, failed, _ := DoctorRoute("test", "sub", "", "", "", nil, "", t.TempDir(), false)
	if failed || len(rows) != 1 || rows[0].Kind != "WARN" || !strings.Contains(rows[0].Msg, "not checked") {
		t.Fatalf("unexpected doctor result: failed=%v rows=%+v", failed, rows)
	}
}

func TestDoctorReadsSubscriptionFromKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain storage is macOS only")
	}
	home := t.TempDir()
	t.Setenv("LOGNAME", "doctor-test")
	old := readSigninKeychain
	t.Cleanup(func() { readSigninKeychain = old })
	readSigninKeychain = func(service, account string) ([]byte, error) {
		if account != "doctor-test" || service != "Claude Code-credentials-"+fmt.Sprintf("%x", sha256.Sum256([]byte(home)))[:8] {
			return nil, errors.New("wrong keychain item")
		}
		return []byte(`{"claudeAiOauth":{"accessToken":"test-token"}}`), nil
	}
	dir := t.TempDir()
	curl := "#!/bin/sh\ncase \"$(/bin/cat)\" in *test-token*) printf '__CCMORPH_STATUS__200';; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	rows, failed, _ := DoctorRoute("test", "sub", "", "", "", nil, "", home, false)
	if failed || len(rows) != 1 || rows[0].Kind != "OK" || !strings.Contains(rows[0].Msg, "accepted the stored sign-in") {
		t.Fatalf("unexpected doctor result: failed=%v rows=%+v", failed, rows)
	}
}

// stepPATH points PATH at a directory holding a brew stub, or one holding
// nothing: the install step the adapter wording names comes from the machine's
// PATH (#369), so each case chooses the machine it runs on.
func stepPATH(t *testing.T, brew bool) {
	t.Helper()
	dir := t.TempDir()
	if brew {
		if err := os.WriteFile(filepath.Join(dir, "brew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
}

// The adapter line's wording is the contract GH-272 fixed: it says whether this
// endpoint needs a translator and whether cliproxyapi is installed, and is
// absent when nothing decides the need. The
// not-installed verdict cannot be produced end to end on a machine that has
// cliproxyapi installed (discovery finds it at a fixed path), so the wording is pinned here, where the verdict is an input. The empty key
// sends no request, so the test needs no curl; the profile name matches no
// real process, so no running session is reported either. The install step the
// FAIL wording names is the machine's own (#369): brew's command where brew is
// on PATH, the project page the installer gives where it is not.
func TestDoctorRouteAdapterLine(t *testing.T) {
	// The table is pinned readable and empty: PATH here holds only the brew
	// stub, so the real ps is missing and every case would otherwise read
	// __UNKNOWN__, whose own SKIP row this test is not about (#518).
	orig := sysctlTable
	sysctlTable = func() (map[int]psRow, bool) { return map[int]psRow{}, true }
	t.Cleanup(func() { sysctlTable = orig })

	cases := []struct {
		name string
		need TranslatorNeed
		brew bool
		kind string
		msg  string
	}{
		{"needed and installed", TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed and not installed, brew on PATH", TranslatorNeed{Known: true, Needed: true}, true, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"needed and not installed, no brew", TranslatorNeed{Known: true, Needed: true}, false, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: https://github.com/router-for-me/CLIProxyAPI"},
		{"not needed", TranslatorNeed{Known: true}, true, "OK",
			"adapter: this endpoint needs no translator"},
		{"unknown, translator installed", TranslatorNeed{Installed: true}, true, "", ""},
		{"unknown, translator not installed", TranslatorNeed{}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stepPATH(t, c.brew)
			rows, failed, _ := DoctorRoute("doctor-route-verdict", "apikey",
				"https://example.test", "", "", func(string) TranslatorNeed { return c.need }, "", "", false)
			var line *DoctorRow
			for i := range rows {
				if strings.HasPrefix(rows[i].Msg, "adapter:") {
					if line != nil {
						t.Fatalf("two adapter lines: %+v", rows)
					}
					line = &rows[i]
				}
			}
			if c.kind == "" {
				// Nothing is cached and no listing was read (the empty key sends
				// no request): nothing honest to say, so no line.
				if line != nil {
					t.Fatalf("unexpected adapter line: %+v", *line)
				}
				return
			}
			if line == nil {
				t.Fatalf("no adapter line; rows: %+v", rows)
			}
			if line.Kind != c.kind || line.Msg != c.msg {
				t.Fatalf("got %s %q, want %s %q", line.Kind, line.Msg, c.kind, c.msg)
			}
			wantFix := ""
			if c.kind == "FAIL" {
				step := "https://github.com/router-for-me/CLIProxyAPI"
				if c.brew {
					step = "brew install cliproxyapi"
				}
				wantFix = "install the translator: " + step
			}
			if line.Fix != wantFix {
				t.Fatalf("fix %q, want %q", line.Fix, wantFix)
			}
			if failed != (c.kind == "FAIL") {
				t.Fatalf("failed=%v, want %v", failed, c.kind == "FAIL")
			}
		})
	}
}

// Doctor's adapter report reads a live session through the real process
// table, with no injected answer: the row a saved, unclassified model depends
// on comes from the kernel's own argv, so a report that drops the live session
// fails here even when a loaded machine would hide it end to end (#518).
func TestDoctorRouteReadsAnOpenSessionThroughTheProcessTable(t *testing.T) {
	// This is about the wiring, not about winning a race with a loaded
	// machine: the read gets room to answer.
	orig := sessionReadBound
	sessionReadBound = 30 * time.Second
	t.Cleanup(func() { sessionReadBound = orig })

	profile := "dr" + strconv.Itoa(os.Getpid())
	holdProc(t, os.Args[0], BridgeFiles(t.TempDir(), "p")[2], "/x/my adapter/ccmorph-core", "adapter", profile)
	rows, _, _ := DoctorRoute(profile, "apikey", "https://example.test", "glm-4.6", "",
		func(string) TranslatorNeed { return TranslatorNeed{Installed: true} }, "", "", false)
	var got []string
	for _, r := range rows {
		if strings.HasPrefix(r.Msg, "adapter:") {
			got = append(got, r.Kind+" "+r.Msg)
		}
	}
	want := []string{"OK adapter: this profile needs no translator now: its open session runs without one"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adapter rows %q, want %q", got, want)
	}
}

// A session read that lost its bound names the check it could not make, in
// the default report, instead of leaving the saved model's line out or
// calling the session absent (#518). The ps is forced to stall and the bound
// is shrunk, so the answer is deterministic in serial CI rather than a race
// with a loaded machine.
func TestDoctorRouteNamesASessionReadThatLostItsBound(t *testing.T) {
	origSysctl, origBound := sysctlTable, sessionReadBound
	sysctlTable = func() (map[int]psRow, bool) { return nil, false }
	sessionReadBound = 200 * time.Millisecond
	t.Cleanup(func() { sysctlTable, sessionReadBound = origSysctl, origBound })

	profile := "dr" + strconv.Itoa(os.Getpid())
	skip := "SKIP adapter: could not tell whether a translator is running (running programs could not be listed within 200ms); check again: ccmorph doctor " + profile
	for _, c := range []struct {
		name, model string
		need        TranslatorNeed
		want        []string
	}{
		{"saved model, session unreadable", "glm-4.6", TranslatorNeed{Installed: true}, []string{skip}},
		{"known need, session unreadable", "", TranslatorNeed{Known: true, Needed: true, Installed: true},
			[]string{"OK adapter: this endpoint needs a translator; cliproxyapi is installed", skip}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "ps.pid")
			script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nexec /bin/sleep 30\n"
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			// The stalled ps must not outlive the test, even when an
			// assertion fails first (the cleanup pattern GH-449 added).
			t.Cleanup(func() {
				b, err := os.ReadFile(pidFile)
				if err != nil {
					return
				}
				if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
					syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			for _, verbose := range []bool{false, true} {
				rows, _, _ := DoctorRoute(profile, "apikey", "https://example.test", c.model, "",
					func(string) TranslatorNeed { return c.need }, "", "", verbose)
				var got []string
				for _, r := range rows {
					if strings.HasPrefix(r.Msg, "adapter:") {
						got = append(got, r.Kind+" "+r.Msg)
					}
				}
				if !reflect.DeepEqual(got, c.want) {
					t.Fatalf("verbose=%v: adapter rows %q, want %q", verbose, got, c.want)
				}
			}
		})
	}
}

// DurationWords is the budget's spelling in a row: whole seconds in words,
// anything else in Go's own form, so a shortened test bound reads honestly.
func TestDurationWords(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{time.Second, "1 second"},
		{5 * time.Second, "5 seconds"},
		{10 * time.Second, "10 seconds"},
		{300 * time.Millisecond, "300ms"},
		{1500 * time.Millisecond, "1.5s"},
	} {
		if got := DurationWords(c.d); got != c.want {
			t.Errorf("DurationWords(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// runningAdapter's answer for a live adapter whose exec path carries a space:
// the joined command line cannot say where the path ends and the arguments
// begin, the kernel's argv can. The session is reported with the adapter's own
// pid as the in-process shim (GH-49, PR #40), exactly as a live session reads.
// The bound is lengthened: this test is about argv, not about winning a race
// with a loaded machine.
func TestRunningAdapterMatchesArgvAcrossSpaces(t *testing.T) {
	orig := sessionReadBound
	sessionReadBound = 30 * time.Second
	t.Cleanup(func() { sessionReadBound = orig })

	profile := "ra" + strconv.Itoa(os.Getpid())
	cmd := holdProc(t, os.Args[0], BridgeFiles(t.TempDir(), "p")[2],
		"/x/my adapter/ccmorph-core", "adapter", profile)
	want := fmt.Sprintf("%d\t%d:shim", cmd.Process.Pid, cmd.Process.Pid)
	if got := runningAdapter(profile); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := runningAdapter("no-such-profile"); got != "" {
		t.Fatalf("a profile nothing serves must report nothing, got %q", got)
	}
}

// A ps that stalls past runningAdapter's one-second bound is work, not only an
// unanswered question (GH-515): the read runs in a goroutine nothing can join,
// so the ps that goroutine is in must end with the answer. The fake ps records
// its pid and sleeps the bound out; __UNKNOWN__ comes at the bound, and the pid
// must be gone shortly after.
func TestRunningAdapterEndsAPsStalledPastItsBound(t *testing.T) {
	// On darwin the table is the kernel's sysctl, which no PATH double reaches;
	// the ps fallback is the reader this bound exists for, so force it.
	orig := sysctlTable
	sysctlTable = func() (map[int]psRow, bool) { return nil, false }
	t.Cleanup(func() { sysctlTable = orig })

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "ps.pid")
	script := "#!/bin/sh\necho $$ > '" + pidFile + "'\nexec /bin/sleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A run that fails before the ESRCH check below must not leave the sleep
	// behind: the cleanup reads the pid the script recorded and kills it
	// (GH-449). The script writes the file before it execs the sleep, so a run
	// that got as far as the read always has it; ESRCH means the check below
	// got there first.
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	start := time.Now()
	if got := runningAdapter("stall-" + strconv.Itoa(os.Getpid())); got != "__UNKNOWN__" {
		t.Fatalf("a stalled ps must be answered __UNKNOWN__, got %q", got)
	}
	// The ps cannot finish inside the bound, so only the bound can answer: an
	// answer sooner means the fake never stalled and the check below is empty.
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 1500*time.Millisecond {
		t.Fatalf("a ps stalled past the bound must be answered at it, took %v", elapsed)
	}

	// The fake names the process the read started; it must not outlive the
	// answer. The kill and the read's Wait land a moment after the deadline.
	limit := time.Now().Add(2 * time.Second)
	var pid int
	for pid == 0 {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			if n, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && n > 0 {
				pid = n
			}
		}
		if pid == 0 && time.Now().After(limit) {
			t.Fatalf("the fake ps never recorded its pid: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	limit = time.Now().Add(2 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(limit) {
			t.Fatalf("the stalled ps (pid %d) still runs after runningAdapter returned", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// adapterLine's answer for every branch, exactly as the reader sees it.
func TestAdapterLine(t *testing.T) {
	const list = `{"data":[]}`
	translatorSession := "4321\t4322:cliproxyapi,4321:shim"
	cases := []struct {
		name                    string
		model, listing, running string
		need                    TranslatorNeed
		brew                    bool
		kind, msg               string
	}{
		{"needed, installed", "", "", "", TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"needed, not installed, brew on PATH", "", "", "", TranslatorNeed{Known: true, Needed: true}, true, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: brew install cliproxyapi"},
		{"needed, not installed, no brew", "", "", "", TranslatorNeed{Known: true, Needed: true}, false, "FAIL",
			"adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: https://github.com/router-for-me/CLIProxyAPI"},
		{"needed, installed, in use", "", "", translatorSession, TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed; an open session on this profile is using one now"},
		{"not needed", "", "", "", TranslatorNeed{Known: true}, true, "OK",
			"adapter: this endpoint needs no translator"},
		{"session running a translator", "", "", translatorSession, TranslatorNeed{}, true, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session with the hooks shim and a translator", "", "", "4321\t4322:cliproxyapi,4323:hooks-shim", TranslatorNeed{}, true, "OK",
			"adapter: this profile needs a translator now: an open session on it is running cliproxyapi"},
		{"session without a translator", "", "", "4321\t4321:shim", TranslatorNeed{}, true, "OK",
			"adapter: this profile needs no translator now: its open session runs without one"},
		{"no model, installed", "", list, "", TranslatorNeed{Installed: true}, true, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is installed; choose one: ccmorph set demo model <model-id>"},
		{"no model, not installed", "", list, "", TranslatorNeed{}, false, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is not installed; choose one: ccmorph set demo model <model-id>"},
		{"no model, process listing unreadable", "", list, "__UNKNOWN__", TranslatorNeed{}, false, "INFO",
			"adapter: this profile needs no translator until a model is chosen (none is set, and the endpoint's model list names no protocol); cliproxyapi is not installed; choose one: ccmorph set demo model <model-id>"},
		{"needed, installed, session unreadable", "", "", "__UNKNOWN__", TranslatorNeed{Known: true, Needed: true, Installed: true}, true, "OK",
			"adapter: this endpoint needs a translator; cliproxyapi is installed"},
		{"saved model, process listing unreadable", "glm-4.6", list, "__UNKNOWN__", TranslatorNeed{Installed: true}, true, "", ""},
		{"saved model nothing classified", "glm-4.6", list, "", TranslatorNeed{Installed: true}, true, "", ""},
		{"no model, no listing", "", "", "", TranslatorNeed{}, false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stepPATH(t, c.brew)
			kind, msg := adapterLine("demo", c.model, c.listing, c.running, c.need)
			if kind != c.kind || msg != c.msg {
				t.Fatalf("got %q %q, want %q %q", kind, msg, c.kind, c.msg)
			}
		})
	}
}
