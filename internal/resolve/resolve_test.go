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

func fixture(t *testing.T) (*config.Config, *sharedcache.Cache, config.ConfigDir, config.ConfigDir) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("JULIENNING_HOME", filepath.Join(home, ".julienning"))
	mk := func(name, email string) config.ConfigDir {
		d := filepath.Join(home, ".claude-"+name)
		os.MkdirAll(d, 0o700)
		if email != "" {
			os.WriteFile(filepath.Join(d, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600)
		}
		return config.ConfigDir{Name: name, Dir: d}
	}
	a := mk("julienning1", "claude1@x.io")
	b := mk("julienning2", "claude1@x.io") // same account twice
	c := mk("julienning3", "")
	cfg := &config.Config{Configs: []config.ConfigDir{a, b, c}}
	cache, err := sharedcache.SaveEntries([]sharedcache.Entry{{Email: "claude1@x.io", Nickname: "alpha"}, {Email: "claude9@x.io", Nickname: "nine"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cfg, cache, a, b
}

func TestTargetNicknameEmailDir(t *testing.T) {
	cfg, cache, a, b := fixture(t)
	m, err := Target(cfg, cache, "ALPHA", nil)
	if err != nil || m.Dir.Name != a.Name || m.Via != ViaNickname || m.Email != "claude1@x.io" || m.Nickname != "alpha" {
		t.Fatalf("nickname: %+v %v", m, err)
	}
	m, err = Target(cfg, cache, "claude1@x.io", &b)
	if err != nil || m.Dir.Name != b.Name || m.Via != ViaEmail {
		t.Fatalf("email prefers current: %+v %v", m, err)
	}
	m, err = Target(cfg, cache, "julienning3", nil)
	if err != nil || m.Via != ViaDir || m.Email != "" {
		t.Fatalf("dir: %+v %v", m, err)
	}
	var nl *NotLocalError
	if _, err := Target(cfg, cache, "nine", nil); !errors.As(err, &nl) || nl.Nickname != "nine" {
		t.Fatalf("not local: %v", err)
	}
	if _, err := Target(cfg, cache, "nobody", nil); err == nil {
		t.Fatal("unknown must fail")
	}
	if _, err := Target(cfg, cache, "ghost@x.io", nil); err == nil || !strings.Contains(err.Error(), "no registered config dir on this machine is logged in as ghost@x.io") {
		t.Fatalf("email not local wording: %v", err)
	}
	if _, err := Target(cfg, nil, "alpha", nil); err == nil {
		t.Fatal("nil cache: nickname unknown, not a dir → error")
	}
}

func TestLabel(t *testing.T) {
	cfg, cache, a, _ := fixture(t)
	_ = cfg
	if Label(cache, a, "claude1@x.io") != "alpha" || Label(cache, a, "other@x.io") != a.Name || Label(nil, a, "claude1@x.io") != a.Name {
		t.Fatal("label")
	}
}
