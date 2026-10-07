package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readString(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// One topic file gets one index entry: a source line is not appended when
// its link points at a file that conflicted or that the target already
// indexes under different wording.
func TestMergeIndexSkipsLinesForConflictedOrAlreadyLinkedFiles(t *testing.T) {
	root := t.TempDir()
	sm, dm := filepath.Join(root, "src", "memory"), filepath.Join(root, "dst", "memory")
	writeFile(t, filepath.Join(sm, "MEMORY.md"), strings.Join([]string{
		"# Memory",
		"- [Stripe](stripe.md) — payments via Stripe",          // conflicted
		"- [Deploy](./deploy.md) — deploys go through CI",      // target links deploy.md already
		"- [Kotlin](<kotlin build.md>) — gradle quirks",        // new, link with spaces
		"- [Kotlin again](kotlin%20build.md) — duplicate",      // same file as the line above
		"- [Topics](topics/api.md#auth) — api auth notes",      // new, nested with anchor
		"- [Docs](https://example.com/docs) — external",        // URLs are not memory files
		"- [Docs](https://example.com/docs) — external, again", // different wording, same URL: kept
		"Plain note without a link",
	}, "\n")+"\n", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "stripe.md"), "source version", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "deploy.md"), "deploy notes", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "kotlin build.md"), "kotlin", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "topics", "api.md"), "api", 0o644, oldTime)

	writeFile(t, filepath.Join(dm, "MEMORY.md"), "# Memory\n- [Deploy](deploy.md) — how we deploy\n", 0o644, oldTime)
	writeFile(t, filepath.Join(dm, "stripe.md"), "target version", 0o644, oldTime)
	writeFile(t, filepath.Join(dm, "deploy.md"), "deploy notes", 0o644, oldTime)

	rep, err := mergeMemory(sm, dm)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Conflicts, ",") != "stripe.md" {
		t.Errorf("conflicts = %v", rep.Conflicts)
	}
	want := strings.Join([]string{
		"# Memory",
		"- [Deploy](deploy.md) — how we deploy",
		"- [Kotlin](<kotlin build.md>) — gradle quirks",
		"- [Topics](topics/api.md#auth) — api auth notes",
		"- [Docs](https://example.com/docs) — external",
		"- [Docs](https://example.com/docs) — external, again",
		"Plain note without a link",
	}, "\n") + "\n"
	if got := readString(t, filepath.Join(dm, "MEMORY.md")); got != want {
		t.Errorf("MEMORY.md =\n%s\nwant\n%s", got, want)
	}
	if rep.IndexLines != 5 {
		t.Errorf("index lines = %d", rep.IndexLines)
	}
	if readString(t, filepath.Join(dm, "stripe.md")) != "target version" {
		t.Error("conflicted file overwritten")
	}
}

func TestIndexLink(t *testing.T) {
	cases := map[string]string{
		"- [A](a.md) — a":              "a.md",
		"- [A](./a.md)":                "a.md",
		"- [A]( a.md \"title\")":       "a.md",
		"- [A](<a b.md>)":              "a b.md",
		"- [A](a%20b.md)":              "a b.md",
		"- [A](sub/../a.md#x)":         "a.md",
		"- [A](a.md) see [B](b.md)":    "a.md",
		"- [Web](https://x.io/a.md)":   "",
		"- [Mail](mailto:a@b.c)":       "",
		"- [Anchor](#section)":         "",
		"no link here":                 "",
		"- [broken](":                  "",
		"- [Abs](/Users/m/notes/x.md)": "/Users/m/notes/x.md",
	}
	for line, want := range cases {
		if got := indexLink(line); got != want {
			t.Errorf("indexLink(%q) = %q, want %q", line, got, want)
		}
	}
}

// A MEMORY.md that is a symlink (e.g. one index shared by several config
// dirs) stays a symlink; the file it points to gets the new lines.
func TestMergeIndexFollowsASymlinkedIndex(t *testing.T) {
	root := t.TempDir()
	sm, dm := filepath.Join(root, "src", "memory"), filepath.Join(root, "dst", "memory")
	shared := filepath.Join(root, "shared", "MEMORY.md")
	writeFile(t, shared, "- [A](a.md) — a\n", 0o640, oldTime)
	if err := os.MkdirAll(dm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "shared", "MEMORY.md"), filepath.Join(dm, "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sm, "MEMORY.md"), "- [A](a.md) — a\n- [B](b.md) — b\n", 0o644, oldTime)
	writeFile(t, filepath.Join(sm, "b.md"), "b", 0o644, oldTime)

	rep, err := mergeMemory(sm, dm)
	if err != nil || rep.IndexLines != 1 {
		t.Fatalf("rep = %+v, %v", rep, err)
	}
	fi, err := os.Lstat(filepath.Join(dm, "MEMORY.md"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("MEMORY.md is no longer a symlink: %v %v", fi.Mode(), err)
	}
	if got := readString(t, shared); got != "- [A](a.md) — a\n- [B](b.md) — b\n" {
		t.Errorf("shared index = %q", got)
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o640 {
		t.Errorf("shared index mode = %v, want 0640 kept", fi.Mode().Perm())
	}
	if tmp := temporaries(t, root); len(tmp) > 0 {
		t.Errorf("temporaries: %v", tmp)
	}
}
