package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStableCommandFindsLinkOnPath(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom-bin")
	versions := filepath.Join(root, "share", "julienning", "versions")
	os.MkdirAll(custom, 0o755)
	os.MkdirAll(versions, 0o755)
	exe := filepath.Join(versions, "0.3.0")
	os.WriteFile(exe, []byte("x"), 0o755)
	os.Symlink(exe, filepath.Join(custom, "julienning"))
	t.Setenv("HOME", root)
	t.Setenv(EnvBinDir, "")
	t.Setenv("PATH", "/usr/bin:"+custom)
	old := Executable
	t.Cleanup(func() { Executable = old })
	Executable = func() (string, error) { return exe, nil }

	got, stable, err := StableCommand()
	if err != nil || !stable || got != filepath.Join(custom, "julienning") {
		t.Fatalf("got %q stable=%v err=%v", got, stable, err)
	}
	if !IsVersionFile(exe) || IsVersionFile(filepath.Join(custom, "julienning")) {
		t.Fatal("IsVersionFile")
	}
}

func TestStableCommandPrefersLinkToRunningBinary(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	versions := filepath.Join(root, "versions")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(versions, 0o755)
	exe := filepath.Join(versions, "1.2.3")
	os.WriteFile(exe, []byte("x"), 0o755)
	t.Setenv(EnvBinDir, bin)
	t.Setenv("HOME", root)
	t.Setenv("PATH", "/usr/bin")
	old := Executable
	t.Cleanup(func() { Executable = old })
	Executable = func() (string, error) { return exe, nil }

	got, stable, err := StableCommand()
	if err != nil || stable || got != exe {
		t.Fatalf("no link: got %q stable=%v err=%v", got, stable, err)
	}
	os.Symlink(exe, filepath.Join(bin, "julienning"))
	got, stable, err = StableCommand()
	if err != nil || !stable || got != filepath.Join(bin, "julienning") {
		t.Fatalf("with link: got %q stable=%v err=%v", got, stable, err)
	}
}
