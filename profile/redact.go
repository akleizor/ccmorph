package profile

import (
	"net/url"
	"strings"
)

// RedactURL is a URL safe to print: no userinfo, no query, no fragment.
func RedactURL(raw string) string {
	// A password can hold /, ? or #, or open on digits Go reads as a port, so
	// neither parser reliably finds the @ that closes the userinfo. Cut through
	// the last @ of the whole input before either one looks: nothing that
	// parses afterwards can hold userinfo. An @ in a path costs a wrong host,
	// never a leaked secret.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		i, tail := strings.Index(raw, "://"), raw[at+1:]
		if i < 0 || i > at || tail == "" || strings.ContainsAny(tail[:1], "/?#") {
			return "<redacted>"
		}
		raw = raw[:i+3] + tail
	}
	u, err := url.Parse(raw)
	if err != nil {
		// Go refuses hosts urlsplit takes: say nothing when no host is left.
		scheme, netloc, path := SplitURL(raw)
		if netloc == "" {
			return "<redacted>"
		}
		return scheme + "://" + strings.ToLower(netloc) + path
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if p := u.Port(); p != "" {
		host += ":" + p
	}
	if host == "" {
		return u.Scheme + ":" + u.Path
	}
	return u.Scheme + "://" + host + u.Path
}

// SplitURL is urlsplit's scheme, netloc and path for "scheme://netloc/path".
func SplitURL(raw string) (scheme, netloc, path string) {
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = strings.ToLower(raw[:i]), raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			netloc, rest = rest[:j], rest[j:]
		} else {
			netloc, rest = rest, ""
		}
	}
	if j := strings.IndexAny(rest, "?#"); j >= 0 {
		rest = rest[:j]
	}
	return scheme, netloc, rest
}
