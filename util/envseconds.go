package util

import (
	"fmt"
	"math"
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
// duration. The value may be fractional ("0.5"); an unset or empty variable
// yields def seconds. Anything else is refused with an error naming the
// variable: a non-number, a non-positive count (0 reads as an already elapsed
// deadline to some callers and as "no timeout" to others, never as what the
// operator meant), or a count above maxEnvSeconds, whose nanoseconds do not
// fit and wrap negative. The range test runs on the parsed float, before the
// conversion, so no out-of-range count reaches time.Duration.
func EnvSeconds(getenv func(string) string, name string, def float64) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return time.Duration(def * float64(time.Second)), nil
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(secs) || secs <= 0 || secs > float64(maxEnvSeconds) {
		return 0, fmt.Errorf("%s must be a number of seconds greater than 0 and at most %d, got %q", name, maxEnvSeconds, raw)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
