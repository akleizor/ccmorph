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
