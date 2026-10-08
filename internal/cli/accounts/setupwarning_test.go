package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// olderPatch is settings.json as a julienning without the StopFailure hook
// left it.
const olderPatch = `{
  "statusLine": {"type": "command", "command": "/usr/local/bin/julienning statusline"},
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/usr/local/bin/julienning hook session-start"}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "/usr/local/bin/julienning hook session-end"}]}]
  }
}
`

const setupWarning = "julienning: warning: this julienning adds a rate-limit hook that settings.json of sixtynine1 does not have yet; run: julienning setup\n"

// `accounts` says once when a registered dir needs `julienning setup` for a
// hook this version adds, and stops once setup has patched it. The hidden
// commands and the status line stay quiet.
func TestAccountsWarnsWhenSetupIsDue(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.addConfig("sixtynine2", email2)
	f.save()
	f.share(email1, email2)
	f.fake.Listing = listing()
	if err := os.WriteFile(filepath.Join(cd.Dir, "settings.json"), []byte(olderPatch), 0o644); err != nil {
		t.Fatal(err)
	}

	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if got.stderr != setupWarning {
		t.Errorf("stderr = %q, want %q", got.stderr, setupWarning)
	}
	if !strings.Contains(got.stdout, "alpha") {
		t.Errorf("the listing must still print: %q", got.stdout)
	}

	f.useDir(cd.Dir)
	f.captureSpawn() // the hooks below must not start real processes
	for _, args := range [][]string{{"statusline"}, {"hook", "session-start"}, {"hook", "stop-failure"}} {
		if got := f.run(startInput, args...); got.stderr != "" {
			t.Errorf("%v: stderr = %q", args, got.stderr)
		}
	}

	if _, err := claudecfg.Patch(cd.Dir, "/usr/local/bin/julienning"); err != nil {
		t.Fatal(err)
	}
	got = f.run("", "accounts")
	assertCode(t, got, 0)
	if got.stderr != "" {
		t.Errorf("after setup: stderr = %q", got.stderr)
	}
}

// An unreadable settings.json is setup's to report, not every command's.
func TestAccountsSkipsUnreadableSettings(t *testing.T) {
	f := setup(t)
	cd := f.addConfig("sixtynine1", email1)
	f.save()
	f.share(email1)
	f.fake.Listing = listing()
	if err := os.WriteFile(filepath.Join(cd.Dir, "settings.json"), []byte("{half"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := f.run("", "accounts")
	assertCode(t, got, 0)
	if got.stderr != "" {
		t.Errorf("stderr = %q", got.stderr)
	}
}
