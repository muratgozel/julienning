package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/remote"
	"github.com/muratgozel/julienning/internal/usage"
)

// Refusal texts as Claude shows them. The fixture's clock is Tue
// 2026-09-15 17:30:57 in Istanbul.
const (
	weeklyRefusal   = "You've hit your weekly limit · resets Sep 18 at 10am (Europe/Istanbul)" // friReset
	fiveHourRefusal = "You've hit your limit · resets 8pm (Europe/Istanbul)"                   // fiveReset
)

// stopFailureInput is a StopFailure payload; details goes to error_details
// and, with the "(error type …)" suffix Claude adds, to the last message.
func stopFailureInput(errType, details string) string {
	doc := map[string]any{
		"session_id": "5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11", "transcript_path": "/x.jsonl", "cwd": "/tmp/p",
		"hook_event_name": "StopFailure", "error": errType, "error_details": details,
		"last_assistant_message": details + " (error type " + errType + ", request req_0)",
	}
	raw, _ := json.Marshal(doc)
	return string(raw)
}

func TestHookStopFailureSpawnsSendExhausted(t *testing.T) {
	cases := map[string]struct{ details, want string }{
		"weekly": {weeklyRefusal,
			fmt.Sprintf("send-exhausted --email %s --window week --resets %d", email1, friReset)},
		"five-hour": {fiveHourRefusal,
			fmt.Sprintf("send-exhausted --email %s --window session --resets %d", email1, fiveReset)},
		"nothing to go on": {"API Error: Rate limited",
			"send-exhausted --email " + email1 + " --window session"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, _ := hookFixture(t)
			start := time.Now()
			assertSilent(t, f, f.run(stopFailureInput("rate_limit", tc.details), "hook", "stop-failure"))
			if d := time.Since(start); d > time.Second {
				t.Errorf("hook took %v", d)
			}
			if spawn.calls != 1 || strings.Join(spawn.args, " ") != tc.want {
				t.Errorf("argv = %v, want %s", spawn.args, tc.want)
			}
			if f.errorLog() != "" {
				t.Errorf("errors.log = %q", f.errorLog())
			}
		})
	}
}

// Without a window or reset in the text, the last report this machine sent
// for the account decides.
func TestHookStopFailureFallsBackToTheSentCache(t *testing.T) {
	cases := map[string]struct {
		details string
		sent    usage.Payload
		want    string
	}{
		"week is fuller": {"API Error: Rate limited",
			usage.Payload{SessionUsed: 60, SessionResets: fiveReset, WeekUsed: 100, WeekResets: weekReset},
			fmt.Sprintf("--window week --resets %d", weekReset)},
		"session is fuller": {"API Error: Rate limited",
			usage.Payload{SessionUsed: 100, SessionResets: fiveReset, WeekUsed: 70, WeekResets: weekReset},
			fmt.Sprintf("--window session --resets %d", fiveReset)},
		"tie: session": {"API Error: Rate limited",
			usage.Payload{SessionUsed: 100, SessionResets: fiveReset, WeekUsed: 100, WeekResets: weekReset},
			fmt.Sprintf("--window session --resets %d", fiveReset)},
		// The session window rolled over since the last report: the refusal is
		// about the week, however full the session was back then.
		"cached session reset passed": {"API Error: Rate limited",
			usage.Payload{SessionUsed: 100, SessionResets: nowEpoch - 60, WeekUsed: 10, WeekResets: weekReset},
			fmt.Sprintf("--window week --resets %d", weekReset)},
		"both cached resets passed": {"API Error: Rate limited",
			usage.Payload{SessionUsed: 100, SessionResets: nowEpoch - 60, WeekUsed: 10, WeekResets: nowEpoch - 1},
			"--window session"},
		"text names the week, cache has its reset": {"You've hit your weekly limit",
			usage.Payload{SessionUsed: 100, SessionResets: fiveReset, WeekUsed: 10, WeekResets: weekReset},
			fmt.Sprintf("--window week --resets %d", weekReset)},
		"text reset wins over the cache": {fiveHourRefusal,
			usage.Payload{SessionUsed: 100, SessionResets: fiveReset + 3600, WeekUsed: 10, WeekResets: weekReset},
			fmt.Sprintf("--window session --resets %d", fiveReset)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, _ := hookFixture(t)
			if err := usage.SaveSent(f.jul, email1, usage.Sent{Payload: tc.sent, AttemptedAt: time.Unix(nowEpoch-60, 0)}); err != nil {
				t.Fatal(err)
			}
			assertSilent(t, f, f.run(stopFailureInput("rate_limit", tc.details), "hook", "stop-failure"))
			want := "send-exhausted --email " + email1 + " " + tc.want
			if spawn.calls != 1 || strings.Join(spawn.args, " ") != want {
				t.Errorf("argv = %v, want %s", spawn.args, want)
			}
		})
	}
}

// Only a usage-limit refusal is reported; the input's session id does not
// matter, and its text never reaches errors.log.
func TestHookStopFailureInput(t *testing.T) {
	t.Run("other API errors", func(t *testing.T) {
		for _, errType := range []string{"server_error", "authentication_failed", "RATE_LIMIT", ""} {
			f, spawn, _ := hookFixture(t)
			assertSilent(t, f, f.run(stopFailureInput(errType, weeklyRefusal), "hook", "stop-failure"))
			if spawn.calls != 0 || f.errorLog() != "" {
				t.Errorf("%q: spawns = %d, errors.log = %q", errType, spawn.calls, f.errorLog())
			}
		}
	})
	t.Run("no session id", func(t *testing.T) {
		f, spawn, _ := hookFixture(t)
		in := `{"hook_event_name":"StopFailure","error":"rate_limit","error_details":"` + weeklyRefusal + `"}`
		assertSilent(t, f, f.run(in, "hook", "stop-failure"))
		if spawn.calls != 1 || spawn.args[0] != "send-exhausted" {
			t.Errorf("spawn = %+v", spawn)
		}
		if f.errorLog() != "" {
			t.Errorf("a missing session id is not a problem here: %q", f.errorLog())
		}
	})
	t.Run("unreadable input", func(t *testing.T) {
		for _, in := range []string{"not json", "", "[1]", `{"error":42}`} {
			f, spawn, _ := hookFixture(t)
			assertSilent(t, f, f.run(in, "hook", "stop-failure"))
			if spawn.calls != 0 {
				t.Errorf("%q: spawned %v", in, spawn.args)
			}
		}
	})
	t.Run("text stays out of errors.log", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		spawn.err = os.ErrPermission
		f.writeAccount(dir, email1)
		assertSilent(t, f, f.run(stopFailureInput("rate_limit", weeklyRefusal+" secret-token"), "hook", "stop-failure"))
		log := f.errorLog()
		if !strings.Contains(log, "SPAWN_FAILED") || !strings.Contains(log, "cannot start send-exhausted") {
			t.Errorf("errors.log = %q", log)
		}
		for _, leak := range []string{"secret-token", "weekly", email1} {
			if strings.Contains(log, leak) {
				t.Errorf("errors.log carries %q: %q", leak, log)
			}
		}
	})
}

// The SessionStart gate, except that a pending share is not eligible:
// nothing about the account is on the Worker yet.
func TestHookStopFailureGate(t *testing.T) {
	in := stopFailureInput("rate_limit", weeklyRefusal)
	cases := map[string]struct {
		prepare func(t *testing.T, f *fixture, dir string)
		log     string // code expected in errors.log; "" = nothing logged
	}{
		"unregistered dir": {func(t *testing.T, f *fixture, _ string) {
			other := filepath.Join(f.home, ".claude-personal")
			if err := os.MkdirAll(other, 0o700); err != nil {
				t.Fatal(err)
			}
			f.writeAccount(other, email1)
			f.useDir(other)
		}, "NOT_SHARED"},
		"not logged in": {func(t *testing.T, f *fixture, dir string) {
			if err := os.Remove(filepath.Join(dir, ".claude.json")); err != nil {
				t.Fatal(err)
			}
		}, ""},
		"unreadable account file": {func(t *testing.T, f *fixture, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{half"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "ACCOUNT_FILE_UNREADABLE"},
		"personal login": {func(t *testing.T, f *fixture, dir string) { f.writeAccount(dir, "me@personal.com") }, "NOT_SHARED"},
		"corrupt shared cache": {func(t *testing.T, f *fixture, _ string) {
			if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "NOT_SHARED"},
		"pending share": {func(t *testing.T, f *fixture, dir string) {
			f.writeAccount(dir, email2) // not on the allowlist yet
			f.markShareOnLogin("sixtynine1", "")
			f.save()
		}, "NOT_SHARED"},
		"pending share, unreadable cache": {func(t *testing.T, f *fixture, _ string) {
			f.markShareOnLogin("sixtynine1", "")
			f.save()
			if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "NOT_SHARED"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := hookFixture(t)
			tc.prepare(t, f, dir)
			assertSilent(t, f, f.run(in, "hook", "stop-failure"))
			if spawn.calls != 0 {
				t.Errorf("spawned %v", spawn.args)
			}
			log := f.errorLog()
			if (tc.log == "") != (log == "") || !strings.Contains(log, tc.log) {
				t.Errorf("errors.log = %q, want %q", log, tc.log)
			}
			if strings.Contains(log, "personal.com") || strings.Contains(log, email2) {
				t.Errorf("email leaked: %q", log)
			}
		})
	}
	t.Run("not set up", func(t *testing.T) {
		f := setup(t)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(in, "hook", "stop-failure"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SETUP") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("mixed-case login is lowercased", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		f.writeAccount(dir, "Claude1@SixtyNine.agency")
		assertSilent(t, f, f.run(in, "hook", "stop-failure"))
		if spawn.calls != 1 || spawn.args[2] != email1 {
			t.Errorf("spawn = %+v", spawn)
		}
	})
	t.Run("default dir", func(t *testing.T) {
		f := setup(t)
		spawn := f.captureSpawn()
		f.addConfig("default", email1)
		f.save()
		f.share(email1)
		f.useDir("")
		assertSilent(t, f, f.run(in, "hook", "stop-failure"))
		if spawn.calls != 1 {
			t.Errorf("spawn calls = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
}

// --- send-exhausted ----------------------------------------------------

func exhaustedArgv(extra ...string) []string {
	return append([]string{"send-exhausted", "--email", email1, "--window", "week"}, extra...)
}

func TestSendExhaustedReports(t *testing.T) {
	f := setup(t)
	f.save()
	f.share(email1)
	got := f.run("", exhaustedArgv("--resets", fmt.Sprint(friReset))...)
	assertCode(t, got, 0)
	if got.stdout != "" || got.stderr != "" {
		t.Errorf("send-exhausted must stay silent: %q %q", got.stdout, got.stderr)
	}
	calls := f.fake.CallsFor("exhausted")
	if f.ops() != "exhausted" || len(calls) != 1 {
		t.Fatalf("calls = %s", f.ops())
	}
	r := calls[0].Exhausted
	if calls[0].Email != email1 || r.Window != "week" || r.ResetsAt == nil || r.ResetsAt.Unix() != friReset || r.ResetsAt.Location() != time.UTC {
		t.Errorf("report = %+v (%+v)", r, calls[0])
	}
	if r.Reporter != (remote.Identity{Dev: "murat", MachineID: "3fa9c2d1e07b"}) {
		t.Errorf("reporter = %+v", r.Reporter)
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}

	// Without --resets the reset is unknown (null), and the email is
	// lowercased.
	f.fake.Reset()
	assertCode(t, f.run("", "send-exhausted", "--email", "Claude1@SixtyNine.agency", "--window", "session"), 0)
	calls = f.fake.CallsFor("exhausted")
	if len(calls) != 1 || calls[0].Email != email1 || calls[0].Exhausted.Window != "session" || calls[0].Exhausted.ResetsAt != nil {
		t.Errorf("calls = %+v", calls)
	}
}

func TestSendExhaustedValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing email", []string{"send-exhausted", "--window", "week"}, "--email must be a valid email address"},
		{"bad email", []string{"send-exhausted", "--email", "nope", "--window", "week"}, "--email must be a valid email address"},
		{"missing window", []string{"send-exhausted", "--email", email1}, "--window must be session or week"},
		{"bad window", []string{"send-exhausted", "--email", email1, "--window", "five_hour"}, "--window must be session or week"},
		{"zero reset", exhaustedArgv("--resets", "0"), "--resets must be positive"},
		{"negative reset", exhaustedArgv("--resets", "-5"), "--resets must be positive"},
		{"positional", exhaustedArgv("extra"), "no positional arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.save()
			f.share(email1)
			got := f.run("", tc.args...)
			assertCode(t, got, cli.ExitUsage)
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr = %q, want %q", got.stderr, tc.want)
			}
			if len(f.fake.Calls) != 0 {
				t.Errorf("invalid input reached the Worker: %+v", f.fake.Calls)
			}
		})
	}
	// A flag parse error fails like send-usage's (exit 1), before any I/O.
	f := setup(t)
	f.save()
	f.share(email1)
	got := f.run("", exhaustedArgv("--resets", "soon")...)
	if got.code == 0 || !strings.Contains(got.stderr, `invalid value "soon"`) || len(f.fake.Calls) != 0 {
		t.Errorf("non-numeric --resets: code %d, stderr %q, calls %s", got.code, got.stderr, f.ops())
	}
}

// Failures are logged (throttled, never with the email) and never returned:
// nothing watches this detached process.
func TestSendExhaustedFailures(t *testing.T) {
	t.Run("Worker error is throttled", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		f.fake.PutExhaustedErr = &remote.Error{Status: 500, Message: "internal error for " + email1}
		for i := 0; i < 3; i++ {
			assertCode(t, f.run("", exhaustedArgv()...), 0)
		}
		log := f.errorLog()
		if n := strings.Count(log, "EXHAUSTED_FAILED"); n != 1 || !strings.Contains(log, "500 internal error for <email>") {
			t.Errorf("EXHAUSTED_FAILED lines = %d: %q", n, log)
		}
		if strings.Contains(log, email1) {
			t.Errorf("email leaked: %q", log)
		}
		t.Setenv("JULIENNING_NOW_EPOCH", fmt.Sprint(nowEpoch+int64(usage.BackgroundFailedEvery/time.Second)))
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if n := strings.Count(f.errorLog(), "EXHAUSTED_FAILED"); n != 2 {
			t.Errorf("after the throttle window: %d lines", n)
		}
		if n := len(f.fake.CallsFor("exhausted")); n != 4 {
			t.Errorf("attempts = %d, want 4 (no debounce)", n)
		}
	})
	t.Run("older Worker without the route", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1)
		f.fake.PutExhaustedErr = &remote.Error{Status: 404, Message: "not found"}
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if log := f.errorLog(); !strings.Contains(log, "EXHAUSTED_FAILED") || !strings.Contains(log, "needs a redeploy") {
			t.Errorf("errors.log = %q", log)
		}
		if !f.sharedCache().Contains(email1) {
			t.Errorf("a plain 404 dropped the account from shared.json")
		}
	})
	t.Run("not shared", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email1, email2)
		f.fake.PutExhaustedErr = remote.NotSharedError()
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		log := f.errorLog()
		if !strings.Contains(log, "NOT_SHARED") || strings.Contains(log, "EXHAUSTED_FAILED") || strings.Contains(log, email1) {
			t.Errorf("errors.log = %q", log)
		}
		if c := f.sharedCache(); c.Contains(email1) || !c.Contains(email2) {
			t.Errorf("cache = %+v", c)
		}
	})
	t.Run("not on the allowlist", func(t *testing.T) {
		f := setup(t)
		f.save()
		f.share(email2)
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if len(f.fake.Calls) != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("calls = %s, errors.log = %q", f.ops(), f.errorLog())
		}
	})
	t.Run("unreadable allowlist", func(t *testing.T) {
		f := setup(t)
		f.save()
		if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if len(f.fake.Calls) != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("calls = %s, errors.log = %q", f.ops(), f.errorLog())
		}
	})
	t.Run("not set up", func(t *testing.T) {
		f := setup(t)
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if len(f.fake.Calls) != 0 || !strings.Contains(f.errorLog(), "NOT_SETUP") {
			t.Errorf("calls = %s, errors.log = %q", f.ops(), f.errorLog())
		}
	})
	t.Run("no remote", func(t *testing.T) {
		f := setup(t)
		f.cfg.Remote.URL = ""
		f.save()
		f.share(email1)
		assertCode(t, f.run("", exhaustedArgv()...), 0)
		if len(f.fake.Calls) != 0 || !strings.Contains(f.errorLog(), "Worker URL or token not configured") {
			t.Errorf("calls = %s, errors.log = %q", f.ops(), f.errorLog())
		}
	})
}
