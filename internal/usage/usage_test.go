package usage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// Epochs shared with the bash predecessor's test suite.
const (
	nowEpoch   = 1789482657 // 2026-09-15T14:30:57Z
	fiveReset  = 1789491600 // 2026-09-15T17:00:00Z
	weekReset  = 1793631600 // 2026-11-02T15:00:00Z, after US DST ends
	fullSample = `{"model":{"id":"claude-opus-5","display_name":"Opus 5"},"context_window":{"used_percentage":8.4},` +
		`"rate_limits":{"five_hour":{"used_percentage":23.54,"resets_at":1789491600},` +
		`"seven_day":{"used_percentage":41,"resets_at":1793631600}}}`
)

func parse(t *testing.T, s string) Input {
	t.Helper()
	in, err := Parse(strings.NewReader(s))
	if err != nil {
		t.Fatalf("Parse(%s): %v", s, err)
	}
	return in
}

func TestParseCompletePayload(t *testing.T) {
	in := parse(t, fullSample)
	if got := StatusLine(in); got != "Opus 5 · ctx 8%" {
		t.Errorf("status line = %q", got)
	}
	if !in.Complete() || in.Invalid() {
		t.Fatalf("windows = %+v %+v", in.Session, in.Week)
	}
	if in.Session.Used != 23.5 || in.Session.ResetsAt != fiveReset {
		t.Errorf("session = %+v", in.Session)
	}
	if in.Week.Used != 41 || in.Week.ResetsAt != weekReset {
		t.Errorf("week = %+v", in.Week)
	}
	if got := FormatPercent(in.Session.Used); got != "23.5" {
		t.Errorf("session pct = %q", got)
	}
	if got := FormatPercent(in.Week.Used); got != "41" {
		t.Errorf("week pct = %q", got)
	}
}

func TestParseInvalidStdin(t *testing.T) {
	for _, bad := range []string{"not json", "", "[1,2]", `"str"`, "null", "17"} {
		if _, err := Parse(strings.NewReader(bad)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("Parse(%q) err = %v, want ErrInvalidInput", bad, err)
		}
	}
}

func TestParseAbsentWindows(t *testing.T) {
	cases := map[string]string{
		"no rate_limits":    `{"model":{"display_name":"Opus 5"}}`,
		"null rate_limits":  `{"model":{"display_name":"Opus 5"},"rate_limits":null}`,
		"missing seven_day": `{"model":{"display_name":"Opus 5"},"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1789491600}}}`,
		"null five_hour":    `{"model":{"display_name":"Opus 5"},"rate_limits":{"five_hour":null,"seven_day":null}}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			in := parse(t, payload)
			if in.Invalid() {
				t.Errorf("absent window must not be invalid: %+v", in)
			}
			if in.Complete() {
				t.Errorf("want incomplete: %+v", in)
			}
			if got := StatusLine(in) + " · " + Pending; got != "Opus 5 · usage pending" {
				t.Errorf("line = %q", got)
			}
		})
	}
}

func TestParseInvalidWindows(t *testing.T) {
	for _, bad := range []string{`"abc"`, "-1", "-0.01", "1e400", "null", "{}", "true"} {
		payload := `{"rate_limits":{"five_hour":{"used_percentage":` + bad + `,"resets_at":1789491600},` +
			`"seven_day":{"used_percentage":1,"resets_at":1793631600}}}`
		in := parse(t, payload)
		if !in.Invalid() {
			t.Errorf("used_percentage %s: want invalid, got %+v", bad, in.Session)
		}
	}
	for _, bad := range []string{`"soon"`, "1.5", "0", "-5", "null"} {
		payload := `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1789491600},` +
			`"seven_day":{"used_percentage":1,"resets_at":` + bad + `}}}`
		in := parse(t, payload)
		if !in.Invalid() {
			t.Errorf("resets_at %s: want invalid, got %+v", bad, in.Week)
		}
	}
	for _, bad := range []string{`"nope"`, "42", "[]", "true"} {
		in := parse(t, `{"rate_limits":`+bad+`}`)
		if !in.Invalid() {
			t.Errorf("rate_limits %s: want invalid", bad)
		}
	}
	in := parse(t, `{"rate_limits":{"five_hour":"x","seven_day":{"used_percentage":1,"resets_at":1}}}`)
	if !in.Invalid() {
		t.Errorf("non-object window: want invalid")
	}
}

// Claude Code reports more than 100 once a limit is exceeded: that is the
// report that marks the account exhausted, so it is clamped, never dropped.
// The signature still shows the shape only.
func TestParseClampsUsageAbove100(t *testing.T) {
	cases := map[string]float64{"100": 100, "100.04": 100, "100.5": 100, "150": 100, "1e6": 100, "99.96": 100, "99.94": 99.9}
	for raw, want := range cases {
		payload := `{"rate_limits":{"five_hour":{"used_percentage":` + raw + `,"resets_at":1789491600},` +
			`"seven_day":{"used_percentage":` + raw + `,"resets_at":1793631600}}}`
		in := parse(t, payload)
		if !in.Complete() || in.Invalid() {
			t.Fatalf("%s: windows = %+v %+v, want OK", raw, in.Session, in.Week)
		}
		if in.Session.Used != want || in.Week.Used != want {
			t.Errorf("%s: used = %v / %v, want %v", raw, in.Session.Used, in.Week.Used, want)
		}
		if got := FormatPercent(in.Session.Used); want == 100 && got != "100" {
			t.Errorf("%s: formatted = %q, want 100", raw, got)
		}
		if sig := in.RateLimitsSignature(); strings.ContainsAny(sig, "0123456789") {
			t.Errorf("%s: signature carries a number: %q", raw, sig)
		}
	}
}

// float64(math.MaxInt64) is exactly 2^63, so 9223372036854775808 used to pass
// validation and then overflow int64 into a negative timestamp.
func TestParseRejectsOutOfRangeResets(t *testing.T) {
	bad := []string{
		"9223372036854775807",  // math.MaxInt64: not representable, rounds to 2^63
		"9223372036854775808",  // exactly 2^63
		"9223372036854775809",  // above 2^63
		"1099511627776",        // 2^40: the plausible-epoch bound, exclusive
		"1e19",                 // far past anything an epoch could mean
		"99999999999999999999", // more digits than float64 can hold exactly
	}
	for _, v := range bad {
		payload := `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":` + v + `},` +
			`"seven_day":{"used_percentage":1,"resets_at":1793631600}}}`
		in := parse(t, payload)
		if !in.Invalid() {
			t.Errorf("resets_at %s: want invalid, got %+v", v, in.Session)
		}
		if in.Session.ResetsAt < 0 {
			t.Errorf("resets_at %s overflowed to %d", v, in.Session.ResetsAt)
		}
	}
	// Just under the bound is still accepted.
	in := parse(t, `{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":1099511627775},`+
		`"seven_day":{"used_percentage":1,"resets_at":1793631600}}}`)
	if in.Invalid() || in.Session.ResetsAt != 1099511627775 {
		t.Errorf("2^40-1 must be valid, got %+v", in.Session)
	}
}

func TestRateLimitsSignature(t *testing.T) {
	cases := []struct{ payload, want string }{
		{`{"rate_limits":{"five_hour":{"used_percentage":1,"resets_at":null}}}`,
			"five_hour={used_percentage:number,resets_at:null} seven_day=absent"},
		{`{"rate_limits":{"five_hour":{"used_percentage":"12%","resets_at":1,"zeta":[]},"seven_day":null}}`,
			"five_hour={used_percentage:string,resets_at:number,zeta:array} seven_day=null"},
		{`{"rate_limits":"nope"}`, "rate_limits=string"},
		{`{"rate_limits":[1]}`, "rate_limits=array"},
		{`{}`, "rate_limits=absent"},
		{`{"rate_limits":null}`, "rate_limits=null"},
	}
	for _, tc := range cases {
		if got := parse(t, tc.payload).RateLimitsSignature(); got != tc.want {
			t.Errorf("signature(%s) = %q, want %q", tc.payload, got, tc.want)
		}
	}
}

func TestStatusLineFallbacks(t *testing.T) {
	cases := []struct{ payload, want string }{
		{`{"rate_limits":null}`, "unknown model"},
		{"{\"model\":{\"display_name\":\"Op\\u001b[31mus\"}}", "Op[31mus"},
		{`{"model":"nope","context_window":{"used_percentage":8.5}}`, "unknown model · ctx 9%"},
		{`{"model":{"display_name":42}}`, "unknown model"},
		{`{"model":{"display_name":"M"},"context_window":{"used_percentage":"80"}}`, "M"},
		{`{"model":{"display_name":"M"},"context_window":{"used_percentage":0}}`, "M · ctx 0%"},
		{`{"model":{"display_name":"M"},"context_window":{"used_percentage":99.5}}`, "M · ctx 100%"},
	}
	for _, tc := range cases {
		if got := StatusLine(parse(t, tc.payload)); got != tc.want {
			t.Errorf("StatusLine(%s) = %q, want %q", tc.payload, got, tc.want)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	cases := map[float64]string{23.54: "23.5", 41: "41", 0: "0", 99.96: "100", 12.25: "12.3", 100: "100"}
	for in, want := range cases {
		if got := FormatPercent(in); got != want {
			t.Errorf("FormatPercent(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatReset(t *testing.T) {
	ist, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(nowEpoch, 0)
	cases := []struct {
		name string
		t    time.Time
		now  time.Time
		loc  *time.Location
		want string
	}{
		{"today, Istanbul", time.Unix(fiveReset, 0), now, ist, "20:00"},
		{"today, UTC", time.Unix(fiveReset, 0), now, time.UTC, "17:00"},
		{"today, New York", time.Unix(fiveReset, 0), now, ny, "13:00"},
		{"far future", time.Unix(weekReset, 0), now, ist, "2026-11-02 18:00"},
		{"within six days", time.Unix(nowEpoch+3*86400, 0), now, ist, "Fri 17:30"},
		{"sixth day", time.Unix(nowEpoch+6*86400, 0), now, ist, "Mon 17:30"},
		{"seventh day", time.Unix(nowEpoch+7*86400, 0), now, ist, "2026-09-22 17:30"},
		{"past", time.Unix(nowEpoch-1, 0), now, ist, "reset"},
		{"exactly now", now, now, ist, "reset"},
		{"zero", time.Time{}, now, ist, "-"},
		// DST: New York falls back at 2026-11-01T06:00Z. Both instants are the
		// same civil day and the same wall clock, an hour apart in real time.
		{"before fall back", time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), ny, "01:30"},
		{"after fall back", time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), ny, "01:30"},
		// Across the transition the calendar-day distance stays 1, not 1.04.
		{"next day across DST", time.Unix(weekReset, 0), time.Date(2026, 11, 1, 5, 0, 0, 0, time.UTC), ny, "Mon 10:00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FormatReset(tc.t, tc.now, tc.loc); got != tc.want {
				t.Errorf("FormatReset = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgo(t *testing.T) {
	cases := map[time.Duration]string{
		0:                 "0s ago",
		-5 * time.Minute:  "0s ago",
		45 * time.Second:  "45s ago",
		90 * time.Second:  "1m ago",
		12 * time.Minute:  "12m ago",
		119 * time.Minute: "1h ago",
		25 * time.Hour:    "1d ago",
		72 * time.Hour:    "3d ago",
	}
	for d, want := range cases {
		if got := Ago(d); got != want {
			t.Errorf("Ago(%v) = %q, want %q", d, got, want)
		}
	}
	if got := ShortDuration(12 * time.Minute); got != "12m" {
		t.Errorf("ShortDuration = %q", got)
	}
}

func TestAccountEmail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")

	if _, err := AccountEmail(p); codeOf(t, err) != CodeAccountFile {
		t.Errorf("missing file: %v", err)
	}
	write(t, p, "{broken")
	if _, err := AccountEmail(p); codeOf(t, err) != CodeAccountFile {
		t.Errorf("unparseable: %v", err)
	}
	for _, bad := range []string{"", "no-at-sign", "../../etc/x@evil.com", "a/b@c.com", strings.Repeat("a", 250) + "@b.com"} {
		write(t, p, `{"oauthAccount":{"emailAddress":"`+bad+`"}}`)
		if _, err := AccountEmail(p); codeOf(t, err) != CodeEmailInvalid {
			t.Errorf("email %q: err = %v", bad, err)
		}
	}
	write(t, p, `{"oauthAccount":{"emailAddress":42}}`)
	if _, err := AccountEmail(p); codeOf(t, err) != CodeEmailInvalid {
		t.Errorf("non-string email: %v", err)
	}
	write(t, p, `{"oauthAccount":{"emailAddress":"claude1@sixtynine.agency"}}`)
	got, err := AccountEmail(p)
	if err != nil || got != "claude1@sixtynine.agency" {
		t.Errorf("AccountEmail = %q, %v", got, err)
	}
}

// LoginEmail is the path-based twin of claudecfg.ReadEmail used by hooks.
func TestLoginEmail(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude.json")

	if _, err := LoginEmail(p); !errors.Is(err, claudecfg.ErrNotLoggedIn) {
		t.Errorf("missing file: %v, want ErrNotLoggedIn", err)
	}
	for _, empty := range []string{`{}`, `{"oauthAccount":{"emailAddress":""}}`, `{"oauthAccount":{"emailAddress":42}}`} {
		write(t, p, empty)
		if _, err := LoginEmail(p); !errors.Is(err, claudecfg.ErrNotLoggedIn) {
			t.Errorf("%s: %v, want ErrNotLoggedIn", empty, err)
		}
	}
	write(t, p, "{broken")
	if _, err := LoginEmail(p); err == nil || errors.Is(err, claudecfg.ErrNotLoggedIn) || !strings.Contains(err.Error(), p) {
		t.Errorf("unparseable: %v", err)
	}
	write(t, p, `{"oauthAccount":{"emailAddress":"secret/x@evil.com"}}`)
	if _, err := LoginEmail(p); err == nil || errors.Is(err, claudecfg.ErrNotLoggedIn) || strings.Contains(err.Error(), "secret") {
		t.Errorf("invalid email: %v", err)
	}
	write(t, p, `{"oauthAccount":{"emailAddress":"Claude1@SixtyNine.agency"}}`)
	if got, err := LoginEmail(p); err != nil || got != "claude1@sixtynine.agency" {
		t.Errorf("LoginEmail = %q, %v", got, err)
	}
}

func TestLoggerLineFormat(t *testing.T) {
	now := time.Unix(nowEpoch, 0)
	ist, _ := time.LoadLocation("Europe/Istanbul")
	l := Logger{Dir: t.TempDir(), ConfigDir: "/cfg", Now: now, Loc: time.UTC}
	if got, want := l.Line(CodeInvalidStdin, "boom"), "2026-09-15T14:30:57+00:00 INVALID_STDIN config_dir=/cfg boom"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	l.Loc, l.ConfigDir = ist, ""
	if got, want := l.Line(CodeNotShared, "x"), "2026-09-15T17:30:57+03:00 NOT_SHARED config_dir=<unset> x"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

func TestLoggerAppendsAndTrims(t *testing.T) {
	dir := t.TempDir()
	l := Logger{Dir: dir, ConfigDir: "/cfg", Now: time.Unix(nowEpoch, 0), Loc: time.UTC}
	if err := l.Log(CodeInvalidStdin, "first"); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := l.Log(CodeUsageInvalid, "second"); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if got := strings.Count(read(t, l.Path()), "\n"); got != 2 {
		t.Errorf("lines = %d", got)
	}

	write(t, l.Path(), strings.Repeat("filler line padding padding padding padding padding\n", 30000))
	if err := l.Log(CodeInvalidStdin, "latest"); err != nil {
		t.Fatalf("Log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(read(t, l.Path()), "\n"), "\n")
	if len(lines) != 500 {
		t.Errorf("trimmed to %d lines, want 500", len(lines))
	}
	if !strings.Contains(lines[len(lines)-1], "INVALID_STDIN config_dir=/cfg latest") {
		t.Errorf("newest line lost: %q", lines[len(lines)-1])
	}
}

func TestLoggerUnwritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	l := Logger{Dir: filepath.Join(dir, "home"), Now: time.Unix(nowEpoch, 0), Loc: time.UTC}
	if err := l.Log(CodeInvalidStdin, "x"); err == nil {
		t.Fatal("want an error for an unwritable dir")
	}
}

func TestLoggerThrottle(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(nowEpoch, 0)
	l := Logger{Dir: dir, Now: now, Loc: time.UTC}
	for i := 0; i < 3; i++ {
		if err := l.LogThrottled(CodeNotShared, "not registered"); err != nil {
			t.Fatalf("LogThrottled: %v", err)
		}
	}
	if got := strings.Count(read(t, l.Path()), "NOT_SHARED"); got != 1 {
		t.Errorf("throttled writes = %d, want 1", got)
	}
	// A different code is throttled independently.
	if err := l.LogThrottled(CodeNotSetup, "no config"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(read(t, l.Path()), "NOT_SETUP"); got != 1 {
		t.Errorf("second code = %d, want 1", got)
	}
	// An hour later the same code is logged again.
	l.Now = now.Add(time.Hour + time.Second)
	if err := l.LogThrottled(CodeNotShared, "not registered"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(read(t, l.Path()), "NOT_SHARED"); got != 2 {
		t.Errorf("after an hour = %d, want 2", got)
	}
}

func TestNow(t *testing.T) {
	got, err := Now(func(string) string { return "" })
	if err != nil || time.Since(got) > time.Minute {
		t.Errorf("wall clock = %v, %v", got, err)
	}
	got, err = Now(func(string) string { return "1789482657" })
	if err != nil || got.Unix() != nowEpoch {
		t.Errorf("frozen = %v, %v", got, err)
	}
	for _, bad := range []string{"abc", "-1", "12.5", " 12"} {
		if _, err := Now(func(string) string { return bad }); codeOf(t, err) != CodeClockInvalid {
			t.Errorf("Now(%q) err = %v", bad, err)
		}
	}
}

func TestSentCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	email := "Claude1@SixtyNine.agency"
	if got, _ := LoadSent(dir, email); got != nil {
		t.Fatalf("empty cache = %+v", got)
	}
	want := Sent{
		Payload:     Payload{SessionUsed: 23.5, SessionResets: fiveReset, WeekUsed: 41, WeekResets: weekReset},
		AttemptedAt: time.Unix(nowEpoch, 0).UTC(),
		SentAt:      time.Unix(nowEpoch, 0).UTC(),
	}
	if err := SaveSent(dir, email, want); err != nil {
		t.Fatalf("SaveSent: %v", err)
	}
	// Named by the throttle-marker hash of the lowercased address: no file
	// name under ~/.julienning may carry an email.
	if got, want := filepath.Base(SentPath(dir, email)), KeyHash("claude1@sixtynine.agency")+".json"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if SentPath(dir, email) != SentPath(dir, strings.ToLower(email)) {
		t.Errorf("path depends on the address's case")
	}
	got, err := LoadSent(dir, strings.ToLower(email))
	if err != nil || got == nil {
		t.Fatalf("LoadSent = %v, %v", got, err)
	}
	if got.Payload != want.Payload || !got.SentAt.Equal(want.SentAt) || !got.AttemptedAt.Equal(want.AttemptedAt) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	write(t, SentPath(dir, email), "{broken")
	if got, _ := LoadSent(dir, email); got != nil {
		t.Errorf("corrupt cache = %+v, want nil", got)
	}
	// A 0-byte file is what a torn non-atomic write used to leave behind.
	write(t, SentPath(dir, email), "")
	if got, err := LoadSent(dir, email); got != nil || err != nil {
		t.Errorf("empty cache = %+v, %v, want nil, nil", got, err)
	}
}

// SaveSent must be atomic: a concurrent LoadSent may never see a partial file,
// and no temporary file may survive.
func TestSaveSentIsAtomic(t *testing.T) {
	dir := t.TempDir()
	email := "claude1@sixtynine.agency"
	base := Sent{Payload: Payload{SessionUsed: 1, SessionResets: fiveReset, WeekUsed: 2, WeekResets: weekReset}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			s := base
			s.SessionUsed = float64(i % 100)
			s.AttemptedAt = time.Unix(nowEpoch+int64(i), 0).UTC()
			if err := SaveSent(dir, email, s); err != nil {
				t.Errorf("SaveSent: %v", err)
				return
			}
		}
	}()
	for {
		select {
		case <-done:
			entries, err := os.ReadDir(filepath.Dir(SentPath(dir, email)))
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Name() != filepath.Base(SentPath(dir, email)) {
					t.Errorf("leftover file %q", e.Name())
				}
			}
			return
		default:
			got, err := LoadSent(dir, email)
			if err != nil {
				t.Fatalf("LoadSent: %v", err)
			}
			if got != nil && got.SessionResets != fiveReset {
				t.Fatalf("torn read: %+v", got)
			}
		}
	}
}

// Files named after the address by older versions are never read and are
// swept on the next write, whichever account it is for.
func TestSaveSentRemovesLegacyEmailNamedFiles(t *testing.T) {
	dir := t.TempDir()
	sent := filepath.Join(dir, "sent")
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []string{"claude1@sixtynine.agency.json", "claude1@sixtynine.agency.inflight", "claude2@sixtynine.agency.json"}
	for _, name := range legacy {
		write(t, filepath.Join(sent, name), `{"session_resets":1,"sent_at":"2026-09-15T14:30:57Z"}`)
	}
	unrelated := filepath.Join(sent, "notes.txt")
	write(t, unrelated, "keep")

	if got, _ := LoadSent(dir, "claude1@sixtynine.agency"); got != nil {
		t.Errorf("legacy file was read: %+v", got)
	}
	if err := SaveSent(dir, "claude1@sixtynine.agency", Sent{}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(sent)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "@") {
			t.Errorf("legacy file %q survived a write", e.Name())
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file removed: %v", err)
	}
}

func TestAcquireInflight(t *testing.T) {
	dir := t.TempDir()
	email := "claude1@sixtynine.agency"
	now := time.Unix(nowEpoch, 0)

	release, ok, err := AcquireInflight(dir, email, now)
	if err != nil || !ok {
		t.Fatalf("first acquire = %v, %v", ok, err)
	}
	if _, err := os.Stat(InflightPath(dir, email)); err != nil {
		t.Fatalf("marker: %v", err)
	}
	// A fresh marker keeps a second process out entirely.
	if _, ok, err := AcquireInflight(dir, email, now.Add(InflightTTL-time.Second)); ok || err != nil {
		t.Errorf("concurrent acquire = %v, %v, want false, nil", ok, err)
	}
	release()
	if _, err := os.Stat(InflightPath(dir, email)); !os.IsNotExist(err) {
		t.Errorf("marker not removed: %v", err)
	}

	// A marker left behind by a killed process must not block forever.
	if _, ok, err := AcquireInflight(dir, email, now); !ok || err != nil {
		t.Fatalf("re-acquire = %v, %v", ok, err)
	}
	release2, ok, err := AcquireInflight(dir, email, now.Add(InflightTTL+time.Second))
	if !ok || err != nil {
		t.Fatalf("stale acquire = %v, %v", ok, err)
	}
	release2()

	// The marker sits next to the cache file it guards and, like it, carries
	// no address.
	if got, want := InflightPath(dir, email), strings.TrimSuffix(SentPath(dir, email), ".json")+".inflight"; got != want {
		t.Errorf("marker path = %q, want %q", got, want)
	}
	if strings.Contains(InflightPath(dir, email), "@") {
		t.Errorf("marker path carries the address: %q", InflightPath(dir, email))
	}
}

func TestShouldSend(t *testing.T) {
	now := time.Unix(nowEpoch, 0)
	base := Payload{SessionUsed: 20, SessionResets: fiveReset, WeekUsed: 30, WeekResets: weekReset}
	sent := func(ago time.Duration, p Payload) *Sent {
		at := now.Add(-ago)
		return &Sent{Payload: p, AttemptedAt: at, SentAt: at}
	}
	// An attempt that never reached the Worker: attempted_at set, sent_at zero.
	tried := func(ago time.Duration, p Payload) *Sent {
		return &Sent{Payload: p, AttemptedAt: now.Add(-ago)}
	}
	changed := base
	changed.SessionUsed = 25
	newWindow := base
	newWindow.SessionResets = fiveReset + 18000

	cases := []struct {
		name string
		prev *Sent
		cur  Payload
		want bool
	}{
		{"nothing sent yet", nil, base, true},
		{"identical, just sent", sent(time.Minute, base), base, false},
		{"identical, 29 minutes", sent(29*time.Minute, base), base, false},
		{"identical, 31 minutes", sent(31*time.Minute, base), base, true},
		{"changed, under min interval", sent(time.Minute, base), changed, false},
		{"changed, over min interval", sent(6*time.Minute, base), changed, true},
		{"new window beats every debounce", sent(time.Second, base), newWindow, true},
		{"future timestamp is treated as just sent", &Sent{Payload: base, AttemptedAt: now.Add(time.Hour), SentAt: now.Add(time.Hour)}, changed, false},
		// A failed attempt debounces on the attempt, not on the 30-minute
		// repeat window: the Worker never got this payload.
		{"failed attempt, under min interval", tried(time.Minute, base), base, false},
		{"failed attempt, over min interval", tried(6*time.Minute, base), base, true},
		{"failed attempt, new window", tried(time.Second, base), newWindow, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldSend(tc.prev, tc.cur, now, 5*time.Minute); got != tc.want {
				t.Errorf("ShouldSend = %v, want %v", got, tc.want)
			}
		})
	}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var ce *CodedError
	if !errors.As(err, &ce) {
		t.Fatalf("err %v is not a *CodedError", err)
	}
	return ce.Code
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
