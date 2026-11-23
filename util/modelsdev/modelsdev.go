// Package modelsdev owns the one request ccmorph makes to models.dev: the
// provider/model catalogue (api.json) that two readers share — the price book
// (shim) and the context windows (ctxwindow). The URL, the User-Agent it must
// carry, and each reader's freshness policy are decided here, once.
package modelsdev

import (
	"context"
	"time"
)

// URL is the provider-keyed catalogue document.
const URL = "https://models.dev/api.json"

// UserAgent rides every models.dev request: models.dev answers 403 to a bare
// default User-Agent, so a request without one never gets the document.
const UserAgent = "ccmorph"

// Transport performs the catalogue GET the caller's way: one URL, the
// headers, and a timeout budget the transport owns. The two readers wait
// differently on purpose, so each passes its own (see Get).
type Transport func(ctx context.Context, url string, header [][2]string, timeout time.Duration) ([]byte, error)

// Get is the models.dev request, the only spelling of it. open is the
// caller's transport: the windows' launcher bounds each wait on the socket,
// because a launch must not lose the 5.9 MB document to a 3 s whole-exchange
// cap on a slow link, while the price book's background refresh caps the
// whole exchange, because nothing waits on it.
//
// Freshness, decided once, here: the context windows take the catalogue fresh
// on every run — a window sizes this turn's prompt, and a cached copy would
// report no window for a model that shipped this morning — while the price
// book may serve a day-old copy from its own state-dir cache (shim/price.go),
// because prices move slowly and a session must not re-download the document
// per upstream. A reader that wants the cached price book asks it, not Get.
func Get(ctx context.Context, timeout time.Duration, open Transport) ([]byte, error) {
	return open(ctx, URL, [][2]string{{"User-Agent", UserAgent}}, timeout)
}
