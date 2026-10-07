package sessions

import (
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

var oldTime = time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)

// seed builds a realistic session with every side item in src, plus a
// second session in the same project that must never be touched.
func seed(t *testing.T, src config.ConfigDir) Session {
	t.Helper()
	d := src.Dir
	proj := filepath.Join(d, "projects", projEnc)
	writeSession(t, d, projEnc, sid1, oldTime,
		userLine(t, "fix checkout", 0, nil),
		metaLine(t, "custom-title", "customTitle", "Checkout fix"),
		assistantLine(t, "done", time.Minute))
	writeFile(t, filepath.Join(proj, sid1, "subagents", "agent-a1b2c3.jsonl"), `{"type":"user"}`+"\n", 0o600, oldTime.Add(1*time.Minute))
	writeFile(t, filepath.Join(proj, sid1, "subagents", "agent-a1b2c3.meta.json"), `{"agentType":"explore"}`, 0o644, oldTime.Add(2*time.Minute))
	writeFile(t, filepath.Join(proj, sid1, "tool-results", "r1.txt"), strings.Repeat("output\n", 500), 0o640, oldTime.Add(3*time.Minute))
	if err := os.Symlink("r1.txt", filepath.Join(proj, sid1, "tool-results", "latest")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(proj, sid1, "custom-title.json"), `{"customTitle":"Checkout fix"}`, 0o600, oldTime.Add(4*time.Minute))
	writeFile(t, filepath.Join(d, "file-history", sid1, "39d0a15be6882c17@v1"), "v1", 0o600, oldTime.Add(5*time.Minute))
	writeFile(t, filepath.Join(d, "file-history", sid1, "39d0a15be6882c17@v2"), "v2", 0o400, oldTime.Add(6*time.Minute))
	if err := os.MkdirAll(filepath.Join(d, "session-env", sid1), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(d, "todos", sid1+"-agent-"+sid1+".json"), "[]", 0o644, oldTime.Add(7*time.Minute))
	writeFile(t, filepath.Join(d, "debug", sid1+".txt"), "debug log", 0o644, oldTime.Add(8*time.Minute))
	writeFile(t, filepath.Join(d, "tasks", sid1, "1.json"), `{"id":"1","subject":"fix checkout","status":"pending"}`, 0o644, oldTime.Add(9*time.Minute))
	writeFile(t, filepath.Join(d, "tasks", sid1, ".lock"), "", 0o644, oldTime.Add(9*time.Minute))

	// Unrelated session in the same project, with its own side files.
	writeSession(t, d, projEnc, sid2, oldTime, userLine(t, "other work", 0, nil))
	writeFile(t, filepath.Join(d, "file-history", sid2, "aa@v1"), "x", 0o600, oldTime)
	writeFile(t, filepath.Join(d, "todos", sid2+"-agent-"+sid2+".json"), "[]", 0o644, oldTime)

	// Directory mtimes last, after their children exist.
	for _, dir := range []string{
		filepath.Join(proj, sid1, "subagents"), filepath.Join(proj, sid1, "tool-results"), filepath.Join(proj, sid1),
		filepath.Join(d, "file-history", sid1), filepath.Join(d, "session-env", sid1), filepath.Join(d, "tasks", sid1),
	} {
		if err := os.Chtimes(dir, oldTime.Add(-time.Hour), oldTime.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	emptyRegistry(t, d)
	// Move resolves the project's memory folder by walking up from Cwd
	// looking for .git, so Cwd must be a hermetic path outside any
	// repository, not projCwd (which may exist on a developer machine).
	cwd := filepath.Join(filepath.Dir(d), "work", "checkout")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	return Session{
		ID: sid1, ConfigName: src.Name, ConfigDir: src.Dir, ProjectDir: projEnc,
		Cwd: cwd, Title: "Checkout fix", FirstPrompt: "fix checkout", LastActive: oldTime, ModTime: oldTime,
	}
}

var movedRels = []string{
	filepath.Join("projects", projEnc, sid1+".jsonl"),
	filepath.Join("projects", projEnc, sid1),
	filepath.Join("file-history", sid1),
	filepath.Join("session-env", sid1),
	filepath.Join("tasks", sid1),
	filepath.Join("debug", sid1+".txt"),
	filepath.Join("todos", sid1+"-agent-"+sid1+".json"),
}

// seedFiles is how many regular files seed gives sid1, i.e. how many times
// the "copy" failpoint fires for a full move.
const seedFiles = 11

// under keeps the snapshot entries at or below rel.
func under(snap map[string]string, rel string) map[string]string {
	out := map[string]string{}
	for k, v := range snap {
		if k == rel || strings.HasPrefix(k, rel+string(filepath.Separator)) {
			out[k] = v
		}
	}
	return out
}

func setupMove(t *testing.T) (src, dst config.ConfigDir, s Session) {
	t.Helper()
	stubAlive(t)
	prev := now
	now = func() time.Time { return baseTime }
	t.Cleanup(func() { now = prev })
	root := t.TempDir()
	src = configDir(t, root, "sixtynine")
	dst = configDir(t, root, "julienning3")
	writeFile(t, filepath.Join(dst.Dir, ".claude.json"), `{"oauthAccount":{"emailAddress":"c3@x.io"}}`, 0o600, oldTime)
	writeSession(t, dst.Dir, "-elsewhere", sid3, oldTime, userLine(t, "target's own", 0, nil))
	emptyRegistry(t, dst.Dir)
	return src, dst, seed(t, src)
}

func TestMoveMovesEveryItemPreservingModesAndTimes(t *testing.T) {
	src, dst, s := setupMove(t)
	before := snapshot(t, src.Dir)

	rep, err := Move(s, dst)
	if err != nil {
		t.Fatal(err)
	}
	after := snapshot(t, dst.Dir)
	for _, rel := range movedRels {
		want := under(before, rel)
		if len(want) == 0 {
			t.Fatalf("fixture lacks %s", rel)
		}
		got := under(after, rel)
		if d := diffSnapshots(want, got); len(d) > 0 {
			t.Errorf("%s differs in target:\n%s", rel, strings.Join(d, "\n"))
		}
		if _, err := os.Lstat(filepath.Join(src.Dir, rel)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still in source (%v)", rel, err)
		}
	}
	if len(rep.Items) != len(movedRels) {
		t.Errorf("report items = %v", rep.Items)
	}
	// The neighbour session and its files stay put.
	for _, rel := range []string{
		filepath.Join("projects", projEnc, sid2+".jsonl"),
		filepath.Join("file-history", sid2, "aa@v1"),
		filepath.Join("todos", sid2+"-agent-"+sid2+".json"),
	} {
		if _, err := os.Stat(filepath.Join(src.Dir, rel)); err != nil {
			t.Errorf("neighbour %s: %v", rel, err)
		}
		if _, err := os.Stat(filepath.Join(dst.Dir, rel)); err == nil {
			t.Errorf("neighbour %s leaked into target", rel)
		}
	}
	if tmp := append(temporaries(t, src.Dir), temporaries(t, dst.Dir)...); len(tmp) > 0 {
		t.Errorf("temporaries left: %v", tmp)
	}
	if got, want := rep.String(), `Moved "Checkout fix" from sixtynine to julienning3.`; got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
	if len(rep.Warnings) > 0 {
		t.Errorf("warnings: %v", rep.Warnings)
	}

	// The moved session lists in the target with its original last-active time.
	got, err := List(ListOptions{Dirs: []config.ConfigDir{dst}, Cwd: projCwd, Now: baseTime})
	if err != nil || len(got) != 1 || got[0].Title != "Checkout fix" || !got[0].ModTime.Equal(oldTime) {
		t.Errorf("listing after move: %+v %v", got, err)
	}
}

// The default dir keeps its sessions in ~/.claude/projects like any other
// dir; only its account file lives elsewhere.
func TestMoveIntoDefaultDir(t *testing.T) {
	stubAlive(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := configDir(t, home, "sixtynine")
	def := config.ConfigDir{Name: "default", Dir: filepath.Join(home, ".claude")}
	if err := os.MkdirAll(def.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := seed(t, src)
	if _, err := Move(s, def); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(def.Dir, "projects", projEnc, sid1+".jsonl")); err != nil {
		t.Errorf("transcript not in ~/.claude/projects: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); err == nil {
		t.Error("wrote next to ~/.claude.json instead of into ~/.claude")
	}
}

// Every refusal and every failure before the deletes must leave both sides
// exactly as they were.
func assertUntouched(t *testing.T, src, dst config.ConfigDir, srcBefore, dstBefore map[string]string) {
	t.Helper()
	if d := diffSnapshots(srcBefore, snapshot(t, src.Dir)); len(d) > 0 {
		t.Errorf("source changed:\n%s", strings.Join(d, "\n"))
	}
	if d := diffSnapshots(withoutDirTimes(dstBefore), withoutDirTimes(snapshot(t, dst.Dir))); len(d) > 0 {
		t.Errorf("target changed:\n%s", strings.Join(d, "\n"))
	}
	if tmp := temporaries(t, dst.Dir); len(tmp) > 0 {
		t.Errorf("temporaries left in target: %v", tmp)
	}
}

func TestMoveFailureLeavesBothSidesIntact(t *testing.T) {
	cases := []struct {
		name string
		step string
		nth  int
	}{
		{"copy of a nested file", "copy", 3},
		{"copy of the last file", "copy", seedFiles},
		{"second rename", "rename", 2},
		{"last rename", "rename", len(movedRels)},
		{"verification", "verify", 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, s := setupMove(t)
			srcBefore, dstBefore := snapshot(t, src.Dir), snapshot(t, dst.Dir)
			n := 0
			failpoint = func(step, path string) error {
				if step == tc.step {
					n++
					if n == tc.nth {
						return errors.New("injected: disk full")
					}
				}
				return nil
			}
			t.Cleanup(func() { failpoint = nil })

			_, err := Move(s, dst)
			if err == nil || !strings.Contains(err.Error(), "injected: disk full") || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v (fired %d times)", err, n)
			}
			assertUntouched(t, src, dst, srcBefore, dstBefore)
		})
	}
}

func TestMoveUnreadableSourceFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	src, dst, s := setupMove(t)
	locked := filepath.Join(src.Dir, "file-history", sid1, "39d0a15be6882c17@v2")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	srcBefore, dstBefore := snapshot(t, src.Dir), snapshot(t, dst.Dir)
	if _, err := Move(s, dst); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err = %v", err)
	}
	assertUntouched(t, src, dst, srcBefore, dstBefore)
}

func TestMoveDeleteFailureIsAWarning(t *testing.T) {
	src, dst, s := setupMove(t)
	failpoint = func(step, path string) error {
		if step == "delete" && strings.HasSuffix(path, filepath.Join("debug", sid1+".txt")) {
			return errors.New("injected: busy")
		}
		return nil
	}
	t.Cleanup(func() { failpoint = nil })
	rep, err := Move(s, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "debug") || !strings.Contains(rep.Warnings[0], "injected: busy") {
		t.Errorf("warnings = %v", rep.Warnings)
	}
	if _, err := os.Stat(filepath.Join(src.Dir, "projects", projEnc, sid1+".jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Error("transcript must still be removed from the source")
	}
}

func TestMoveRefusals(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T, src, dst config.ConfigDir, s *Session)
		want    error
		msg     string
	}{
		{"live in source registry", func(t *testing.T, src, _ config.ConfigDir, _ *Session) {
			writeRegistry(t, src.Dir, 4242, sid1)
		}, ErrLive, "open in another terminal"},
		{"live in target registry", func(t *testing.T, _, dst config.ConfigDir, _ *Session) {
			writeRegistry(t, dst.Dir, 4343, sid1)
		}, ErrLive, "open in another terminal"},
		{"flagged live by the listing", func(_ *testing.T, _, _ config.ConfigDir, s *Session) {
			s.Live, s.LiveIn = true, "other"
		}, ErrLive, "(other)"},
		{"recent without registry", func(t *testing.T, src, _ config.ConfigDir, _ *Session) {
			if err := os.RemoveAll(filepath.Join(src.Dir, "sessions")); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(src.Dir, "projects", projEnc, sid1+".jsonl")
			if err := os.Chtimes(p, baseTime.Add(-30*time.Second), baseTime.Add(-30*time.Second)); err != nil {
				t.Fatal(err)
			}
		}, ErrMaybeOpen, "may still be open"},
		{"same id in another target project", func(t *testing.T, _, dst config.ConfigDir, _ *Session) {
			writeSession(t, dst.Dir, "-somewhere-else", sid1, oldTime, userLine(t, "dup", 0, nil))
		}, ErrCollision, "already has a session with id " + sid1},
		{"leftover side item in target", func(t *testing.T, _, dst config.ConfigDir, _ *Session) {
			writeFile(t, filepath.Join(dst.Dir, "file-history", sid1, "x@v1"), "x", 0o600, oldTime)
		}, ErrCollision, "already has a session"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, s := setupMove(t)
			tc.prepare(t, src, dst, &s)
			srcBefore, dstBefore := snapshot(t, src.Dir), snapshot(t, dst.Dir)
			_, err := Move(s, dst)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want %v containing %q", err, tc.want, tc.msg)
			}
			assertUntouched(t, src, dst, srcBefore, dstBefore)
		})
	}
}

// Recent changes are fine when the dir has a registry that says "not open".
func TestMoveRecentWithRegistryProceeds(t *testing.T) {
	src, dst, s := setupMove(t)
	p := filepath.Join(src.Dir, "projects", projEnc, sid1+".jsonl")
	if err := os.Chtimes(p, baseTime.Add(-10*time.Second), baseTime.Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := Move(s, dst); err != nil {
		t.Fatal(err)
	}
}

func TestMoveRejectsBadInput(t *testing.T) {
	src, dst, s := setupMove(t)
	cases := map[string]func() (Session, config.ConfigDir){
		"same dir":     func() (Session, config.ConfigDir) { return s, src },
		"traversal id": func() (Session, config.ConfigDir) { b := s; b.ID = "../x"; return b, dst },
		"flag-like id": func() (Session, config.ConfigDir) { b := s; b.ID = "-rf"; return b, dst },
		"bad project":  func() (Session, config.ConfigDir) { b := s; b.ProjectDir = ".."; return b, dst },
		"missing target": func() (Session, config.ConfigDir) {
			return s, config.ConfigDir{Name: "x", Dir: filepath.Join(dst.Dir, "nope")}
		},
		"gone session": func() (Session, config.ConfigDir) { b := s; b.ID = sid3; return b, dst },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			ss, target := mk()
			if _, err := Move(ss, target); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestMoveMergesProjectMemory(t *testing.T) {
	src, dst, s := setupMove(t)
	sm := filepath.Join(src.Dir, "projects", projEnc, "memory")
	dm := filepath.Join(dst.Dir, "projects", projEnc, "memory")
	writeFile(t, filepath.Join(sm, "MEMORY.md"), "# Memory\n- [A](a.md) — a\n- [B](b.md) — b\n\n- [D](d.md) — d\n", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "a.md"), "same", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "b.md"), "only in source", 0o600, oldTime)
	writeFile(t, filepath.Join(sm, "d.md"), "source version", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "sub", "e.md"), "nested", 0o644, oldTime)
	writeFile(t, filepath.Join(dm, "MEMORY.md"), "# Memory\n- [A](a.md) — a\n- [C](c.md) — c", 0o600, oldTime)
	writeFile(t, filepath.Join(dm, "a.md"), "same", 0o644, oldTime)
	writeFile(t, filepath.Join(dm, "c.md"), "only in target", 0o644, oldTime)
	writeFile(t, filepath.Join(dm, "d.md"), "target version", 0o644, oldTime)
	srcMem := snapshot(t, sm)

	rep, err := Move(s, dst)
	if err != nil {
		t.Fatal(err)
	}
	want := `Moved "Checkout fix" from sixtynine to julienning3 (memory: 2 files added, 1 index line merged, 1 conflict kept).`
	if rep.String() != want {
		t.Errorf("report = %q\nwant       %q", rep.String(), want)
	}
	if strings.Join(rep.Memory.Conflicts, ",") != "d.md" {
		t.Errorf("conflicts = %v", rep.Memory.Conflicts)
	}
	read := func(p string) string {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	// d.md conflicted: its source index line describes a version the target
	// does not have, so it is not appended.
	if got := read(filepath.Join(dm, "MEMORY.md")); got != "# Memory\n- [A](a.md) — a\n- [C](c.md) — c\n- [B](b.md) — b\n" {
		t.Errorf("MEMORY.md = %q", got)
	}
	if fi, _ := os.Stat(filepath.Join(dm, "MEMORY.md")); fi.Mode().Perm() != 0o600 {
		t.Errorf("MEMORY.md mode = %v, want the target's 0600 kept", fi.Mode().Perm())
	}
	if read(filepath.Join(dm, "d.md")) != "target version" || read(filepath.Join(dm, "c.md")) != "only in target" {
		t.Error("target memory overwritten")
	}
	if read(filepath.Join(dm, "b.md")) != "only in source" || read(filepath.Join(dm, "sub", "e.md")) != "nested" {
		t.Error("missing files not copied")
	}
	if fi, _ := os.Stat(filepath.Join(dm, "b.md")); fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(oldTime) {
		t.Errorf("b.md mode/mtime not preserved: %v %v", fi.Mode(), fi.ModTime())
	}
	if d := diffSnapshots(srcMem, snapshot(t, sm)); len(d) > 0 {
		t.Errorf("source memory must stay (other sessions of the project use it):\n%s", strings.Join(d, "\n"))
	}
	if tmp := temporaries(t, dst.Dir); len(tmp) > 0 {
		t.Errorf("temporaries: %v", tmp)
	}

	// Merging again adds nothing: the index union is idempotent.
	rep2, err := mergeMemory(sm, dm)
	if err != nil || rep2.IndexLines != 0 || len(rep2.Added) != 0 || len(rep2.Conflicts) != 1 {
		t.Errorf("second merge: %+v %v", rep2, err)
	}
}

func TestMergeMemoryIntoEmptyTarget(t *testing.T) {
	root := t.TempDir()
	sm, dm := filepath.Join(root, "src", "memory"), filepath.Join(root, "dst", "memory")
	writeFile(t, filepath.Join(sm, "MEMORY.md"), "- [A](a.md)\n", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "a.md"), "a", 0o644, oldTime)
	rep, err := mergeMemory(sm, dm)
	if err != nil || len(rep.Added) != 2 || rep.summary() != "2 files added" {
		t.Errorf("rep = %+v (%q) %v", rep, rep.summary(), err)
	}
	rep, err = mergeMemory(filepath.Join(root, "absent"), dm)
	if err != nil || rep.summary() != "" {
		t.Errorf("absent source: %+v %v", rep, err)
	}
}

func TestMergeMemoryKeepsGoingAfterAFailure(t *testing.T) {
	root := t.TempDir()
	sm, dm := filepath.Join(root, "src", "memory"), filepath.Join(root, "dst", "memory")
	writeFile(t, filepath.Join(sm, "a.md"), "a", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "b.md"), "b", 0o644, oldTime)
	failpoint = func(step, path string) error {
		if step == "memory" && strings.HasSuffix(path, "a.md") {
			return errors.New("injected")
		}
		return nil
	}
	t.Cleanup(func() { failpoint = nil })
	rep, err := mergeMemory(sm, dm)
	if err == nil || !strings.Contains(err.Error(), "a.md: injected") {
		t.Errorf("err = %v", err)
	}
	if strings.Join(rep.Added, ",") != "b.md" {
		t.Errorf("added = %v", rep.Added)
	}
}

// restoreWritable makes every directory under root owner-writable again at
// the end of a test, so t.TempDir's cleanup can remove read-only fixtures.
func restoreWritable(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
}

// Copies keep the source's modes, so a read-only directory in the session
// used to make the cleanup of a failed move fail halfway: the partial copy
// stayed, the error still said "nothing was changed", and the retry was
// refused as an id collision.
func TestMoveFailureWithReadOnlyDirsCleansUpAndRetrySucceeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	cases := []struct {
		name string
		step string
		nth  int
	}{
		{"after the read-only copies were renamed into place", "verify", 3},
		{"while read-only temporaries exist", "copy", seedFiles},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst, s := setupMove(t)
			restoreWritable(t, filepath.Dir(src.Dir))
			for _, rel := range []string{
				filepath.Join("projects", projEnc, sid1, "tool-results"),
				filepath.Join("file-history", sid1),
				filepath.Join("tasks", sid1),
			} {
				if err := os.Chmod(filepath.Join(src.Dir, rel), 0o555); err != nil {
					t.Fatal(err)
				}
			}
			srcBefore, dstBefore := snapshot(t, src.Dir), snapshot(t, dst.Dir)
			n := 0
			failpoint = func(step, path string) error {
				if step == tc.step {
					n++
					if n == tc.nth {
						return errors.New("injected: disk full")
					}
				}
				return nil
			}
			t.Cleanup(func() { failpoint = nil })

			_, err := Move(s, dst)
			if err == nil || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v (fired %d times)", err, n)
			}
			assertUntouched(t, src, dst, srcBefore, dstBefore)

			failpoint = nil
			rep, err := Move(s, dst)
			if err != nil {
				t.Fatalf("retry after a cleaned-up failure: %v", err)
			}
			if len(rep.Items) != len(movedRels) {
				t.Errorf("retry moved %v", rep.Items)
			}
			if _, err := os.Stat(filepath.Join(dst.Dir, "tasks", sid1, "1.json")); err != nil {
				t.Errorf("retry: %v", err)
			}
		})
	}
}

// When the partial copy cannot be removed, the error must say so (and not
// "nothing was changed"), naming what is left.
func TestMoveReportsCleanupFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	src, dst, s := setupMove(t)
	restoreWritable(t, filepath.Dir(src.Dir))
	srcBefore := snapshot(t, src.Dir)
	failpoint = func(step, path string) error {
		if step == "copy" && strings.HasSuffix(path, filepath.Join("debug", sid1+".txt")) {
			// The file-history temporary exists by now; locking its parent
			// makes it impossible to remove.
			if err := os.Chmod(filepath.Join(dst.Dir, "file-history"), 0o555); err != nil {
				t.Error(err)
			}
			return errors.New("injected: disk full")
		}
		return nil
	}
	t.Cleanup(func() { failpoint = nil })

	_, err := Move(s, dst)
	if err == nil || strings.Contains(err.Error(), "nothing was changed") ||
		!strings.Contains(err.Error(), "could not be fully removed") ||
		!strings.Contains(err.Error(), "file-history") || !strings.Contains(err.Error(), "injected: disk full") {
		t.Fatalf("err = %v", err)
	}
	if d := diffSnapshots(srcBefore, snapshot(t, src.Dir)); len(d) > 0 {
		t.Errorf("source changed:\n%s", strings.Join(d, "\n"))
	}
}

// Ctrl-C (or SIGTERM) during the copy removes the temporaries and leaves
// the source alone instead of killing the process mid-copy.
func TestMoveInterruptedBySignalCleansUp(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			// Guard: should the trap not work, the signal lands here instead
			// of killing the test binary.
			guard := make(chan os.Signal, 1)
			signal.Notify(guard, sig)
			defer signal.Stop(guard)

			src, dst, s := setupMove(t)
			srcBefore, dstBefore := snapshot(t, src.Dir), snapshot(t, dst.Dir)
			n := 0
			failpoint = func(step, path string) error {
				if step == "copy" {
					n++
					if n == 4 {
						if err := syscall.Kill(os.Getpid(), sig); err != nil {
							t.Error(err)
						}
						select {
						case <-guard:
						case <-time.After(5 * time.Second):
							t.Error("signal never delivered")
						}
						// Delivery to every registered channel happens in
						// one pass; give it a moment to reach the trap too.
						time.Sleep(20 * time.Millisecond)
					}
				}
				return nil
			}
			t.Cleanup(func() { failpoint = nil })

			_, err := Move(s, dst)
			if !errors.Is(err, ErrInterrupted) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v", err)
			}
			if n >= seedFiles {
				t.Errorf("copy went on after the signal (%d files)", n)
			}
			assertUntouched(t, src, dst, srcBefore, dstBefore)

			// The trap is gone afterwards: a retry works normally.
			failpoint = nil
			if _, err := Move(s, dst); err != nil {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}
