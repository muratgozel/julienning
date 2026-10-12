package dirs

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
	"github.com/muratgozel/julienning/internal/sessions"
)

const (
	sidA = "0b7f6c1e-2a4d-4e8b-9f10-1c2d3e4f5a6b"
	sidB = "1c8a7d2f-3b5e-4f9c-8a21-2d3e4f5a6b7c"
	sidC = "2d9b8e30-4c6f-4a0d-9b32-3e4f5a6b7c8d"
)

// transcript writes dir/projects/<enc(cwd)>/<id>.jsonl the way Claude Code
// does, an hour old so a move does not take it for a session still open.
// cwd is not a git repository, so its memory folder is its own project
// folder. It returns the transcript's path relative to dir.
func (h *harness) transcript(dir, cwd, id, prompt string) string {
	h.t.Helper()
	rel := filepath.Join("projects", sessions.EncodeCwd(cwd), id+".jsonl")
	h.writeFile(filepath.Join(dir, rel),
		`{"type":"user","isSidechain":false,"cwd":"`+cwd+`","sessionId":"`+id+`","message":{"role":"user","content":"`+prompt+`"},"timestamp":"2026-09-29T09:00:00.000Z"}`+"\n")
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, rel), old, old); err != nil {
		h.t.Fatal(err)
	}
	return rel
}

// before returns whether a appears in out before b.
func before(out, a, b string) bool {
	i, j := strings.Index(out, a), strings.Index(out, b)
	return i >= 0 && j >= 0 && i < j
}

func TestUninstallMovesSessionsIntoTheDefaultDir(t *testing.T) {
	h := newHarness(t)
	custom, fresh := installed(t, h)
	proj := filepath.Join(h.home, "proj")
	relA := h.transcript(custom, proj, sidA, "fix login")
	h.writeFile(filepath.Join(custom, "file-history", sidA, "v1"), "checkpoint")
	h.writeFile(filepath.Join(custom, "projects", sessions.EncodeCwd(proj), "memory", "notes.md"), "remember")
	relB := h.transcript(fresh, proj, sidB, "add tests")
	def := filepath.Join(h.home, ".claude")
	assertGone(t, def)

	h.interactive = true
	h.stdin = strings.NewReader("\n") // Enter takes the default: yes
	out := h.mustRun(runUninstall)
	contains(t, out, "Sessions: 2 sessions in 2 registered config dirs. Without julienning, plain claude opens ~/.claude (not signed in yet; claude asks you to log in) and cannot resume them.\n")
	contains(t, out, "  ~/.claude does not exist yet; moving creates it.\n")
	contains(t, out, "Move them into ~/.claude? Project memory is merged, never overwritten. [Y/n] ")
	contains(t, out, "Sessions: created ~/.claude\n")
	contains(t, out, "Sessions: moving 1 session from ~/.claude-custom…\n")
	contains(t, out, "Sessions: moving 1 session from ~/.claude-fresh…\n")
	contains(t, out, "Sessions: moved 2 sessions into ~/.claude (memory: 1 file added)\n")
	contains(t, out, "New terminals run claude natively: plain claude opens ~/.claude (not signed in yet; claude asks you to log in).\n")
	contains(t, out, "Terminals already open keep julienning's shell functions until you close them or run: exec $SHELL\n")
	if !before(out, "Sessions: moved", "Settings:") {
		t.Errorf("sessions must move before anything else changes:\n%s", out)
	}

	for _, rel := range []string{relA, relB, filepath.Join("file-history", sidA, "v1"), filepath.Join("projects", sessions.EncodeCwd(proj), "memory", "notes.md")} {
		assertExists(t, filepath.Join(def, rel))
	}
	assertGone(t, filepath.Join(custom, relA))
	assertGone(t, filepath.Join(custom, "file-history", sidA))
	assertGone(t, filepath.Join(fresh, relB))
	if fi, err := os.Stat(def); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("~/.claude: %v %v, want a 0700 dir", fi, err)
	}
	if _, ok, _ := h.config().Current(); ok {
		t.Error("the rest of the uninstall did not run")
	}

	// Safe twice: nothing left to offer.
	out = h.mustRun(runUninstall)
	notContains(t, out, "Sessions:")
	contains(t, out, "Nothing to uninstall.")
}

func TestUninstallSessionsDeclinedThenMovedWithTheFlag(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	h.loggedIn(".claude", "me@personal.io")
	rel := h.transcript(custom, filepath.Join(h.home, "proj"), sidA, "fix login")

	h.interactive = true
	h.stdin = strings.NewReader("maybe\nn\n")
	out := h.mustRun(runUninstall)
	contains(t, out, "plain claude opens ~/.claude (signed in as me@personal.io) and cannot resume them.\n")
	contains(t, out, "Answer y or n.\n")
	contains(t, out, "Sessions: kept 1 session in the registered config dirs; resume one with CLAUDE_CONFIG_DIR=<its dir> claude --resume <id>, or move them all with `julienning uninstall --move-sessions`.\n")
	notContains(t, out, "does not exist yet")
	assertExists(t, filepath.Join(custom, rel))
	contains(t, out, "Shell:    removed the julienning hook line from ~/.zshrc")
	contains(t, out, "New terminals run claude natively: plain claude opens ~/.claude (signed in as me@personal.io).\n")

	// Plain uninstall keeps the registrations, so the move can come later,
	// without a terminal.
	h.interactive = false
	h.stdin = strings.NewReader("")
	out = h.mustRun(runUninstall, "--move-sessions")
	contains(t, out, "Sessions: 1 session in 1 registered config dir.")
	notContains(t, out, "[Y/n]")
	contains(t, out, "Sessions: moved 1 session into ~/.claude\n")
	assertExists(t, filepath.Join(h.home, ".claude", rel))
	assertGone(t, filepath.Join(custom, rel))
}

// --yes confirms the purge only; without a terminal nobody can answer the
// sessions question, so they stay.
func TestUninstallWithoutTerminalKeepsSessions(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	rel := h.transcript(custom, filepath.Join(h.home, "proj"), sidA, "fix login")

	out := h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Sessions: kept 1 session in the registered config dirs (no terminal to ask, and --move-sessions was not passed); resume one with CLAUDE_CONFIG_DIR=<its dir> claude --resume <id>.\n")
	notContains(t, out, "--move-sessions`")
	notContains(t, out, "[Y/n]")
	assertExists(t, filepath.Join(custom, rel))
	assertGone(t, filepath.Join(h.home, ".claude"))
	assertGone(t, h.jl)
}

func TestUninstallAnsweringNoToThePurgeChangesNothing(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	rel := h.transcript(custom, filepath.Join(h.home, "proj"), sidA, "fix login")
	h.interactive = true
	h.stdin = strings.NewReader("y\nn\n")
	out := h.mustRun(runUninstall, "--purge")
	contains(t, out, "[Y/n] ")
	contains(t, out, "Cancelled; nothing was changed.")
	assertExists(t, filepath.Join(custom, rel))
	assertGone(t, filepath.Join(h.home, ".claude"))
	if !strings.Contains(h.read(filepath.Join(h.home, ".zshrc")), rcMarker) {
		t.Fatal("a cancelled purge removed the hook")
	}
}

// An open session is named before the question, stays where it is with the
// command that resumes it, and does not stop the purge.
func TestUninstallOpenSessionStaysAndThePurgeFinishes(t *testing.T) {
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return true }
	t.Cleanup(func() { livesess.Alive = prev })

	h := newHarness(t)
	custom, _ := installed(t, h)
	proj := filepath.Join(h.home, "proj")
	relA := h.transcript(custom, proj, sidA, "fix login")
	relC := h.transcript(custom, proj, sidC, "write docs")
	h.writeFile(filepath.Join(custom, "sessions", "4242.json"), `{"pid":4242,"sessionId":"`+sidA+`","kind":"interactive"}`)

	out := h.mustRun(runUninstall, "--purge", "--yes", "--move-sessions")
	contains(t, out, "  1 is open in a terminal and stays where it is unless you exit it first:\n    \"fix login\" in ~/.claude-custom\n")
	contains(t, out, "Sessions: moved 1 session into ~/.claude\n")
	contains(t, out, "Sessions: 1 stays where it is; resume one with CLAUDE_CONFIG_DIR set to its dir:\n  \"fix login\" (open in a terminal):\n    CLAUDE_CONFIG_DIR=~/.claude-custom claude --resume "+sidA+"\n")
	contains(t, out, "Purged:   ~/.julienning")
	assertExists(t, filepath.Join(custom, relA))
	assertExists(t, filepath.Join(h.home, ".claude", relC))
	assertGone(t, h.jl)
}

func TestUninstallCollisionAndMemoryConflictAreReportedOnce(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	def := h.loggedIn(".claude", "me@personal.io")
	proj := filepath.Join(h.home, "proj")
	mem := filepath.Join("projects", sessions.EncodeCwd(proj), "memory", "notes.md")
	// Two sessions of one project: each move merges the same memory again.
	h.transcript(custom, proj, sidA, "fix login")
	h.transcript(custom, proj, sidC, "write docs")
	h.writeFile(filepath.Join(custom, mem), "custom's version")
	h.writeFile(filepath.Join(def, mem), "default's version")
	// The same id already in the default dir, in another project folder.
	relB := h.transcript(custom, proj, sidB, "add tests")
	h.transcript(def, filepath.Join(h.home, "elsewhere"), sidB, "already here")

	out := h.mustRun(runUninstall, "--move-sessions")
	contains(t, out, "Sessions: moved 2 sessions into ~/.claude (memory: 1 conflict kept)\n")
	contains(t, out, "\"add tests\" (~/.claude already has a session with this id):\n    CLAUDE_CONFIG_DIR=~/.claude-custom claude --resume "+sidB+"\n")
	warn := "julienning: warning: kept ~/.claude's version of project memory file notes.md; the one from ~/.claude-custom stays at ~/" + filepath.Join(".claude-custom", mem)
	if n := strings.Count(h.stderr.String(), warn); n != 1 {
		t.Errorf("conflict warned %d times, want once:\n%s", n, h.stderr.String())
	}
	assertExists(t, filepath.Join(custom, relB))
	if h.read(filepath.Join(def, mem)) != "default's version" {
		t.Error("the default dir's memory was overwritten")
	}
}

// The default dir is never a source, even when registered; a dir registered
// twice (through a symlink) is listed once.
func TestUninstallSessionSourcesSkipTheDefaultDirAndDuplicates(t *testing.T) {
	h := newHarness(t)
	def := h.loggedIn(".claude", "me@personal.io")
	custom := h.loggedIn(".claude-custom", "a@team.io")
	link := filepath.Join(h.home, ".claude-link")
	if err := os.Symlink(custom, link); err != nil {
		t.Fatal(err)
	}
	h.initConfig(false,
		config.ConfigDir{Name: "default", Dir: def},
		config.ConfigDir{Name: "custom", Dir: custom},
		config.ConfigDir{Name: "link", Dir: link})
	proj := filepath.Join(h.home, "proj")
	relDef := h.transcript(def, proj, sidC, "personal work")
	rel := h.transcript(custom, proj, sidA, "fix login")

	out := h.mustRun(runUninstall, "--move-sessions")
	contains(t, out, "Sessions: 1 session in 1 registered config dir.")
	contains(t, out, "Sessions: moved 1 session into ~/.claude\n")
	h.stderr.Reset()
	assertExists(t, filepath.Join(def, rel))
	assertExists(t, filepath.Join(def, relDef))
	assertGone(t, filepath.Join(custom, rel))
}

// An error that is not a refusal (disk full, permissions) stops the moves,
// keeps the rest where they are and blocks the purge, so a re-run can finish.
func TestUninstallMoveErrorStopsMovingAndBlocksThePurge(t *testing.T) {
	h := newHarness(t)
	custom, fresh := installed(t, h)
	proj := filepath.Join(h.home, "proj")
	relA := h.transcript(custom, proj, sidA, "fix login")
	relB := h.transcript(fresh, proj, sidB, "add tests")
	calls := 0
	moveSession = func(s sessions.Session, target config.ConfigDir) (sessions.Report, error) {
		calls++
		return sessions.Report{}, errors.New("could not move session 0b7f6c1e to ~/.claude (nothing was changed): no space left on device")
	}
	t.Cleanup(func() { moveSession = sessions.Move })

	err := h.run(runUninstall, "--purge", "--yes", "--move-sessions")
	if err == nil || !strings.Contains(err.Error(), "uninstall finished with 2 problem(s)") {
		t.Fatalf("got %v", err)
	}
	if calls != 1 {
		t.Errorf("tried %d moves after an error, want 1", calls)
	}
	out, stderr := h.stdout.String(), h.stderr.String()
	contains(t, stderr, "julienning: stopped moving sessions: could not move session 0b7f6c1e to ~/.claude (nothing was changed): no space left on device\n")
	contains(t, stderr, "julienning: not purging: moving the sessions stopped on an error (see above)")
	contains(t, out, "Sessions: 2 stay where they are; resume one with CLAUDE_CONFIG_DIR set to its dir:\n  … and 2 more\n")
	contains(t, out, "Shell:    removed the julienning hook line from ~/.zshrc")
	notContains(t, out, "Purged:")
	notContains(t, out, "New terminals run claude natively")
	assertExists(t, filepath.Join(custom, relA))
	assertExists(t, filepath.Join(fresh, relB))
	assertExists(t, filepath.Join(h.jl, config.ConfigFile))

	// Fixed: the re-run moves them and purges.
	moveSession = sessions.Move
	out = h.mustRun(runUninstall, "--purge", "--yes", "--move-sessions")
	contains(t, out, "Sessions: moved 2 sessions into ~/.claude\n")
	contains(t, out, "Purged:   ~/.julienning")
}

// Ctrl-C between two moves stops before the next one and before anything
// else changes; a re-run finishes.
func TestUninstallInterruptedBetweenMovesChangesNothingElse(t *testing.T) {
	// Guard: should the trap not work, the signal lands here instead of
	// killing the test binary.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGINT)
	defer signal.Stop(guard)

	h := newHarness(t)
	custom, fresh := installed(t, h)
	proj := filepath.Join(h.home, "proj")
	relA := h.transcript(custom, proj, sidA, "fix login")
	relB := h.transcript(fresh, proj, sidB, "add tests")
	moveSession = func(s sessions.Session, target config.ConfigDir) (sessions.Report, error) {
		rep, err := sessions.Move(s, target)
		if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
			t.Error(err)
		}
		select {
		case <-guard:
		case <-time.After(5 * time.Second):
			t.Error("signal never delivered")
		}
		time.Sleep(20 * time.Millisecond) // let it reach the uninstall's trap too
		return rep, err
	}
	t.Cleanup(func() { moveSession = sessions.Move })

	err := h.run(runUninstall, "--move-sessions")
	if err == nil || err.Error() != "uninstall interrupted while moving sessions (1 session moved, the rest are where they were); nothing else was changed, run it again to finish" {
		t.Fatalf("got %v", err)
	}
	contains(t, h.stdout.String(), "Sessions: moved 1 session into ~/.claude\n")
	assertExists(t, filepath.Join(h.home, ".claude", relA))
	assertExists(t, filepath.Join(fresh, relB))
	if !strings.Contains(h.read(filepath.Join(h.home, ".zshrc")), rcMarker) {
		t.Fatal("the hook went although the uninstall was interrupted")
	}

	moveSession = sessions.Move
	out := h.mustRun(runUninstall, "--move-sessions")
	contains(t, out, "Sessions: moved 1 session into ~/.claude\n")
	contains(t, out, "Shell:    removed the julienning hook line from ~/.zshrc")
}

func TestUninstallTargetInTheWayStopsTheMoves(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	rel := h.transcript(custom, filepath.Join(h.home, "proj"), sidA, "fix login")
	h.writeFile(filepath.Join(h.home, ".claude"), "not a dir")
	err := h.run(runUninstall, "--move-sessions")
	if err == nil {
		t.Fatal("want an error")
	}
	contains(t, h.stderr.String(), "julienning: stopped moving sessions: ~/.claude is not a directory; move it out of the way to let uninstall move the sessions there\n")
	assertExists(t, filepath.Join(custom, rel))
}

// Plain uninstall drops pins too: a terminal opened before it still has the
// claude function, which follows pins.json while it exists.
func TestUninstallRemovesPins(t *testing.T) {
	h := newHarness(t)
	installed(t, h)
	p := filepath.Join(h.jl, config.PinsFile)
	h.writeFile(p, `{"version":1,"pins":[{"dir":"/x","email":"me@x.io"},{"dir":"/y","email":"a@team.io"}]}`)
	out := h.mustRun(runUninstall)
	contains(t, out, "Pins:     removed 2 pins\n")
	assertGone(t, p)

	h.writeFile(p, "{broken")
	out = h.mustRun(runUninstall)
	contains(t, out, "Pins:     removed ~/.julienning/pins.json\n")
	assertGone(t, p)
}

func TestResumeCommandQuotesOnlyWhenNeeded(t *testing.T) {
	newHarness(t)
	home, _ := os.UserHomeDir()
	for dir, want := range map[string]string{
		filepath.Join(home, ".claude-julienning1"): "CLAUDE_CONFIG_DIR=~/.claude-julienning1 claude --resume " + sidA,
		"/opt/claude":                      "CLAUDE_CONFIG_DIR=/opt/claude claude --resume " + sidA,
		filepath.Join(home, "my dirs/c"):   "CLAUDE_CONFIG_DIR='" + home + "/my dirs/c' claude --resume " + sidA,
		"/tmp/it's":                        `CLAUDE_CONFIG_DIR='/tmp/it'\''s' claude --resume ` + sidA,
		filepath.Join(home, "$(rm -rf x)"): "CLAUDE_CONFIG_DIR='" + home + "/$(rm -rf x)' claude --resume " + sidA,
	} {
		if got := resumeCommand(sessions.Session{ID: sidA, ConfigDir: dir}); got != want {
			t.Errorf("%s:\n got %s\nwant %s", dir, got, want)
		}
	}
}

// flipReader runs flip on the first read: the user doing something while
// the question waits for the answer.
type flipReader struct {
	flip func()
	r    io.Reader
}

func (f *flipReader) Read(p []byte) (int, error) {
	if f.flip != nil {
		f.flip()
		f.flip = nil
	}
	return f.r.Read(p)
}

// A session named as open, then exited before the answer, moves: the
// listing's view is stale by then and Move checks the registries itself.
func TestUninstallSessionExitedWhileAskingMoves(t *testing.T) {
	alive := true
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return alive }
	t.Cleanup(func() { livesess.Alive = prev })

	h := newHarness(t)
	custom, _ := installed(t, h)
	rel := h.transcript(custom, filepath.Join(h.home, "proj"), sidA, "fix login")
	h.writeFile(filepath.Join(custom, "sessions", "4242.json"), `{"pid":4242,"sessionId":"`+sidA+`","kind":"interactive"}`)
	h.interactive = true
	h.stdin = &flipReader{flip: func() { alive = false }, r: strings.NewReader("y\n")}

	out := h.mustRun(runUninstall)
	contains(t, out, "1 is open in a terminal and stays where it is unless you exit it first:")
	contains(t, out, "Sessions: moved 1 session into ~/.claude\n")
	notContains(t, out, "stays where it is;")
	assertExists(t, filepath.Join(h.home, ".claude", rel))
}
