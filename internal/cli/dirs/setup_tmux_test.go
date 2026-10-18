package dirs

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/launch"
	"github.com/muratgozel/julienning/internal/shell"
)

// The rc line calls julienning by its stable path; a line from an older
// julienning (or another install path) is rewritten where it is, never
// moved, and a build run from the repo writes the PATH form.
func TestSetupRewritesTheHookLineInPlace(t *testing.T) {
	h := newHarness(t)
	rc := filepath.Join(h.home, ".bashrc")
	t.Setenv("SHELL", "/bin/bash")
	old := `command -v julienning >/dev/null 2>&1 && eval "$(julienning shell-init bash)"  # julienning-shell-hook`
	h.writeFile(filepath.Join(h.home, ".bash_profile"), "export A=1\n"+old+"\nclaude() { echo provisioned; }\n")
	h.writeFile(rc, "export A=1\n"+old+"\nclaude() { echo provisioned; }\n")
	target := rcPath(h.home, shell.Bash)

	out := h.mustRun(runSetup)
	contains(t, out, "Shell:    "+shortenHome(target)+" hook line updated")
	want := "export A=1\n[ -x '/opt/bin/julienning' ] && eval \"$('/opt/bin/julienning' shell-init bash)\"  # julienning-shell-hook\nclaude() { echo provisioned; }\n"
	if got := h.read(target); got != want {
		t.Fatalf("rc =\n%q\nwant\n%q", got, want)
	}
	out = h.mustRun(runSetup)
	contains(t, out, "unchanged (hook already present)")

	h.stable = false
	out = h.mustRun(runSetup)
	contains(t, out, "hook line updated")
	if got := h.read(target); !strings.Contains(got, "\n"+old+"\n") {
		t.Fatalf("unstable build: rc =\n%s", got)
	}
}

func TestSetupHookLineQuotesThePath(t *testing.T) {
	if got, want := hookLine("zsh", "/home/o'neil/.local/bin/julienning"),
		`[ -x '/home/o'\''neil/.local/bin/julienning' ] && eval "$('/home/o'\''neil/.local/bin/julienning' shell-init zsh)"  # julienning-shell-hook`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A claude function before julienning's line is replaced by the wrapper
// (noted); one after it replaces the wrapper (warned).
func TestSetupReportsClaudeFunctions(t *testing.T) {
	h := newHarness(t)
	rc := filepath.Join(h.home, ".zshrc")
	h.writeFile(rc, "claude() {\n  tmux new -As claude\n}\nfunction claude-alpha { :; }\n# claude() { old; }\n")
	out := h.mustRun(runSetup)
	contains(t, out, "note: ~/.zshrc:1 defines a claude function before julienning's line; julienning's definition wins\n")
	notContains(t, out, "~/.zshrc:4")
	notContains(t, h.stderr.String(), "claude function")

	h.writeFile(rc, h.read(rc)+"function claude {\n  echo later\n}\nclaude () { :; }\n")
	h.mustRun(runSetup)
	for _, n := range []string{"7", "10"} {
		contains(t, h.stderr.String(), "warning: ~/.zshrc:"+n+" defines a claude function after julienning's line, so plain claude skips julienning (selection, pins, tmux mode); move the line ending in # julienning-shell-hook below it")
	}
}

func TestClaudeFunctionLines(t *testing.T) {
	src := strings.Join([]string{
		"claude() {", "  claude () { :; }", "function claude {", "function claude() {", "function claude",
		"claude-alpha() {", "function claude-alpha {", "claudex() {", "# claude() {", "echo claude()", "alias claude=x",
	}, "\n")
	got := claudeFunctionLines(src)
	if want := []int{1, 2, 3, 4, 5}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func tmuxConfig(t *testing.T) *config.Tmux {
	t.Helper()
	c, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return c.Tmux
}

func TestSetupTmuxMode(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true) // nothing else to ask in a terminal
	out := h.mustRun(runSetup)
	contains(t, out, "Tmux:     not installed\n")
	if tmuxConfig(t) != nil {
		t.Fatal("decided without tmux")
	}

	// Installed, no terminal: not asked, still undecided.
	h.tmux, h.tmuxErr = launch.Tmux{Path: "/usr/bin/tmux", Version: "3.5a"}, nil
	out = h.mustRun(runSetup)
	contains(t, out, "Tmux:     off (enable with: julienning setup --tmux claude)\n")
	if tmuxConfig(t) != nil {
		t.Fatal("decided without asking")
	}

	// A terminal on this machine: asked once, default no.
	h.interactive = true
	h.stdin = strings.NewReader("\n")
	out = h.mustRun(runSetup)
	contains(t, out, `Run claude inside tmux session "claude"? Sessions keep running when your connection drops. [y/N] `)
	contains(t, out, "Tmux:     off (enable with: julienning setup --tmux claude)\n")
	if got := tmuxConfig(t); got == nil || got.Enabled {
		t.Fatalf("declined: %+v", got)
	}
	h.stdin = strings.NewReader("y\n")
	out = h.mustRun(runSetup)
	notContains(t, out, "Run claude inside tmux")

	// The flags change it.
	h.interactive = false
	out = h.mustRun(runSetup, "--tmux", "claude")
	contains(t, out, `Tmux:     claude runs in tmux session "claude" (enabled)`)
	out = h.mustRun(runSetup)
	contains(t, out, `Tmux:     claude runs in tmux session "claude" (unchanged)`)
	out = h.mustRun(runSetup, "--tmux", "work_2")
	contains(t, out, `Tmux:     claude runs in tmux session "work_2" (updated)`)
	if got := tmuxConfig(t); got == nil || !got.Enabled || got.Session != "work_2" {
		t.Fatalf("--tmux: %+v", got)
	}
	out = h.mustRun(runSetup, "--no-tmux")
	contains(t, out, "Tmux:     off (enable with: julienning setup --tmux claude)\n")
	if got := tmuxConfig(t); got == nil || got.Enabled {
		t.Fatalf("--no-tmux: %+v", got)
	}

	// tmux gone while the mode is on.
	h.mustRun(runSetup, "--tmux", "claude")
	h.tmuxErr = launch.ErrTmuxNotFound
	out = h.mustRun(runSetup)
	contains(t, out, `Tmux:     on (session "claude"), but tmux is not installed; claude runs in place until it is fixed`)
	h.tmuxErr = &launch.TmuxTooOldError{Path: "/usr/bin/tmux", Version: "1.8"}
	h.mustRun(runSetup, "--no-tmux")
	out = h.mustRun(runSetup)
	contains(t, out, "Tmux:     tmux 1.8 is too old (tmux mode needs 2.1 or newer)\n")
}

// Over ssh the question defaults to yes: a dropped connection would end the
// session.
func TestSetupTmuxQuestionOverSSH(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	t.Setenv("SSH_CONNECTION", "100.64.0.1 50000 100.64.0.7 22")
	h.tmux, h.tmuxErr = launch.Tmux{Path: "/usr/bin/tmux", Version: "3.5a"}, nil
	h.interactive = true
	h.stdin = strings.NewReader("\n")
	out := h.mustRun(runSetup)
	contains(t, out, `[Y/n] `)
	contains(t, out, `Tmux:     claude runs in tmux session "claude" (enabled)`)
}

func TestSetupTmuxFlagErrors(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	for _, args := range [][]string{{"--tmux", "a", "--no-tmux"}, {"--tmux", "a:b"}, {"--tmux", strings.Repeat("a", 33)}} {
		if err := h.run(runSetup, args...); !asUsage(err, &ue) {
			t.Errorf("%q: want a usage error, got %v", args, err)
		}
	}
	err := h.run(runSetup, "--tmux", "claude")
	if err == nil || !strings.Contains(err.Error(), "--tmux claude: tmux is not installed (install tmux 2.1 or newer, or leave out --tmux)") {
		t.Fatalf("got %v", err)
	}
	if _, err := config.Load(); !errors.Is(err, config.ErrNotSetup) {
		t.Errorf("setup changed something before refusing: %v", err)
	}
}

func TestYesNo(t *testing.T) {
	for _, c := range []struct {
		in       string
		def, out bool
	}{{"", true, true}, {"", false, false}, {"Y", false, true}, {"yes", false, true}, {"n", true, false}, {"NO", true, false}, {"x", true, true}} {
		if got := yesNo(c.in, c.def); got != c.out {
			t.Errorf("yesNo(%q, %v) = %v", c.in, c.def, got)
		}
	}
}
