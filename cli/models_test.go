package cli

import "testing"

func TestListingFailureNamesTheEndpoint(t *testing.T) {
	msg := listingFailure("gw", "https://gw.test/v1", 404)
	if msg != "'https://gw.test/v1' has no model list (HTTP 404) — check the endpoint with: ccmorph config gw" {
		t.Fatalf("404: %s", msg)
	}
}
