package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

func ids(ss []Session) string {
	var out []string
	for _, s := range ss {
		out = append(out, s.ConfigName+":"+s.ShortID())
	}
	return strings.Join(out, ",")
}

func TestListCurrentProjectAcrossDirs(t *testing.T) {
	stubAlive(t)
	root := t.TempDir()
	a := configDir(t, root, "a")
	b := configDir(t, root, "b")
	emptyRegistry(t, a.Dir)
	emptyRegistry(t, b.Dir)

	writeSession(t, a.Dir, projEnc, sid1, baseTime.Add(-3*time.Hour),
		userLine(t, "oldest", 0, nil))
	writeSession(t, b.Dir, projEnc, sid2, baseTime.Add(-1*time.Hour),
		userLine(t, "middle", 0, nil), metaLine(t, "ai-title", "aiTitle", "Middle title"),
		assistantLine(t, "x", 2*time.Hour))
	writeSession(t, a.Dir, projEnc, sid3, baseTime.Add(-30*time.Minute),
		userLine(t, "newest", 0, nil), assistantLine(t, "x", 3*time.Hour))
	// Noise that must be ignored.
	writeSession(t, a.Dir, projEnc, "agent-abc123", baseTime, userLine(t, "subagent", 0, nil))
	writeFile(t, filepath.Join(a.Dir, "projects", projEnc, "notes.txt"), "x", 0o600, baseTime)
	writeSession(t, a.Dir, projEnc, "-rf", baseTime, userLine(t, "bad id", 0, nil))
	writeSession(t, a.Dir, "-other-project", "3e0c9f41-5d70-4b1e-8c43-4f5a6b7c8d9e", baseTime,
		userLine(t, "other project", 0, nil))
	// No title and no prompt: skipped.
	writeSession(t, b.Dir, projEnc, "4f1da052-6e81-4c2f-9d54-5a6b7c8d9e0f", baseTime.Add(-10*time.Minute),
		mustJSON(t, map[string]any{"type": "permission-mode", "permissionMode": "default"}))
	// Live in dir b's registry.
	writeRegistry(t, b.Dir, 4242, sid2)

	got, err := List(ListOptions{Dirs: []config.ConfigDir{a, b}, Cwd: projCwd, Now: baseTime})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a:2d9b8e30,b:1c8a7d2f,a:0b7f6c1e"; ids(got) != want {
		t.Fatalf("ids = %s, want %s", ids(got), want)
	}
	newest, middle, oldest := got[0], got[1], got[2]
	if newest.FirstPrompt != "newest" || newest.Title != "" || newest.Cwd != projCwd || newest.ProjectDir != projEnc {
		t.Errorf("newest = %+v", newest)
	}
	if !newest.LastActive.Equal(baseTime.Add(3 * time.Hour)) {
		t.Errorf("newest.LastActive = %v (want the last timestamp, not mtime)", newest.LastActive)
	}
	if middle.Title != "Middle title" || !middle.Live || middle.LiveIn != "b" || middle.ConfigDir != b.Dir {
		t.Errorf("middle = %+v", middle)
	}
	if oldest.Live || oldest.MaybeOpen || oldest.Size == 0 {
		t.Errorf("oldest = %+v", oldest)
	}

	got, err = List(ListOptions{Dirs: []config.ConfigDir{a, b}, Cwd: projCwd, Limit: 1, Now: baseTime})
	if err != nil || ids(got) != "a:2d9b8e30" {
		t.Errorf("limit 1: %s %v", ids(got), err)
	}

	got, err = List(ListOptions{Dirs: []config.ConfigDir{a, b}, All: true, Now: baseTime})
	if err != nil || ids(got) != "a:2d9b8e30,b:1c8a7d2f,a:3e0c9f41,a:0b7f6c1e" {
		t.Errorf("all: %s %v", ids(got), err)
	}
}

// With no live registry (older Claude Code), a session touched in the last
// two minutes is flagged as possibly open.
func TestListMaybeOpen(t *testing.T) {
	root := t.TempDir()
	a := configDir(t, root, "a")
	now := time.Now()
	writeSession(t, a.Dir, projEnc, sid1, now.Add(-30*time.Second), userLine(t, "recent", 0, nil))
	writeSession(t, a.Dir, projEnc, sid2, now.Add(-10*time.Minute), userLine(t, "older", 0, nil))
	got, err := List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: projCwd, Now: now})
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for _, s := range got {
		if s.MaybeOpen != (s.ID == sid1) {
			t.Errorf("%s MaybeOpen = %v", s.FirstPrompt, s.MaybeOpen)
		}
	}
	emptyRegistry(t, a.Dir)
	got, _ = List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: projCwd, Now: now})
	for _, s := range got {
		if s.MaybeOpen {
			t.Errorf("with a registry nothing is a guess: %+v", s)
		}
	}
}

// Encoded names over 200 characters are cut and hashed by Claude Code: match
// the prefix and confirm with the recorded cwd.
func TestListLongPath(t *testing.T) {
	root := t.TempDir()
	a := configDir(t, root, "a")
	long := "/Users/murat/" + strings.Repeat("deeply/nested/", 16) + "project"
	other := long[:len(long)-len("project")] + "projecX"
	enc := EncodeCwd(long)
	if len(enc) <= maxEncodedLen || EncodeCwd(other)[:maxEncodedLen] != enc[:maxEncodedLen] {
		t.Fatal("fixture must share the 200-char prefix")
	}
	folder := enc[:maxEncodedLen] + "-1a2b3c"
	line := func(cwd, prompt string) string {
		return strings.Replace(userLine(t, prompt, 0, nil), `"cwd":"`+projCwd+`"`, `"cwd":"`+cwd+`"`, 1)
	}
	writeSession(t, a.Dir, folder, sid1, baseTime, line(long, "mine"))
	writeSession(t, a.Dir, enc[:maxEncodedLen]+"-9f8e7d", sid2, baseTime, line(other, "someone else's"))

	got, err := List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: long, Now: baseTime})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FirstPrompt != "mine" || got[0].ProjectDir != folder {
		t.Fatalf("got %+v", got)
	}
}

// $PWD may go through a symlink; Claude Code records the real path.
func TestListSymlinkedCwd(t *testing.T) {
	root := t.TempDir()
	a := configDir(t, root, "a")
	real := filepath.Join(root, "real", "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ = filepath.EvalSymlinks(real)
	link := filepath.Join(root, "link")
	if err := os.Symlink(filepath.Dir(real), link); err != nil {
		t.Fatal(err)
	}
	writeSession(t, a.Dir, EncodeCwd(real), sid1, baseTime, userLine(t, "via real path", 0, nil))
	got, err := List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: filepath.Join(link, "proj"), Now: baseTime})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestListReportsUnreadableFolderButKeepsGoing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root := t.TempDir()
	a := configDir(t, root, "a")
	b := configDir(t, root, "b")
	writeSession(t, a.Dir, projEnc, sid1, baseTime, userLine(t, "readable", 0, nil))
	writeSession(t, b.Dir, projEnc, sid2, baseTime, userLine(t, "hidden", 0, nil))
	locked := filepath.Join(b.Dir, "projects", projEnc)
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	got, err := List(ListOptions{Dirs: []config.ConfigDir{a, b}, Cwd: projCwd, Now: baseTime})
	if err == nil || !strings.Contains(err.Error(), locked) {
		t.Errorf("err = %v, want it to name %s", err, locked)
	}
	if len(got) != 1 || got[0].FirstPrompt != "readable" {
		t.Errorf("got %+v", got)
	}
}

func TestListMissingProjectsIsEmpty(t *testing.T) {
	root := t.TempDir()
	a := configDir(t, root, "a")
	got, err := List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: projCwd})
	if err != nil || len(got) != 0 {
		t.Errorf("got %v %v", got, err)
	}
}

// A folder full of sessions left empty (no title, no prompt) must not turn a
// listing into a full scan: at most limit*4+20 files are opened, so a real
// session behind that many empty ones is not reached.
func TestListOpensAtMostFourTimesLimitPlusTwenty(t *testing.T) {
	var opened int
	prev := readMetaFn
	readMetaFn = func(p string) (meta, error) {
		opened++
		return prev(p)
	}
	t.Cleanup(func() { readMetaFn = prev })

	for _, tc := range []struct {
		name  string
		limit int // 0: DefaultLimit
		all   bool
	}{
		{"default limit", 0, false},
		{"limit 2", 2, false},
		{"limit 2, every project", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit := tc.limit
			if limit == 0 {
				limit = DefaultLimit
			}
			bound := limit*4 + 20
			root := t.TempDir()
			a := configDir(t, root, "a")
			emptyRegistry(t, a.Dir)
			empty := mustJSON(t, map[string]any{"type": "permission-mode", "permissionMode": "default"})
			var paths []string
			for i := 0; i < bound; i++ {
				id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
				paths = append(paths, writeSession(t, a.Dir, projEnc, id, baseTime.Add(-time.Duration(i)*time.Second), empty))
			}
			// Older than every empty session: candidate number bound+1.
			writeSession(t, a.Dir, projEnc, sid1, baseTime.Add(-time.Hour), userLine(t, "real", 0, nil))
			opts := ListOptions{Dirs: []config.ConfigDir{a}, Cwd: projCwd, All: tc.all, Limit: tc.limit, Now: baseTime}

			opened = 0
			got, err := List(opts)
			if err != nil {
				t.Fatal(err)
			}
			if opened != bound || len(got) != 0 {
				t.Fatalf("opened %d files (want %d), listed %d sessions (want 0)", opened, bound, len(got))
			}

			// One empty session fewer: the real one is within the bound.
			if err := os.Remove(paths[len(paths)-1]); err != nil {
				t.Fatal(err)
			}
			opened = 0
			got, err = List(opts)
			if err != nil {
				t.Fatal(err)
			}
			if opened != bound || len(got) != 1 || got[0].FirstPrompt != "real" {
				t.Fatalf("opened %d files (want %d), got %+v", opened, bound, got)
			}
		})
	}
}

func TestListDefaultLimit(t *testing.T) {
	if DefaultLimit != 100 {
		t.Fatalf("DefaultLimit = %d, want 100", DefaultLimit)
	}
	root := t.TempDir()
	a := configDir(t, root, "a")
	emptyRegistry(t, a.Dir)
	for i := 0; i < DefaultLimit+5; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-000000000000", i)
		writeSession(t, a.Dir, projEnc, id, baseTime.Add(-time.Duration(i)*time.Minute), userLine(t, fmt.Sprintf("prompt %d", i), 0, nil))
	}
	got, err := List(ListOptions{Dirs: []config.ConfigDir{a}, Cwd: projCwd, Now: baseTime})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != DefaultLimit {
		t.Fatalf("listed %d sessions, want %d", len(got), DefaultLimit)
	}
	// Newest first; the 5 oldest fall outside the limit.
	if got[0].FirstPrompt != "prompt 0" || got[len(got)-1].FirstPrompt != fmt.Sprintf("prompt %d", DefaultLimit-1) {
		t.Errorf("first %q, last %q", got[0].FirstPrompt, got[len(got)-1].FirstPrompt)
	}
}
