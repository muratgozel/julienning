package dirs

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// --- setup: rc file handling ---

func TestSetupWarnsAboutClaudeAlias(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	rc := filepath.Join(h.home, ".zshrc")
	h.writeFile(rc, "export A=1\nalias claude=\"$HOME/.claude/local/claude\"\n# alias claude=old\nalias claude-x='claude'\n")
	h.mustRun(runSetup)
	contains(t, h.stderr.String(), "warning: ~/.zshrc:2 defines `alias claude=…`")
	if n := strings.Count(h.stderr.String(), "alias claude="); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, h.stderr.String())
	}
	contains(t, h.read(rc), rcMarker)
}

// rc files are often symlinks into a dotfiles repo: the link must survive,
// the real file is rewritten atomically, and its mode is kept.
func TestSetupAndUninstallRewriteSymlinkedRCInPlace(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	dotfiles := h.mkdir("dotfiles")
	real := filepath.Join(dotfiles, "zshrc")
	h.writeFile(real, "export A=1")
	if err := os.Chmod(real, 0o600); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(h.home, ".zshrc")
	if err := os.Symlink(filepath.Join("dotfiles", "zshrc"), rc); err != nil {
		t.Fatal(err)
	}

	h.mustRun(runSetup)
	assertLinkAndMode := func() {
		t.Helper()
		fi, err := os.Lstat(rc)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("~/.zshrc is no longer a symlink: %v %v", fi.Mode(), err)
		}
		fi, err = os.Stat(real)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, %v; want 0600", fi.Mode().Perm(), err)
		}
		entries, _ := os.ReadDir(dotfiles)
		if len(entries) != 1 {
			t.Fatalf("temp files left behind: %v", entries)
		}
	}
	assertLinkAndMode()
	if got := h.read(real); got != "export A=1\n"+hookLine("zsh")+"\n" {
		t.Fatalf("rc = %q", got)
	}

	h.mustRun(runUninstall)
	assertLinkAndMode()
	if got := h.read(real); got != "export A=1\n" {
		t.Fatalf("rc after uninstall = %q", got)
	}
}

func TestSetupCreatesMissingRCThroughDanglingSymlink(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.mkdir("dotfiles")
	rc := filepath.Join(h.home, ".zshrc")
	if err := os.Symlink(filepath.Join(h.home, "dotfiles", "zshrc"), rc); err != nil {
		t.Fatal(err)
	}
	h.mustRun(runSetup)
	if fi, err := os.Lstat(rc); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
	if got := h.read(filepath.Join(h.home, "dotfiles", "zshrc")); got != hookLine("zsh")+"\n" {
		t.Fatalf("rc = %q", got)
	}
}

// --- share/unshare: KV listings lag writes ---

func TestShareAppliesItsChangeOverALaggingListing(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	h.allow("old@team.io") // the listing does not show the new share yet
	h.mustRun(runShare, "new@team.io")
	cache, _ := sharedcache.Load()
	if !reflect.DeepEqual(cache.Emails, []string{"new@team.io", "old@team.io"}) {
		t.Fatalf("cache = %v", cache.Emails)
	}
	if !cache.FetchedAt.Equal(fixedNow) {
		t.Fatalf("fetched_at = %v, want the refresh time", cache.FetchedAt)
	}

	h.allow("new@team.io", "old@team.io") // still lists the email just unshared
	h.mustRun(runUnshare, "--yes", "new@team.io")
	cache, _ = sharedcache.Load()
	if !reflect.DeepEqual(cache.Emails, []string{"old@team.io"}) {
		t.Fatalf("cache after unshare = %v", cache.Emails)
	}
}

// A failed refresh updates the cached list but must not make it look fresh.
func TestShareFallbackKeepsFetchedAt(t *testing.T) {
	h := newHarness(t)
	h.initConfig(true)
	old := fixedNow.Add(-3 * time.Hour)
	if _, err := sharedcache.Save([]string{"old@team.io"}, old); err != nil {
		t.Fatal(err)
	}
	h.fake.ListErr = os.ErrDeadlineExceeded
	h.mustRun(runShare, "x@team.io")
	cache, _ := sharedcache.Load()
	if !cache.Contains("x@team.io") || !cache.FetchedAt.Equal(old) {
		t.Fatalf("cache = %+v", cache)
	}
}

func TestSetupSharedEmailSurvivesALaggingListing(t *testing.T) {
	h := newHarness(t)
	dir := h.loggedIn(".claude-a", "a@x.io")
	h.allow() // the Worker lists nothing yet
	out := h.mustRun(runSetup, remoteFlags("--no-rc", "--share", "a@x.io")...)
	contains(t, row(t, out, "~/.claude-a"), "~/.claude-a  a  a@x.io  shared (registered as julienning1)")
	cache, _ := sharedcache.Load()
	if !cache.Contains("a@x.io") || cache.Nickname("a@x.io") != "a" || !cache.FetchedAt.Equal(fixedNow) {
		t.Fatalf("cache = %+v", cache)
	}
	assertPatched(t, dir)
}

// --- forget / uninstall with a shared settings.json ---

// sharedSettingsDirs registers two dirs whose settings.json is one file:
// a as julienning1 and b as julienning2.
func sharedSettingsDirs(t *testing.T, h *harness, original string) (a, b string) {
	t.Helper()
	a = h.loggedIn(".claude-a", "a@x.io")
	b = h.loggedIn(".claude-b", "b@x.io")
	h.writeFile(filepath.Join(a, "settings.json"), original)
	if err := os.Symlink(filepath.Join(a, "settings.json"), filepath.Join(b, "settings.json")); err != nil {
		t.Fatal(err)
	}
	h.allow("a@x.io", "b@x.io")
	h.mustRun(runSetup, remoteFlags("--no-rc")...)
	assertPatched(t, a)
	return a, b
}

func TestForgetSkipsUnpatchOfSharedSettings(t *testing.T) {
	h := newHarness(t)
	original := "{\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"}\n}\n"
	a, b := sharedSettingsDirs(t, h, original)

	out := h.mustRun(runForget, "julienning2")
	contains(t, out, `Forgot "julienning2".`)
	contains(t, out, "settings.json: left as is; it is the same file as the settings.json of julienning1, which still uses julienning.")
	assertPatched(t, a) // a still reports usage and claims
	if _, ok := h.config().Find("julienning2"); ok {
		t.Fatal("b still registered")
	}
	if fi, err := os.Lstat(filepath.Join(b, "settings.json")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("b's settings.json link was touched")
	}

	// The last dir using the file restores it.
	h.mustRun(runForget, "julienning1")
	if got := h.read(filepath.Join(a, "settings.json")); got != original {
		t.Fatalf("not restored:\n%s", got)
	}
}

func TestUninstallUnpatchesSharedSettingsOnce(t *testing.T) {
	h := newHarness(t)
	original := "{\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"}\n}\n"
	a, _ := sharedSettingsDirs(t, h, original)
	out := h.mustRun(runUninstall)
	if n := strings.Count(out, "Settings: "); n != 1 {
		t.Fatalf("%d Settings lines, want 1:\n%s", n, out)
	}
	contains(t, out, "Settings: ~/.claude-a/settings.json: restored previous statusLine")
	if got := h.read(filepath.Join(a, "settings.json")); got != original {
		t.Fatalf("not restored:\n%s", got)
	}
}

// --- new-config --copy-settings-from ---

// Copying a patched settings.json must not carry julienning's entries over:
// forget then leaves the copy exactly as the user's file, with no empty
// "hooks" arrays and the user's own statusLine.
func TestNewConfigCopyStripsJulienningEntries(t *testing.T) {
	h := newHarness(t)
	original := "{\n  \"model\": \"opus\",\n  \"statusLine\": {\"type\": \"command\", \"command\": \"mine.sh\"}\n}\n"
	src := h.loggedIn(".claude-src", "a@x.io")
	h.writeFile(filepath.Join(src, "settings.json"), original)
	h.allow("a@x.io")
	h.mustRun(runSetup, remoteFlags("--no-rc")...)
	assertPatched(t, src)

	h.mustRun(runNewConfig, "--name", "copy", "--copy-settings-from", "julienning1")
	dir := filepath.Join(h.home, ".claude-copy")
	assertPatched(t, dir)

	out := h.mustRun(runForget, "copy")
	contains(t, out, "restored previous statusLine")
	got := h.read(filepath.Join(dir, "settings.json"))
	if got != original {
		t.Fatalf("copy after forget:\n%s\nwant\n%s", got, original)
	}
	assertPatched(t, src) // the source is untouched
}

func TestNewConfigCopyOfJulienningOnlySettingsIsNotKept(t *testing.T) {
	h := newHarness(t)
	src := h.loggedIn(".claude-src", "a@x.io") // no settings.json: setup creates it
	h.allow("a@x.io")
	h.mustRun(runSetup, remoteFlags("--no-rc")...)
	h.mustRun(runNewConfig, "--name", "copy", "--copy-settings-from", "julienning1")
	dir := filepath.Join(h.home, ".claude-copy")
	assertPatched(t, dir)
	h.mustRun(runForget, "copy")
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings.json julienning created is still there: %v", err)
	}
	assertPatched(t, src)
}

// --- adopt: stable command warning ---

func TestAdoptWarnsOnceWhenCommandIsNotInstalled(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	h.stable = false
	dir := h.mkdir(".claude-x")
	h.mustRun(runAdopt, dir)
	if n := strings.Count(h.stderr.String(), "warning: settings.json will run "+fakeExe); n != 1 {
		t.Fatalf("warnings: %q", h.stderr.String())
	}
	assertPatched(t, dir)
}

// --- uninstall --purge ---

func TestUninstallPurgeKeepsStateWhenUnpatchFails(t *testing.T) {
	h := newHarness(t)
	custom, _ := installed(t, h)
	versions, link := installBinary(t, h)
	h.writeFile(filepath.Join(custom, "settings.json"), "[]")

	err := h.run(runUninstall, "--purge", "--yes")
	if err == nil {
		t.Fatal("want an error")
	}
	contains(t, h.stderr.String(), "not purging: 1 settings.json could not be restored")
	for _, p := range []string{filepath.Join(h.jl, "config.json"), filepath.Join(h.jl, claudecfg.PatchesFile), filepath.Join(versions, "0.3.0"), link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was purged: %v", p, err)
		}
	}
	notContains(t, h.stdout.String(), "Purged:")
}

func TestUninstallPurgeKeepsStateWhenConfigIsUnreadable(t *testing.T) {
	h := newHarness(t)
	installed(t, h)
	versions, link := installBinary(t, h)
	h.writeFile(filepath.Join(h.jl, "config.json"), "{nope")

	err := h.run(runUninstall, "--purge", "--yes")
	if err == nil {
		t.Fatal("want an error")
	}
	contains(t, h.stderr.String(), "not purging: ~/.julienning/config.json could not be read")
	for _, p := range []string{filepath.Join(h.jl, "config.json"), filepath.Join(h.jl, claudecfg.PatchesFile), filepath.Join(versions, "0.3.0"), link} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was purged: %v", p, err)
		}
	}
}

// installBinary lays out the install: a version file and the bin symlink.
func installBinary(t *testing.T, h *harness) (versions, link string) {
	t.Helper()
	versions = h.mkdir(".local/share/julienning/versions")
	h.writeFile(filepath.Join(versions, "0.3.0"), "bin")
	link = filepath.Join(h.mkdir(".local/bin"), "julienning")
	if err := os.Symlink(filepath.Join(versions, "0.3.0"), link); err != nil {
		t.Fatal(err)
	}
	return versions, link
}

// Everything julienning writes is purged, including leftovers, and the
// directories go once empty.
func TestUninstallPurgeRemovesEveryStateFile(t *testing.T) {
	h := newHarness(t)
	installed(t, h)
	versions, link := installBinary(t, h)
	for _, name := range []string{"errors.log", "errors.log.trim", "update-check.json", "claim-sync.lock", ".claims-reconciled", ".last-STATUS", ".last-X-1a2b3c4d", ".config.json.12345", "sent/1a2b3c4d.json", "sent/1a2b3c4d.inflight", "sent/.tmp-99", "sent/a@b.io.json"} {
		h.writeFile(filepath.Join(h.jl, name), "x")
	}
	for _, name := range []string{"dev", "0.2.9-rc.1", ".download-123.tar.gz", ".0.3.1.4567.tmp"} {
		h.writeFile(filepath.Join(versions, name), "x")
	}

	out := h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Purged:   ~/.julienning\n")
	contains(t, out, "Purged:   ~/.local/share/julienning/versions\n")
	contains(t, out, "Purged:   ~/.local/bin/julienning\n")
	for _, p := range []string{h.jl, versions, filepath.Dir(versions), link} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := os.Stat(filepath.Join(h.home, ".local", "bin")); err != nil {
		t.Fatal("the bin dir itself must stay")
	}
}

// A user-chosen JULIENNING_HOME / JULIENNING_VERSIONS_DIR may hold the
// user's own files: only julienning's go, the dirs stay, and a bin entry that
// is not julienning's symlink is left alone.
func TestUninstallPurgeOnlyDeletesJulienningFiles(t *testing.T) {
	h := newHarness(t)
	state := h.mkdir("stuff")
	t.Setenv("JULIENNING_HOME", state)
	h.jl = state
	versions := h.mkdir("tools/versions")
	t.Setenv("JULIENNING_VERSIONS_DIR", versions)
	installed(t, h)
	h.writeFile(filepath.Join(state, "notes.txt"), "mine")
	h.writeFile(filepath.Join(state, "sent", "letter.txt"), "mine")
	h.writeFile(filepath.Join(state, "sent", "1a2b3c4d.json"), "{}")
	h.writeFile(filepath.Join(state, "current.bak"), "mine")
	h.writeFile(filepath.Join(versions, "0.3.0"), "bin")
	h.writeFile(filepath.Join(versions, "README"), "mine")
	h.mkdir("tools/versions/1.0.0") // a directory is never a version file
	bin := h.mkdir(".local/bin")
	h.writeFile(filepath.Join(bin, "julienning"), "a hand-installed binary")

	out := h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Purged:   julienning's files in ~/stuff")
	contains(t, out, "Kept ~/stuff: it also holds files julienning did not create (current.bak, notes.txt, sent).")
	contains(t, out, "Kept ~/tools/versions: it also holds files julienning did not create (1.0.0, README).")
	contains(t, out, "Kept ~/.local/bin/julienning: it is not a symlink into ~/tools/versions")
	for _, p := range []string{"stuff/notes.txt", "stuff/current.bak", "stuff/sent/letter.txt", "tools/versions/README", "tools/versions/1.0.0", ".local/bin/julienning"} {
		if _, err := os.Lstat(filepath.Join(h.home, p)); err != nil {
			t.Errorf("%s was deleted: %v", p, err)
		}
	}
	for _, p := range []string{"stuff/config.json", "stuff/patches.json", "stuff/shared.json", "stuff/sent/1a2b3c4d.json", "tools/versions/0.3.0"} {
		if _, err := os.Lstat(filepath.Join(h.home, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived the purge", p)
		}
	}
}

func TestUninstallPurgeKeepsForeignSymlink(t *testing.T) {
	h := newHarness(t)
	h.initConfig(false)
	other := h.mkdir("opt")
	h.writeFile(filepath.Join(other, "julienning"), "bin")
	bin := h.mkdir(".local/bin")
	if err := os.Symlink(filepath.Join(other, "julienning"), filepath.Join(bin, "julienning")); err != nil {
		t.Fatal(err)
	}
	out := h.mustRun(runUninstall, "--purge", "--yes")
	contains(t, out, "Kept ~/.local/bin/julienning: it points to ~/opt/julienning, outside ~/.local/share/julienning/versions.")
	if _, err := os.Lstat(filepath.Join(bin, "julienning")); err != nil {
		t.Fatal("foreign symlink removed")
	}
}

func TestOwnedVersionEntry(t *testing.T) {
	root := t.TempDir()
	entry := func(name string, dir bool) os.DirEntry {
		t.Helper()
		p := filepath.Join(root, name)
		var err error
		if dir {
			err = os.Mkdir(p, 0o700)
		} else {
			err = os.WriteFile(p, nil, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fs.FileInfoToDirEntry(fi)
	}
	cases := map[string]bool{
		"dev": true, "0.3.0": true, "1.2.3-rc.1": true, "1.2.3+build.5": true,
		".download-42.tar.gz": true, ".0.3.0.123.tmp": true, ".dev.9.tmp": true,
		"v0.3.0": false, "0.3": false, "README": false, "julienning": false, ".x.123.tmp": false,
	}
	for name, want := range cases {
		if got := ownedVersionEntry(root, entry(name, false)); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
	if ownedVersionEntry(root, entry("9.9.9", true)) {
		t.Error("a directory counted as a version file")
	}
}

func TestAdoptRefusesHome(t *testing.T) {
	h := newHarness(t)
	err := h.run(runAdopt, h.home)
	if err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Fatalf("adopt home: err=%v", err)
	}
}
