package netroute

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func urlOf(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad test URL %q: %v", raw, err)
	}
	return u
}

func reqOf(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		t.Fatalf("bad test request %q: %v", raw, err)
	}
	return req
}

func TestParse(t *testing.T) {
	for _, c := range []struct{ knob, want string }{
		{"", ""},
		{"   ", ""},
		{"http://bridge.test:3128", "http://bridge.test:3128"},
		{" http://bridge.test:3128 ", "http://bridge.test:3128"}, // the conf's trailing space is no proxy it names
		{"127.0.0.1:3128", ""}, // no scheme: unparsable, as it always was
		{"://bad", ""},
		{"http://proxy:bad-port", ""},  // a port Go refuses and no reading recovers
		{"http://[::1", ""},            // an unclosed host
		{"http://:3128", ""},           // a port with no host dials this machine, not a proxy
		{"http://user:pw@:3128", ""},   // nor does userinfo make a hostless authority usable
		{"http://user:p^ss@:3128", ""}, // recovery must not invent a host either
		{"not a url", ""},
	} {
		got := Parse(c.knob)
		if c.want == "" {
			if got != nil {
				t.Errorf("Parse(%q) = %v, want no proxy", c.knob, got)
			}
			continue
		}
		if got == nil || got.String() != c.want {
			t.Errorf("Parse(%q) = %v, want %s", c.knob, got, c.want)
		}
	}
}

// An operator pastes a proxy password with a space, '/', '?', '#', '^', '%' or a
// brace; Go's parser refuses it mid-authority and #65's --print-config still has
// to show the proxy cut to its host. One resolver recovers the spelling the
// probes dial and the translator is handed, so neither falls back to the
// environment while the other takes the knob (GH-580 review).
func TestParseRecoversAPastedUserinfo(t *testing.T) {
	for _, c := range []struct{ name, knob, wantHost, wantUser, wantPass string }{
		{"caret", "http://alice:p^ss@proxy:3128?token=qq", "proxy:3128", "alice", "p^ss"},
		{"space", "http://alice:p ss@proxy:3128?token=qq", "proxy:3128", "alice", "p ss"},
		{"brace", "http://alice:{{PASSWORD_x}}@proxy:3128?token=qq", "proxy:3128", "alice", "{{PASSWORD_x}}"},
		{"empty user", "http://:p^ss@proxy:3128", "proxy:3128", "", "p^ss"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.knob)
			if got == nil {
				t.Fatalf("Parse(%q) = nil; a pasted userinfo inside the authority still names the host after the last @", c.knob)
			}
			if got.Host != c.wantHost {
				t.Fatalf("Parse(%q).Host = %q, want %q", c.knob, got.Host, c.wantHost)
			}
			if got.User == nil || got.User.Username() != c.wantUser {
				t.Fatalf("Parse(%q).User = %v, want username %q", c.knob, got.User, c.wantUser)
			}
			if pass, _ := got.User.Password(); pass != c.wantPass {
				t.Fatalf("Parse(%q).User password = %q, want %q", c.knob, pass, c.wantPass)
			}
			// The recovered spelling is handed to the translator, whose parser
			// is Go's own: it must parse back to the same route.
			back, err := url.Parse(got.String())
			if err != nil || back.Host != c.wantHost {
				t.Fatalf("Parse(%q).String() = %q, does not parse back to %q: %v", c.knob, got, c.wantHost, err)
			}
		})
	}
}

// An '@' after a path, query or fragment may belong to that path or query value,
// not to proxy userinfo: reading it as userinfo would invent a destination no
// part of the knob names as a host. The resolver refuses to guess and leaves the
// call to the environment, the same fallback an unusable knob always had.
func TestParseRejectsAnAtOutsideTheAuthority(t *testing.T) {
	// A query value that itself holds an '@' (built here so no literal address
	// sits in the source): the parser rejects the port, and reading the text
	// after the last '@' as the host would route to a name the knob never gives
	// as a host.
	queryAt := "http://proxy.invalid:bad-port/?tag=" + "someone" + "@" + "other.invalid:3128"
	for _, c := range []struct{ name, knob string }{
		{"query value names a host", queryAt},
		{"password with a slash", "http://alice:pa/ss@proxy:3128?token=qq"},
		{"password with a question mark", "http://alice:pa?ss@proxy:3128?token=qq"},
		{"password with a hash", "http://alice:pa#ss@proxy:3128?token=qq"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Parse(c.knob); got != nil {
				t.Fatalf("Parse(%q) = %v; an '@' outside the authority is ambiguous and must fall back", c.knob, got)
			}
		})
	}
}

func TestProxyTakesTheProfilesRouteUnlessExempt(t *testing.T) {
	via := urlOf(t, "http://bridge.test:3128")
	for _, c := range []struct {
		name, target, noProxy string
		wantVia               bool
	}{
		{"a remote gateway takes the profile's proxy", "https://gateway.lan/v1/messages", "", true},
		{"loopback never takes a proxy", "http://127.0.0.1:4000/v1/messages", "", false},
		{"localhost never takes a proxy", "http://localhost:4000/v1/messages", "", false},
		{"an IPv6 loopback never takes a proxy", "http://[::1]:4000/v1/messages", "", false},
		{"a scoped IPv6 loopback never takes a proxy", "http://[::1%25lo0]:4000/v1/messages", "", false},
		{"NO_PROXY's host goes direct", "http://gateway.lan:8080/v1/models", "gateway.lan", false},
		{"NO_PROXY's IDN entry exempts the punycode host", "https://xn--bcher-kva.example/v1/models", "bücher.example", false},
		{"NO_PROXY's punycode entry exempts the IDN host", "https://bücher.example/v1/models", "xn--bcher-kva.example", false},
		{"NO_PROXY's domain entry never matches an IP literal", "http://192.168.1.10/v1/models", "10", true},
		{"NO_PROXY's leading dot matches subdomains only", "https://api.gateway.lan/v1/models", ".lan", false},
		{"NO_PROXY's bare host matches a subdomain too", "https://api.gateway.lan/v1/models", "gateway.lan", false},
		{"NO_PROXY's other host leaves the route alone", "https://gateway.lan/v1/models", "other.lan", true},
		{"NO_PROXY's port must match", "https://gateway.lan:8443/v1/models", "gateway.lan:443", true},
		{"NO_PROXY's port exempts its own port", "https://gateway.lan:8443/v1/models", "gateway.lan:8443", false},
		{"NO_PROXY's star exempts every host", "https://gateway.lan/v1/models", "*", false},
		{"NO_PROXY's CIDR exempts its range", "http://10.1.2.3/v1/models", "10.0.0.0/8", false},
		{"NO_PROXY's CIDR leaves another range alone", "http://192.168.1.9/v1/models", "10.0.0.0/8", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NO_PROXY", c.noProxy)
			t.Setenv("no_proxy", "")
			got, err := Proxy(via, reqOf(t, c.target))
			if err != nil {
				t.Fatal(err)
			}
			if c.wantVia {
				if got == nil || got.String() != via.String() {
					t.Fatalf("Proxy = %v, want the profile's %s", got, via)
				}
				return
			}
			if got != nil {
				t.Fatalf("Proxy = %v, want a direct dial", got)
			}
		})
	}
}

// TestStdlibProxyChild answers, for the URL its environment names, whether the
// shipped toolchain's own resolution would dial it directly. stdlibExempt runs
// it as a fresh test binary because net/http caches the first environment it
// reads process-wide, and the comparison table needs one answer per
// environment. Run on its own it does nothing.
func TestStdlibProxyChild(t *testing.T) {
	target := os.Getenv("CCMORPH_NETROUTE_STDLIB_URL")
	if target == "" {
		return
	}
	u, err := url.Parse(target)
	if err != nil {
		fmt.Printf("ccmorph-netroute-stdlib: bad URL: %v\n", err)
		return
	}
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: u})
	switch {
	case err != nil:
		fmt.Printf("ccmorph-netroute-stdlib: %v\n", err)
	case proxy == nil:
		fmt.Println("ccmorph-netroute-stdlib: direct")
	default:
		fmt.Println("ccmorph-netroute-stdlib: proxy")
	}
}

// stdlibExempt asks http.ProxyFromEnvironment -- the code the shipped toolchain
// runs -- whether target is dialed directly under env. A fresh process answers
// each row: net/http reads its proxy environment once per process, so a table
// cannot call it in-process. Both schemes name a proxy there, so "direct" is
// distinguishable from "nothing configured"; that is the same question Exempt
// asks.
func stdlibExempt(t *testing.T, target string, env map[string]string) bool {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdlibProxyChild$")
	var child []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
			"REQUEST_METHOD", "CCMORPH_NETROUTE_STDLIB_URL":
			continue // the row decides these; REQUEST_METHOD would arm the CGI refusal
		}
		child = append(child, kv)
	}
	child = append(child, "CCMORPH_NETROUTE_STDLIB_URL="+target,
		"HTTP_PROXY="+exemptConfig, "HTTPS_PROXY="+exemptConfig)
	for k, v := range env {
		child = append(child, k+"="+v)
	}
	cmd.Env = child
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the stdlib comparison child failed: %v (output %q)", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		got, ok := strings.CutPrefix(line, "ccmorph-netroute-stdlib: ")
		if !ok {
			continue
		}
		switch got {
		case "direct":
			return true
		case "proxy":
			return false
		}
		t.Fatalf("the stdlib comparison child answered %q", got)
	}
	t.Fatalf("the stdlib comparison child printed no answer: %q", out)
	return false
}

// Exempt is the resolver's answer to the environment's exemption question, so
// it must match the shipped net/http for every spelling of NO_PROXY -- above
// all when both are set: net/http reads NO_PROXY first, and reading no_proxy
// first turns that host's one answer into two (GH-773).
func TestExemptMatchesTheShippedProxyFromEnvironment(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"neither spelling", nil},
		{"NO_PROXY alone", map[string]string{"NO_PROXY": "gateway.lan"}},
		{"no_proxy alone", map[string]string{"no_proxy": "gateway.lan"}},
		{"both name different hosts", map[string]string{"NO_PROXY": "gateway.lan", "no_proxy": "lan.internal"}},
		{"both name different hosts, reversed", map[string]string{"NO_PROXY": "lan.internal", "no_proxy": "gateway.lan"}},
		{"NO_PROXY empty, no_proxy set", map[string]string{"NO_PROXY": "", "no_proxy": "gateway.lan"}},
		{"NO_PROXY set, no_proxy empty", map[string]string{"NO_PROXY": "gateway.lan", "no_proxy": ""}},
		{"both empty", map[string]string{"NO_PROXY": "", "no_proxy": ""}},
		{"NO_PROXY star, no_proxy a host", map[string]string{"NO_PROXY": "*", "no_proxy": "gateway.lan"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"NO_PROXY", "no_proxy"} {
				t.Setenv(k, c.env[k])
			}
			for _, target := range []string{
				"http://gateway.lan/v1/models",
				"https://lan.internal/v1/models",
				"https://other.lan/v1/models",
			} {
				want := stdlibExempt(t, target, c.env)
				if got := Exempt(urlOf(t, target)); got != want {
					t.Errorf("%s: Exempt = %v but http.ProxyFromEnvironment = %v (direct); NO_PROXY=%q no_proxy=%q. "+
						"The exemption reader must take NO_PROXY before no_proxy, as the shipped net/http does (GH-773)",
						target, got, want, c.env["NO_PROXY"], c.env["no_proxy"])
				}
			}
		})
	}
}

// A pasted password may already be spelled with %HH and still need recovery for
// one unparseable character (a space). Recovery decodes every well-formed escape
// and leaves a literal percent to be re-encoded, exactly as url.Parse treats a
// knob it accepts, so the same credential keeps one spelling on every reader
// (GH-773).
func TestParseRecoveryKeepsASpelledEscape(t *testing.T) {
	for _, c := range []struct{ name, knob, wantPass string }{
		{"spelled escape and a space", "http://alice:svc%2Bteam cert@proxy:3128", "svc+team cert"},
		{"spelled escape beside a literal percent", "http://alice:svc%2Bteam%off cert@proxy:3128", "svc+team%off cert"},
	} {
		t.Run(c.name, func(t *testing.T) {
			recovered := Parse(c.knob)
			if recovered == nil {
				t.Fatalf("Parse(%q) = nil; the unparseable character is inside the authority, so recovery still names proxy:3128", c.knob)
			}
			// Go's own spelling for the same credential: each escape decoded
			// once and the literal '%' re-encoded as %25, never a double escape.
			want := url.UserPassword("alice", c.wantPass).String()
			if got := recovered.User.String(); got != want {
				t.Fatalf("Parse(%q) userinfo = %q, want %q; decode each well-formed escape and leave a literal '%%' to be re-encoded (GH-773)",
					c.knob, got, want)
			}
			back, err := url.Parse(recovered.String())
			if err != nil {
				t.Fatalf("the recovered spelling does not parse back: %v", err)
			}
			if pass, _ := back.User.Password(); pass != c.wantPass {
				t.Fatalf("the recovered spelling parses back to password %q, want %q", pass, c.wantPass)
			}
		})
	}
	// The fully encoded twin of the first case: the parseable knob and the
	// recovered one spell the shared credential the same way.
	parseable := Parse("http://alice:svc%2Bteam@proxy:3128")
	if parseable == nil {
		t.Fatal("the parseable knob named no proxy")
	}
	if got, want := parseable.User.String(), url.UserPassword("alice", "svc+team").String(); got != want {
		t.Fatalf("the parseable knob's userinfo = %q, want Go's own spelling %q", got, want)
	}
}

// A profile that names no usable proxy leaves the route to the environment,
// exactly as net/http resolves it; an unparsable knob is no usable proxy.
func TestNoUsableKnobLeavesTheEnvironmentItsOwnResolution(t *testing.T) {
	for _, knob := range []string{"", "127.0.0.1:3128", "://bad"} {
		req := reqOf(t, "https://gateway.lan/v1/models")
		want, wantErr := http.ProxyFromEnvironment(req)
		got, err := Proxy(Parse(knob), req)
		if (err == nil) != (wantErr == nil) || (got == nil) != (want == nil) ||
			(got != nil && got.String() != want.String()) {
			t.Errorf("knob %q: Proxy = (%v, %v), want the environment's (%v, %v)", knob, got, err, want, wantErr)
		}
	}
}
