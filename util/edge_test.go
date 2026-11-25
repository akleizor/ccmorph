package util

import "testing"

// #636: the shim skipped usage rows on CCMORPH_EDGE_ROUTE_ENV alone while the
// cli cleared it only beside a loopback proxy; InEdge is the one test.
func TestInEdgeNeedsTheVariableAndALoopbackProxy(t *testing.T) {
	for _, c := range []struct {
		env, proxy string
		want       bool
	}{
		{"K=v", "http://127.0.0.1:47100", true},
		{"K=v", "http://proxy.corp:8080", false},
		{"K=v", "", false},
		{"", "http://127.0.0.1:47100", false},
	} {
		t.Setenv("CCMORPH_EDGE_ROUTE_ENV", c.env)
		t.Setenv("HTTPS_PROXY", c.proxy)
		t.Setenv("https_proxy", "")
		if got := InEdge(); got != c.want {
			t.Errorf("env %q proxy %q: InEdge = %v, want %v", c.env, c.proxy, got, c.want)
		}
	}
}
