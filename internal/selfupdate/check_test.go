package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)

func writeCheck(t *testing.T, e env, checkedAt time.Time, latest string) {
	t.Helper()
	raw, _ := json.Marshal(checkCache{CheckedAt: checkedAt, Latest: latest})
	writeFile(t, filepath.Join(e.state, CheckFile), string(raw), 0o600)
}

func TestHint(t *testing.T) {
	cases := []struct {
		name, current, cache, want string
	}{
		{"newer", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, "julienning 0.3.0 is available (you have 0.2.1): julienning update\n"},
		{"tag-style current", "v0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, "julienning 0.3.0 is available (you have 0.2.1): julienning update\n"},
		{"stale cache still hints", "0.2.1", `{"checked_at":"2020-01-01T00:00:00Z","latest":"0.3.0"}`, "julienning 0.3.0 is available (you have 0.2.1): julienning update\n"},
		{"same", "0.3.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"older", "0.4.0", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"dev", "dev", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"git describe", "v0.2.1-3-gabc1234", `{"checked_at":"2026-09-29T09:00:00Z","latest":"0.3.0"}`, ""},
		{"nothing published", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":""}`, ""},
		{"malformed latest", "0.2.1", `{"checked_at":"2026-09-29T09:00:00Z","latest":"banana"}`, ""},
		{"corrupt cache", "0.2.1", `{not json`, ""},
		{"no cache", "0.2.1", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			setVersion(t, c.current)
			if c.cache != "" {
				writeFile(t, filepath.Join(e.state, CheckFile), c.cache, 0o600)
			}
			var buf bytes.Buffer
			Hint(&buf)
			if buf.String() != c.want {
				t.Fatalf("Hint = %q, want %q", buf.String(), c.want)
			}
		})
	}
}

func TestRefreshIfStale(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")

	// No cache → network, written with checked_at = now (UTC).
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, err := readCheck()
	if err != nil || c.Latest != "0.3.0" || !c.CheckedAt.Equal(t0) {
		t.Fatalf("cache = %+v, %v", c, err)
	}
	if info, _ := os.Stat(filepath.Join(e.state, CheckFile)); info.Mode().Perm() != 0o600 {
		t.Fatalf("cache mode = %v", info.Mode())
	}
	if n := f.hitCount("/releases/latest"); n != 1 {
		t.Fatalf("hits = %d", n)
	}

	// Fresh (23h) → no network.
	f.setLatest("v0.4.0")
	setNow(t, t0.Add(23*time.Hour))
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.hitCount("/releases/latest"); n != 1 {
		t.Fatalf("fresh cache hit the network (%d)", n)
	}

	// Stale (> 24h) → refreshed.
	setNow(t, t0.Add(24*time.Hour+time.Second))
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := readCheck(); c.Latest != "0.4.0" {
		t.Fatalf("cache = %+v", c)
	}

	// checked_at in the future (clock moved back) → refreshed.
	writeCheck(t, e, t0.Add(48*time.Hour), "0.4.0")
	f.setLatest("v0.5.0")
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, _ := readCheck(); c.Latest != "0.5.0" {
		t.Fatalf("future cache not refreshed: %+v", c)
	}
}

func TestRefreshIfStaleNoReleaseAndErrors(t *testing.T) {
	e := newEnv(t)
	setVersion(t, "0.2.1")
	setNow(t, t0)
	f := newFakeReleases(t)

	// Nothing published: recorded (so background runs back off), no error.
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c, err := readCheck(); err != nil || c.Latest != "" || !c.CheckedAt.Equal(t0) {
		t.Fatalf("cache = %+v, %v", c, err)
	}

	// Server error: returned, cache left as it was.
	writeCheck(t, e, t0.Add(-48*time.Hour), "0.2.5")
	f.setLatestCode(500)
	if err := RefreshIfStale(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v", err)
	}
	if c, _ := readCheck(); c.Latest != "0.2.5" {
		t.Fatalf("cache changed on error: %+v", c)
	}
}

func TestRefreshIfStaleSkipsDevBuilds(t *testing.T) {
	newEnv(t)
	setVersion(t, "dev")
	f := newFakeReleases(t)
	f.setLatest("v0.3.0")
	if err := RefreshIfStale(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.hitCount("/releases/latest"); n != 0 {
		t.Fatalf("dev build hit the network (%d)", n)
	}
}

func TestRecordLatestRejectsMalformed(t *testing.T) {
	newEnv(t)
	if err := RecordLatest("nightly"); err == nil {
		t.Fatal("accepted a malformed tag")
	}
}
