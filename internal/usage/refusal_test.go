package usage

import (
	"testing"
	"time"
)

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestParseRefusalReset(t *testing.T) {
	const weekly = "You've hit your weekly limit · resets Oct 13 at 8pm (Europe/Istanbul)"
	// Istanbul is +03:00 all year: 2026-10-08 12:00 local.
	istNow := "2026-10-08T09:00:00Z"
	cases := []struct {
		name, now, text string
		want            string // RFC 3339 UTC; "" = no reset
	}{
		{"weekly, dated", istNow, weekly, "2026-10-13T17:00:00Z"},
		{"dated with minutes", istNow, "You've hit your weekly limit · resets Oct 13 at 8:30pm (Europe/Istanbul)", "2026-10-13T17:30:00Z"},
		{"error type appended", istNow, weekly + " (error type rate_limit, request req_011)", "2026-10-13T17:00:00Z"},
		{"five-hour, later today", istNow, "You've hit your limit · resets 3pm (Europe/Istanbul)", "2026-10-08T12:00:00Z"},
		{"time with minutes, passed today: tomorrow", istNow, "You've hit your limit · resets 3:15am (Europe/Istanbul)", "2026-10-09T00:15:00Z"},
		{"12am is midnight", istNow, "resets 12am (Europe/Istanbul)", "2026-10-08T21:00:00Z"},
		{"12pm equal to now: tomorrow", istNow, "resets 12pm (Europe/Istanbul)", "2026-10-09T09:00:00Z"},
		{"case and spacing", istNow, "RESETS 3 PM (Europe/Istanbul)", "2026-10-08T12:00:00Z"},
		{"full month name", istNow, "resets October 13 at 8pm (Europe/Istanbul)", "2026-10-13T17:00:00Z"},
		{"another zone", istNow, "resets 3pm (America/New_York)", "2026-10-08T19:00:00Z"},
		{"first usable match wins", istNow, "resets 3pm (Mars/Olympus) · resets 4pm (Europe/Istanbul)", "2026-10-08T13:00:00Z"},

		// Year boundary: the current year, unless that is over 30 days ago.
		{"January date seen in December", "2026-12-30T12:00:00Z", "resets Jan 2 at 8pm (Europe/Istanbul)", "2027-01-02T17:00:00Z"},
		{"date a few days ago", istNow, "resets Oct 1 at 8pm (Europe/Istanbul)", ""},
		{"earlier today, dated", istNow, "resets Oct 8 at 9am (Europe/Istanbul)", ""},
		{"too far ahead", istNow, "resets Dec 25 at 8pm (Europe/Istanbul)", ""},
		{"last December read in January", "2027-01-02T12:00:00Z", "resets Dec 31 at 11pm (Europe/Istanbul)", ""},

		// DST: built in the zone, never by adding hours.
		{"fall back between now and the reset", "2026-10-24T22:00:00Z", "resets 3pm (Europe/Berlin)", "2026-10-25T14:00:00Z"},
		{"spring forward between now and the reset", "2026-03-08T05:00:00Z", "resets 8pm (America/New_York)", "2026-03-09T00:00:00Z"},
		{"tomorrow across spring forward", "2026-03-08T04:00:00Z", "resets 10pm (America/New_York)", "2026-03-09T02:00:00Z"},
		{"dated, after fall back", "2026-10-20T12:00:00Z", "resets Oct 27 at 9am (Europe/Berlin)", "2026-10-27T08:00:00Z"},

		// No usable reset.
		{"unknown zone", istNow, "resets 3pm (Mars/Olympus)", ""},
		{"Local is not a zone Claude prints", istNow, "resets 3pm (Local)", ""},
		{"path in the zone", istNow, "resets 3pm (../../etc/passwd)", ""},
		{"no zone", istNow, "You've hit your limit · resets 3pm", ""},
		{"hour 13", istNow, "resets 13pm (Europe/Istanbul)", ""},
		{"hour 0", istNow, "resets 0am (Europe/Istanbul)", ""},
		{"minute 75", istNow, "resets 3:75pm (Europe/Istanbul)", ""},
		{"Feb 30", istNow, "resets Feb 30 at 8pm (Europe/Istanbul)", ""},
		{"unknown month", istNow, "resets Foo 13 at 8pm (Europe/Istanbul)", ""},
		{"24-hour clock", istNow, "resets 15:00 (Europe/Istanbul)", ""},
		{"nothing", istNow, "API Error: rate limited", ""},
		{"empty", istNow, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseRefusalReset(tc.text, utc(t, tc.now))
			switch {
			case tc.want == "" && ok:
				t.Errorf("got %v, want no reset", got.UTC())
			case tc.want != "" && !ok:
				t.Errorf("no reset, want %s", tc.want)
			case tc.want != "" && !got.Equal(utc(t, tc.want)):
				t.Errorf("got %s, want %s", got.UTC().Format(time.RFC3339), tc.want)
			}
		})
	}
}

// The window: a refusal naming the week is the weekly limit; otherwise the
// last report sent from here decides (higher usage, session on a tie), with
// a window whose reset has passed counting as empty.
func TestRefusalWindow(t *testing.T) {
	now := time.Unix(nowEpoch, 0)
	sent := func(session, week float64) *Sent {
		return &Sent{Payload: Payload{SessionUsed: session, SessionResets: fiveReset, WeekUsed: week, WeekResets: weekReset}}
	}
	cases := []struct {
		name, text string
		sent       *Sent
		want       string
	}{
		{"weekly text", "You've hit your weekly limit · resets Oct 13 at 8pm (Europe/Istanbul)", sent(100, 10), "week"},
		{"week in another case", "WEEK limit reached", nil, "week"},
		{"week in the details only", "rate_limit: seven-day (week) window", sent(90, 10), "week"},
		{"five-hour text, no cache", "You've hit your limit · resets 3pm (Europe/Istanbul)", nil, "session"},
		{"cache: week higher", "You've hit your limit", sent(40, 100), "week"},
		{"cache: session higher", "You've hit your limit", sent(100, 40), "session"},
		{"cache: tie", "You've hit your limit", sent(100, 100), "session"},
		{"cache without a week window", "You've hit your limit", &Sent{Payload: Payload{SessionUsed: 10, SessionResets: fiveReset, WeekUsed: 90}}, "session"},
		{"nothing at all", "", nil, "session"},
		{"cache: session reset passed", "You've hit your limit", &Sent{Payload: Payload{SessionUsed: 100, SessionResets: nowEpoch - 60, WeekUsed: 93, WeekResets: weekReset}}, "week"},
		{"cache: both resets passed", "You've hit your limit", &Sent{Payload: Payload{SessionUsed: 10, SessionResets: nowEpoch - 60, WeekUsed: 90, WeekResets: nowEpoch - 1}}, "session"},
	}
	for _, tc := range cases {
		if got := RefusalWindow(tc.text, tc.sent, now); got != tc.want {
			t.Errorf("%s: window = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The text's reset wins; otherwise the last sent report's reset for that
// window, while it is still ahead.
func TestRefusalReset(t *testing.T) {
	now := time.Unix(nowEpoch, 0) // 2026-09-15T14:30:57Z
	sent := &Sent{Payload: Payload{SessionUsed: 100, SessionResets: fiveReset, WeekUsed: 50, WeekResets: weekReset}}
	cases := []struct {
		name, text, window string
		sent               *Sent
		want               int64 // 0 = none
	}{
		{"text wins", "resets 8pm (Europe/Istanbul)", "session", sent, utc(t, "2026-09-15T17:00:00Z").Unix()},
		{"session from the cache", "You've hit your limit", "session", sent, fiveReset},
		{"week from the cache", "You've hit your weekly limit", "week", sent, weekReset},
		{"unparseable zone falls back", "resets 8pm (Nowhere/Zone)", "week", sent, weekReset},
		{"cached reset passed", "You've hit your limit", "session",
			&Sent{Payload: Payload{SessionResets: nowEpoch - 1, WeekResets: weekReset}}, 0},
		{"cached reset is now", "You've hit your limit", "session",
			&Sent{Payload: Payload{SessionResets: nowEpoch, WeekResets: weekReset}}, 0},
		{"no cache", "You've hit your limit", "session", nil, 0},
		{"empty cache entry", "You've hit your limit", "week", &Sent{}, 0},
	}
	for _, tc := range cases {
		got, ok := RefusalReset(tc.text, tc.window, tc.sent, now)
		switch {
		case tc.want == 0 && ok:
			t.Errorf("%s: got %v, want none", tc.name, got.UTC())
		case tc.want != 0 && (!ok || got.Unix() != tc.want):
			t.Errorf("%s: got %v (%v), want %d", tc.name, got.Unix(), ok, tc.want)
		}
	}
}
