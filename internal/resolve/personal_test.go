package resolve

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// personalHome is a temp HOME with julienning set up and helpers to log
// config dirs into accounts. ~/.claude is the default dir: its login lives in
// ~/.claude.json.
type personalHome struct {
	t    *testing.T
	home string
}

func newPersonalHome(t *testing.T, registered ...config.ConfigDir) *personalHome {
	t.Helper()
	home := t.TempDir()
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	t.Setenv("HOME", home)
	t.Setenv("JULIENNING_HOME", filepath.Join(home, ".julienning"))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("JULIENNING_REMOTE_URL", "")
	t.Setenv("JULIENNING_TOKEN", "")
	cfg := &config.Config{Version: config.SchemaVersion, Dev: "murat", MachineID: "3fa9c2d1e07b", Configs: registered}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return &personalHome{t: t, home: home}
}

// login logs dir (relative to HOME) into email; "" logs it out.
func (h *personalHome) login(rel, email string) string {
	h.t.Helper()
	dir := filepath.Join(h.home, rel)
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		h.t.Fatal(err)
	}
	acct := filepath.Join(dir, ".claude.json")
	if rel == ".claude" {
		acct = filepath.Join(h.home, ".claude.json")
	}
	body := `{"oauthAccount":{}}`
	if email != "" {
		body = `{"oauthAccount":{"emailAddress":"` + email + `"}}`
	}
	if err := os.WriteFile(acct, []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return dir
}

func (h *personalHome) cfg() *config.Config {
	h.t.Helper()
	c, err := config.Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return c
}

func TestPersonalFindsTheDefaultDirAndRemembersIt(t *testing.T) {
	h := newPersonalHome(t)
	def := h.login(".claude", "me@personal.io")
	h.login(".claude-team", "claude1@x.io")

	m, err := Personal(h.cfg(), " Me@Personal.io ")
	if err != nil {
		t.Fatal(err)
	}
	if m.Dir != def || m.Email != "me@personal.io" || !m.Changed || m.Moved != "" || len(m.Others) != 0 || m.Name != "" {
		t.Fatalf("match = %+v", m)
	}
	if len(m.Notes()) != 0 {
		t.Errorf("a plain find needs no note: %q", m.Notes())
	}
	if err := m.Remember(); err != nil {
		t.Fatal(err)
	}
	m, err = Personal(h.cfg(), "me@personal.io")
	if err != nil || m.Dir != def || m.Changed {
		t.Fatalf("remembered: %+v %v", m, err)
	}
}

// A remembered dir is used only while its login still matches; when the
// account moved, the search runs again and says where it went.
func TestPersonalSearchesAgainWhenTheLoginMoved(t *testing.T) {
	h := newPersonalHome(t)
	def := h.login(".claude", "me@personal.io")
	m, err := Personal(h.cfg(), "me@personal.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Remember(); err != nil {
		t.Fatal(err)
	}
	h.login(".claude", "someone@else.io")
	moved := h.login(".claude-me", "me@personal.io")

	m, err = Personal(h.cfg(), "me@personal.io")
	if err != nil {
		t.Fatal(err)
	}
	if m.Dir != moved || m.Moved != def || !m.Changed {
		t.Fatalf("match = %+v", m)
	}
	if want := []string{"me@personal.io is no longer logged in at ~/.claude; found it at ~/.claude-me"}; strings.Join(m.Notes(), "|") != want[0] {
		t.Errorf("notes = %q, want %q", m.Notes(), want)
	}
	if err := m.Remember(); err != nil {
		t.Fatal(err)
	}
	if d, _ := h.cfg().PersonalDirOf("me@personal.io"); d != moved {
		t.Errorf("remembered %q, want %q", d, moved)
	}

	// Logged out everywhere: the error names the lost dir and how to sign in.
	h.login(".claude-me", "")
	_, err = Personal(h.cfg(), "me@personal.io")
	var nf *PersonalNotFoundError
	if !errors.As(err, &nf) || nf.Moved != moved {
		t.Fatalf("err = %v", err)
	}
	if want := "me@personal.io is no longer logged in at ~/.claude-me, and no other Claude config dir on this machine is (" + searched + "); sign in again with: CLAUDE_CONFIG_DIR=~/.claude-me command claude"; err.Error() != want {
		t.Errorf("err =\n%s\nwant\n%s", err, want)
	}
}

func TestPersonalNotFoundSaysHowToSignIn(t *testing.T) {
	h := newPersonalHome(t)
	h.login(".claude", "someone@else.io")
	_, err := Personal(h.cfg(), "first+side@example.com")
	want := "no Claude config dir on this machine is logged in as first+side@example.com (" + searched + "); sign in first, for example: CLAUDE_CONFIG_DIR=~/.claude-first-side command claude"
	if err == nil || err.Error() != want {
		t.Fatalf("err =\n%v\nwant\n%s", err, want)
	}

	// The default dir is signed into with the variable unset.
	h = newPersonalHome(t)
	if err := config.RememberPersonalDir("me@personal.io", filepath.Join(h.home, ".claude")); err != nil {
		t.Fatal(err)
	}
	_, err = Personal(h.cfg(), "me@personal.io")
	if err == nil || !strings.HasSuffix(err.Error(), "sign in again with: command claude") {
		t.Fatalf("default dir hint: %v", err)
	}
}

// Several dirs with one login: the default dir first, then by path; the
// others are named so the user knows a choice was made.
func TestPersonalPrefersDiscoveryOrder(t *testing.T) {
	h := newPersonalHome(t)
	b := h.login(".claude-b", "me@personal.io")
	a := h.login(".claude-a", "me@personal.io")
	m, err := Personal(h.cfg(), "me@personal.io")
	if err != nil || m.Dir != a || len(m.Others) != 1 || m.Others[0] != b {
		t.Fatalf("match = %+v %v", m, err)
	}
	if want := "me@personal.io is also logged in at ~/.claude-b; using ~/.claude-a"; strings.Join(m.Notes(), "|") != want {
		t.Errorf("notes = %q", m.Notes())
	}
	def := h.login(".claude", "me@personal.io")
	if m, _ := Personal(h.cfg(), "me@personal.io"); m.Dir != def {
		t.Errorf("the default dir comes first: %+v", m)
	}
}

// A registered dir is found even where discovery does not look (adopted
// from deep inside the home directory), and carries its config name.
func TestPersonalFindsRegisteredDirsOutsideDiscovery(t *testing.T) {
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	deep := filepath.Join(root, "work", "claude", "side")
	h := newPersonalHome(t, config.ConfigDir{Name: "side", Dir: deep})
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@personal.io"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Personal(h.cfg(), "me@personal.io")
	if err != nil || m.Dir != deep || m.Name != "side" {
		t.Fatalf("match = %+v %v", m, err)
	}
}

// Dirs named in shell rc files are searched, like setup does.
func TestPersonalFindsRCFileDirs(t *testing.T) {
	h := newPersonalHome(t)
	dir := filepath.Join(h.home, "elsewhere", "claude-side")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@personal.io"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.home, ".zshrc"), []byte("alias claude-side='CLAUDE_CONFIG_DIR=~/elsewhere/claude-side claude'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, err := Personal(h.cfg(), "me@personal.io"); err != nil || m.Dir != dir {
		t.Fatalf("match = %+v %v", m, err)
	}
}

// An account file that cannot be parsed is an error, not a silent search
// elsewhere; a remembered unsafe path ($HOME) is never trusted.
func TestPersonalRememberedDirEdgeCases(t *testing.T) {
	h := newPersonalHome(t)
	dir := h.login(".claude-me", "me@personal.io")
	if err := config.RememberPersonalDir("me@personal.io", dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Personal(h.cfg(), "me@personal.io"); err == nil || !strings.Contains(err.Error(), "cannot read the login in ~/.claude-me") {
		t.Fatalf("unreadable: %v", err)
	}

	h = newPersonalHome(t)
	if err := os.WriteFile(filepath.Join(h.home, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"me@personal.io"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// $HOME/.claude.json would make $HOME itself "hold" the login.
	if err := config.RememberPersonalDir("me@personal.io", h.home); err != nil {
		t.Fatal(err)
	}
	var nf *PersonalNotFoundError
	if _, err := Personal(h.cfg(), "me@personal.io"); !errors.As(err, &nf) {
		t.Fatalf("an unsafe remembered dir must not be used: %v", err)
	}
}

func TestAccountTeamOrPersonal(t *testing.T) {
	team := config.ConfigDir{Name: "julienning1"}
	h := newPersonalHome(t)
	team.Dir = h.login(".claude-julienning1", "claude1@x.io")
	cfg := h.cfg()
	cfg.Configs = []config.ConfigDir{team}
	def := h.login(".claude", "me@personal.io")
	cache, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: "claude1@x.io", Nickname: "alpha"}, {Email: "claude2@x.io", Nickname: "beta"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	m, pm, err := Account(cfg, cache, "Claude1@x.io", nil)
	if err != nil || pm != nil || m.Dir.Dir != team.Dir || m.Nickname != "alpha" || m.Via != ViaEmail {
		t.Fatalf("team: %+v %+v %v", m, pm, err)
	}
	m, pm, err = Account(cfg, cache, "me@personal.io", nil)
	if err != nil || pm == nil || m.Dir.Dir != def || m.Via != ViaPersonal {
		t.Fatalf("personal: %+v %+v %v", m, pm, err)
	}
	var nl *NotLocalError
	if _, _, err := Account(cfg, cache, "claude2@x.io", nil); !errors.As(err, &nl) {
		t.Fatalf("a team account without a local login is NotLocalError: %v", err)
	}
	// No allowlist known (shared.json missing, unreadable or never fetched):
	// an email logged into a registered dir keeps the team path, as before
	// personal accounts existed; any other email is personal.
	for name, c := range map[string]*sharedcache.Cache{"nil": nil, "never fetched": {}} {
		if m, pm, err := Account(cfg, c, "claude1@x.io", nil); err != nil || pm != nil || m.Dir.Dir != team.Dir || m.Via != ViaEmail {
			t.Errorf("%s cache, team dir: %+v %+v %v", name, m, pm, err)
		}
		if m, pm, err := Account(cfg, c, "me@personal.io", nil); err != nil || pm == nil || m.Dir.Dir != def {
			t.Errorf("%s cache, personal: %+v %+v %v", name, m, pm, err)
		}
		if IsPersonal(cfg, c, "claude1@x.io") || !IsPersonal(cfg, c, "me@personal.io") {
			t.Errorf("%s cache: IsPersonal disagrees with Account", name)
		}
	}
	// A known allowlist decides alone: a registered dir with a personal login
	// is personal, a team account is not.
	if IsPersonal(cfg, cache, "claude1@x.io") || !IsPersonal(cfg, cache, "me@personal.io") {
		t.Error("known allowlist")
	}
}

// A dir whose login cannot be read may hold the account: when nothing else
// does, the error says so instead of claiming the account is nowhere.
func TestPersonalNamesUnreadableLogins(t *testing.T) {
	h := newPersonalHome(t)
	broken := h.login(".claude-me", "me@personal.io")
	if err := os.WriteFile(filepath.Join(broken, ".claude.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Personal(h.cfg(), "me@personal.io")
	var nf *PersonalNotFoundError
	if !errors.As(err, &nf) || len(nf.Unreadable) != 1 || !strings.Contains(err.Error(), "; the login could not be read in ~/.claude-me: ") {
		t.Fatalf("err = %v", err)
	}
	// Found elsewhere: the unreadable dir does not matter.
	good := h.login(".claude-ok", "me@personal.io")
	if m, err := Personal(h.cfg(), "me@personal.io"); err != nil || m.Dir != good {
		t.Fatalf("found elsewhere: %+v %v", m, err)
	}
}
