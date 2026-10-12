package pins

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/config"
)

func setup(t *testing.T) (home, jl string) {
	t.Helper()
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r // macOS: the temp dir is reached through /var -> /private/var
	}
	home = filepath.Join(root, "home")
	jl = filepath.Join(home, ".julienning")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("JULIENNING_HOME", jl)
	return home, jl
}

func mkdir(t *testing.T, p string) string {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadWithoutFile(t *testing.T) {
	setup(t)
	list, err := Load()
	if err != nil || list != nil {
		t.Fatalf("Load = %v, %v; want no pins", list, err)
	}
}

func TestSetReplaceRemoveAndFileLifecycle(t *testing.T) {
	home, jl := setup(t)
	side := filepath.Join(home, "Code", "side")
	shop := filepath.Join(home, "Code", "shop")
	file := filepath.Join(jl, config.PinsFile)

	_, replaced, created, err := Set(side, "Me@Personal.io")
	if err != nil || replaced || !created {
		t.Fatalf("first Set: replaced %v created %v err %v", replaced, created, err)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if _, _, created, err = Set(shop, "claude1@x.io"); err != nil || created {
		t.Fatalf("second Set: created %v err %v", created, err)
	}
	prev, replaced, _, err := Set(side, "other@personal.io")
	if err != nil || !replaced || prev.Email != "me@personal.io" {
		t.Fatalf("replace: prev %+v replaced %v err %v", prev, replaced, err)
	}
	list, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []Pin{{Dir: shop, Email: "claude1@x.io"}, {Dir: side, Email: "other@personal.io"}}
	if len(list) != 2 || list[0] != want[0] || list[1] != want[1] {
		t.Fatalf("pins = %+v, want %+v (sorted by dir, email lowercased)", list, want)
	}

	if _, ok, err := Remove(filepath.Join(side, "sub")); err != nil || ok {
		t.Fatalf("Remove of an unpinned subdir: ok %v err %v", ok, err)
	}
	if p, ok, err := Remove(side); err != nil || !ok || p.Email != "other@personal.io" {
		t.Fatalf("Remove: %+v %v %v", p, ok, err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("one pin left, the file must stay: %v", err)
	}
	if _, ok, err := Remove(shop); err != nil || !ok {
		t.Fatalf("Remove last: %v %v", ok, err)
	}
	// The wrapper only asks julienning while the file exists.
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the last unpin must remove %s: %v", file, err)
	}
}

func TestMatchDeepestWins(t *testing.T) {
	home, _ := setup(t)
	code := mkdir(t, filepath.Join(home, "Code"))
	side := mkdir(t, filepath.Join(code, "side"))
	web := mkdir(t, filepath.Join(side, "web"))
	sibling := mkdir(t, filepath.Join(code, "side-car"))
	list := []Pin{{Dir: code, Email: "a@x.io"}, {Dir: side, Email: "b@x.io"}}

	for _, tc := range []struct {
		cwd, want string
	}{
		{code, "a@x.io"},
		{side, "b@x.io"},
		{web, "b@x.io"},
		{sibling, "a@x.io"}, // "side-car" is not below "side"
	} {
		p, ok := Match(list, tc.cwd)
		if !ok || p.Email != tc.want {
			t.Errorf("Match(%s) = %+v %v, want %s", tc.cwd, p, ok, tc.want)
		}
	}
	if _, ok := Match(list, home); ok {
		t.Error("a parent of every pin must not match")
	}
	if p, ok := Match([]Pin{{Dir: "/", Email: "r@x.io"}}, web); !ok || p.Email != "r@x.io" {
		t.Error("a pin on / covers everything")
	}
}

// The shell's $PWD and the pinned spelling may reach one directory through
// different symlinks; the pin applies either way, and unpin finds it.
func TestMatchThroughSymlinks(t *testing.T) {
	home, _ := setup(t)
	real := mkdir(t, filepath.Join(home, "work", "side"))
	link := filepath.Join(home, "side")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(real, "web"))

	if p, ok := Match([]Pin{{Dir: real, Email: "a@x.io"}}, filepath.Join(link, "web")); !ok || p.Email != "a@x.io" {
		t.Errorf("pinned by real path, cwd through the link: %+v %v", p, ok)
	}
	if p, ok := Match([]Pin{{Dir: link, Email: "a@x.io"}}, filepath.Join(real, "web")); !ok || p.Email != "a@x.io" {
		t.Errorf("pinned through the link, cwd by real path: %+v %v", p, ok)
	}
	if _, _, _, err := Set(link, "a@x.io"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Remove(real); err != nil || !ok {
		t.Errorf("unpin by the real path: %v %v", ok, err)
	}
}

// One directory carries one pin: pinning it again through another spelling
// replaces the pin instead of adding a second one that would keep winning,
// and unpin removes every spelling.
func TestOnePinPerDirectoryAcrossSpellings(t *testing.T) {
	home, jl := setup(t)
	real := mkdir(t, filepath.Join(home, "work", "side"))
	link := filepath.Join(home, "side")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Set(real, "a@x.io"); err != nil {
		t.Fatal(err)
	}
	prev, replaced, _, err := Set(link, "b@x.io")
	if err != nil || !replaced || prev.Email != "a@x.io" {
		t.Fatalf("re-pin through the link: prev %+v replaced %v err %v", prev, replaced, err)
	}
	list, err := Load()
	if err != nil || len(list) != 1 || list[0] != (Pin{Dir: link, Email: "b@x.io"}) {
		t.Fatalf("pins = %+v %v, want only the new pin", list, err)
	}
	if p, ok := Match(list, real); !ok || p.Email != "b@x.io" {
		t.Errorf("Match = %+v %v", p, ok)
	}

	// A file that already holds both spellings (written by hand, or by an
	// earlier build): unpin removes both.
	body := `{"version":1,"pins":[{"dir":"` + link + `","email":"b@x.io"},{"dir":"` + real + `","email":"a@x.io"}]}`
	if err := os.WriteFile(filepath.Join(jl, config.PinsFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := Remove(real); err != nil || !ok {
		t.Fatalf("Remove: %v %v", ok, err)
	}
	if list, err := Load(); err != nil || len(list) != 0 {
		t.Errorf("pins left: %+v %v", list, err)
	}
}

// Depth is compared on resolved paths: a child pin written through a short
// symlink beats a parent pin written with its long real path.
func TestMatchDepthAcrossSpellings(t *testing.T) {
	home, _ := setup(t)
	parent := mkdir(t, filepath.Join(home, "Volumes", "Data", "work", "project"))
	child := mkdir(t, filepath.Join(parent, "sub"))
	short := filepath.Join(home, "w")
	if err := os.Symlink(filepath.Join(home, "Volumes", "Data", "work"), short); err != nil {
		t.Fatal(err)
	}
	childViaLink := filepath.Join(short, "project", "sub")
	if len(childViaLink) >= len(parent) {
		t.Fatalf("fixture: the child spelling must be the shorter string (%q vs %q)", childViaLink, parent)
	}
	list := []Pin{{Dir: parent, Email: "parent@x.io"}, {Dir: childViaLink, Email: "child@x.io"}}
	for _, cwd := range []string{child, childViaLink, filepath.Join(child, "deeper")} {
		mkdir(t, filepath.Join(child, "deeper"))
		if p, ok := Match(list, cwd); !ok || p.Email != "child@x.io" {
			t.Errorf("Match(%s) = %+v %v, want the child pin", cwd, p, ok)
		}
	}
	if p, _ := Match(list, parent); p.Email != "parent@x.io" {
		t.Errorf("Match(parent) = %+v", p)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	_, jl := setup(t)
	file := filepath.Join(jl, config.PinsFile)
	mkdir(t, jl)
	for name, tc := range map[string]struct{ body, want string }{
		"not json":       {`{`, "parse"},
		"newer schema":   {`{"version":2,"pins":[]}`, "schema version 2"},
		"relative dir":   {`{"version":1,"pins":[{"dir":"Code/x","email":"a@x.io"}]}`, "absolute"},
		"unclean dir":    {`{"version":1,"pins":[{"dir":"/a/../b","email":"a@x.io"}]}`, "absolute, clean"},
		"uppercase mail": {`{"version":1,"pins":[{"dir":"/a","email":"A@x.io"}]}`, "invalid email"},
		"no at":          {`{"version":1,"pins":[{"dir":"/a","email":"ax.io"}]}`, "invalid email"},
		"duplicate":      {`{"version":1,"pins":[{"dir":"/a","email":"a@x.io"},{"dir":"/a","email":"b@x.io"}]}`, "duplicate"},
	} {
		if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "delete") {
			t.Errorf("%s: err = %v, want %q and how to recover", name, err, tc.want)
		}
	}
}

func TestUseErrorNamesTheWayOut(t *testing.T) {
	home, _ := setup(t)
	err := UseError(Pin{Dir: filepath.Join(home, "Code", "side"), Email: "me@x.io"}, errors.New("boom"))
	want := "~/Code/side is pinned to me@x.io, which cannot be opened: boom (or remove the pin: julienning unpin ~/Code/side)"
	if err.Error() != want {
		t.Errorf("got  %q\nwant %q", err, want)
	}
}
