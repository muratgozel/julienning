package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAccountFilePathDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got, want := AccountFilePath(filepath.Join(home, ".claude")), filepath.Join(home, ".claude.json"); got != want {
		t.Fatalf("default dir: got %s want %s", got, want)
	}
	if got, want := AccountFilePath(filepath.Join(home, ".claude-x")), filepath.Join(home, ".claude-x", ".claude.json"); got != want {
		t.Fatalf("custom dir: got %s want %s", got, want)
	}
	if d, _ := ActiveDir(""); d != filepath.Join(home, ".claude") {
		t.Fatalf("ActiveDir unset: %s", d)
	}
}

func TestReadEmailDefaultDirAndLowercase(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	def := filepath.Join(home, ".claude")
	if err := os.MkdirAll(def, 0o700); err != nil {
		t.Fatal(err)
	}
	// A stale inner file must be ignored for the default dir.
	os.WriteFile(filepath.Join(def, AccountFile), []byte(`{"oauthAccount":{"emailAddress":"stale@example.com"}}`), 0o600)
	os.WriteFile(filepath.Join(home, AccountFile), []byte(`{"oauthAccount":{"emailAddress":"Me@Example.com"}}`), 0o600)
	got, err := ReadEmail(def)
	if err != nil || got != "me@example.com" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := ReadEmail(filepath.Join(home, "missing")); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("missing dir: %v", err)
	}
}
