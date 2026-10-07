package switching

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/cli"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/sessions"
	"github.com/muratgozel/julienning/internal/tui"
)

const (
	flakyTitle   = "Fix flaky clock tests"
	wantQuestion = `Move "Fix flaky clock tests" from beta to alpha?`
	wantDetail   = "Its transcript, checkpoints, task list and session environment move to alpha; beta will no longer have it. Project memory is merged, never overwritten."
)

// The owner's wording, verbatim.
func TestMoveConfirmText(t *testing.T) {
	if got := moveQuestion(flakyTitle, "beta", "alpha"); got != wantQuestion {
		t.Errorf("question = %q", got)
	}
	if got := moveDetail("beta", "alpha"); got != wantDetail {
		t.Errorf("detail = %q", got)
	}
}

// moveFixture: target alpha (julienning3), other account beta (sixtynine)
// holding "Fix flaky clock tests" (row 1) with checkpoints and project
// memory, and a session already in alpha (row 2).
type moveFixture struct {
	*fixture
	target, other config.ConfigDir
	src           string // beta's transcript
	srcBody       []byte
	checkpoint    string // beta's file-history entry
	srcMemory     string // beta's project memory file
}

func newMoveFixture(t *testing.T) *moveFixture {
	f := setup(t)
	f.tty = true
	m := &moveFixture{fixture: f}
	m.target = f.addConfig("julienning3", email1)
	m.other = f.addConfig("sixtynine", email2)
	f.save()
	f.fake.Listing = listing(nacct(email1, "alpha", 12, 28), nacct(email2, "beta", 50, 60))
	m.src = f.writeSession(m.other, sidA, flakyTitle, "the clock tests flake on CI", 2*time.Hour)
	f.writeSession(m.target, sidB, "Here already", "p", 3*time.Hour)
	body, err := os.ReadFile(m.src)
	if err != nil {
		t.Fatal(err)
	}
	m.srcBody = body
	m.checkpoint = filepath.Join(m.other.Dir, "file-history", sidA, "a1b2c3@v1")
	m.srcMemory = filepath.Join(m.other.Dir, "projects", sessions.EncodeCwd(f.proj), "memory", "clock.md")
	for _, p := range []string{m.checkpoint, m.srcMemory} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// movedTo is where the transcript lands in the target.
func (m *moveFixture) movedTo() string {
	return filepath.Join(m.target.Dir, "projects", sessions.EncodeCwd(m.proj), sidA+".jsonl")
}

// assertUntouched checks that no part of the session or its memory moved.
func (m *moveFixture) assertUntouched(t *testing.T) {
	t.Helper()
	body, err := os.ReadFile(m.src)
	if err != nil || string(body) != string(m.srcBody) {
		t.Errorf("source transcript changed: %v", err)
	}
	for _, p := range []string{m.checkpoint, m.srcMemory} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("source item gone: %v", err)
		}
	}
	for _, p := range []string{
		m.movedTo(),
		filepath.Join(m.target.Dir, "file-history", sidA),
		filepath.Join(m.target.Dir, "projects", sessions.EncodeCwd(m.proj), "memory", "clock.md"),
	} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s must not exist in the target (err %v)", p, err)
		}
	}
}

// Declining the move goes back to the same list with the cursor on that
// row; picking a session already in the target then resumes it in place.
func TestLaunchDeclinedMoveReturnsToPicker(t *testing.T) {
	m := newMoveFixture(t)
	var shown [][]tui.Item
	answers := []int{1, 2} // beta's session, then alpha's own
	m.pick = func(items []tui.Item) (int, error) {
		shown = append(shown, items)
		i := answers[0]
		answers = answers[1:]
		return i, nil
	}
	m.confirm = func(string, string) (bool, error) {
		m.assertUntouched(t) // asked before anything moves
		return false, nil
	}

	got := m.run("use", "alpha")
	assertCode(t, got, 0)
	if len(m.confirms) != 1 || m.confirms[0] != (confirmCall{wantQuestion, wantDetail}) {
		t.Fatalf("confirms = %+v", m.confirms)
	}
	if !reflect.DeepEqual(m.pickAt, []int{0, 1}) {
		t.Errorf("picker opened on rows %v, want [0 1]", m.pickAt)
	}
	if len(shown) != 2 || !reflect.DeepEqual(shown[0], shown[1]) {
		t.Errorf("the picker must come back with the same list:\n%+v", shown)
	}
	m.assertUntouched(t)
	if len(m.execs) != 1 || m.execs[0].dir != m.target.Dir || strings.Join(m.execs[0].args, " ") != "--resume "+sidB {
		t.Fatalf("execs = %+v", m.execs)
	}
	// Nothing is printed for the declined move: only the summary line.
	if got.stdout != m.pickHdr+"\n" {
		t.Errorf("stdout = %q, want just the summary line", got.stdout)
	}
	if strings.Contains(got.stderr, "Move") || strings.Contains(got.stderr, "move") {
		t.Errorf("stderr = %q", got.stderr)
	}
}

// Declining and then leaving the picker starts nothing and moves nothing.
func TestLaunchDeclinedMoveThenCancel(t *testing.T) {
	m := newMoveFixture(t)
	calls := 0
	m.pick = func([]tui.Item) (int, error) {
		if calls++; calls == 1 {
			return 1, nil
		}
		return 0, tui.ErrCanceled
	}
	m.confirm = func(string, string) (bool, error) { return false, nil }
	got := m.run("use", "alpha")
	assertCode(t, got, 0)
	if !strings.HasSuffix(got.stdout, "Not starting claude; alpha stays selected.\n") || len(m.execs) != 0 {
		t.Errorf("stdout = %q execs %+v", got.stdout, m.execs)
	}
	m.assertUntouched(t)
}

func TestLaunchConfirmedMoveMovesThenResumes(t *testing.T) {
	m := newMoveFixture(t)
	m.pick = func([]tui.Item) (int, error) { return 1, nil }
	m.confirm = func(string, string) (bool, error) {
		m.assertUntouched(t)
		return true, nil
	}
	got := m.run("use", "alpha")
	assertCode(t, got, 0)
	if len(m.confirms) != 1 || m.confirms[0] != (confirmCall{wantQuestion, wantDetail}) {
		t.Fatalf("confirms = %+v", m.confirms)
	}
	if !strings.Contains(got.stdout, `Moved "Fix flaky clock tests" from beta to alpha (memory: 1 file added).`) {
		t.Errorf("stdout = %q", got.stdout)
	}
	if _, err := os.Stat(m.src); !errors.Is(err, os.ErrNotExist) {
		t.Error("source transcript must be gone")
	}
	if _, err := os.Stat(m.movedTo()); err != nil {
		t.Errorf("transcript not in target: %v", err)
	}
	if !reflect.DeepEqual(m.pickAt, []int{0}) {
		t.Errorf("picker calls %v", m.pickAt)
	}
	if len(m.execs) != 1 || m.execs[0].dir != m.target.Dir || strings.Join(m.execs[0].args, " ") != "--resume "+sidA {
		t.Fatalf("execs = %+v", m.execs)
	}
}

// A failing confirmation is the command's error; nothing moves or starts.
func TestLaunchConfirmErrorStopsTheMove(t *testing.T) {
	m := newMoveFixture(t)
	m.pick = func([]tui.Item) (int, error) { return 1, nil }
	m.confirm = func(string, string) (bool, error) { return false, errors.New("read from terminal: input/output error") }
	got := m.run("use", "alpha")
	assertCode(t, got, cli.ExitError)
	if !strings.Contains(got.stderr, "read from terminal: input/output error") || len(m.execs) != 0 {
		t.Errorf("stderr = %q execs %+v", got.stderr, m.execs)
	}
	m.assertUntouched(t)
}

// New session and sessions already in the target never ask (the fixture
// fails the test if a confirmation is shown).
func TestLaunchNoConfirmWithoutAMove(t *testing.T) {
	for _, row := range []int{0, 2} {
		m := newMoveFixture(t)
		m.pick = func([]tui.Item) (int, error) { return row, nil }
		got := m.run("use", "alpha")
		assertCode(t, got, 0)
		if len(m.confirms) != 0 || len(m.execs) != 1 {
			t.Errorf("row %d: confirms %+v execs %+v", row, m.confirms, m.execs)
		}
		m.assertUntouched(t)
	}
}

// Two dirs logged into one account share a nickname, so the confirmation
// names them by their shortened paths, as the move report does.
func TestLaunchConfirmSameAccountUsesPaths(t *testing.T) {
	m := newMoveFixture(t)
	twin := m.addConfig("julienning4", email1)
	m.save()
	m.writeSession(twin, sidC, "Twin work", "p", time.Hour) // newest: row 1
	m.pick = func([]tui.Item) (int, error) { return 1, nil }
	m.confirm = acceptMoves
	got := m.run("use", "julienning3")
	assertCode(t, got, 0)
	want := confirmCall{
		`Move "Twin work" from ~/.claude-julienning4 to ~/.claude-julienning3?`,
		"Its transcript, checkpoints, task list and session environment move to ~/.claude-julienning3; ~/.claude-julienning4 will no longer have it. Project memory is merged, never overwritten.",
	}
	if len(m.confirms) != 1 || m.confirms[0] != want {
		t.Fatalf("confirms = %+v\nwant %+v", m.confirms, want)
	}
	if !strings.Contains(got.stdout, `Moved "Twin work" from ~/.claude-julienning4 to ~/.claude-julienning3.`) {
		t.Errorf("stdout = %q", got.stdout)
	}
}
