package usage

import (
	"strconv"
	"time"
)

// EnvNow freezes the clock (unix epoch seconds) for tests and for reproducing
// status line bugs. Honoured by every command that renders or reports times.
const EnvNow = "JULIENNING_NOW_EPOCH"

// Now returns the wall clock, or the frozen clock when EnvNow is set. The
// returned time is in time.Local so callers render in the user's zone.
func Now(env func(string) string) (time.Time, error) {
	raw := env(EnvNow)
	if raw == "" {
		return time.Now(), nil
	}
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || secs < 0 {
		return time.Time{}, &CodedError{
			Code:    CodeClockInvalid,
			Message: EnvNow + " must be integer epoch seconds",
		}
	}
	return time.Unix(secs, 0), nil
}
