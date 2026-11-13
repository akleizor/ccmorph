package util

// ResponseKey is the identity a response's rows are counted under: its id,
// its stamp as written and, when it has one, its turn — so a response the
// store holds under two names (a flat copy and its day folder's, left by a
// move that stopped halfway) counts once, while a recycled id's other turn
// at the same stamp is still its own response (#298). The writer (the shim's
// usage rows) and the readers (quota's spend walk, the report's own count)
// build it here, the one spelling, so the two sides never drift (#514's
// handoff named the doubling; #539 lands it). A row with no id has no
// identity — the caller counts it always — and a row with no turn does not
// carry an empty one.
func ResponseKey(id, stamp, turn string) string {
	key := id + "\x00" + stamp
	if turn != "" {
		key += "\x00" + turn
	}
	return key
}
