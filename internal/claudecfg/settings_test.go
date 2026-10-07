package claudecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const exe = "/usr/local/bin/julienning"

// isolate points $JULIENNING_HOME (patches.json) and $HOME at temp dirs.
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("JULIENNING_HOME", filepath.Join(root, "jl"))
	return root
}

func TestPatchKeepsMode(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	p := filepath.Join(dir, SettingsFile)
	write(t, dir, `{}`)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// A settings.json symlinked into a dotfiles repo (or shared between config
// dirs) must survive the patch: renaming onto the link path replaced it.
func TestPatchFollowsSymlink(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	shared := filepath.Join(root, "shared.json")
	if err := os.WriteFile(shared, []byte(`{"model":"opus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, SettingsFile)
	if err := os.Symlink(shared, link); err != nil {
		t.Fatal(err)
	}

	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	body := readFile(t, shared)
	for _, want := range []string{`"model":"opus"`, `"command":"` + exe + ` statusline"`} {
		if !strings.Contains(body, want) {
			t.Errorf("shared target missing %q:\n%s", want, body)
		}
	}
	tfi, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if tfi.Mode().Perm() != 0o600 {
		t.Fatalf("target mode = %v, want 0600", tfi.Mode().Perm())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
		t.Fatalf("config dir = %v (%v), want only the symlink", entries, err)
	}

	// Unpatch goes through the link as well and restores the original bytes.
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, shared); got != `{"model":"opus"}` {
		t.Fatalf("after unpatch: %q", got)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("unpatch replaced the symlink")
	}
}

// A dangling symlink still writes through, so the link becomes valid instead
// of being clobbered.
func TestPatchDanglingSymlink(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	target := filepath.Join(root, "shared.json")
	dir := filepath.Join(root, "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, SettingsFile)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readFile(t, target), exe+" statusline") {
		t.Fatal("dangling symlink target was not written")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink was replaced (%v, %v)", fi, err)
	}
}

// Editors that prepend a UTF-8 BOM used to make the patch fail with
// "invalid character 'ï'".
func TestPatchStripsBOM(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, "\ufeff"+`{"model":"opus"}`)

	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatalf("BOM rejected: %v", err)
	}
	if res.Action != Added {
		t.Fatalf("got %v, want added", res.Action)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	if strings.HasPrefix(body, "\ufeff") {
		t.Fatal("BOM was written back")
	}
	if !strings.HasPrefix(body, `{"model":"opus",`) {
		t.Errorf("existing member changed:\n%s", body)
	}
}

func TestReadSettingsStripsBOM(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "\ufeff"+`{"model":"opus"}`)
	got, err := ReadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"model":"opus"}` {
		t.Fatalf("got %q", got)
	}
}

func TestReadSettingsMissing(t *testing.T) {
	got, err := ReadSettings(t.TempDir())
	if err != nil || string(got) != "{}\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestWriteSettingsAddsNewline(t *testing.T) {
	dir := t.TempDir()
	if err := WriteSettings(dir, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != "{\"a\":1}\n" {
		t.Fatalf("got %q", got)
	}
}

func write(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, SettingsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
