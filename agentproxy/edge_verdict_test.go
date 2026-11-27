package agentproxy

import (
	"net/url"
	"strings"
	"testing"
)

// test_every_session_turn_answers_which_upstream_served_it (GH-920), the verdict
// each turn's reply line carries. The evidence is the address the answering
// connection was dialled for.
func TestEveryTurnAnswersWhichUpstreamServedIt(t *testing.T) {
	gw := &edgeState{asked: "gw", via: "gw"}
	account := &edgeState{asked: "account", via: "account", account: true}
	for _, c := range []struct {
		name       string
		es         *edgeState
		firstParty bool
		host       string
		port       int
		dialed     string
		want       string
	}{
		{"its pick's endpoint answered", gw, false, "127.0.0.1", 54505, "127.0.0.1:54505", "route     <- 200 /v1/messages asked=gw served-by=127.0.0.1:54505"},
		{"subscription answered a routed turn", gw, true, "127.0.0.1", 54505, "127.0.0.1:54505", "miss      <- 200 /v1/messages asked=gw served-by=ANTHROPIC (not routed!)"},
		{"another endpoint replied", gw, false, "127.0.0.1", 54505, "127.0.0.1:60111", "miss      <- 200 /v1/messages asked=gw served-by=127.0.0.1:60111"},
		{"the pick fell back to the route", &edgeState{asked: "ds", via: "route"}, false, "127.0.0.1", 60111, "127.0.0.1:60111", "miss      <- 200 /v1/messages asked=ds via=route served-by=127.0.0.1:60111"},
		{"account answered with its signature", account, true, "api.anthropic.com", 443, "api.anthropic.com:443", "account   <- 200 /v1/messages asked=account served-by=ANTHROPIC"},
		{"account answered without it", account, false, "api.anthropic.com", 443, "api.anthropic.com:443", "miss      <- 200 /v1/messages asked=account served-by=api.anthropic.com:443 (no Anthropic signature)"},
		{"the shared route, no false miss", &edgeState{asked: "route", via: "route"}, false, "127.0.0.1", 60111, "127.0.0.1:60111", "route     <- 200 /v1/messages asked=route served-by=127.0.0.1:60111"},
		{"another IP on the pick's port", gw, false, "127.0.0.1", 54505, "127.0.0.2:54505", "miss      <- 200 /v1/messages asked=gw served-by=127.0.0.2:54505"},
		{"::1 in another spelling", gw, false, "::1", 54505, "[0:0:0:0:0:0:0:1]:54505", "route     <- 200 /v1/messages asked=gw served-by=[0:0:0:0:0:0:0:1]:54505"},
		{"no evidence is no match", gw, false, "127.0.0.1", 54505, "", "unverified <- 200 /v1/messages asked=gw served-by=127.0.0.1:54505"},
	} {
		if got := verdict(c.es, 200, "/v1/messages", c.firstParty, c.host, c.port, c.dialed); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// test_which_upstream_served_compares_a_named_upstream_by_host: a named
// upstream is served only when that name answered. The connection is known by
// the name it was dialled for, never by the IP it resolved to, so a reply from
// another name on the same port is a miss, and the pick's own name is not made
// a miss by its IP (#920).
func TestWhichUpstreamServedComparesANamedUpstreamByHost(t *testing.T) {
	named := &edgeState{asked: "named", via: "named"}
	var kinds []string
	for _, c := range []struct {
		port   int
		dialed string
	}{
		{443, "gw.example:443"},    // the pick's own name answered
		{443, "other.example:443"}, // another name, same port
		{8443, "gw.example:443"},   // the pick's name, another port
		{443, "GW.Example.:443"},   // the same name, spelled otherwise
	} {
		line := verdict(named, 200, "/v1/messages", false, "gw.example", c.port, c.dialed)
		if !strings.Contains(line, "asked=named served-by="+c.dialed) {
			t.Errorf("%q does not name the endpoint that answered", line)
		}
		kinds = append(kinds, strings.Fields(line)[0])
	}
	if strings.Join(kinds, " ") != "route miss miss route" {
		t.Fatalf("verdicts: %v", kinds)
	}
}

// End to end: the reply line names the address the connection was dialled
// for, so a named upstream reads as its name.
func TestTheReplyLineNamesTheAddressDialled(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.e.Upstream, _ = url.Parse(strings.Replace(r.e.Upstream.String(), "127.0.0.1", "localhost", 1))
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	r.expect(t, r.route, "the route")
	want := "route     <- 200 /v1/messages asked=route served-by=" + r.e.Upstream.Host
	if !strings.Contains(r.log.String(), want) {
		t.Fatalf("the reply line lacks %q:\n%s", want, r.log)
	}
}
