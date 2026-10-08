package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeRecord stores rec as dir's patches.json record, the way an older
// julienning left it.
func writeRecord(t *testing.T, root, dir string, rec patchRecord) {
	t.Helper()
	key, err := ResolvedSettingsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(patchFileFormat{Version: patchesVersion, Files: map[string]*patchRecord{key: &rec}}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "jl"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "jl", PatchesFile), append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// readRecord returns dir's patches.json record.
func readRecord(t *testing.T, dir string) patchRecord {
	t.Helper()
	store, err := loadPatches()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ResolvedSettingsPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := store.Files[key]
	if !ok {
		t.Fatalf("no patches.json record for %s", key)
	}
	return *rec
}

// oldLayout is userSettings as a julienning without the StopFailure hook
// patched it: its statusLine replaced, julienning's SessionStart group
// appended to the user's, and a SessionEnd array it created.
const oldLayout = `{
  "model": "opus",
  "ratio": 1.50,
  "big": 1e3,
  "note": "çağrı \"quoted\" é",
  "statusLine": {
    "type": "command",
    "command": "/usr/local/bin/julienning statusline"
  },
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          { "type": "command", "command": "echo hi" }
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
    ]
  },
  "env": {"A": "1"}
}
`

// Re-running setup after an upgrade adds only the StopFailure entry: every
// other byte stays, the result is "updated", and the new array is recorded
// as created so Unpatch (forget, uninstall) takes it away again and restores
// the user's original file byte for byte.
func TestPatchUpgradeAddsOnlyStopFailure(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	write(t, dir, oldLayout)
	prior := `{"type": "command", "command": "~/.claude/statusline.sh", "padding": 0}`
	writeRecord(t, root, dir, patchRecord{StatusLine: &prior, CreatedEvents: []string{"SessionEnd"}})

	res, err := Patch(dir, exe)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != Updated || res.ReplacedStatusLine != "" {
		t.Fatalf("upgrade = %+v, want updated without replacing a statusLine", res)
	}
	want := strings.Replace(oldLayout, `            "command": "/usr/local/bin/julienning hook session-end"
          }
        ]
      }
    ]
  },`, `            "command": "/usr/local/bin/julienning hook session-end"
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
	p := filepath.Join(dir, SettingsFile)
	if got := readFile(t, p); got != want {
		t.Fatalf("upgraded =\n%s\nwant\n%s", got, want)
	}
	rec := readRecord(t, dir)
	if !slices.Equal(rec.CreatedEvents, []string{"SessionEnd", "StopFailure"}) || rec.CreatedHooks || rec.CreatedFile ||
		rec.StatusLine == nil || *rec.StatusLine != prior {
		t.Fatalf("record = %+v, want SessionEnd and StopFailure created and the user's statusLine kept", rec)
	}

	res, err = Patch(dir, exe)
	if err != nil || res.Action != Unchanged {
		t.Fatalf("re-run after the upgrade = %v, %v", res.Action, err)
	}

	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Changes, ", "); got != "restored previous statusLine, removed SessionStart hook, removed SessionEnd hook, removed StopFailure hook" {
		t.Fatalf("changes = %q", got)
	}
	if got := readFile(t, p); got != userSettings {
		t.Fatalf("unpatch after the upgrade did not restore the original:\n%s", got)
	}
}

// When an older julienning created the hooks object itself, the upgrade's
// StopFailure array joins the record and Unpatch removes the whole object.
func TestPatchUpgradeOfCreatedHooksUnpatchesCleanly(t *testing.T) {
	root := isolate(t)
	dir := t.TempDir()
	const original = "{\n  \"model\": \"opus\"\n}\n"
	old := "{\n  \"model\": \"opus\",\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"/old/julienning statusline\"\n  },\n" +
		"  \"hooks\": {\n    \"SessionStart\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n" +
		"            \"command\": \"/old/julienning hook session-start\"\n          }\n        ]\n      }\n    ],\n" +
		"    \"SessionEnd\": [\n      {\n        \"hooks\": [\n          {\n            \"type\": \"command\",\n" +
		"            \"command\": \"/old/julienning hook session-end\"\n          }\n        ]\n      }\n    ]\n  }\n}\n"
	write(t, dir, old)
	writeRecord(t, root, dir, patchRecord{CreatedHooks: true, CreatedEvents: []string{"SessionEnd", "SessionStart"}})

	res, err := Patch(dir, exe)
	if err != nil || res.Action != Updated {
		t.Fatalf("upgrade = %v, %v", res.Action, err)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	if strings.Contains(body, "/old/") || strings.Count(body, exe+" hook stop-failure") != 1 || !strings.Contains(body, `"matcher": "rate_limit"`) {
		t.Fatalf("upgraded =\n%s", body)
	}
	if g := hookGroups(t, body); g["SessionStart"] != 1 || g["SessionEnd"] != 1 || g["StopFailure"] != 1 {
		t.Fatalf("hook groups = %v", g)
	}
	if rec := readRecord(t, dir); !rec.CreatedHooks || !slices.Equal(rec.CreatedEvents, []string{"SessionEnd", "SessionStart", "StopFailure"}) {
		t.Fatalf("record = %+v", rec)
	}

	res, err = Unpatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Changes, ", "); got != "removed statusLine, removed SessionStart hook, removed SessionEnd hook, removed StopFailure hook, removed empty hooks" {
		t.Fatalf("changes = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != original {
		t.Fatalf("got %q, want %q", got, original)
	}
	if patchesRecords(t) != 0 {
		t.Fatal("patches.json still holds a record")
	}
}

// A StopFailure array of the user's own gets julienning's group appended,
// with the matcher; Unpatch takes only that group away.
func TestPatchStopFailureNextToTheUsersGroups(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	const original = `{"hooks":{"StopFailure":[{"matcher":"server_error","hooks":[{"type":"command","command":"notify.sh"}]}]}}`
	write(t, dir, original)
	if _, err := Patch(dir, exe); err != nil {
		t.Fatal(err)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	want := `"StopFailure":[{"matcher":"server_error","hooks":[{"type":"command","command":"notify.sh"}]},` +
		`{"matcher":"rate_limit","hooks":[{"type":"command","command":"` + exe + ` hook stop-failure"}]}]`
	if !strings.Contains(body, want) {
		t.Fatalf("patched =\n%s\nwant it to contain\n%s", body, want)
	}
	if _, err := Unpatch(dir); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dir, SettingsFile)); got != original {
		t.Fatalf("got %s", got)
	}
}

// An existing julienning stop-failure entry, in whatever group the user moved
// it to, is updated in place: no second group, the user's matcher stays.
func TestPatchUpdatesStopFailureInPlace(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	write(t, dir, `{"hooks":{"StopFailure":[{"matcher":"rate_limit|server_error","hooks":[{"type":"command","command":"/old/julienning hook stop-failure","timeout":5}]}]}}`)
	res, err := Patch(dir, exe)
	if err != nil || res.Action != Updated {
		t.Fatalf("patch = %v, %v", res.Action, err)
	}
	body := readFile(t, filepath.Join(dir, SettingsFile))
	want := `"StopFailure":[{"matcher":"rate_limit|server_error","hooks":[{"type":"command","command":"` + exe + ` hook stop-failure","timeout":5}]}]`
	if !strings.Contains(body, want) || strings.Count(body, "stop-failure") != 1 {
		t.Fatalf("patched =\n%s\nwant it to contain\n%s", body, want)
	}
}
