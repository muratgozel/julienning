package usage

import (
	"math"
	"strconv"
	"time"
)

// sep is the separator the bash status line used: U+00B7 with spaces.
const sep = " · "

// Pending is appended when a rate-limit window is not there yet.
const Pending = "usage pending"

// StatusLine renders `<model> · ctx <n>%`. The model falls back to
// "unknown model" and ctx is dropped when the payload has no context window.
func StatusLine(in Input) string {
	model := in.Model
	if model == "" {
		model = "unknown model"
	}
	if !in.HasCtx {
		return model
	}
	// math.Round matches jq's `round`: halves go away from zero.
	return model + sep + "ctx " + strconv.Itoa(int(math.Round(in.Ctx))) + "%"
}

// FormatReset renders a rate-limit reset time in loc: `HH:MM` today,
// `Mon HH:MM` within the next six days, else `2006-01-02 15:04`. A reset that
// has already passed renders as "reset".
func FormatReset(t time.Time, now time.Time, loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	if t.IsZero() {
		return "-"
	}
	if !t.After(now) {
		return "reset"
	}
	tl, nl := t.In(loc), now.In(loc)
	switch d := calendarDays(nl, tl); {
	case d <= 0:
		return tl.Format("15:04")
	case d <= 6:
		return tl.Format("Mon 15:04")
	default:
		return tl.Format("2006-01-02 15:04")
	}
}

// calendarDays counts whole calendar days from a to b in their (shared)
// location. Comparing civil dates rather than subtracting durations keeps the
// answer right across DST transitions, where a "day" is 23 or 25 hours.
func calendarDays(a, b time.Time) int {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	da := time.Date(ay, am, ad, 0, 0, 0, 0, time.UTC)
	db := time.Date(by, bm, bd, 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}

// ShortDuration renders a compact, human duration: 45s, 12m, 3h, 2d.
// Negative durations (clock skew between machines) clamp to 0s.
func ShortDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}

// Ago renders an age for the UPDATED column: "2m ago".
func Ago(d time.Duration) string { return ShortDuration(d) + " ago" }
