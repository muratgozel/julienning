package claudecfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsValuesReadsOnlyThePresentKeys(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "{\n  \"theme\": \"dark\",\n  \"model\": \"opus[1m]\",\n  \"hooks\": {}\n}\n")
	got, err := SettingsValues(dir, []string{"model", "theme", "editorMode"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["model"]) != `"opus[1m]"` || string(got["theme"]) != `"dark"` {
		t.Fatalf("got %v", got)
	}
	empty := t.TempDir()
	if got, err := SettingsValues(empty, []string{"model"}); err != nil || len(got) != 0 {
		t.Fatalf("missing file: %v, %v", got, err)
	}
	write(t, empty, `[1]`)
	if _, err := SettingsValues(empty, []string{"model"}); err == nil {
		t.Fatal("a non-object settings.json was read")
	}
}

func TestEditSettingsPreservesEveryOtherByte(t *testing.T) {
	dir := t.TempDir()
	before := "{\n    \"model\": \"opus\",\n    \"n\": 1.50,\n    \"theme\": \"light\"\n}\n"
	write(t, dir, before)
	changed, err := EditSettings(dir, []SettingEdit{
		{Key: "theme", Value: json.RawMessage(`"dark"`)},
		{Key: "model"},
		{Key: "editorMode", Value: json.RawMessage(`"vim"`)},
		{Key: "absent"},
	})
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	want := "{\n    \"n\": 1.50,\n    \"theme\": \"dark\",\n    \"editorMode\": \"vim\"\n}\n"
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	changed, err = EditSettings(dir, []SettingEdit{{Key: "theme", Value: json.RawMessage(`"dark"`)}, {Key: "model"}})
	if err != nil || changed {
		t.Fatalf("a no-op edit: changed %v, err %v", changed, err)
	}
}

func TestEditSettingsRefusesBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, `{"model": `)
	if _, err := EditSettings(dir, []SettingEdit{{Key: "model", Value: json.RawMessage(`"opus"`)}}); err == nil {
		t.Fatal("edited invalid JSON")
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != `{"model": ` {
		t.Fatalf("the broken file changed: %q", got)
	}
}

func TestClaudeMDReadAndGuardedWrite(t *testing.T) {
	dir := t.TempDir()
	md, err := ReadClaudeMD(dir)
	if err != nil || md.Exists {
		t.Fatalf("missing file: %+v, %v", md, err)
	}
	if err := WriteClaudeMD(dir, []byte("new\n"), md); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, ClaudeMDFile)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	md, err = ReadClaudeMD(dir)
	if err != nil || !md.Exists || string(md.Content) != "new\n" || md.Mode != 0o600 {
		t.Fatalf("read back: %+v, %v", md, err)
	}
	// Edited after the read: the write must not clobber it.
	if err := os.WriteFile(p, []byte("edited meanwhile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteClaudeMD(dir, []byte("x"), md); !errors.Is(err, ErrClaudeMDChanged) {
		t.Fatalf("err = %v, want ErrClaudeMDChanged", err)
	}
	md, _ = ReadClaudeMD(dir)
	if err := WriteClaudeMD(dir, []byte("synced\n"), md); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
}

// A CLAUDE.md symlinked into a dotfiles repo stays a link; the target gets
// the content.
func TestClaudeMDWriteFollowsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "dotfiles.md")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "cfg")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, ClaudeMDFile)); err != nil {
		t.Fatal(err)
	}
	md, err := ReadClaudeMD(dir)
	if err != nil || string(md.Content) != "old" {
		t.Fatalf("%+v, %v", md, err)
	}
	if err := WriteClaudeMD(dir, []byte("new"), md); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(filepath.Join(dir, ClaudeMDFile)); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced by a file")
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Fatalf("target = %q", b)
	}
}

func TestClaudeMDRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ClaudeMDFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadClaudeMD(dir); err == nil {
		t.Fatal("a directory was read as CLAUDE.md")
	}
}
