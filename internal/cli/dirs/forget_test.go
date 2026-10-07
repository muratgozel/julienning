package dirs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

const forgetQuestion = "Also delete ~/.claude-julienning4 and everything in it (its sessions and login)? [y/N] "

// forgetFixture registers ~/.claude-julienning4 as julienning4, logged in
// as claude3@example.com, which the team shares as "claude3".
func forgetFixture(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t)
	dir := h.loggedIn(".claude-julienning4", "claude3@example.com")
	h.writeFile(filepath.Join(dir, "projects", "p", "s.jsonl"), "{}\n")
	h.initConfig(false, config.ConfigDir{Name: "julienning4", Dir: dir})
	h.cacheNicks("claude3@example.com=claude3")
	return h, dir
}

func assertExists(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("%s should still exist: %v", dir, err)
	}
}

func assertGone(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("%s should be deleted: %v", dir, err)
	}
}

func assertForgotten(t *testing.T, h *harness, name string) {
	t.Helper()
	if _, ok := h.config().Find(name); ok {
		t.Fatalf("%s is still registered", name)
	}
}

func TestForgetByTarget(t *testing.T) {
	for _, target := range []string{"claude3", "CLAUDE3", "claude3@example.com", "julienning4"} {
		t.Run(target, func(t *testing.T) {
			h, dir := forgetFixture(t)
			out := h.mustRun(runForget, target, "--keep")
			contains(t, out, "Forgot claude3 (claude3@example.com, ~/.claude-julienning4).\n")
			contains(t, out, "Kept ~/.claude-julienning4.\n")
			assertForgotten(t, h, "julienning4")
			assertExists(t, dir)
		})
	}
}

func TestForgetNotLoggedInLine(t *testing.T) {
	h := newHarness(t)
	dir := h.mkdir(".claude-julienning4")
	h.initConfig(false, config.ConfigDir{Name: "julienning4", Dir: dir})
	out := h.mustRun(runForget, "--keep", "julienning4")
	contains(t, out, "Forgot julienning4 (~/.claude-julienning4, not logged in).\n")
	notContains(t, out, "stays shared")
}

func TestForgetRefusesAnAccountInSeveralDirs(t *testing.T) {
	h := newHarness(t)
	d2 := h.loggedIn(".claude-julienning2", "claude3@example.com")
	d4 := h.loggedIn(".claude-julienning4", "claude3@example.com")
	h.initConfig(false, config.ConfigDir{Name: "julienning2", Dir: d2}, config.ConfigDir{Name: "julienning4", Dir: d4})
	h.cacheNicks("claude3@example.com=claude3")

	for _, target := range []string{"claude3", "claude3@example.com"} {
		err := h.run(runForget, target, "--delete")
		want := "claude3 (claude3@example.com) is logged into more than one config dir here: julienning2 (~/.claude-julienning2), julienning4 (~/.claude-julienning4); pass the config name of the one to forget"
		if err == nil || err.Error() != want {
			t.Fatalf("%s: got %v\nwant %s", target, err, want)
		}
	}
	if got := h.names(); len(got) != 2 {
		t.Fatalf("registrations changed: %v", got)
	}
	assertExists(t, d2)
	assertExists(t, d4)

	out := h.mustRun(runForget, "julienning4", "--keep")
	contains(t, out, "Forgot claude3 (claude3@example.com, ~/.claude-julienning4).")
	if got := h.names(); len(got) != 1 || got[0] != "julienning2" {
		t.Fatalf("names = %v", got)
	}
}

// A config name that a nickname shadows must still pick its dir when the
// nickname alone is ambiguous, or that dir could never be forgotten by name.
func TestForgetConfigNameShadowedByAmbiguousNickname(t *testing.T) {
	h := newHarness(t)
	d1 := h.loggedIn(".claude-one", "claude3@example.com")
	d2 := h.loggedIn(".claude-two", "claude3@example.com")
	h.initConfig(false, config.ConfigDir{Name: "claude3", Dir: d1}, config.ConfigDir{Name: "other", Dir: d2})
	h.cacheNicks("claude3@example.com=claude3")
	out := h.mustRun(runForget, "claude3", "--keep")
	contains(t, out, "(claude3@example.com, ~/.claude-one).")
	if got := h.names(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("names = %v", got)
	}
}

func TestForgetNotLocal(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "julienning1", Dir: h.mkdir(".claude-julienning1")})
	h.cacheNicks("claude3@example.com=claude3")
	for target, want := range map[string]string{
		"claude3":            "claude3 (claude3@example.com) is not logged in on this machine; nothing to forget",
		"stray@example.com":  "stray@example.com is not logged in on this machine; nothing to forget",
		"Stray@Example.com ": "stray@example.com is not logged in on this machine; nothing to forget",
	} {
		err := h.run(runForget, target)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", target, err, want)
		}
	}
	if got := h.names(); len(got) != 1 {
		t.Fatalf("registrations changed: %v", got)
	}
}

func TestForgetFlags(t *testing.T) {
	h, _ := forgetFixture(t)
	var ue *usageErr
	if !asUsage(h.run(runForget, "claude3", "--delete", "--keep"), &ue) || !strings.Contains(ue.Msg, "not both") {
		t.Errorf("--delete --keep: got %v", ue)
	}
	if !asUsage(h.run(runForget, "claude3", "julienning4"), &ue) {
		t.Error("two targets: want usage error")
	}
	if !asUsage(h.run(runForget, "--yes", "claude3"), &ue) {
		t.Error("--yes: want usage error")
	}
	if _, ok := h.config().Find("julienning4"); !ok {
		t.Fatal("a usage error changed the registration")
	}
}

func TestForgetDeleteFlag(t *testing.T) {
	h, dir := forgetFixture(t)
	out := h.mustRun(runForget, "claude3", "--delete")
	contains(t, out, "Deleted ~/.claude-julienning4.\n")
	notContains(t, out, "[y/N]")
	assertForgotten(t, h, "julienning4")
	assertGone(t, dir)
}

func TestForgetKeepFlagInATerminal(t *testing.T) {
	h, dir := forgetFixture(t)
	h.interactive = true
	h.stdin = strings.NewReader("y\n")
	out := h.mustRun(runForget, "--keep", "claude3")
	notContains(t, out, "[y/N]")
	contains(t, out, "Kept ~/.claude-julienning4.\n")
	assertExists(t, dir)
}

func TestForgetAsks(t *testing.T) {
	for _, tc := range []struct {
		answer  string
		deleted bool
	}{
		{"y\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false}, // EOF takes the default
		{"maybe\n", false},
	} {
		t.Run(strings.TrimSpace(tc.answer), func(t *testing.T) {
			h, dir := forgetFixture(t)
			h.interactive = true
			h.stdin = strings.NewReader(tc.answer)
			out := h.mustRun(runForget, "claude3")
			contains(t, out, forgetQuestion)
			// The question comes after the forget is reported.
			if strings.Index(out, "Forgot claude3") > strings.Index(out, forgetQuestion) {
				t.Errorf("asked before reporting the forget:\n%s", out)
			}
			assertForgotten(t, h, "julienning4")
			if tc.deleted {
				contains(t, out, "Deleted ~/.claude-julienning4.\n")
				assertGone(t, dir)
			} else {
				contains(t, out, "Kept ~/.claude-julienning4.\n")
				assertExists(t, dir)
			}
		})
	}
}

func TestForgetNonInteractiveKeeps(t *testing.T) {
	h, dir := forgetFixture(t)
	h.stdin = strings.NewReader("y\n") // piped input is not a terminal: never read
	out := h.mustRun(runForget, "claude3")
	notContains(t, out, "[y/N]")
	contains(t, out, "Kept ~/.claude-julienning4: no terminal to ask, and --delete was not passed.\n")
	assertForgotten(t, h, "julienning4")
	assertExists(t, dir)
}

func TestForgetSharedAccountNote(t *testing.T) {
	h, _ := forgetFixture(t)
	out := h.mustRun(runForget, "claude3", "--keep")
	contains(t, out, "claude3 stays shared with the team; `julienning unshare claude3` removes it for everyone.\n")

	// A shared account without a nickname is named by its email.
	h2 := newHarness(t)
	dir := h2.loggedIn(".claude-x", "legacy@example.com")
	h2.initConfig(false, config.ConfigDir{Name: "x", Dir: dir})
	h2.cacheEmails("legacy@example.com")
	out = h2.mustRun(runForget, "x", "--keep")
	contains(t, out, "Forgot x (legacy@example.com, ~/.claude-x).")
	contains(t, out, "legacy@example.com stays shared with the team; `julienning unshare legacy@example.com` removes it for everyone.\n")

	// A personal account gets no note.
	h3 := newHarness(t)
	dir = h3.loggedIn(".claude-me", "me@personal.io")
	h3.initConfig(false, config.ConfigDir{Name: "me", Dir: dir})
	out = h3.mustRun(runForget, "me@personal.io", "--keep")
	notContains(t, out, "stays shared")
}

func TestForgetNeverDeletesTheDefaultDir(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude", "me@personal.io")
	h.initConfig(false, config.ConfigDir{Name: "default", Dir: dir})

	h.interactive = true
	h.stdin = strings.NewReader("y\n")
	out := h.mustRun(runForget, "default")
	notContains(t, out, "[y/N]")
	contains(t, out, "Kept ~/.claude: it is Claude's default config dir, which julienning never deletes.\n")
	assertExists(t, dir)

	// An explicit --delete is refused with an error, after the forget.
	h.initConfig(false, config.ConfigDir{Name: "default", Dir: dir})
	err := h.run(runForget, "default", "--delete")
	if err == nil || err.Error() != "kept ~/.claude: it is Claude's default config dir, which julienning never deletes" {
		t.Fatalf("got %v", err)
	}
	contains(t, h.stdout.String(), "Forgot default (me@personal.io, ~/.claude).")
	assertForgotten(t, h, "default")
	assertExists(t, dir)
}

func TestForgetRefusesToDeleteADirWithRunningClaude(t *testing.T) {
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return true }
	t.Cleanup(func() { livesess.Alive = prev })

	h, dir := forgetFixture(t)
	// A daemon entry counts too: any live process blocks deletion.
	h.writeFile(filepath.Join(dir, "sessions", "4242.json"), `{"pid":4242,"sessionId":"s1","kind":"daemon"}`)
	h.interactive = true
	h.stdin = strings.NewReader("y\n")
	out := h.mustRun(runForget, "claude3")
	notContains(t, out, "[y/N]")
	contains(t, out, "Kept ~/.claude-julienning4: Claude is running in it (pid 4242); quit Claude there, then delete the dir yourself if you still want it gone.\n")
	assertForgotten(t, h, "julienning4")
	assertExists(t, dir)

	h.initConfig(false, config.ConfigDir{Name: "julienning4", Dir: dir})
	err := h.run(runForget, "julienning4", "--delete")
	if err == nil || !strings.HasPrefix(err.Error(), "kept ~/.claude-julienning4: Claude is running in it (pid 4242)") {
		t.Fatalf("got %v", err)
	}
	assertExists(t, dir)
}

func TestForgetDeletesWhenSessionsAreGone(t *testing.T) {
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return false }
	t.Cleanup(func() { livesess.Alive = prev })

	h, dir := forgetFixture(t)
	h.writeFile(filepath.Join(dir, "sessions", "4242.json"), `{"pid":4242,"sessionId":"s1"}`)
	out := h.mustRun(runForget, "claude3", "--delete")
	contains(t, out, "Deleted ~/.claude-julienning4.\n")
	assertGone(t, dir)
}

func TestForgetRefusesToDeleteHome(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "home", Dir: h.home})
	err := h.run(runForget, "home", "--delete")
	if err == nil || err.Error() != "kept ~: it is /, your home directory or one of its parents" {
		t.Fatalf("got %v", err)
	}
	assertExists(t, h.home)
	assertExists(t, h.jl)

	parent := filepath.Dir(h.home)
	h.initConfig(false, config.ConfigDir{Name: "parent", Dir: parent})
	h.interactive = true
	h.stdin = strings.NewReader("y\n")
	out := h.mustRun(runForget, "parent")
	notContains(t, out, "[y/N]")
	contains(t, out, "Kept "+parent+": it is /, your home directory or one of its parents.\n")
	assertExists(t, h.home)
}

// Deleting a dir whose settings.json another registered dir links to would
// break that dir.
func TestForgetRefusesToDeleteADirAnotherDirUses(t *testing.T) {
	h := newHarness(t)
	original := "{\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"}\n}\n"
	a, b := sharedSettingsDirs(t, h, original)
	err := h.run(runForget, "julienning1", "--delete")
	if err == nil || err.Error() != "kept ~/.claude-a: julienning2 still uses files in it" {
		t.Fatalf("got %v", err)
	}
	assertExists(t, a)
	assertPatched(t, b)
}

func TestForgetMissingDir(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false, config.ConfigDir{Name: "gone", Dir: filepath.Join(h.home, ".claude-gone")})
	h.interactive = true
	out := h.mustRun(runForget, "gone", "--delete")
	contains(t, out, "Forgot gone (~/.claude-gone, not logged in).\n")
	contains(t, out, "Nothing to delete: ~/.claude-gone does not exist.\n")
	assertForgotten(t, h, "gone")
}

func TestForgetDeletesOnlyTheLinkOfASymlinkedDir(t *testing.T) {
	h := newHarness(t)
	target := h.loggedIn("dotfiles/claude", "me@personal.io")
	link := filepath.Join(h.home, ".claude-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	h.initConfig(false, config.ConfigDir{Name: "link", Dir: link})
	out := h.mustRun(runForget, "link", "--delete")
	contains(t, out, "Deleted the link ~/.claude-link; the directory it points to was kept.\n")
	assertGone(t, link)
	assertExists(t, filepath.Join(target, ".claude.json"))
}
