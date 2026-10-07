package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

// repoLayout builds, under a fresh temp dir, a main worktree with a
// subdirectory and a linked worktree the way `git worktree add` and Claude
// Code's .claude/worktrees lay them out. Everything is synthetic; git is
// never run.
type repoLayout struct {
	base, main, sub, worktree string
}

func newRepoLayout(t *testing.T, base string) repoLayout {
	t.Helper()
	l := repoLayout{base: base, main: filepath.Join(base, "shop")}
	l.sub = filepath.Join(l.main, "web", "src")
	l.worktree = filepath.Join(l.main, ".claude", "worktrees", "app-hook")
	for _, d := range []string{filepath.Join(l.main, ".git", "worktrees", "app-hook"), l.sub, l.worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	wtGit := filepath.Join(l.main, ".git", "worktrees", "app-hook")
	writeFile(t, filepath.Join(l.worktree, ".git"), "gitdir: "+wtGit+"\n", 0o644, time.Time{})
	writeFile(t, filepath.Join(wtGit, "commondir"), "../..\n", 0o644, time.Time{})
	writeFile(t, filepath.Join(wtGit, "gitdir"), filepath.Join(l.worktree, ".git")+"\n", 0o644, time.Time{})
	return l
}

func TestGitRoot(t *testing.T) {
	base := t.TempDir()
	l := newRepoLayout(t, base)

	// A worktree whose .git file uses a relative gitdir.
	relWT := filepath.Join(base, "shop-feature")
	relGit := filepath.Join(l.main, ".git", "worktrees", "shop-feature")
	writeFile(t, filepath.Join(relWT, ".git"), "gitdir: ../shop/.git/worktrees/shop-feature\n", 0o644, time.Time{})
	writeFile(t, filepath.Join(relGit, "commondir"), "../..\n", 0o644, time.Time{})

	// A submodule: .git file without commondir.
	subm := filepath.Join(l.main, "vendor", "lib")
	writeFile(t, filepath.Join(subm, ".git"), "gitdir: ../../.git/modules/lib\n", 0o644, time.Time{})
	if err := os.MkdirAll(filepath.Join(l.main, ".git", "modules", "lib"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A worktree of a bare repository.
	bare := filepath.Join(base, "tools.git")
	bareWT := filepath.Join(base, "tools-main")
	writeFile(t, filepath.Join(bare, "worktrees", "tools-main", "commondir"), "../..\n", 0o644, time.Time{})
	writeFile(t, filepath.Join(bareWT, ".git"), "gitdir: "+filepath.Join(bare, "worktrees", "tools-main")+"\n", 0o644, time.Time{})

	// Not a repository.
	plain := filepath.Join(base, "notes", "2026")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, dir, want string
		ok              bool
	}{
		{"main worktree", l.main, l.main, true},
		{"subdirectory", l.sub, l.main, true},
		{"linked worktree", l.worktree, l.main, true},
		{"linked worktree subdirectory", filepath.Join(l.worktree, "pkg"), l.main, true},
		{"relative gitdir", relWT, l.main, true},
		{"submodule keeps its own root", filepath.Join(subm, "src"), subm, true},
		{"bare repository worktree", bareWT, bare, true},
		{"deleted directory inside a repo", filepath.Join(l.main, "gone", "deeper"), l.main, true},
		{"outside any repository", plain, "", false},
		{"empty", "", "", false},
		{"relative path", "shop", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := gitRoot(tc.dir)
			if got != tc.want || ok != tc.ok {
				t.Errorf("gitRoot(%q) = %q, %v; want %q, %v", tc.dir, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// memorySession writes a session whose cwd is cwd into src and returns it.
func memorySession(t *testing.T, src config.ConfigDir, cwd, project string) Session {
	t.Helper()
	writeSession(t, src.Dir, project, sid1, oldTime, userLine(t, "fix checkout", 0, map[string]any{"cwd": cwd}))
	emptyRegistry(t, src.Dir)
	return Session{
		ID: sid1, ConfigName: src.Name, ConfigDir: src.Dir, ProjectDir: project,
		Cwd: cwd, FirstPrompt: "fix checkout", LastActive: oldTime, ModTime: oldTime,
	}
}

func setupMemoryMove(t *testing.T) (root string, src, dst config.ConfigDir) {
	t.Helper()
	stubAlive(t)
	prev := now
	now = func() time.Time { return baseTime }
	t.Cleanup(func() { now = prev })
	root = t.TempDir()
	src = configDir(t, root, "sixtynine")
	dst = configDir(t, root, "julienning3")
	emptyRegistry(t, dst.Dir)
	return root, src, dst
}

// Claude keys auto-memory by the repository's main worktree, so a session
// from a worktree or a subdirectory brings projects/<enc(main)>/memory.
func TestMoveMergesMemoryOfTheGitRoot(t *testing.T) {
	for _, where := range []string{"worktree", "subdirectory"} {
		t.Run(where, func(t *testing.T) {
			root, src, dst := setupMemoryMove(t)
			l := newRepoLayout(t, filepath.Join(root, "code"))
			cwd := l.worktree
			if where == "subdirectory" {
				cwd = l.sub
			}
			s := memorySession(t, src, cwd, EncodeCwd(cwd))

			rootMem := filepath.Join(src.Dir, "projects", EncodeCwd(l.main), "memory")
			writeFile(t, filepath.Join(rootMem, "MEMORY.md"), "- [Checkout](checkout.md) — flow\n", 0o644, oldTime)
			writeFile(t, filepath.Join(rootMem, "checkout.md"), "uses stripe", 0o644, oldTime)
			// Memory under the session's own folder is not what Claude reads
			// for this project; it must not be taken as the project's.
			decoy := filepath.Join(src.Dir, "projects", EncodeCwd(cwd), "memory")
			writeFile(t, filepath.Join(decoy, "stale.md"), "stale", 0o644, oldTime)

			rep, err := Move(s, dst)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(rep.Memory.Added, ","); got != "MEMORY.md,checkout.md" {
				t.Errorf("added = %q (warnings %v)", got, rep.Warnings)
			}
			dm := filepath.Join(dst.Dir, "projects", EncodeCwd(l.main), "memory")
			if raw, err := os.ReadFile(filepath.Join(dm, "checkout.md")); err != nil || string(raw) != "uses stripe" {
				t.Errorf("git root memory not merged: %q %v", raw, err)
			}
			if _, err := os.Stat(filepath.Join(dst.Dir, "projects", EncodeCwd(cwd), "memory")); err == nil {
				t.Error("memory merged into the session's folder instead of the git root's")
			}
			if _, err := os.Stat(filepath.Join(rootMem, "checkout.md")); err != nil {
				t.Errorf("source memory must stay: %v", err)
			}
		})
	}
}

// Outside a repository the session's own folder holds the memory.
func TestMoveMergesMemoryOutsideARepo(t *testing.T) {
	root, src, dst := setupMemoryMove(t)
	cwd := filepath.Join(root, "scratch")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	s := memorySession(t, src, cwd, EncodeCwd(cwd))
	writeFile(t, filepath.Join(src.Dir, "projects", EncodeCwd(cwd), "memory", "a.md"), "a", 0o644, oldTime)
	rep, err := Move(s, dst)
	if err != nil || strings.Join(rep.Memory.Added, ",") != "a.md" {
		t.Fatalf("rep = %+v, %v", rep, err)
	}
}

// Encoded roots over 200 characters carry a hash suffix we cannot compute;
// the folder is found by prefix in the source and keeps its name in the
// target.
func TestMoveMergesMemoryOfALongGitRoot(t *testing.T) {
	root, src, dst := setupMemoryMove(t)
	long := filepath.Join(root, strings.Repeat("a", 90), strings.Repeat("b", 90))
	l := newRepoLayout(t, long)
	if len(EncodeCwd(l.main)) <= maxEncodedLen {
		t.Fatalf("fixture root too short: %d", len(EncodeCwd(l.main)))
	}
	prefix := EncodeCwd(l.main)[:maxEncodedLen]
	// Both the session's folder and the root's share the prefix; only the
	// root's has memory/.
	sessFolder := prefix + "-k2x9"
	rootFolder := prefix + "-1q7z"
	s := memorySession(t, src, l.worktree, sessFolder)
	writeFile(t, filepath.Join(src.Dir, "projects", rootFolder, "memory", "a.md"), "a", 0o644, oldTime)

	rep, err := Move(s, dst)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Memory.Added, ",") != "a.md" || len(rep.Warnings) > 0 {
		t.Fatalf("rep = %+v", rep)
	}
	if _, err := os.Stat(filepath.Join(dst.Dir, "projects", rootFolder, "memory", "a.md")); err != nil {
		t.Errorf("not merged under the same folder name: %v", err)
	}

	// Two candidates with memory: ambiguous, so nothing is merged and the
	// user is told.
	root2, src2, dst2 := setupMemoryMove(t)
	l2 := newRepoLayout(t, filepath.Join(root2, strings.Repeat("a", 90), strings.Repeat("b", 90)))
	prefix2 := EncodeCwd(l2.main)[:maxEncodedLen]
	s2 := memorySession(t, src2, l2.worktree, prefix2+"-k2x9")
	writeFile(t, filepath.Join(src2.Dir, "projects", prefix2+"-1q7z", "memory", "a.md"), "a", 0o644, oldTime)
	writeFile(t, filepath.Join(src2.Dir, "projects", prefix2+"-k2x9", "memory", "b.md"), "b", 0o644, oldTime)
	rep, err = Move(s2, dst2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Memory.Added) != 0 || len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "several project folders") {
		t.Errorf("ambiguous: %+v", rep)
	}
}
