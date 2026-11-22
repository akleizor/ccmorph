package util

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// maxEnvSeconds is the largest count of seconds a time.Duration holds: one
// more multiplies past MaxInt64 ns and wraps negative, which would arm a
// deadline already past. An out-of-range count is refused before the
// multiplication, like any other unusable value.
const maxEnvSeconds = math.MaxInt64 / int64(time.Second)

// EnvSeconds reads a number of seconds from getenv(name) and returns it as a
// duration. The value may be whole or fractional ("0.5", "1e3"); an unset or
// empty variable yields def seconds. Anything else is refused with an error
// naming the variable: a non-number; a value under one nanosecond (0 reads as
// an already elapsed deadline to some callers and as "no timeout" to others,
// never as what the operator meant, and a smaller fraction truncates to that
// same 0); or a count above maxEnvSeconds, whose nanoseconds do not fit and
// wrap negative. ParseFloat screens only the syntax (and so refuses an exponent
// float64 cannot hold at all, like "1e400"); the bound is never compared in
// float64, which near 9223372036 cannot tell 9223372036.0000001 from
// 9223372036: the bound and the nanoseconds come from the exact decimal, before
// any conversion to time.Duration (#770).
func EnvSeconds(getenv func(string) string, name string, def float64) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return time.Duration(def * float64(time.Second)), nil
	}
	bad := fmt.Errorf("%s must be a number of seconds from 0.000000001 to %d, got %q", name, maxEnvSeconds, raw)
	s := strings.TrimSpace(raw)
	// !(secs > 0) refuses NaN as well as zero and negatives; the parsed value
	// decides nothing else.
	if secs, err := strconv.ParseFloat(s, 64); err != nil || !(secs > 0) {
		return 0, bad
	}
	exact, ok := new(big.Rat).SetString(s)
	if !ok || exact.Cmp(new(big.Rat).SetInt64(maxEnvSeconds)) > 0 {
		return 0, bad
	}
	ns := new(big.Int).Mul(exact.Num(), big.NewInt(int64(time.Second)))
	ns.Quo(ns, exact.Denom())
	if ns.Sign() <= 0 {
		return 0, bad
	}
	return time.Duration(ns.Int64()), nil
}
