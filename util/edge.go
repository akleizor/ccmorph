package util

import (
	"os"
	"regexp"
	"strconv"
)

var loopbackProxy = regexp.MustCompile(`^http://127\.0\.0\.1:([0-9]+)/?$`)

// LoopbackProxyPort is the port of HTTPS_PROXY (or https_proxy) when it is
// http://127.0.0.1:<port>, the shape of a Remote Control edge's proxy.
func LoopbackProxyPort() (int, bool) {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	m := loopbackProxy.FindStringSubmatch(proxy)
	if m == nil {
		return 0, false
	}
	p, err := strconv.Atoi(m[1])
	return p, err == nil && p > 0 && p <= 65535
}

// InEdge reports that this process runs inside a Remote Control bridge session:
// the add-on sets CCMORPH_EDGE_ROUTE_ENV beside its loopback proxy, and a
// loopback proxy alone may be the user's own. The one predicate the cli's
// edge-env clearing and the shim's usage-row skip both read, so an
// HTTPS_PROXY override inside an edge session cannot keep the variable on one
// side and act on it on the other (#636).
func InEdge() bool {
	_, loopback := LoopbackProxyPort()
	return os.Getenv("CCMORPH_EDGE_ROUTE_ENV") != "" && loopback
}
