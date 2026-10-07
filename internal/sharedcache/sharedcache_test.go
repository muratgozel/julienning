package sharedcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

func TestSaveLoadContainsStale(t *testing.T) {
	t.Setenv("JULIENNING_HOME", t.TempDir())
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	c, err := Load()
	if err != nil || len(c.Emails) != 0 || !c.Stale(now, MaxAge) {
		t.Fatalf("empty load: %+v %v", c, err)
	}
	if _, err := Save([]string{"B@x.io", "a@x.io", "b@x.io"}, now); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil || len(c.Emails) != 2 || !c.Contains("A@X.io") || c.Contains("c@x.io") {
		t.Fatalf("load: %+v %v", c, err)
	}
	if c.Stale(now.Add(30*time.Minute), MaxAge) || !c.Stale(now.Add(2*time.Hour), MaxAge) {
		t.Fatal("stale boundaries")
	}
}

func TestAddRemoveKeepFetchTime(t *testing.T) {
	t.Setenv("JULIENNING_HOME", t.TempDir())
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	Save([]string{"a@x.io"}, now)
	c, err := Add("B@x.io")
	if err != nil || !c.Contains("b@x.io") || !c.FetchedAt.Equal(now) {
		t.Fatalf("add: %+v %v", c, err)
	}
	c, err = Remove("a@x.io")
	if err != nil || c.Contains("a@x.io") || !c.Contains("b@x.io") || !c.FetchedAt.Equal(now) {
		t.Fatalf("remove: %+v %v", c, err)
	}
}

func TestRefreshFromFake(t *testing.T) {
	t.Setenv("JULIENNING_HOME", t.TempDir())
	f := &remote.Fake{Listing: &remote.Listing{Accounts: []remote.Account{{Email: "claude1@x.io", Nickname: "Alpha"}, {Email: "legacy@x.io"}}}}
	c, _, err := Refresh(context.Background(), f, "murat", time.Now())
	if err != nil || !c.Contains("claude1@x.io") || c.Nickname("claude1@x.io") != "alpha" || c.Nickname("legacy@x.io") != "" {
		t.Fatalf("%+v %v", c, err)
	}
	if e, ok := c.ByNickname("ALPHA"); !ok || e != "claude1@x.io" {
		t.Fatalf("ByNickname: %q %v", e, ok)
	}
	c, err = Load()
	if err != nil || c.Nickname("claude1@x.io") != "alpha" {
		t.Fatalf("nickname not persisted: %+v %v", c, err)
	}
}

func TestAddWithNicknameAndLegacyFile(t *testing.T) {
	t.Setenv("JULIENNING_HOME", t.TempDir())
	// A v2 cache file without nicknames still loads.
	p, _ := config.Path(File)
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"fetched_at":"2026-09-29T12:00:00Z","emails":["old@x.io"]}`), 0o600)
	c, err := Load()
	if err != nil || !c.Contains("old@x.io") || c.Nickname("old@x.io") != "" {
		t.Fatalf("legacy: %+v %v", c, err)
	}
	c, err = AddWithNickname("New@x.io", "beta")
	if err != nil || c.Nickname("new@x.io") != "beta" || !c.Contains("old@x.io") || c.FetchedAt.IsZero() {
		t.Fatalf("add: %+v %v", c, err)
	}
	c, err = AddWithNickname("old@x.io", "gamma")
	if err != nil || c.Nickname("old@x.io") != "gamma" || len(c.Emails) != 2 {
		t.Fatalf("rename via add: %+v %v", c, err)
	}
	c, _ = Remove("old@x.io")
	if c.Contains("old@x.io") || c.Nickname("old@x.io") != "" || c.Nickname("new@x.io") != "beta" {
		t.Fatalf("remove: %+v", c)
	}
}
