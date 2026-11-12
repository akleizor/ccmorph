package util

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

// runningAdapter's answer for a live adapter whose exec path carries a space:
// the joined command line cannot say where the path ends and the arguments
// begin, the kernel's argv can. The session is reported with the adapter's own
// pid as the in-process shim (GH-49, PR #40), exactly as a live session reads.
func TestRunningAdapterMatchesArgvAcrossSpaces(t *testing.T) {
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
