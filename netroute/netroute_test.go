package netroute

import (
	"net/http"
	"net/url"
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
