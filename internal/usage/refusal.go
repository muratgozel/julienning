package usage

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// RefusalReset returns when the account takes work again: the reset named in
// the refusal text (see ParseRefusalReset), else the weekly reset of the last
// sent report while it is still ahead of now. ok is false when neither is
// known. Every refusal is reported against the week, the only usage window
// left (the five-hour one went away around 2026-10-31), a refusal for a
// model's own weekly limit ("You've hit your Opus limit") included.
func RefusalReset(text string, sent *Sent, now time.Time) (time.Time, bool) {
	if t, ok := ParseRefusalReset(text, now); ok {
		return t, true
	}
	if sent == nil || sent.WeekResets <= 0 {
		return time.Time{}, false
	}
	if t := time.Unix(sent.WeekResets, 0); t.After(now) {
		return t, true
	}
	return time.Time{}, false
}

// refusalResetRe matches the reset in Claude's refusal text:
//
//	resets Oct 13 at 8pm (Europe/Istanbul)
//	resets Oct 13 at 8:30pm (Europe/Istanbul)
//	resets 3pm (Europe/Istanbul)
//	resets 3:15am (Europe/Istanbul)
//
// Groups: month, day (dated form only), hour, minute (optional), am/pm, zone.
var refusalResetRe = regexp.MustCompile(`(?i)\bresets\s+(?:([a-z]{3,9})\s+(\d{1,2})\s+at\s+)?(\d{1,2})(?::(\d{2}))?\s*([ap]m)\s*\(([^()\s]+)\)`)

// maxResetAhead bounds a parsed reset. Usage limits reset within days (a
// week at most); a date further out is a misread (say, last December's date
// read in January as next December's) that would sink an account for months.
const maxResetAhead = 32 * 24 * time.Hour

// ParseRefusalReset reads the reset from a refusal text, best effort. The
// zone in parentheses is an IANA name (an unknown one yields no reset). The
// dated form is in now's year in that zone, or the next year when that lands
// more than 30 days in the past; the time-only form is today in that zone,
// or tomorrow when today's is not after now. Times are built with time.Date
// in the zone, so a DST change between now and the reset is accounted for.
// ok is false unless the result is after now (and within maxResetAhead).
func ParseRefusalReset(text string, now time.Time) (time.Time, bool) {
	for _, m := range refusalResetRe.FindAllStringSubmatch(text, -1) {
		if t, ok := resetFromMatch(m, now); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func resetFromMatch(m []string, now time.Time) (time.Time, bool) {
	monthName, dayStr, hourStr, minStr, ampm, zone := m[1], m[2], m[3], m[4], m[5], m[6]
	hour, err := strconv.Atoi(hourStr)
	if err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	hour %= 12
	if strings.EqualFold(ampm, "pm") {
		hour += 12
	}
	minute := 0
	if minStr != "" {
		if minute, err = strconv.Atoi(minStr); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	// "Local" would silently mean this machine's zone, which is not what
	// Claude printed.
	if zone == "Local" {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, false
	}
	nl := now.In(loc)

	var t time.Time
	if monthName != "" {
		month, ok := parseMonth(monthName)
		if !ok {
			return time.Time{}, false
		}
		day, err := strconv.Atoi(dayStr)
		if err != nil {
			return time.Time{}, false
		}
		t, ok = civil(nl.Year(), month, day, hour, minute, loc)
		if !ok {
			return time.Time{}, false
		}
		if now.Sub(t) > 30*24*time.Hour {
			if t, ok = civil(nl.Year()+1, month, day, hour, minute, loc); !ok {
				return time.Time{}, false
			}
		}
	} else {
		y, mo, d := nl.Date()
		t = time.Date(y, mo, d, hour, minute, 0, 0, loc)
		if !t.After(now) {
			t = time.Date(y, mo, d+1, hour, minute, 0, 0, loc)
		}
	}
	if !t.After(now) || t.Sub(now) > maxResetAhead {
		return time.Time{}, false
	}
	return t, true
}

// civil builds a wall-clock time in loc, refusing dates time.Date would
// normalise into another day (Feb 30, Sep 31).
func civil(year int, month time.Month, day, hour, minute int, loc *time.Location) (time.Time, bool) {
	t := time.Date(year, month, day, hour, minute, 0, 0, loc)
	if t.Month() != month || t.Day() != day {
		return time.Time{}, false
	}
	return t, true
}

var monthNames = []string{"january", "february", "march", "april", "may", "june",
	"july", "august", "september", "october", "november", "december"}

// parseMonth accepts an English month name or any abbreviation of at least
// three letters ("Oct", "Sept", "October").
func parseMonth(s string) (time.Month, bool) {
	s = strings.ToLower(s)
	for i, name := range monthNames {
		if len(s) >= 3 && strings.HasPrefix(name, s) {
			return time.Month(i + 1), true
		}
	}
	return 0, false
}
