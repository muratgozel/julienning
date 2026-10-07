package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShareOnLoginRoundTripAndValidation(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	c, err := New("murat")
	if err != nil {
		t.Fatal(err)
	}
	for _, cd := range []ConfigDir{
		{Name: "julienning1", Dir: "/h/.claude-julienning1", ShareOnLogin: &ShareOnLogin{}},
		{Name: "julienning2", Dir: "/h/.claude-julienning2", ShareOnLogin: &ShareOnLogin{Nickname: "delta"}},
		{Name: "julienning3", Dir: "/h/.claude-julienning3"},
	} {
		if err := c.Add(cd); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	raw := readConfig(t)
	for _, want := range []string{`"share_on_login": {}`, `"nickname": "delta"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("config.json lacks %s:\n%s", want, raw)
		}
	}
	if n := strings.Count(raw, "share_on_login"); n != 2 {
		t.Errorf("share_on_login written %d times, want 2 (omitted when nil):\n%s", n, raw)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s := got.Configs[0].ShareOnLogin; s == nil || s.Nickname != "" {
		t.Errorf("julienning1 = %+v", s)
	}
	if s := got.Configs[1].ShareOnLogin; s == nil || s.Nickname != "delta" {
		t.Errorf("julienning2 = %+v", s)
	}
	if s := got.Configs[2].ShareOnLogin; s != nil {
		t.Errorf("julienning3 = %+v", s)
	}

	// A hand-edited nickname that could never be shared is refused at load.
	p, _ := Path(ConfigFile)
	if err := os.WriteFile(p, []byte(strings.Replace(raw, `"delta"`, `"Bad Name"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), `config "julienning2": invalid share_on_login nickname "Bad Name"`) {
		t.Fatalf("Load = %v", err)
	}
}

func TestNicknameFor(t *testing.T) {
	var none *ShareOnLogin
	cases := []struct {
		s    *ShareOnLogin
		want string
	}{
		{&ShareOnLogin{Nickname: "delta"}, "delta"},
		{&ShareOnLogin{}, "john-team"},
		{none, "john-team"},
	}
	for _, c := range cases {
		if got := c.s.NicknameFor("John+Team@x.io"); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.s, got, c.want)
		}
	}
	if got := (&ShareOnLogin{}).NicknameFor("+++@x.io"); got != "" {
		t.Errorf("underivable: got %q", got)
	}
}

func TestUpdateDir(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv("JULIENNING_TOKEN", "")
	t.Setenv("JULIENNING_REMOTE_URL", "")
	newSaved(t, Remote{URL: "https://w.example.dev", Token: "file-token"})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join("/h", ".claude-julienning1")
	if err := c.Add(ConfigDir{Name: "julienning1", Dir: dir, ShareOnLogin: &ShareOnLogin{Nickname: "delta"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	// An env override in the updating process must not leak into the file.
	t.Setenv("JULIENNING_TOKEN", "env-token")

	found, err := UpdateDir(dir+"/", func(cd *ConfigDir) { cd.ShareOnLogin = nil })
	if err != nil || !found {
		t.Fatalf("UpdateDir = %v, %v", found, err)
	}
	raw := readConfig(t)
	if strings.Contains(raw, "share_on_login") || strings.Contains(raw, "env-token") || !strings.Contains(raw, "file-token") {
		t.Fatalf("config.json:\n%s", raw)
	}

	found, err = UpdateDir("/h/.claude-gone", func(*ConfigDir) { t.Fatal("mutate called for an unregistered dir") })
	if err != nil || found {
		t.Fatalf("unregistered dir: %v, %v", found, err)
	}
}
