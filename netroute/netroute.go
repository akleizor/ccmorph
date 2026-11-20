// Package netroute decides how one call reaches a profile's upstream: through
// the proxy the profile names, or through the environment's own resolution.
// One resolver, so the probe that admits a session and the client that serves
// it cannot take different routes to the same gateway (GH-580).
package netroute

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"golang.org/x/net/http/httpproxy"
)

// Parse returns the proxy a profile's CCMORPH_UPSTREAM_PROXY names, or nil when
// the knob is empty or unusable: the environment's resolution then stands, as
// it always did for the translator. A value without a scheme and host is
// unusable, and so is one whose authority Go refuses for a reason no reading
// recovers.
func Parse(knob string) *url.URL {
	knob = strings.TrimSpace(knob)
	if knob == "" {
		return nil
	}
	u, err := url.Parse(knob)
	if err == nil && u.Host != "" {
		return u
	}
	return parseSpelledUserinfo(knob)
}

// parseSpelledUserinfo recovers a proxy whose userinfo Go's parser refuses: an
// operator pastes a password holding a space, '^', '%' or a brace, and
// url.Parse stops inside the authority. Recovery stays inside the authority. A
// '/', '?' or '#' before the last '@' may instead be a path or query value that
// holds an '@', and reading it as userinfo would invent a destination named
// nowhere as a host; such a knob is not recovered and the environment resolves
// the call. Inside the authority the last '@' can only be the userinfo
// delimiter -- a host cannot carry one -- so re-encoding that userinfo yields
// the URL the probes dial and the translator is handed, and both take one
// route.
func parseSpelledUserinfo(knob string) *url.URL {
	i := strings.Index(knob, "://")
	if i <= 0 {
		return nil
	}
	scheme := strings.ToLower(knob[:i])
	if scheme != "http" && scheme != "https" {
		return nil
	}
	rest := knob[i+3:]
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return nil
	}
	if strings.ContainsAny(rest[:at], "/?#") {
		return nil
	}
	userinfo, authority := rest[:at], rest[at+1:]
	u, err := url.Parse(scheme + "://" + authority)
	if err != nil || u.Host == "" {
		return nil
	}
	if user, pass, ok := strings.Cut(userinfo, ":"); ok {
		u.User = url.UserPassword(user, pass)
	} else if userinfo != "" {
		u.User = url.User(userinfo)
	}
	return u
}

// Proxy is an http.Transport Proxy selector for calls to one profile's
// upstream. via -- the profile's parsed knob -- decides the route, unless
// NO_PROXY or the loopback exemption says the call must go direct: a proxy
// cannot reach this machine's 127.0.0.1. With no usable via the environment
// resolves the call itself, exactly as net/http does.
func Proxy(via *url.URL, req *http.Request) (*url.URL, error) {
	if via == nil {
		return http.ProxyFromEnvironment(req)
	}
	if req == nil || req.URL == nil || Exempt(req.URL) {
		return nil, nil
	}
	return via, nil
}

// exemptConfig names a proxy no call ever reaches, so that asking httpproxy
// about a URL answers only the exemption question: either scheme's configured
// proxy comes back when the call would be proxied, and nil when it would not.
const exemptConfig = "http://netroute.invalid:1"

// Exempt reports whether the environment's own resolution would dial u
// directly: this machine's loopback, or a host NO_PROXY lists. The rules are
// net/http's own -- httpproxy is the code ProxyFromEnvironment runs -- so
// internationalized names compare in their ASCII form, a scoped ::1 counts as
// loopback, a domain entry never matches an IP literal, and "*", CIDR and
// optional-port entries keep their exact meanings. Only http and https are
// schemes a proxy takes; anything else is not this resolver's route.
func Exempt(u *url.URL) bool {
	if u == nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	if scheme != u.Scheme {
		lowered := *u
		lowered.Scheme = scheme
		u = &lowered
	}
	cfg := &httpproxy.Config{
		HTTPProxy:  exemptConfig,
		HTTPSProxy: exemptConfig,
		// net/http reads no_proxy before NO_PROXY when both are set.
		NoProxy: envAny("no_proxy", "NO_PROXY"),
	}
	proxy, err := cfg.ProxyFunc()(u)
	return err == nil && proxy == nil
}

func envAny(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}
