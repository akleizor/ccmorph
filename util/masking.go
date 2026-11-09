package util

import "strings"

// MaskOn reads env.CCMORPH_MASK the way the shim does (shim/main.go Listen):
// "1" and "on" turn masking on, "", "0" and "off" leave it off, and any other
// value the shim refuses when the route it fronts starts (#293).
func MaskOn(v string) (on, bad bool) {
	switch strings.TrimSpace(v) {
	case "1", "on":
		return true, false
	case "", "0", "off":
		return false, false
	default:
		return false, true
	}
}

// Maskable is whether a profile of type typ on endpoint base is fronted by the
// shim that masks: an apikey profile on a gateway endpoint, whose launch starts
// the adapter, and every route the adapter serves goes through the shim. A
// subscription, a direct api.anthropic.com connection and a provider's own
// bridge reach their backend without it (docs/guide.md, Secret masking).
func Maskable(typ, base string) bool {
	return typ == "apikey" && !NativeBase(base)
}
