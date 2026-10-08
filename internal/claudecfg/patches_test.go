package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const patchedFresh = `{
  "statusLine": {
    "type": "command",
    "command": "/usr/local/bin/julienning statusline"
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook session-start"
          }
        ]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook session-end"
          }
        ]
      }
    ],
    "StopFailure": [
      {
        "matcher": "rate_limit",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook stop-failure"
          }
        ]
      }
    ]
  }
}
`

func TestPatchMissingFileAndUnpatchDeletesIt(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Added || res.String() != "added" {
		t.Fatalf("got %v", res.Action)
	}
	p := filepath.Join(dir, SettingsFile)
	if got := readFile(t, p); got != patchedFresh {
		t.Fatalf("settings.json =\n%s\nwant\n%s", got, patchedFresh)
	}

	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Removed {
		t.Fatalf("unpatch = %v", res.Action)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("settings.json julienning created still exists: %v", err)
	}
	if patchesRecords(t) != 0 {
		t.Fatal("patches.json still holds a record")
	}
}

// userSettings is a realistic hand-edited file with a foreign statusLine and
// the user's own hooks, including one on SessionStart.
const userSettings = `{
  "model": "opus",
  "ratio": 1.50,
  "big": 1e3,
  "note": "çağrı \"quoted\" é",
  "statusLine": {"type": "command", "command": "~/.claude/statusline.sh", "padding": 0},
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          { "type": "command", "command": "echo hi" }
        ]
      }
    ],
    "PostToolUse": []
  },
  "env": {"A": "1"}
}
`

func TestPatchPreservesUserBytesAndUnpatchRestoresExactly(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, userSettings)

	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Added || res.ReplacedStatusLine != "~/.claude/statusline.sh" {
		t.Fatalf("got %+v", res)
	}
	got := readFile(t, filepath.Join(dir, SettingsFile))
	want := strings.Replace(userSettings,
		`"statusLine": {"type": "command", "command": "~/.claude/statusline.sh", "padding": 0},`,
		`"statusLine": {
    "type": "command",
    "command": "/usr/local/bin/julienning statusline"
  },`, 1)
	want = strings.Replace(want, `          { "type": "command", "command": "echo hi" }
        ]
      }
    ],
    "PostToolUse": []
  },`, `          { "type": "command", "command": "echo hi" }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook session-start"
          }
        ]
      }
    ],
    "PostToolUse": [],
    "SessionEnd": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook session-end"
          }
        ]
      }
    ],
    "StopFailure": [
      {
        "matcher": "rate_limit",
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/julienning hook stop-failure"
          }
        ]
      }
    ]
  },`, 1)
	if got != want {
		t.Fatalf("patched =\n%s\nwant\n%s", got, want)
	}

	// Idempotent: second run is unchanged and does not rewrite.
	p := filepath.Join(dir, SettingsFile)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	res, err = Patch(dir, exe)
	if err != nil || res.Action != Unchanged {
		t.Fatalf("second patch = %v, %v", res.Action, err)
	}
	if fi, err := os.Stat(p); err != nil || fi.ModTime().After(old.Add(time.Second)) {
		t.Fatal("unchanged patch rewrote the file")
	}

	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	wantChanges := "restored previous statusLine, removed SessionStart hook, removed SessionEnd hook, removed StopFailure hook"
	if strings.Join(res.Changes, ", ") != wantChanges {
		t.Fatalf("changes = %q", res.Changes)
	}
	if got := readFile(t, p); got != userSettings {
		t.Fatalf("unpatch did not restore the original:\n%s", got)
	}

	// Safe twice.
	res, err = Unpatch(dir)
	if err != nil || res.Action != Unchanged || len(res.Changes) != 0 {
		t.Fatalf("second unpatch = %+v, %v", res, err)
	}
	if got := readFile(t, p); got != userSettings {
		t.Fatal("second unpatch changed the file")
	}
}

func TestPatchUpdatesCommandInPlace(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, `{"statusLine":{"type":"command","command":"/old/julienning statusline","padding":2}}`)
	if _, err := Patch(dir, "/old/julienning"); err != nil {
		t.Fatal(err)
	}
	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Updated || res.ReplacedStatusLine != "" {
		t.Fatalf("got %+v", res)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	if !strings.HasPrefix(body, `{"statusLine":{"type":"command","command":"`+exe+` statusline","padding":2},`) {
		t.Fatalf("statusLine not updated in place:\n%s", body)
	}
	if strings.Contains(body, "/old/") {
		t.Fatalf("old command left behind:\n%s", body)
	}
	if n := strings.Count(body, "hook session-start"); n != 1 {
		t.Fatalf("%d session-start hooks, want 1:\n%s", n, body)
	}
	// Our own statusLine was never recorded as a foreign one.
	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != `{}` {
		t.Fatalf("after unpatch: %s (%v)", got, res.Changes)
	}
}

func TestPatchCollapsesDuplicateEntries(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, `{
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/a/julienning hook session-start"}]},
      {"hooks": [{"type": "command", "command": "echo mine"}, {"type": "command", "command": "/b/julienning hook session-start"}]},
      {"hooks": [{"type": "command", "command": "/c/julienning hook session-start"}]}
    ]
  }
}`)
	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Updated {
		t.Fatalf("got %v", res.Action)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string } `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	start := doc.Hooks["SessionStart"]
	if len(start) != 2 {
		t.Fatalf("groups = %d, want 2 (julienning's first + the user's):\n%s", len(start), body)
	}
	if start[0].Hooks[0].Command != exe+" hook session-start" {
		t.Errorf("first entry not updated in place: %q", start[0].Hooks[0].Command)
	}
	if len(start[1].Hooks) != 1 || start[1].Hooks[0].Command != "echo mine" {
		t.Errorf("user group damaged: %+v", start[1])
	}
}

func TestPatchRejectsNonObject(t *testing.T) {
	isolate(t)
	for _, body := range []string{`[1,2,3]`, `"a string"`, `not json`, `{"hooks": []}`, `{"hooks": {"SessionEnd": {}}}`} {
		dir := t.TempDir()
		write(t, dir, body)
		_, err := Patch(dir, exe)
		if err == nil {
			t.Fatalf("%s: want error", body)
		}
		if !strings.Contains(err.Error(), filepath.Join(dir, SettingsFile)) || !strings.Contains(err.Error(), "leaving") {
			t.Errorf("%s: error not actionable: %v", body, err)
		}
		if got := readFile(t, filepath.Join(dir, SettingsFile)); got != body {
			t.Errorf("%s: file was modified", body)
		}
	}
}

func TestPatchEmptyFileTreatedAsObject(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, "  \n")
	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Added {
		t.Fatalf("got %v", res.Action)
	}
	// The file existed, so unpatch leaves an empty object rather than deleting.
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != "{}\n" {
		t.Fatalf("got %q", got)
	}
}

func TestPatchEmptyExe(t *testing.T) {
	isolate(t)
	if _, err := Patch(t.TempDir(), " "); err == nil {
		t.Fatal("want error for empty executable")
	}
}

// v1 only wrote statusLine. Upgrading adds the hooks and reports "updated";
// uninstall then removes the statusLine (nothing recorded to restore).
func TestPatchUpgradesV1(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	v1 := "{\n  \"model\": \"opus\",\n  \"statusLine\": {\n    \"command\": \"/usr/local/bin/julienning statusline\",\n    \"type\": \"command\"\n  }\n}\n"
	write(t, dir, v1)
	res, err := Patch(dir, exe)
	if err != nil || res.Action != Updated {
		t.Fatalf("got %v, %v", res.Action, err)
	}
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != "{\n  \"model\": \"opus\"\n}\n" {
		t.Fatalf("got %q", got)
	}
}

// Containers julienning created but the user has since put hooks into stay.
func TestUnpatchKeepsContainersTheUserFilled(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, "{}\n")
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, SettingsFile)
	body := strings.Replace(readFile(t, p), `"SessionEnd": [`, `"SessionEnd": [
      {"hooks": [{"type": "command", "command": "say bye"}]},`, 1)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"hooks\": {\n    \"SessionEnd\": [\n      {\"hooks\": [{\"type\": \"command\", \"command\": \"say bye\"}]}\n    ]\n  }\n}\n"
	if got := readFile(t, p); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// A statusLine the user switched to after patching is theirs: unpatch keeps
// it and drops the stale saved one.
func TestUnpatchLeavesForeignStatusLine(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, `{"statusLine":{"type":"command","command":"old.sh"}}`)
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, SettingsFile)
	body := strings.Replace(readFile(t, p), exe+" statusline", "new.sh", 1)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != `{"statusLine":{"type":"command","command":"new.sh"}}` {
		t.Fatalf("got %s", got)
	}
	if patchesRecords(t) != 0 {
		t.Fatal("stale record kept")
	}
}

// The record is keyed by the resolved path, so a dir reached through a
// symlink shares it with the real path.
func TestRecordKeyResolvesSymlinkedDir(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	write(t, real, `{"statusLine":{"type":"command","command":"mine.sh"}}`)
	if _, err := Patch(link, exe); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpatch(real); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(real, SettingsFile)); got != `{"statusLine":{"type":"command","command":"mine.sh"}}` {
		t.Fatalf("got %s", got)
	}
}

func TestCorruptPatchesFileBlocksPatch(t *testing.T) {
	root := isolate(t)
	if err := os.MkdirAll(filepath.Join(root, "jl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jl", PatchesFile), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	write(t, dir, `{"statusLine":{"type":"command","command":"mine.sh"}}`)
	_, err := Patch(dir, exe)
	if err == nil || !strings.Contains(err.Error(), PatchesFile) {
		t.Fatalf("got %v", err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); !strings.Contains(got, "mine.sh") {
		t.Fatal("statusLine replaced although it could not be recorded")
	}
}

func TestIsJulienningCommand(t *testing.T) {
	cases := map[string]bool{
		"/usr/local/bin/julienning statusline":               true,
		"julienning hook session-start":                      true,
		"/x/julienning hook session-end  ":                   true,
		"'/Users/John Doe/.local/bin/julienning' statusline": true,
		`"/Users/John Doe/.local/bin/julienning" statusline`: true,
		`/Users/John\ Doe/.local/bin/julienning statusline`:  true,
		"/usr/local/bin/julienning-old statusline":           false,
		"/usr/local/bin/julienning status":                   false,
		"~/.claude/statusline.sh":                            false,
		"bash -c 'julienning statusline'":                    false,
		"/opt/julienning/bin/other hook session-start":       false,
		"": false,
		"/usr/local/bin/julienning hook session-start --json": false,
		"/usr/local/bin/julienning hook stop-failure":         true,
		"'/Users/John Doe/bin/julienning' hook stop-failure":  true,
		"/usr/local/bin/julienning hook stop":                 false,
		"/usr/local/bin/julienning-old hook stop-failure":     false,
		"echo hook stop-failure":                              false,
	}
	for cmd, want := range cases {
		if got := IsJulienningCommand(cmd); got != want {
			t.Errorf("%q: got %v, want %v", cmd, got, want)
		}
	}
}

func TestCommandQuotesUnsafePaths(t *testing.T) {
	cases := map[string]string{
		"/usr/local/bin/julienning":           "/usr/local/bin/julienning statusline",
		"/Users/John Doe/bin/julienning":      "'/Users/John Doe/bin/julienning' statusline",
		"/Users/it's/julienning":              `'/Users/it'\''s/julienning' statusline`,
		"/Users/$HOME/julienning":             "'/Users/$HOME/julienning' statusline",
		"/Users/murat/.local/bin/julienning+": "/Users/murat/.local/bin/julienning+ statusline",
	}
	for in, want := range cases {
		got := Command(in, "statusline")
		if got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
		if strings.HasSuffix(in, "julienning") && !IsJulienningCommand(got) {
			t.Errorf("%q: quoted command fails the ownership test", got)
		}
	}
}

func patchesRecords(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(os.Getenv("JULIENNING_HOME"), PatchesFile))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Files map[string]json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return len(f.Files)
}
