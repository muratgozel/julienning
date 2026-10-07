package config

import (
	"os"
	"strings"
	"testing"
)

func newSaved(t *testing.T, r Remote) {
	t.Helper()
	c, err := New("murat")
	if err != nil {
		t.Fatal(err)
	}
	c.Remote = r
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
}

func readConfig(t *testing.T) string {
	t.Helper()
	p, _ := Path(ConfigFile)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestEnvOverridesAreNotPersisted(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	newSaved(t, Remote{URL: "https://file.example", Token: "file-token"})
	t.Setenv("JULIENNING_REMOTE_URL", "https://env.example")
	t.Setenv("JULIENNING_TOKEN", "env-token")

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Remote.URL != "https://env.example" || c.Remote.Token != "env-token" {
		t.Fatalf("env should win at runtime: %+v", c.Remote)
	}
	c.Decline("Me@Personal.com")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got := readConfig(t)
	if strings.Contains(got, "env-token") || strings.Contains(got, "env.example") || !strings.Contains(got, "file-token") {
		t.Fatalf("env values leaked into config.json:\n%s", got)
	}

	// An explicit change (setup --remote-url) is persisted.
	c.Remote = Remote{URL: "https://new.example", Token: "new-token"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t); !strings.Contains(got, "new-token") {
		t.Fatalf("explicit change not saved:\n%s", got)
	}
}

func TestEnvOverrideForOneFieldNotPersistedWhenOtherChanges(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	newSaved(t, Remote{URL: "https://file.example", Token: "OLD"})
	t.Setenv("JULIENNING_TOKEN", "SECRET")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	c.Remote.URL = "https://new.example"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got := readConfig(t)
	if strings.Contains(got, "SECRET") || !strings.Contains(got, "OLD") || !strings.Contains(got, "new.example") {
		t.Fatalf("per-field persistence wrong:\n%s", got)
	}
}

func TestDeclineUndecline(t *testing.T) {
	c := &Config{}
	c.Decline("A@x.io")
	c.Decline("a@x.io")
	if !c.Declined("a@X.io") || len(c.DeclinedEmails) != 1 {
		t.Fatalf("%v", c.DeclinedEmails)
	}
	c.Undecline("A@X.IO")
	if c.Declined("a@x.io") || len(c.DeclinedEmails) != 0 {
		t.Fatalf("%v", c.DeclinedEmails)
	}
}

func TestValidNamePrefix(t *testing.T) {
	for _, s := range []string{"julienning", "team", "Team_x", "a-", "9lives_", "x"} {
		if !ValidNamePrefix(s) {
			t.Errorf("%q rejected", s)
		}
	}
	// A trailing digit would make <prefix><N> ambiguous (abc1 + 2 = abc12).
	for _, s := range []string{"", "abc1", "team-2", "0", "-x", "_x", "a b", "a.b", "çay"} {
		if ValidNamePrefix(s) {
			t.Errorf("%q accepted", s)
		}
	}
}

func TestPrefixDefaultAndPersistence(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	newSaved(t, Remote{})
	if got := readConfig(t); strings.Contains(got, "name_prefix") {
		t.Fatalf("default prefix written to config.json:\n%s", got)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix() != DefaultNamePrefix || DefaultNamePrefix != "julienning" {
		t.Fatalf("Prefix() = %q", c.Prefix())
	}
	c.NamePrefix = "team"
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t); !strings.Contains(got, `"name_prefix": "team"`) {
		t.Fatalf("prefix not saved:\n%s", got)
	}
	if c, err = Load(); err != nil || c.Prefix() != "team" {
		t.Fatalf("Prefix() = %q, %v", c.Prefix(), err)
	}

	c.NamePrefix = "team1"
	if err := c.Save(); err == nil || !strings.Contains(err.Error(), "invalid name_prefix") {
		t.Fatalf("invalid prefix saved: %v", err)
	}
	p, _ := Path(ConfigFile)
	raw := strings.Replace(readConfig(t), `"name_prefix": "team"`, `"name_prefix": "abc1"`, 1)
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `invalid name_prefix "abc1"`) {
		t.Fatalf("Load accepted an invalid prefix: %v", err)
	}
}

func TestRename(t *testing.T) {
	c := &Config{}
	for _, cd := range []ConfigDir{{Name: "julienning1", Dir: "/h/.claude-a1"}, {Name: "julienning2", Dir: "/h/.claude-a2"}} {
		if err := c.Add(cd); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Rename("julienning2", "alpha"); err != nil {
		t.Fatal(err)
	}
	if c.Configs[0].Name != "alpha" || c.Configs[0].Dir != "/h/.claude-a2" {
		t.Fatalf("not renamed or not re-sorted: %+v", c.Configs)
	}
	cases := map[[2]string]string{
		{"alpha", "julienning1"}: `config name "julienning1" is already used by /h/.claude-a1`,
		{"alpha", "alpha"}:       `config name "alpha" is already used by /h/.claude-a2`,
		{"alpha", "bad name"}:    `invalid config name "bad name"`,
		{"nope", "beta"}:         `config "nope" is not registered`,
	}
	for args, want := range cases {
		if err := c.Rename(args[0], args[1]); err == nil || err.Error() != want {
			t.Errorf("Rename(%q, %q) = %v, want %q", args[0], args[1], err, want)
		}
	}
}

func TestAutoUpdateDefaultAndPersistence(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(EnvAutoUpdate, "")
	newSaved(t, Remote{})
	if got := readConfig(t); strings.Contains(got, "auto_update") {
		t.Fatalf("default auto_update written to config.json:\n%s", got)
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if on, err := c.AutoUpdateEnabled(); !on || err != nil {
		t.Fatalf("default AutoUpdateEnabled = %v, %v; want on", on, err)
	}

	off := false
	c.AutoUpdate = &off
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if got := readConfig(t); !strings.Contains(got, `"auto_update": false`) {
		t.Fatalf("auto_update not saved:\n%s", got)
	}
	if c, err = Load(); err != nil {
		t.Fatal(err)
	}
	if on, err := c.AutoUpdateEnabled(); on || err != nil {
		t.Fatalf("after auto_update false: %v, %v; want off", on, err)
	}
	// The env var only switches off; it never overrides a false in the file.
	t.Setenv(EnvAutoUpdate, "1")
	if on, err := c.AutoUpdateEnabled(); on || err != nil {
		t.Fatalf("env 1 over auto_update false: %v, %v; want off", on, err)
	}

	p, _ := Path(ConfigFile)
	raw := strings.Replace(readConfig(t), `"auto_update": false`, `"auto_update": "no"`, 1)
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "auto_update") {
		t.Fatalf("Load accepted a non-boolean auto_update: %v", err)
	}
}

func TestAutoUpdateEnv(t *testing.T) {
	on := true
	for _, c := range []*Config{nil, {}, {AutoUpdate: &on}} {
		for v, want := range map[string]bool{"": true, "1": true, "true": true, " TRUE ": true, "0": false, "false": false, "False": false, "f": false} {
			t.Setenv(EnvAutoUpdate, v)
			if got, err := c.AutoUpdateEnabled(); got != want || err != nil {
				t.Errorf("config %+v, %s=%q: got %v, %v; want %v", c, EnvAutoUpdate, v, got, err, want)
			}
		}
	}
	t.Setenv(EnvAutoUpdate, "banana")
	got, err := (&Config{}).AutoUpdateEnabled()
	if got || err == nil || !strings.Contains(err.Error(), `JULIENNING_AUTO_UPDATE="banana" is not a boolean`) {
		t.Fatalf("invalid env: got %v, %v; want off with an error", got, err)
	}
}
