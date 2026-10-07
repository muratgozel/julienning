package sessions

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/livesess"
)

// Fixtures follow the shapes Claude Code 2.1.28x writes (see SPEC "Session
// listing"); they are synthetic, never copied from a real config dir.

const (
	projCwd = "/Users/murat/Code/sixtynine/sixtynine-commerce/.claude/worktrees/app-hook"
	projEnc = "-Users-murat-Code-sixtynine-sixtynine-commerce--claude-worktrees-app-hook"
	sid1    = "0b7f6c1e-2a4d-4e8b-9f10-1c2d3e4f5a6b"
	sid2    = "1c8a7d2f-3b5e-4f9c-8a21-2d3e4f5a6b7c"
	sid3    = "2d9b8e30-4c6f-4a0d-9b32-3e4f5a6b7c8d"
)

var baseTime = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func tsAt(d time.Duration) string { return baseTime.Add(d).Format("2006-01-02T15:04:05.000Z") }

// userLine is a typed prompt as Claude Code records it.
func userLine(t testing.TB, content any, at time.Duration, extra map[string]any) string {
	t.Helper()
	e := map[string]any{
		"parentUuid":  nil,
		"isSidechain": false,
		"promptId":    "p-1",
		"type":        "user",
		"message":     map[string]any{"role": "user", "content": content},
		"uuid":        "u-" + at.String(),
		"timestamp":   tsAt(at),
		"userType":    "external",
		"entrypoint":  "cli",
		"cwd":         projCwd,
		"sessionId":   sid1,
		"version":     "2.1.283",
		"gitBranch":   "main",
		"origin":      nil,
	}
	for k, v := range extra {
		e[k] = v
	}
	return mustJSON(t, e)
}

func assistantLine(t testing.TB, text string, at time.Duration) string {
	t.Helper()
	return mustJSON(t, map[string]any{
		"parentUuid": "x", "isSidechain": false, "type": "assistant",
		"message":   map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}},
		"uuid":      "a-" + at.String(),
		"timestamp": tsAt(at), "cwd": projCwd, "sessionId": sid1,
	})
}

func metaLine(t testing.TB, typ, key, val string) string {
	t.Helper()
	return mustJSON(t, map[string]any{"type": typ, key: val, "sessionId": sid1})
}

// writeFile writes content with mode and mtime, creating parents.
func writeFile(t testing.TB, path, content string, mode os.FileMode, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if !mtime.IsZero() {
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
}

// writeSession writes projects/<enc>/<id>.jsonl in dir.
func writeSession(t testing.TB, dir, enc, id string, mtime time.Time, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, "projects", enc, id+".jsonl")
	writeFile(t, p, strings.Join(lines, "\n")+"\n", 0o600, mtime)
	return p
}

func configDir(t testing.TB, root, name string) config.ConfigDir {
	t.Helper()
	d := filepath.Join(root, ".claude-"+name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return config.ConfigDir{Name: name, Dir: d}
}

// stubAlive makes every registry entry count as a running process.
func stubAlive(t *testing.T) {
	t.Helper()
	prev := livesess.Alive
	livesess.Alive = func(livesess.Session) bool { return true }
	t.Cleanup(func() { livesess.Alive = prev })
}

// writeRegistry records sessionID as open in dir (sessions/<pid>.json).
func writeRegistry(t testing.TB, dir string, pid int, sessionID string) {
	t.Helper()
	body := mustJSON(t, map[string]any{
		"pid": pid, "sessionId": sessionID, "cwd": projCwd, "name": "", "status": "idle",
		"procStart": "", "startedAt": baseTime.UnixMilli(), "updatedAt": baseTime.UnixMilli(), "kind": "interactive",
	})
	writeFile(t, filepath.Join(dir, "sessions", fmt.Sprintf("%d.json", pid)), body, 0o600, time.Time{})
}

// emptyRegistry creates sessions/ so the dir counts as having a registry.
func emptyRegistry(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every path under root to mode, size, mtime and content hash.
func snapshot(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		desc := fi.Mode().String()
		if fi.Mode().IsRegular() {
			hash := "unreadable"
			if raw, err := os.ReadFile(p); err == nil {
				sum := sha256.Sum256(raw)
				hash = hex.EncodeToString(sum[:8])
			}
			desc += fmt.Sprintf(" %d %s %s", fi.Size(), fi.ModTime().UTC().Format(time.RFC3339Nano), hash)
		} else if fi.IsDir() {
			desc += " " + fi.ModTime().UTC().Format(time.RFC3339Nano)
		}
		out[rel] = desc
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnapshots(a, b map[string]string) []string {
	var d []string
	for k, v := range a {
		if w, ok := b[k]; !ok {
			d = append(d, "- "+k)
		} else if w != v {
			d = append(d, "~ "+k+": "+v+" → "+w)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			d = append(d, "+ "+k)
		}
	}
	sort.Strings(d)
	return d
}

// temporaries lists leftover temp files/dirs under root.
func temporaries(t testing.TB, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".julienning-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// withoutDirTimes drops directory mtimes: creating and removing temporaries
// necessarily touches the target's parent dirs, which is not a leftover.
func withoutDirTimes(snap map[string]string) map[string]string {
	out := make(map[string]string, len(snap))
	for k, v := range snap {
		if strings.HasPrefix(v, "d") {
			v = strings.Fields(v)[0]
		}
		out[k] = v
	}
	return out
}
