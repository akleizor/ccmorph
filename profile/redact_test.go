package profile

import (
	"net/url"
	"testing"
)

func TestRedactURLNeverPrintsTheUserinfo(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		want   string
		parses bool // url.Parse accepts raw, so the success path is the one under test
	}{
		{"caret password", "http://alice:p^ss@proxy:3128", "http://proxy:3128", false},
		{"slash password", "http://alice:pa/ss@proxy:3128", "http://proxy:3128", false},
		{"question password", "http://alice:pa?ss@proxy:3128", "http://proxy:3128", false},
		{"hash password", "http://alice:pa#ss@proxy:3128", "http://proxy:3128", false},
		{"password with an @ after its slash", "http://alice:p/@ss@proxy:3128", "http://proxy:3128", false},
		{"password with an @ before its slash", "http://alice:p@s:s/x@proxy:3128", "http://proxy:3128", false},
		{"password with every terminator", "http://alice:p/?#s@proxy:3128", "http://proxy:3128", false},
		{"slash password before an IPv6 host", "http://alice:pa/ss@[::1]:3128", "http://[::1]:3128", false},
		{"slash password with a path behind the host", "http://alice:pa/ss@proxy:3128/api", "<redacted>", false},
		{"at in a gateway path", "https://gw.example.com/v1/@team/proxy", "<redacted>", true},
		// A gateway URL with an @ in its path or query names no endpoint the
		// URL does not have (#139): the answer is <redacted> or its own host.
		{"gateway path ending in an @ segment (#139)", "https://gw.example.com/v1/@team", "<redacted>", true},
		{"at inside a query (#139)", "https://gw.internal/api?x=me@corp.com", "<redacted>", true},
		{"at path segment on a gateway with a port (#139)", "https://gw.example.com:8443/v1/@team", "<redacted>", true},
		// Whatever the tail behind that @ parses as, it still belongs to the
		// path or the query: a tail that reads as host:port must not turn the
		// segment into an endpoint behind a ported gateway (#164).
		{"path @ behind a ported gateway, tail host:port (#164)", "https://gw.example.com:8443/v1/@team:8080", "<redacted>", true},
		{"query @ behind a ported gateway, tail host:port (#164)", "https://gw.internal:8443/api?x=me@corp.com:8080", "<redacted>", true},
		{"path @ opening the first segment behind a ported gateway (#164)", "https://gw.example.com:8443/@team:8080", "<redacted>", true},
		// An @ that only ends a path segment names no host when the tail
		// carries no port: nothing behind the authority marks a userinfo,
		// so the segment is the URL's own (#164).
		{"path @ ending a segment behind a ported gateway, tail without a port (#164)", "https://gw.example.com:8443/v1@team", "<redacted>", true},
		{"path @ ending a longer segment, host-shaped tail without a port (#164)", "https://gw.example.com:8443/team@corp.com", "<redacted>", true},
		// The same string read from the password's end: a digit head whose
		// password ends on its separator cannot be told from a segment
		// boundary, so no host is named for it either (#164).
		{"digit-headed password ending on its separator (#164)", "http://alice:2024/@proxy:3128", "<redacted>", true},
		{"hash password whose host never comes", "http://alice:pa?ss@", "<redacted>", false},
		{"slash password whose tail is only a path", "http://alice:pa/ss@/api", "<redacted>", false},
		{"empty scheme before the authority", "://alice:pa/ss@proxy:3128", "://proxy:3128", false},
		// An @ past a refused authority cannot be told from a password's tail,
		// so it is read as the separator: a wrong host, never a leaked one.
		{"at in the path of a refused host", "http://proxy:notaport/p@th", "http://th", false},
		{"refused host with no @ anywhere", "http://proxy:notaport/", "http://proxy:notaport/", false},
		// A digit or empty password head reads as a port, so Go accepts these.
		{"digit-headed password before a slash", "http://alice:2024/ss@proxy:3128", "http://proxy:3128", true},
		{"empty password before a slash", "http://alice:/secret@proxy:3128", "http://proxy:3128", true},
		{"digit-headed password before a question", "http://alice:12?ss@proxy:3128", "http://proxy:3128", true},
		{"digit-headed password before a hash", "http://alice:12#ss@proxy:3128", "http://proxy:3128", true},
		{"userinfo with no scheme", "alice:pa/ss@proxy:3128", "<redacted>", true},
		{"plain proxy", "http://Proxy:3128", "http://proxy:3128", true},
		{"IPv6 proxy", "http://[::1]:3128", "http://[::1]:3128", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := url.Parse(c.raw); (err == nil) != c.parses {
				t.Fatalf("url.Parse(%q) error = %v, but the case says parses=%v", c.raw, err, c.parses)
			}
			if got := RedactURL(c.raw); got != c.want {
				t.Fatalf("RedactURL(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}
