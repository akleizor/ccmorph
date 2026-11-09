package cli

import "testing"

func TestListingFailureNamesTheEndpoint(t *testing.T) {
	// Issue #357: HTTP 404 cannot tell a gateway that offers no model list from a
	// wrong endpoint, so both fixes are named: a model id given by hand, and the
	// endpoint check the 404 message has always named.
	msg := listingFailure("gw", "https://gw.test/v1", 404)
	if msg != "'https://gw.test/v1' has no model list (HTTP 404) — a model id must be given by hand: ccmorph set gw model <model-id>, or check the endpoint with: ccmorph config gw" {
		t.Fatalf("404: %s", msg)
	}
	// A rejected key is repaired on the profile, and the failure must keep
	// naming the profile's own endpoint command.
	msg = listingFailure("gw", "https://gw.test/v1", 401)
	if msg != "'https://gw.test/v1' rejected the API key for 'gw' (HTTP 401) — replace it with: ccmorph config gw" {
		t.Fatalf("401: %s", msg)
	}
}
