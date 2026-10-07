package accounts

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
)

// hookFixture wires a registered, logged-in, shared dir as the active one.
func hookFixture(t *testing.T) (*fixture, *spawnRecord, string) {
	t.Helper()
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.useDir(cd.Dir)
	return f, f.captureSpawn(), cd.Dir
}

const startInput = `{"session_id":"5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11","transcript_path":"/x.jsonl",` +
	`"cwd":"/tmp/p","hook_event_name":"SessionStart","source":"startup","brand_new_field":{"x":1}}`

const endInput = `{"session_id":"5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11","cwd":"/tmp/p",` +
	`"hook_event_name":"SessionEnd","reason":"prompt_input_exit"}`

// assertSilent checks the hook contract: exit 0, no output, no network.
func assertSilent(t *testing.T, f *fixture, got result) {
	t.Helper()
	assertCode(t, got, 0)
	if got.stdout != "" || got.stderr != "" {
		t.Errorf("hooks never print: stdout %q stderr %q", got.stdout, got.stderr)
	}
	if len(f.fake.Calls) != 0 {
		t.Errorf("hooks never touch the network: %+v", f.fake.Calls)
	}
}

func TestHookSessionStartSpawnsClaimSync(t *testing.T) {
	f, spawn, _ := hookFixture(t)
	got := f.run(startInput, "hook", "session-start")
	assertSilent(t, f, got)
	if spawn.calls != 1 {
		t.Fatalf("spawn calls = %d", spawn.calls)
	}
	want := "claim-sync --starting 5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11"
	if strings.Join(spawn.args, " ") != want {
		t.Errorf("argv = %v, want %s", spawn.args, want)
	}
	if spawn.self == "" {
		t.Errorf("spawn path is empty")
	}
	if f.errorLog() != "" {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

func TestHookSessionEndPassesEnding(t *testing.T) {
	f, spawn, _ := hookFixture(t)
	got := f.run(endInput, "hook", "session-end")
	assertSilent(t, f, got)
	want := "claim-sync --ending 5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11"
	if spawn.calls != 1 || strings.Join(spawn.args, " ") != want {
		t.Errorf("spawn = %+v, want %s", spawn, want)
	}
}

// /clear ends a session inside a process that keeps running.
func TestHookSessionEndOnClearDoesNothing(t *testing.T) {
	f, spawn, _ := hookFixture(t)
	got := f.run(`{"session_id":"abc","reason":"clear"}`, "hook", "session-end")
	assertSilent(t, f, got)
	if spawn.calls != 0 {
		t.Errorf("spawned on /clear: %v", spawn.args)
	}
	// SessionStart after /clear still syncs.
	assertSilent(t, f, f.run(`{"session_id":"def","source":"clear"}`, "hook", "session-start"))
	if spawn.calls != 1 {
		t.Errorf("spawn calls = %d", spawn.calls)
	}
}

// Bad input is logged (throttled) and ignored: the sync still runs, just
// without the session id (here the ending session is not in the registry, so
// the hook's parent pid is passed instead).
func TestHookToleratesBadInput(t *testing.T) {
	cases := map[string]string{
		"not json":          "not json",
		"empty":             "",
		"array":             "[1,2]",
		"null":              "null",
		"no session id":     `{"cwd":"/tmp/p"}`,
		"session id number": `{"session_id":42}`,
		"hostile id":        `{"session_id":"../../x; rm -rf ~"}`,
		"overlong id":       `{"session_id":"` + strings.Repeat("a", 129) + `"}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, _ := hookFixture(t)
			for i := 0; i < 3; i++ {
				assertSilent(t, f, f.run(in, "hook", "session-end"))
			}
			if want := fmt.Sprintf("claim-sync --ending-pid %d", hookPPID); spawn.calls != 3 || strings.Join(spawn.args, " ") != want {
				t.Errorf("spawn = %+v, want %s", spawn, want)
			}
			if n := strings.Count(f.errorLog(), "HOOK_INPUT_INVALID"); n != 1 {
				t.Errorf("HOOK_INPUT_INVALID logged %d times, want 1: %q", n, f.errorLog())
			}
			if strings.Contains(f.errorLog(), "rm -rf") {
				t.Errorf("input echoed into errors.log: %q", f.errorLog())
			}
		})
	}
}

// Without a session id in its input, SessionEnd identifies the ending
// session by process ancestry: Claude runs the hook directly (parent) or
// through a shell (grandparent). Otherwise the ending session would count as
// live and keep its claim until the next reconcile.
func TestHookSessionEndWithoutSessionID(t *testing.T) {
	const noID = `{"cwd":"/tmp/p","reason":"prompt_input_exit"}`
	cases := map[string]struct {
		prepare func(f *fixture, dir string)
		want    string
	}{
		"parent is the session": {func(f *fixture, dir string) {
			f.session(dir, 7000, "other")
			f.session(dir, hookPPID, "s-parent")
		}, "claim-sync --ending s-parent"},
		"grandparent is the session": {func(f *fixture, dir string) {
			f.session(dir, 7000, "other")
			f.session(dir, 4200, "s-grand")
			f.parents[hookPPID] = 4200
		}, "claim-sync --ending s-grand"},
		"no registry entry": {func(f *fixture, dir string) {
			f.session(dir, 7000, "other")
			f.parents[hookPPID] = 4200
		}, fmt.Sprintf("claim-sync --ending-pid %d", hookPPID)},
		"ps fails": {func(f *fixture, dir string) {
			f.session(dir, 7000, "other")
		}, fmt.Sprintf("claim-sync --ending-pid %d", hookPPID)},
		"dead entry": {func(f *fixture, dir string) {
			f.session(dir, hookPPID, "s-stale")
			f.dead[hookPPID] = true
		}, fmt.Sprintf("claim-sync --ending-pid %d", hookPPID)},
		"id unsafe to pass on": {func(f *fixture, dir string) {
			f.session(dir, 4200, "../../x y")
			f.parents[hookPPID] = 4200
		}, "claim-sync --ending-pid 4200"},
		"no registry": {func(f *fixture, dir string) {
			if err := os.RemoveAll(filepath.Join(dir, "sessions")); err != nil {
				f.t.Fatal(err)
			}
		}, fmt.Sprintf("claim-sync --ending-pid %d", hookPPID)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := hookFixture(t)
			tc.prepare(f, dir)
			assertSilent(t, f, f.run(noID, "hook", "session-end"))
			if spawn.calls != 1 || strings.Join(spawn.args, " ") != tc.want {
				t.Errorf("argv = %v, want %s", spawn.args, tc.want)
			}
		})
	}
	t.Run("reparented to init", func(t *testing.T) {
		f, spawn, _ := hookFixture(t)
		hookParent = func() int { return 1 }
		assertSilent(t, f, f.run(noID, "hook", "session-end"))
		if spawn.calls != 1 || strings.Join(spawn.args, " ") != "claim-sync" {
			t.Errorf("argv = %v, want plain claim-sync", spawn.args)
		}
	})
	t.Run("unreadable registry is logged", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		reg := filepath.Join(dir, "sessions")
		if err := os.RemoveAll(reg); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reg, nil, 0o600); err != nil { // a file, not a dir
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(noID, "hook", "session-end"))
		if want := fmt.Sprintf("claim-sync --ending-pid %d", hookPPID); strings.Join(spawn.args, " ") != want {
			t.Errorf("argv = %v, want %s", spawn.args, want)
		}
		if !strings.Contains(f.errorLog(), "CLAIM_SYNC_FAILED") || !strings.Contains(f.errorLog(), "cannot identify the ending session") {
			t.Errorf("errors.log = %q", f.errorLog())
		}
	})
	// A valid id in the input wins; ancestry is not consulted.
	t.Run("input id wins", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		f.session(dir, hookPPID, "s-parent")
		assertSilent(t, f, f.run(endInput, "hook", "session-end"))
		if want := "claim-sync --ending 5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11"; strings.Join(spawn.args, " ") != want {
			t.Errorf("argv = %v, want %s", spawn.args, want)
		}
	})
}

func TestHookBadArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"none":    {"hook"},
		"unknown": {"hook", "pre-tool-use"},
		"two":     {"hook", "session-start", "session-end"},
	} {
		t.Run(name, func(t *testing.T) {
			f, spawn, _ := hookFixture(t)
			assertSilent(t, f, f.run(startInput, args...))
			if spawn.calls != 0 {
				t.Errorf("spawned: %v", spawn.args)
			}
			if !strings.Contains(f.errorLog(), "HOOK_INPUT_INVALID") {
				t.Errorf("errors.log = %q", f.errorLog())
			}
		})
	}
}

// A writer that never closes stdin must not hold Claude Code up.
func TestHookStdinThatNeverArrives(t *testing.T) {
	f, spawn, _ := hookFixture(t)
	hookStdinWait = 20 * time.Millisecond
	r, w := io.Pipe()
	t.Cleanup(func() { w.Close() })

	start := time.Now()
	var out, errOut strings.Builder
	env := cli.Env{Args: []string{"session-start"}, Stdin: r, Stdout: &out, Stderr: &errOut}
	if err := runHook(env); err != nil {
		t.Fatalf("runHook: %v", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("hook took %v", d)
	}
	if spawn.calls != 1 || strings.Join(spawn.args, " ") != "claim-sync" {
		t.Errorf("spawn = %+v", spawn)
	}
	if !strings.Contains(f.errorLog(), "nothing on stdin") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

func TestHookGate(t *testing.T) {
	t.Run("not set up", func(t *testing.T) {
		f := setup(t)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SETUP") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("unregistered dir", func(t *testing.T) {
		f, spawn, _ := hookFixture(t)
		other := filepath.Join(f.home, ".claude-personal")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		f.writeAccount(other, email1)
		f.useDir(other)
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("not logged in", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		if err := os.Remove(filepath.Join(dir, ".claude.json")); err != nil {
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || f.errorLog() != "" {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("unreadable account file", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{half"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "ACCOUNT_FILE_UNREADABLE") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("not shared", func(t *testing.T) {
		f, spawn, dir := hookFixture(t)
		f.writeAccount(dir, "me@personal.com")
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
		if strings.Contains(f.errorLog(), "personal.com") {
			t.Errorf("email leaked: %q", f.errorLog())
		}
	})
	t.Run("corrupt shared cache", func(t *testing.T) {
		f, spawn, _ := hookFixture(t)
		if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 {
			t.Errorf("spawned on an unreadable allowlist")
		}
	})
	t.Run("default dir", func(t *testing.T) {
		f := setup(t)
		spawn := f.captureSpawn()
		f.addConfig("default", email1)
		f.save()
		f.share(email1)
		f.useDir("")
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 1 {
			t.Errorf("spawn calls = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
}

// SessionEnd must reach claim-sync whenever the dir is registered: after
// /logout the login is gone, and a broken or stale allowlist proves nothing;
// claim-sync only releases what claims.json says this machine holds.
func TestHookSessionEndGate(t *testing.T) {
	spawns := map[string]func(f *fixture, dir string){
		"logged out": func(f *fixture, dir string) {
			if err := os.Remove(filepath.Join(dir, ".claude.json")); err != nil {
				f.t.Fatal(err)
			}
		},
		"unreadable account file": func(f *fixture, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte("{half"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		},
		"not shared": func(f *fixture, dir string) { f.writeAccount(dir, "me@personal.com") },
		"corrupt shared cache": func(f *fixture, dir string) {
			if err := os.WriteFile(filepath.Join(f.jul, "shared.json"), []byte("{"), 0o600); err != nil {
				f.t.Fatal(err)
			}
		},
		"missing shared cache": func(f *fixture, dir string) {
			if err := os.Remove(filepath.Join(f.jul, "shared.json")); err != nil {
				f.t.Fatal(err)
			}
		},
	}
	for name, prepare := range spawns {
		t.Run(name, func(t *testing.T) {
			f, spawn, dir := hookFixture(t)
			prepare(f, dir)
			assertSilent(t, f, f.run(endInput, "hook", "session-end"))
			want := "claim-sync --ending 5f2c1e0a-9b7d-4c3e-8a21-0d6f4b9e7c11"
			if spawn.calls != 1 || strings.Join(spawn.args, " ") != want {
				t.Errorf("spawn = %+v, want %s", spawn, want)
			}
			if f.errorLog() != "" {
				t.Errorf("errors.log = %q", f.errorLog())
			}
		})
	}
	t.Run("unregistered dir", func(t *testing.T) {
		f, spawn, _ := hookFixture(t)
		other := filepath.Join(f.home, ".claude-personal")
		if err := os.MkdirAll(other, 0o700); err != nil {
			t.Fatal(err)
		}
		f.useDir(other)
		assertSilent(t, f, f.run(endInput, "hook", "session-end"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("not set up", func(t *testing.T) {
		f := setup(t)
		spawn := f.captureSpawn()
		assertSilent(t, f, f.run(endInput, "hook", "session-end"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SETUP") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
}

// An explicitly exported CLAUDE_CONFIG_DIR=~/.claude makes Claude read
// ~/.claude/.claude.json, not ~/.claude.json: the hook must read the same
// file, while the registration lookup still matches the default dir.
func TestHookExplicitDefaultDirReadsItsOwnAccountFile(t *testing.T) {
	prepare := func(t *testing.T) (*fixture, *spawnRecord, string) {
		f := setup(t)
		spawn := f.captureSpawn()
		cd := f.addConfig("default", email1) // ~/.claude.json: a shared login
		f.save()
		f.share(email1, email2)
		f.useDir(cd.Dir)
		return f, spawn, cd.Dir
	}
	t.Run("logged out there", func(t *testing.T) {
		f, spawn, _ := prepare(t)
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 {
			t.Errorf("claimed the login of ~/.claude.json: %v", spawn.args)
		}
	})
	t.Run("logged in there", func(t *testing.T) {
		f, spawn, dir := prepare(t)
		body := `{"oauthAccount":{"emailAddress":"` + email2 + `"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 1 {
			t.Errorf("spawn calls = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
	t.Run("unshared login there", func(t *testing.T) {
		f, spawn, dir := prepare(t)
		body := `{"oauthAccount":{"emailAddress":"me@personal.com"}}`
		if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		assertSilent(t, f, f.run(startInput, "hook", "session-start"))
		if spawn.calls != 0 || !strings.Contains(f.errorLog(), "NOT_SHARED") {
			t.Errorf("spawns = %d, errors.log = %q", spawn.calls, f.errorLog())
		}
	})
}

func TestHookSpawnFailureIsLogged(t *testing.T) {
	f, spawn, _ := hookFixture(t)
	spawn.err = os.ErrPermission
	assertSilent(t, f, f.run(startInput, "hook", "session-start"))
	if !strings.Contains(f.errorLog(), "SPAWN_FAILED") {
		t.Errorf("errors.log = %q", f.errorLog())
	}
}

// psParent against the real process table: the test binary's parent.
func TestPSParent(t *testing.T) {
	got, err := psParent(os.Getpid())
	if err != nil && !strings.Contains(err.Error(), "unexpected output") {
		t.Skipf("ps unavailable or slower than %v here: %v", psParentWait, err)
	}
	if err != nil || got != os.Getppid() {
		t.Errorf("psParent = %d, %v, want %d", got, err, os.Getppid())
	}
	if _, err := psParent(1 << 30); err == nil {
		t.Errorf("a pid that cannot exist has a parent")
	}
}
