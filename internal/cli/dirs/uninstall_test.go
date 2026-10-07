package dirs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claims"
	"github.com/muratgozel/julienning/internal/config"
)

const userSettings = "{\n  \"model\": \"opus\",\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"},\n  \"hooks\": {\n    \"SessionStart\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"echo hi\"}]}\n    ]\n  }\n}\n"

// installed sets up a machine the way setup leaves it: two registered dirs
// (one with the user's own settings, one fresh), the rc hook, a held claim
// and a selection.
func installed(t *testing.T, h *harness) (custom, fresh string) {
	t.Helper()
	custom = h.loggedIn(".claude-custom", "a@team.io")
	h.writeFile(filepath.Join(custom, "settings.json"), userSettings)
	fresh = h.loggedIn(".claude-fresh", "b@team.io")
	h.allow("a@team.io", "b@team.io")
	h.writeFile(filepath.Join(h.home, ".zshrc"), "export A=1\n")
	h.mustRun(runSetup, remoteFlags()...)
	if err := claims.SaveHeld(claims.Held{"a@team.io": fixedNow}); err != nil {
		t.Fatal(err)
	}
	if err := config.SetCurrent(config.ConfigDir{Name: "fresh", Dir: fresh}); err != nil {
		t.Fatal(err)
	}
	return custom, fresh
}

func TestUninstallUndoesSetupAndIsSafeTwice(t *testing.T) {
	h := newHarness(t)
	custom, fresh := installed(t, h)
	legacy := `command -v julienning >/dev/null 2>&1 && eval "$(julienning shell-init bash)"  ` + legacyRCMarker
	h.writeFile(filepath.Join(h.home, ".bashrc"), "export B=2\n"+legacy+"\nfoo # julienning\n")

	out := h.mustRun(runUninstall)
	contains(t, out, "Settings: ~/.claude-custom/settings.json: restored previous statusLine, removed SessionStart hook, removed SessionEnd hook")
	contains(t, out, "Settings: ~/.claude-fresh/settings.json: removed statusLine, removed SessionStart hook, removed SessionEnd hook, removed empty hooks, deleted the settings.json julienning created")
	contains(t, out, "Shell:    removed the julienning hook line from ~/.zshrc")
	contains(t, out, "Shell:    removed the julienning hook line from ~/.bashrc")
	contains(t, out, "Claims:   released 1")
	contains(t, out, "Current:  cleared")
	contains(t, out, "Kept ~/.julienning")

	if got := h.read(filepath.Join(custom, "settings.json")); got != userSettings {
		t.Fatalf("custom settings not restored byte-for-byte:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(fresh, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings.json julienning created is still there")
	}
	if got := h.read(filepath.Join(h.home, ".zshrc")); got != "export A=1\n" {
		t.Fatalf(".zshrc = %q", got)
	}
	if got := h.read(filepath.Join(h.home, ".bashrc")); got != "export B=2\nfoo # julienning\n" {
		t.Fatalf(".bashrc = %q (unrelated `# julienning` line must stay)", got)
	}
	var unclaim int
	for _, c := range h.fake.Calls {
		if c.Op == "unclaim" && c.Email == "a@team.io" && c.Identity.Dev == "murat" {
			unclaim++
		}
	}
	if unclaim != 1 {
		t.Fatalf("claims released %d times: %v", unclaim, h.fake.Ops())
	}
	if held, _ := claims.LoadHeld(); len(held) != 0 {
		t.Fatalf("claims.json still holds %v", held)
	}
	if _, ok, _ := h.config().Current(); ok {
		t.Fatal("current not cleared")
	}
	if len(h.config().Configs) != 2 {
		t.Fatal("uninstall without --purge must keep the registrations")
	}

	out = h.mustRun(runUninstall)
	contains(t, out, "Nothing to uninstall.")
	notContains(t, out, "Settings:")
}

func TestUninstallReportsBrokenSettingsAndContinues(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	h.writeFile(filepath.Join(custom, "settings.json"), "[]")
	err := h.run(runUninstall)
	if err == nil || !strings.Contains(err.Error(), "1 problem") {
		t.Fatalf("got %v", err)
	}
	contains(t, h.stderr.String(), "not a JSON object")
	contains(t, h.stdout.String(), "Settings: ~/.claude-fresh/settings.json")
	contains(t, h.stdout.String(), "Shell:    removed")
}

func TestUninstallNotSetUp(t *testing.T) {
	h := newHarness(t)
	h.writeFile(filepath.Join(h.home, ".zshrc"), hookLine("zsh")+"\n")
	out := h.mustRun(runUninstall)
	contains(t, out, "removed the julienning hook line from ~/.zshrc")
	notContains(t, out, "Kept")
}

func TestUninstallPurgeNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	installed(t, h)
	err := h.run(runUninstall, "--purge")
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("got %v", err)
	}
	if h.stdout.Len() != 0 {
		t.Fatalf("changed things before refusing:\n%s", h.stdout.String())
	}
	if _, err := os.Stat(filepath.Join(h.jl, "config.json")); err != nil {
		t.Fatal("config.json touched by a refused purge")
	}

	h.interactive = true
	h.stdin = strings.NewReader("n\n")
	out := h.mustRun(runUninstall, "--purge")
	contains(t, out, "Delete ~/.julienning, ~/.local/share/julienning/versions, ~/.local/bin/julienning? This cannot be undone. [y/N] ")
	contains(t, out, "Cancelled; nothing was changed.")
	if !strings.Contains(h.read(filepath.Join(h.home, ".zshrc")), rcMarker) {
		t.Fatal("a cancelled purge removed the hook")
	}
}

func TestUninstallPurge(t *testing.T) {
	h := newHarness(t)
	installed(t, h)
	versions := h.mkdir(".local/share/julienning/versions")
	h.writeFile(filepath.Join(versions, "0.3.0"), "bin")
	binDir := h.mkdir(".local/bin")
	if err := os.Symlink(filepath.Join(versions, "0.3.0"), filepath.Join(binDir, "julienning")); err != nil {
		t.Fatal(err)
	}
	h.writeFile(filepath.Join(binDir, "other-tool"), "keep")

	out := h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Purged:   ~/.julienning")
	contains(t, out, "Purged:   ~/.local/share/julienning/versions")
	contains(t, out, "Purged:   ~/.local/bin/julienning")
	for _, p := range []string{h.jl, versions, filepath.Join(binDir, "julienning")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	if h.read(filepath.Join(binDir, "other-tool")) != "keep" {
		t.Fatal("purge touched an unrelated file")
	}
	out = h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Nothing to uninstall.")
}

func TestUninstallPurgeRefusesDangerousPaths(t *testing.T) {
	h := newHarness(t)
	t.Setenv("JULIENNING_VERSIONS_DIR", h.home)
	err := h.run(runUninstall, "--purge", "--yes")
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(h.home); err != nil {
		t.Fatal("home gone")
	}
}

func TestSafeToDelete(t *testing.T) {
	home := "/Users/x"
	for p, ok := range map[string]bool{
		"/":                          false,
		"/Users":                     false,
		"/Users/x":                   false,
		"/tmp":                       false,
		"/Users/x/.julienning":       true,
		"/opt/julienning":            true,
		"/Users/x/.local/bin/julien": true,
	} {
		if err := safeToDelete(p, home); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", p, err, ok)
		}
	}
}

func TestUninstallBadArgs(t *testing.T) {
	h := newHarness(t)
	var ue *usageErr
	if !asUsage(h.run(runUninstall, "extra"), &ue) {
		t.Fatal("want usage error")
	}
}
