package accounts

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// spawnRecord captures what the status line would have started.
type spawnRecord struct {
	self  string
	args  []string
	calls int
	err   error
}

func (f *fixture) captureSpawn() *spawnRecord {
	f.t.Helper()
	rec := &spawnRecord{}
	prev := spawner
	spawner = func(self string, args []string) error {
		rec.self, rec.args, rec.calls = self, args, rec.calls+1
		return rec.err
	}
	f.t.Cleanup(func() { spawner = prev })
	return rec
}

func (f *fixture) useDir(dir string) {
	f.t.Helper()
	f.t.Setenv("CLAUDE_CONFIG_DIR", dir)
}

func payload(rateLimits string) string {
	if rateLimits == "" {
		rateLimits = fmt.Sprintf(`{"five_hour":{"used_percentage":23.54,"resets_at":%d},`+
			`"seven_day":{"used_percentage":41,"resets_at":%d}}`, fiveReset, weekReset)
	}
	return `{"model":{"id":"claude-opus-5","display_name":"Opus 5"},` +
		`"context_window":{"used_percentage":8.4},"rate_limits":` + rateLimits + `}`
}

// statuslineFixture wires a registered, logged-in, shared config dir and a
// spawn probe.
func statuslineFixture(t *testing.T) (*fixture, *spawnRecord, string) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.useDir(cd.Dir)
	return f, f.captureSpawn(), cd.Dir
}

func TestStatuslineHappyPath(t *testing.T) {
	f, spawn, _ := statuslineFixture(t)

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q", got.stderr)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q, want empty", f.errorLog())
	}
	if spawn.calls != 1 {
		t.Fatalf("spawn calls = %d", spawn.calls)
	}
	want := []string{
		"send-usage",
		"--email", email1,
		"--session-used", "23.5",
		"--session-resets", "1789491600",
		"--week-used", "41",
		"--week-resets", "1793631600",
		"--collected-at", "1789482657",
	}
	if strings.Join(spawn.args, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v\nwant %v", spawn.args, want)
	}
	if spawn.self == "" {
		t.Errorf("spawn path is empty")
	}
}

// An absent window is normal (no error row), but one that never shows up
// must be diagnosable: a USAGE_PENDING line, throttled to one an hour, that
// carries the payload's shape and nothing else.
func TestStatuslinePendingWindows(t *testing.T) {
	cases := map[string]struct{ in, signature string }{
		"no rate_limits":   {`{"model":{"display_name":"Opus 5"}}`, "rate_limits=absent"},
		"null rate_limits": {`{"model":{"display_name":"Opus 5"},"rate_limits":null}`, "rate_limits=null"},
		"missing seven_day": {payload(fmt.Sprintf(`{"five_hour":{"used_percentage":23.54,"resets_at":%d}}`, fiveReset)),
			"five_hour={used_percentage:number,resets_at:number} seven_day=absent"},
		"null five_hour": {payload(fmt.Sprintf(`{"five_hour":null,"seven_day":{"used_percentage":41,"resets_at":%d,"extra":"x"}}`, weekReset)),
			"five_hour=null seven_day={used_percentage:number,resets_at:number,extra:string}"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := statuslineFixture(t)
			for i := 0; i < 3; i++ {
				got := f.run(tc.in, "statusline")
				assertCode(t, got, 0)
				if !strings.HasSuffix(got.stdout, "· usage pending\n") || strings.Contains(got.stdout, "julienning:") {
					t.Errorf("render %d: stdout = %q, want the pending line and no error row", i, got.stdout)
				}
			}
			log := f.errorLog()
			if n := strings.Count(log, " USAGE_PENDING config_dir="+dir+" "); n != 1 {
				t.Errorf("USAGE_PENDING logged %d times, want 1 (throttled): %q", n, log)
			}
			if !strings.Contains(log, tc.signature) {
				t.Errorf("errors.log = %q, want signature %q", log, tc.signature)
			}
			// Shapes only: no usage numbers, no resets, no account. The
			// temp dir in config_dir has digits of its own, so only the
			// message after it is checked.
			_, msg, _ := strings.Cut(log, "config_dir="+dir+" ")
			for _, leak := range []string{"23", "41", "1789", email1, "sixtynine.agency"} {
				if strings.Contains(msg, leak) {
					t.Errorf("errors.log carries %q: %q", leak, log)
				}
			}
			if spawn.calls != 0 {
				t.Errorf("must not report incomplete usage")
			}
		})
	}
}

// Claude Code reports more than 100 once a limit is exceeded; the status
// line reports it as 100 instead of calling the payload malformed.
func TestStatuslineReportsUsageAbove100As100(t *testing.T) {
	f, spawn, _ := statuslineFixture(t)
	rl := fmt.Sprintf(`{"five_hour":{"used_percentage":112.5,"resets_at":%d},"seven_day":{"used_percentage":64,"resets_at":%d}}`, fiveReset, weekReset)
	got := f.run(payload(rl), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if spawn.calls != 1 || !strings.Contains(strings.Join(spawn.args, " "), "--session-used 100 --session-resets") {
		t.Errorf("argv = %v, want --session-used 100", spawn.args)
	}
}

func TestStatuslineUsageInvalid(t *testing.T) {
	cases := map[string]string{
		"percentage is a string":  fmt.Sprintf(`{"five_hour":{"used_percentage":"abc","resets_at":%d},"seven_day":{"used_percentage":1,"resets_at":%d}}`, fiveReset, weekReset),
		"percentage negative":     fmt.Sprintf(`{"five_hour":{"used_percentage":-1,"resets_at":%d},"seven_day":{"used_percentage":1,"resets_at":%d}}`, fiveReset, weekReset),
		"reset is a string":       fmt.Sprintf(`{"five_hour":{"used_percentage":1,"resets_at":%d},"seven_day":{"used_percentage":1,"resets_at":"soon"}}`, fiveReset),
		"reset is fractional":     fmt.Sprintf(`{"five_hour":{"used_percentage":1,"resets_at":%d},"seven_day":{"used_percentage":1,"resets_at":1.5}}`, fiveReset),
		"reset is zero":           fmt.Sprintf(`{"five_hour":{"used_percentage":1,"resets_at":%d},"seven_day":{"used_percentage":1,"resets_at":0}}`, fiveReset),
		"rate_limits is a string": `"nope"`,
	}
	for name, rl := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := statuslineFixture(t)
			got := f.run(payload(rl), "statusline")
			assertCode(t, got, 0)
			if !strings.Contains(got.stdout, "julienning: rate_limits in status line input are malformed") {
				t.Errorf("stdout = %q", got.stdout)
			}
			if !strings.HasPrefix(got.stdout, "Opus 5 · ctx 8%\n") {
				t.Errorf("the status line must still be printed: %q", got.stdout)
			}
			log := f.errorLog()
			if !strings.Contains(log, "USAGE_INVALID config_dir="+dir) {
				t.Errorf("errors.log = %q", log)
			}
			// The type signature is what makes an unknown shape debuggable.
			if !strings.Contains(log, "five_hour=") && !strings.Contains(log, "rate_limits=") {
				t.Errorf("no type signature in %q", log)
			}
			if spawn.calls != 0 {
				t.Errorf("must not report invalid usage")
			}
		})
	}
}

func TestStatuslineInvalidStdin(t *testing.T) {
	for _, in := range []string{"not json", "", "[1,2]"} {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			f, spawn, _ := statuslineFixture(t)
			got := f.run(in, "statusline")
			assertCode(t, got, 0)
			if got.stdout != "julienning: status line input is not valid JSON\n" {
				t.Errorf("stdout = %q", got.stdout)
			}
			if !strings.Contains(f.errorLog(), "2026-09-15T17:30:57+03:00 INVALID_STDIN") {
				t.Errorf("errors.log = %q", f.errorLog())
			}
			if spawn.calls != 0 {
				t.Errorf("spawned on broken input")
			}
		})
	}
}

// A registered dir waiting for `julienning login` is a steady state: the status
// line stays clean and the log gets one throttled line, not one per render.
func TestStatuslineAccountFileProblems(t *testing.T) {
	cases := map[string]struct {
		prepare func(f *fixture, dir string)
		code    string
	}{
		"missing": {func(f *fixture, dir string) {
			if err := os.Remove(filepath.Join(dir, ".claude.json")); err != nil {
				f.t.Fatal(err)
			}
		}, "ACCOUNT_FILE_UNREADABLE"},
		"unparseable": {func(f *fixture, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{broken"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}, "ACCOUNT_FILE_UNREADABLE"},
		"empty email":      {func(f *fixture, dir string) { f.writeAccount(dir, "") }, "EMAIL_INVALID"},
		"no at sign":       {func(f *fixture, dir string) { f.writeAccount(dir, "no-at-sign") }, "EMAIL_INVALID"},
		"path traversal":   {func(f *fixture, dir string) { f.writeAccount(dir, "../../etc/x@evil.com") }, "EMAIL_INVALID"},
		"separator inside": {func(f *fixture, dir string) { f.writeAccount(dir, "a/b@c.com") }, "EMAIL_INVALID"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := statuslineFixture(t)
			tc.prepare(f, dir)

			got := f.run(payload(""), "statusline")
			assertCode(t, got, 0)
			if got.stdout != "Opus 5 · ctx 8%\n" {
				t.Errorf("stdout = %q, want the status line only", got.stdout)
			}
			if !strings.Contains(f.errorLog(), tc.code+" config_dir="+dir) {
				t.Errorf("errors.log = %q, want %s", f.errorLog(), tc.code)
			}
			// Throttled like NOT_SHARED: this runs on every render.
			f.run(payload(""), "statusline")
			f.run(payload(""), "statusline")
			if n := strings.Count(f.errorLog(), tc.code); n != 1 {
				t.Errorf("%s logged %d times, want 1", tc.code, n)
			}
			if spawn.calls != 0 {
				t.Errorf("spawned without a valid account")
			}
		})
	}
}

// The registration gate runs before the account file is read, so a personal or
// brand new dir never produces an error row or an account-file log line.
func TestStatuslineUnregisteredDirWithoutAccountFile(t *testing.T) {
	f := setup(t)
	spawn := f.captureSpawn()
	f.addConfig("sixtynine1", email1)
	f.save()
	fresh := filepath.Join(f.home, ".claude-personal")
	if err := os.MkdirAll(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	f.useDir(fresh)

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q, want one status row", got.stdout)
	}
	log := f.errorLog()
	if strings.Contains(log, "ACCOUNT_FILE_UNREADABLE") || strings.Contains(log, "EMAIL_INVALID") {
		t.Errorf("an unregistered dir must not be inspected: %q", log)
	}
	if n := strings.Count(log, "NOT_SHARED"); n != 1 {
		t.Errorf("NOT_SHARED logged %d times, want 1: %q", n, log)
	}
	// Still one line only after more renders.
	f.run(payload(""), "statusline")
	f.run(payload(""), "statusline")
	if n := strings.Count(f.errorLog(), "NOT_SHARED"); n != 1 {
		t.Errorf("NOT_SHARED logged %d times, want 1", n)
	}
	if spawn.calls != 0 {
		t.Errorf("spawned for an unregistered dir")
	}
}

// errors.log is shared in bug reports: it must never carry an address.
func TestStatuslineNeverLogsTheEmail(t *testing.T) {
	f, _, dir := statuslineFixture(t)
	f.writeAccount(dir, "secret@example.com")
	// Both a malformed payload and a rejected address must stay anonymous.
	f.run(payload(`{"five_hour":"x","seven_day":"y"}`), "statusline")
	f.writeAccount(dir, "secret-not-an-email")
	f.run(payload(""), "statusline")
	if strings.Contains(f.errorLog(), "secret@") {
		t.Errorf("email leaked to errors.log: %q", f.errorLog())
	}
}

func TestStatuslineNotSetUp(t *testing.T) {
	f := setup(t)
	spawn := f.captureSpawn()
	dir := filepath.Join(f.home, ".claude-sixtynine1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeAccount(dir, email1)
	f.useDir(dir)

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q, want the status line only", got.stdout)
	}
	if !strings.Contains(f.errorLog(), "NOT_SETUP") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if spawn.calls != 0 {
		t.Errorf("spawned without a config")
	}
	// Throttled: the status line runs on every render.
	f.run(payload(""), "statusline")
	f.run(payload(""), "statusline")
	if n := strings.Count(f.errorLog(), "NOT_SETUP"); n != 1 {
		t.Errorf("NOT_SETUP logged %d times, want 1", n)
	}
}

func TestStatuslineNotShared(t *testing.T) {
	f := setup(t)
	spawn := f.captureSpawn()
	f.addConfig("sixtynine1", email1)
	f.save()
	personal := filepath.Join(f.home, ".claude")
	if err := os.MkdirAll(personal, 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeAccount(personal, "personal@example.com")
	f.useDir(personal)

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(f.errorLog(), "NOT_SHARED config_dir="+personal) {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if spawn.calls != 0 {
		t.Errorf("a personal dir must never be reported")
	}
}

// With CLAUDE_CONFIG_DIR unset Claude uses the default dir; while that dir is
// not registered, its login ($HOME/.claude.json) is never reported.
func TestStatuslineFallsBackToHome(t *testing.T) {
	f := setup(t)
	spawn := f.captureSpawn()
	f.addConfig("sixtynine1", email1)
	f.save()
	f.writeAccount(f.home, "home@example.com")
	f.useDir("")

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(f.errorLog(), "NOT_SHARED config_dir=<unset>") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if spawn.calls != 0 {
		t.Errorf("spawn calls = %d", spawn.calls)
	}
}

func TestStatuslineSpawnFailure(t *testing.T) {
	f, spawn, _ := statuslineFixture(t)
	spawn.err = errors.New("fork/exec: no such file")

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "julienning: cannot start the usage reporter") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(f.errorLog(), "SPAWN_FAILED") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

func TestStatuslineInvalidFrozenClock(t *testing.T) {
	f, _, _ := statuslineFixture(t)
	t.Setenv("JULIENNING_NOW_EPOCH", "abc")

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "julienning: JULIENNING_NOW_EPOCH must be integer epoch seconds") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if !strings.Contains(f.errorLog(), "CLOCK_INVALID") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

func TestStatuslineUnwritableLog(t *testing.T) {
	f := setup(t)
	f.captureSpawn()
	ro := filepath.Join(f.home, "readonly")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JULIENNING_HOME", filepath.Join(ro, "julienning"))

	got := f.run("not json", "statusline")
	assertCode(t, got, 0)
	if !strings.Contains(got.stdout, "julienning: LOG_FAILED") {
		t.Errorf("stdout = %q", got.stdout)
	}
}

// A registered dir reports only while its current login is shared: logins
// move between dirs, so registration alone proves nothing.
func TestStatuslineRegisteredButNotShared(t *testing.T) {
	f, spawn, dir := statuslineFixture(t)
	f.writeAccount(dir, "me@personal.com")

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if spawn.calls != 0 {
		t.Errorf("an unshared login was reported")
	}
	log := f.errorLog()
	if !strings.Contains(log, "NOT_SHARED config_dir="+dir+" account is not on the team allowlist") {
		t.Errorf("errors.log = %q", log)
	}
	if strings.Contains(log, "personal.com") {
		t.Errorf("email leaked: %q", log)
	}
	f.run(payload(""), "statusline")
	if n := strings.Count(f.errorLog(), "NOT_SHARED"); n != 1 {
		t.Errorf("NOT_SHARED logged %d times, want 1", n)
	}
}

// No readable allowlist snapshot means nothing is shared.
func TestStatuslineSharedCacheMissingOrBroken(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f, spawn, _ := statuslineFixture(t)
		if err := os.Remove(filepath.Join(f.jul, "shared.json")); err != nil {
			t.Fatal(err)
		}
		got := f.run(payload(""), "statusline")
		assertCode(t, got, 0)
		if got.stdout != "Opus 5 · ctx 8%\n" || spawn.calls != 0 {
			t.Errorf("stdout = %q, spawns = %d", got.stdout, spawn.calls)
		}
		if !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("errors.log = %q", f.errorLog())
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		f, spawn, _ := statuslineFixture(t)
		if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{nope"), 0o600); err != nil {
			t.Fatal(err)
		}
		got := f.run(payload(""), "statusline")
		assertCode(t, got, 0)
		if got.stdout != "Opus 5 · ctx 8%\n" || spawn.calls != 0 {
			t.Errorf("stdout = %q, spawns = %d", got.stdout, spawn.calls)
		}
		if !strings.Contains(f.errorLog(), "NOT_SHARED") || !strings.Contains(f.errorLog(), "unreadable") {
			t.Errorf("errors.log = %q", f.errorLog())
		}
	})
}

// A registered default dir (CLAUDE_CONFIG_DIR unset) reads ~/.claude.json.
func TestStatuslineDefaultDir(t *testing.T) {
	f := setup(t)
	spawn := f.captureSpawn()
	f.addConfig("default", email1)
	f.save()
	f.share(email1)
	f.useDir("")

	got := f.run(payload(""), "statusline")
	assertCode(t, got, 0)
	if got.stdout != "Opus 5 · ctx 8%\n" {
		t.Errorf("stdout = %q", got.stdout)
	}
	if spawn.calls != 1 || spawn.args[2] != email1 {
		t.Fatalf("spawn = %+v", spawn)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

// An explicitly exported CLAUDE_CONFIG_DIR=~/.claude makes Claude read
// ~/.claude/.claude.json: the status line must report that login, not the
// one in ~/.claude.json, while the registration lookup still matches.
func TestStatuslineExplicitDefaultDirReadsItsOwnAccountFile(t *testing.T) {
	prepare := func(t *testing.T) (*fixture, *spawnRecord, string) {
		f := setup(t)
		spawn := f.captureSpawn()
		cd := f.addConfig("default", email1) // ~/.claude.json
		f.save()
		f.share(email1, email2)
		f.useDir(cd.Dir)
		return f, spawn, cd.Dir
	}
	t.Run("logged in there", func(t *testing.T) {
		f, spawn, dir := prepare(t)
		body := `{"oauthAccount":{"emailAddress":"` + email2 + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		assertCode(t, f.run(payload(""), "statusline"), 0)
		if spawn.calls != 1 || spawn.args[2] != email2 {
			t.Errorf("spawn = %+v, want a report for %s", spawn, email2)
		}
	})
	t.Run("logged out there", func(t *testing.T) {
		f, spawn, dir := prepare(t)
		got := f.run(payload(""), "statusline")
		assertCode(t, got, 0)
		if got.stdout != "Opus 5 · ctx 8%\n" {
			t.Errorf("stdout = %q", got.stdout)
		}
		if spawn.calls != 0 {
			t.Errorf("reported the login of ~/.claude.json: %v", spawn.args)
		}
		if want := "ACCOUNT_FILE_UNREADABLE config_dir=" + dir + " cannot read " + filepath.Join(dir, ".claude.json"); !strings.Contains(f.errorLog(), want) {
			t.Errorf("errors.log = %q, want %q", f.errorLog(), want)
		}
	})
}

// Every status line code is throttled: the status line renders several times
// a second, and a broken input or spawn must not flood errors.log. The error
// row itself still shows on every render.
func TestStatuslineErrorCodesAreThrottled(t *testing.T) {
	cases := map[string]struct {
		stdin   string
		prepare func(t *testing.T, f *fixture, spawn *spawnRecord)
		row     string
	}{
		"INVALID_STDIN": {"not json", nil, "julienning: status line input is not valid JSON"},
		"USAGE_INVALID": {payload(`"nope"`), nil, "julienning: rate_limits in status line input are malformed"},
		"CLOCK_INVALID": {payload(""), func(t *testing.T, f *fixture, _ *spawnRecord) {
			t.Setenv("JULIENNING_NOW_EPOCH", "abc")
		}, "julienning: JULIENNING_NOW_EPOCH must be integer epoch seconds"},
		"SPAWN_FAILED": {payload(""), func(t *testing.T, f *fixture, spawn *spawnRecord) {
			spawn.err = errors.New("fork/exec: no such file")
		}, "julienning: cannot start the usage reporter"},
	}
	for code, tc := range cases {
		t.Run(code, func(t *testing.T) {
			f, spawn, _ := statuslineFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, f, spawn)
			}
			for i := 0; i < 3; i++ {
				got := f.run(tc.stdin, "statusline")
				assertCode(t, got, 0)
				if !strings.Contains(got.stdout, tc.row) {
					t.Errorf("render %d: stdout = %q, want the error row", i, got.stdout)
				}
				if strings.Contains(got.stdout, "LOG_FAILED") {
					t.Errorf("render %d: a throttled line is not a log failure: %q", i, got.stdout)
				}
			}
			// Match the code field: the temp dir in config_dir carries the test name.
			if n := strings.Count(f.errorLog(), " "+code+" config_dir="); n != 1 {
				t.Errorf("%s logged %d times, want 1: %q", code, n, f.errorLog())
			}
		})
	}
}

// Claude may hand over a mixed-case address; the reporter gets it lowercased.
func TestStatuslineLowercasesTheEmail(t *testing.T) {
	f, spawn, dir := statuslineFixture(t)
	f.writeAccount(dir, "Claude1@SixtyNine.agency")
	assertCode(t, f.run(payload(""), "statusline"), 0)
	if spawn.calls != 1 || spawn.args[2] != email1 {
		t.Errorf("spawn = %+v", spawn)
	}
}

// The status line must never touch the network, whatever state it is in.
func TestStatuslineNeverCallsTheWorker(t *testing.T) {
	f, _, _ := statuslineFixture(t)
	f.run(payload(""), "statusline")
	f.run("not json", "statusline")
	if len(f.fake.Calls) != 0 {
		t.Errorf("calls = %+v", f.fake.Calls)
	}
}

// shared.json written by a nickname-aware listing gates exactly like a plain
// one: the status line, the hook and the reporter key on the email, and the
// nickname never stands in for it.
func TestGatesReadANicknamedCache(t *testing.T) {
	f, spawn, _ := statuslineFixture(t)
	f.shareNicks(map[string]string{email1: "alpha", email2: ""})

	assertCode(t, f.run(payload(""), "statusline"), 0)
	if spawn.calls != 1 || len(spawn.args) < 3 || spawn.args[0] != "send-usage" || spawn.args[2] != email1 {
		t.Fatalf("statusline spawn = %d %v", spawn.calls, spawn.args)
	}
	assertCode(t, f.run(startInput, "hook", "session-start"), 0)
	if spawn.calls != 2 || spawn.args[0] != "claim-sync" {
		t.Fatalf("hook spawn = %d %v", spawn.calls, spawn.args)
	}
	assertCode(t, f.run("", sendArgs()...), 0)
	if u := f.fake.CallsFor("usage"); len(u) != 1 || u[0].Email != email1 {
		t.Errorf("usage calls = %+v", u)
	}
	if strings.Contains(f.errorLog(), "NOT_SHARED") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
	if c := f.sharedCache(); c.Nickname(email1) != "alpha" || !c.Contains(email2) {
		t.Errorf("cache changed: %+v", c)
	}
}
