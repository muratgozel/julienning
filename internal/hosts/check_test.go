package hosts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A path the user types when nothing was found: ~ is this machine's home,
// it may be outside it, and it must be a checkout of the same repository
// with the session's sub folder.
func TestCheckTypedPath(t *testing.T) {
	src := t.TempDir()
	repo := filepath.Join(src, "Code", "shop")
	gitRepo(t, repo, "main", "origin", "git@github.com:team/shop.git")
	if err := os.MkdirAll(filepath.Join(repo, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := ProjectOf(filepath.Join(repo, "web"), src)
	home := t.TempDir()
	o := LocateOptions{Home: home, Machine: "box", RanIn: "the session ran in ~/Code/shop/web here"}

	good := filepath.Join(home, "w", "shop")
	gitRepo(t, good, "feat/x", "origin", "https://github.com/team/shop.git")
	if err := os.MkdirAll(filepath.Join(good, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	loc, err := Check(p, "~/w/shop", o)
	if err != nil || loc.Cwd != filepath.Join(good, "web") || loc.Checkout != good || loc.Branch != "feat/x" || loc.How != "typed" {
		t.Fatalf("good: %+v %v", loc, err)
	}
	outside := filepath.Join(t.TempDir(), "shop")
	gitRepo(t, outside, "main", "origin", "git@github.com:team/shop.git")
	if err := os.MkdirAll(filepath.Join(outside, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if loc, err := Check(p, outside, o); err != nil || loc.Checkout != outside {
		t.Fatalf("outside home: %+v %v", loc, err)
	}

	other := filepath.Join(home, "other")
	gitRepo(t, other, "main", "origin", "git@github.com:team/blog.git")
	for typed, want := range map[string]string{
		"w/shop":           "is not an absolute path",
		"~/missing":        "box has no directory ~/missing",
		"~/other":          "that is a checkout of github.com/team/blog, not github.com/team/shop",
		"~":                "is not a git checkout",
		filepath.Dir(good): "is not a git checkout",
	} {
		if _, err := Check(p, typed, o); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", typed, err, want)
		}
	}
	if err := os.RemoveAll(filepath.Join(good, "web")); err != nil {
		t.Fatal(err)
	}
	var pm *PathMissingError
	if _, err := Check(p, "~/w/shop", o); !errors.As(err, &pm) {
		t.Errorf("missing sub folder: %v", err)
	}

	// Outside a repository the typed directory is the project itself.
	plain := ProjectOf(filepath.Join(src, "notes"), src)
	if err := os.MkdirAll(filepath.Join(home, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if loc, err := Check(plain, "~/elsewhere", o); err != nil || loc.Cwd != filepath.Join(home, "elsewhere") || loc.Checkout != "" {
		t.Fatalf("plain: %+v %v", loc, err)
	}
}
