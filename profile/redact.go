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
	// parses afterwards can hold userinfo. The cut is trusted only where that
	// @ must close a userinfo; a path or query @ names no host, because a
	// diagnostic never invents an endpoint.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		i, tail := strings.Index(raw, "://"), raw[at+1:]
		if i < 0 || i > at || tail == "" || strings.ContainsAny(tail[:1], "/?#") {
			return "<redacted>"
		}
		// A / on both sides: the @ may be in a path (gw/v1/@team/x) or end a
		// password holding a /. Either host could be wrong, so name none.
		if strings.Contains(raw[i+3:at], "/") && strings.Contains(tail, "/") {
			return "<redacted>"
		}
		if !closesUserinfo(raw, i, at) {
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

// closesUserinfo reports whether the @ at raw[at] must close a userinfo when
// raw[:i] holds the scheme and its "://". Inside the authority an @ is
// userinfo, its only RFC home. Past the authority it may instead belong to a
// path or a query (gw/v1/@team, gw/api?x=me@corp.com), so the cut is kept
// only where no other reading exists: when no host stands at the authority —
// Go refuses alice:pa as a port, so an @ behind it cannot be a path's — or
// when the userinfo would be one whose digit or empty head Go mistakes for a
// port, whose password runs on unbroken to the @ and whose host carries a
// port of its own, the shape the suite has always answered. Behind any other
// standing authority the @ may be the URL's own, so no host is named for it.
func closesUserinfo(raw string, i, at int) bool {
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if at-i < 3+end {
		return true
	}
	auth := rest[:end]
	if _, err := url.Parse("http://" + auth); err != nil {
		return true
	}
	if !strings.Contains(auth, ":") {
		return false
	}
	// The authority stands with a port, and a digit or empty password head is
	// the one thing that can hide in it: Go reads the head as the port. The
	// @ closes that userinfo only while the run between the authority and it
	// is the password's own tail — non-empty, holding no /, ? or #
	// (alice:2024/ss@proxy:3128) — and the tail behind the @ still parses as
	// a host that carries a port of its own, as the password's always has.
	// Past a second separator (gw:8443/v1/@team:8080), a query's ?
	// (gw:8443/api?x=me@corp.com:8080), a segment boundary
	// (gw:8443/@team:8080) or a tail with no port (gw:8443/v1@team) the @
	// may be the path's or the query's, so no host is named for it. One shape
	// stays cut as the known limit: gw:8443/v1@team:8080 cannot be told from
	// alice:2024/ss@proxy:3128 by anything local to the @.
	run := rest[end+1 : at-i-3]
	if run == "" || strings.ContainsAny(run, "/?#") {
		return false
	}
	u, err := url.Parse("http://" + raw[at+1:])
	return err == nil && u.Port() != ""
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
