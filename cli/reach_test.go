package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
)

// launchCurl puts a curl on PATH that runs body for every request.
func launchCurl(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// gatewayProfile writes one gateway profile with no saved model.
func gatewayProfile(t *testing.T, name, conf string) (state, profiles string) {
	t.Helper()
	state, profiles = t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(profiles, name), 0o755)
	if err := os.WriteFile(filepath.Join(state, name+".conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	return state, profiles
}

// launchAs runs one launch pass with isTTY answering terminal, and returns its
// exit status and everything it wrote to stderr.
func launchAs(t *testing.T, terminal bool, state, profiles, libexec, name string, argv []string) (int, string) {
	t.Helper()
	saved := isTTY
	isTTY = func(int) bool { return terminal }
	defer func() { isTTY = saved }()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedErr := os.Stderr
	os.Stderr = w
	l := &launcher{dir: state, profileDirs: profiles, libexec: libexec, launchEnv: profile.LaunchEnv, memo: map[string]string{}}
	_, rc := l.launch(name, false, false, argv)
	w.Close()
	os.Stderr = savedErr
	out, _ := io.ReadAll(r)
	return rc, string(out)
}

// GH-260: `ccmorph run` on a gateway that never answers sat silent for about
// 21 s in a terminal and 30 s without one. It now says it is checking within
// the progress bound, gives up at doctor's bound, and ends on the same words
// with and without a terminal: the cause with its host and port, the
// private-network advice, and the route check, before any adapter starts.
func TestRunGivesUpOnASilentGatewayWithAProgressLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	launchCurl(t, "exec sleep 30")
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	want := "ccmorph: could not reach 'http://10.255.255.1:8080' — no answer from 10.255.255.1 port 8080 after 1 second " +
		"(if it is an internal endpoint, connect your private network and try again)\n" +
		"  Check the route: ccmorph doctor hole\n"
	outs := map[bool]string{}
	for _, terminal := range []bool{false, true} {
		state, profiles := gatewayProfile(t, "hole", "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k")
		libexec, calls := coreCalls(t)
		start := time.Now()
		rc, out := launchAs(t, terminal, state, profiles, libexec, "hole", nil)
		took := time.Since(start)
		if rc != util.RCGatewayDown {
			t.Fatalf("terminal=%v: rc %d, want %d (the gateway class): %s", terminal, rc, util.RCGatewayDown, out)
		}
		if took > 3*time.Second {
			t.Fatalf("terminal=%v: gave up after %v, want the 0.8s bound", terminal, took)
		}
		progress := "ccmorph: Checking 'hole' gateway (up to 1s); Ctrl-C to cancel.\n"
		if strings.Count(out, progress) != 1 || !strings.HasSuffix(out, want) || strings.Index(out, progress) > strings.Index(out, want) {
			t.Fatalf("terminal=%v: want one progress line, then the ending\n%s\ngot:\n%s", terminal, want, out)
		}
		if strings.Contains(calls(), "adapter-start") {
			t.Fatalf("terminal=%v: the adapter started on a silent gateway:\n%s", terminal, calls())
		}
		outs[terminal] = out
	}
	if outs[true] != outs[false] {
		t.Fatalf("the terminal launch ended differently:\n%s\nno terminal:\n%s", outs[true], outs[false])
	}
}

// GH-260: a refused gateway is named once, in plain words, and the launch goes
// on the way it always has. In a terminal with no saved model the picker used
// to open on that gateway and end on a different sentence than the
// no-terminal launch; the terminal launch now takes the same path, so both end
// on the same words, and neither blames a network.
func TestRunNamesARefusedGatewayTheSameWithAndWithoutATerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	launchCurl(t, "printf '%s\\n' 'curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused' >&2; exit 7")
	cause := "could not reach 'http://127.0.0.1:9' — connection refused: 127.0.0.1 port 9"
	outs, rcs := map[bool]string{}, map[bool]int{}
	for _, terminal := range []bool{false, true} {
		state, profiles := gatewayProfile(t, "gw260", "type=apikey\nbase_url=http://127.0.0.1:9\napi_key=k")
		libexec := t.TempDir()
		log := filepath.Join(libexec, "calls.log")
		os.WriteFile(filepath.Join(libexec, "ccmorph-core"), []byte(
			"#!/bin/sh\n{ for a do printf '%s\\n' \"$a\"; done; printf '.\\n'; } >> '"+log+"'\n"+
				"if [ \"$1\" = util ] && [ \"$2\" = adapter-start ]; then\n"+
				"  echo \"ccmorph: 'gw260': adapter exited before readiness; session not started.\" >&2\n"+
				"  exit 1\nfi\nexit 0\n"), 0o755)
		rc, out := launchAs(t, terminal, state, profiles, libexec, "gw260", nil)
		if rc == 0 {
			t.Fatalf("terminal=%v: rc 0 on a refused gateway: %s", terminal, out)
		}
		if !strings.HasPrefix(out, "ccmorph: "+cause+"\n") {
			t.Fatalf("terminal=%v: the cause must be the launch's first words:\n%s", terminal, out)
		}
		if strings.Contains(out, "network") || strings.Contains(out, "did not answer") || strings.Contains(out, "Checking") {
			t.Fatalf("terminal=%v: a refused port is not a network problem and needs no wait:\n%s", terminal, out)
		}
		if b, _ := os.ReadFile(log); !strings.Contains(string(b), "adapter-start") {
			t.Fatalf("terminal=%v: the launch did not go on to its adapter:\n%s", terminal, b)
		}
		outs[terminal], rcs[terminal] = out, rc
	}
	if outs[true] != outs[false] || rcs[true] != rcs[false] {
		t.Fatalf("the terminal launch (rc %d) ended differently:\n%s\nno terminal (rc %d):\n%s", rcs[true], outs[true], rcs[false], outs[false])
	}
}

// GH-260: `ccmorph run <profile> -i` on a gateway named unreachable said the
// cause twice and stopped, naming no next step. It says the cause once, then
// the route check, opens no picker, starts no adapter, and does not fall back to
// the saved model -i was asked to replace.
func TestRunInteractiveOnAnUnreachableGatewayEndsOnTheRouteCheck(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	launchCurl(t, "printf '%s\\n' 'curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused' >&2; exit 7")
	want := "ccmorph: could not reach 'http://127.0.0.1:9' — connection refused: 127.0.0.1 port 9\n" +
		"  Check the route: ccmorph doctor gwi\n"
	for _, conf := range []string{
		"type=apikey\nbase_url=http://127.0.0.1:9\napi_key=k",
		"type=apikey\nmodel=saved-model\nbase_url=http://127.0.0.1:9\napi_key=k",
	} {
		for _, terminal := range []bool{false, true} {
			state, profiles := gatewayProfile(t, "gwi", conf)
			libexec, calls := coreCalls(t)
			rc, out := launchAs(t, terminal, state, profiles, libexec, "gwi", []string{"-i"})
			if rc != 1 || out != want {
				t.Fatalf("terminal=%v: rc %d, out:\n%s\nwant rc 1 and:\n%s", terminal, rc, out, want)
			}
			if strings.Contains(calls(), "adapter-start") {
				t.Fatalf("terminal=%v: the adapter started:\n%s", terminal, calls())
			}
		}
	}

	// A gateway that never answers ends -i where it ends every other launch.
	launchCurl(t, "exec sleep 30")
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	state, profiles := gatewayProfile(t, "gwi", "type=apikey\nbase_url=http://10.255.255.1:8080\napi_key=k")
	libexec, calls := coreCalls(t)
	rc, out := launchAs(t, true, state, profiles, libexec, "gwi", []string{"-i"})
	silent := "ccmorph: Checking 'gwi' gateway (up to 1s); Ctrl-C to cancel.\n" +
		"ccmorph: could not reach 'http://10.255.255.1:8080' — no answer from 10.255.255.1 port 8080 after 1 second " +
		"(if it is an internal endpoint, connect your private network and try again)\n" +
		"  Check the route: ccmorph doctor gwi\n"
	if rc != util.RCGatewayDown || out != silent || strings.Contains(calls(), "adapter-start") {
		t.Fatalf("silent gateway: rc %d, out:\n%s\nwant rc %d and:\n%s", rc, out, util.RCGatewayDown, silent)
	}
}

// The probe asks the route the launch itself builds (util.ProfileEnv, the read
// the route check uses too): a legacy proxy conf is the same gateway, and the
// key is the stored one, asked once per pass (#260).
func TestRunProbeAsksTheRouteTheLaunchUses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved2 := probeListing
	probeListing = func(string, string, ...string) (string, string) { return "", "000" }
	t.Cleanup(func() { probeListing = saved2 })
	for _, conf := range []string{
		"type=apikey\nbase_url=http://127.0.0.1:9\napi_key=route-key-260",
		"type=proxy\nbase_url=http://127.0.0.1:9\napi_key=route-key-260\nmodel=m",
	} {
		var asked []string
		saved := reachListing
		reachListing = func(base, key, via string, strip []string, progress func()) util.Reach {
			asked = append(asked, base+" "+key)
			return util.Reach{}
		}
		state, profiles := gatewayProfile(t, "gwroute", conf)
		libexec, _ := coreCalls(t)
		_, out := launchAs(t, false, state, profiles, libexec, "gwroute", []string{"--model", "m", "-p", "hi"})
		reachListing = saved
		if strings.Join(asked, ",") != "http://127.0.0.1:9 route-key-260" {
			t.Fatalf("%q: the probe asked %q, want the launch's route once: %s", conf, asked, out)
		}
	}
}

// A launch that opens no session asks the route nothing (#650), the probe
// included, and a URL's credential never reaches the probe's sentence.
func TestRunProbeSkipsALaunchThatOpensNoSessionAndHidesTheURLsCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCMORPH_SUBCOMMAND_LAUNCH", "0")
	asked := 0
	saved := reachListing
	reachListing = func(base, key, via string, strip []string, progress func()) util.Reach {
		asked++
		return util.Reach{}
	}
	state, profiles := gatewayProfile(t, "gw650r", "type=apikey\nbase_url=http://127.0.0.1:9\napi_key=k")
	libexec, _ := coreCalls(t)
	launchAs(t, false, state, profiles, libexec, "gw650r", []string{"--help"})
	reachListing = saved
	if asked != 0 {
		t.Fatalf("a --help launch asked the gateway %d time(s)", asked)
	}

	launchCurl(t, "printf '%s\\n' 'curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused' >&2; exit 7")
	state, profiles = gatewayProfile(t, "gwpw", "type=apikey\nbase_url=http://alice:2024/ss@127.0.0.1:9\napi_key=k")
	libexec, _ = coreCalls(t)
	_, out := launchAs(t, false, state, profiles, libexec, "gwpw", []string{"--model", "m", "-p", "hi"})
	if !strings.Contains(out, "connection refused: 127.0.0.1 port 9") || strings.Contains(out, "alice") || strings.Contains(out, "2024") {
		t.Fatalf("the cause must name the redacted host only:\n%s", out)
	}
}

// A key Claude Code was told to refuse stops the launch before any request
// (#236): the probe does not ask the gateway first.
func TestRunProbeAsksNothingWithARefusedKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	asked := 0
	saved := reachListing
	reachListing = func(base, key, via string, strip []string, progress func()) util.Reach {
		asked++
		return util.Reach{}
	}
	t.Cleanup(func() { reachListing = saved })
	key := "sk-refused-260-fixture-key"
	state, profiles := gatewayProfile(t, "gwno", "type=apikey\nbase_url=http://127.0.0.1:9\napi_key="+key)
	os.WriteFile(filepath.Join(profiles, "gwno", ".claude.json"),
		[]byte(`{"customApiKeyResponses":{"approved":[],"rejected":["`+key[len(key)-20:]+`"]}}`), 0o600)
	libexec, _ := coreCalls(t)
	rc, out := launchAs(t, false, state, profiles, libexec, "gwno", []string{"--model", "m", "-p", "hi"})
	if rc == 0 || asked != 0 {
		t.Fatalf("rc %d, %d probe(s): a refused key must stop the launch before any request: %s", rc, asked, out)
	}
}

// GH-260: a gateway reachable only through the profile's
// env.CCMORPH_UPSTREAM_PROXY is asked through that proxy, the route its
// session takes (docs/guide.md, the profile proxy), never directly; and a shell's
// own CCMORPH_UPSTREAM_PROXY is not the profile's route (adapter environWith).
func TestIssue260ProfileProxyReachProbe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "requests")
	launchCurl(t, "case \" $* \" in *' -f '*) { printf 'ARGS %s\\n' \"$*\"; cat; printf '.\\n'; } >> '"+log+"';; esac\nexit 0")
	for _, c := range []struct{ name, conf, shell, want string }{
		{"the profile's proxy", "type=apikey\nbase_url=http://10.0.0.5:8080\napi_key=k\nenv.CCMORPH_UPSTREAM_PROXY=http://127.0.0.1:3128",
			"", `proxy = "http://127.0.0.1:3128"`},
		{"a shell's knob is not the profile's", "type=apikey\nbase_url=http://10.0.0.5:8080\napi_key=k",
			"http://127.0.0.1:3999", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(log)
			t.Setenv("CCMORPH_UPSTREAM_PROXY", c.shell)
			state, profiles := gatewayProfile(t, "gwvia", c.conf)
			libexec, _ := coreCalls(t)
			launchAs(t, false, state, profiles, libexec, "gwvia", []string{"--model", "m", "-p", "hi"})
			b, _ := os.ReadFile(log)
			got := string(b)
			if !strings.Contains(got, "ARGS ") {
				t.Fatalf("the launch asked the gateway nothing:\n%s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want+"\n") {
				t.Fatalf("launch reachability curl did not use the profile's proxy route:\n%s", got)
			}
			if c.want == "" && strings.Contains(got, "proxy") {
				t.Fatalf("the shell's knob became the probe's route:\n%s", got)
			}
		})
	}
}
