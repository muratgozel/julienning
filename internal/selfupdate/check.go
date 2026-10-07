package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/version"
)

// CheckFile caches the latest known release in config.Dir().
const CheckFile = "update-check.json"

// checkInterval is how often background processes look up the latest tag.
const checkInterval = 24 * time.Hour

// now is the clock; a var for tests.
var now = time.Now

type checkCache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"` // display version ("0.3.0"); "" when nothing is published
}

func readCheck() (checkCache, error) {
	var c checkCache
	p, err := config.Path(CheckFile)
	if err != nil {
		return c, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", p, err)
	}
	return c, nil
}

// RecordLatest stores tag as the latest release (e.g. after `update` resolved
// it). An empty tag records "nothing published".
func RecordLatest(tag string) error {
	if tag != "" {
		if _, err := parseVersion(tag); err != nil {
			return fmt.Errorf("record latest release: %w", err)
		}
	}
	p, err := config.Path(CheckFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.Marshal(checkCache{CheckedAt: now().UTC(), Latest: DisplayVersion(tag)})
	if err != nil {
		return fmt.Errorf("encode %s: %w", CheckFile, err)
	}
	return config.WriteFileAtomic(p, append(raw, '\n'), 0o600)
}

// fresh reports whether the cache is younger than checkInterval. A
// checked_at in the future (clock moved back) counts as stale so a skewed
// clock cannot suppress checks indefinitely.
func (c checkCache) fresh() bool {
	age := now().Sub(c.CheckedAt)
	return age >= -time.Minute && age < checkInterval
}

// RefreshIfStale refreshes update-check.json when older than 24h. It does
// network I/O (up to the tag lookup timeout), so only detached/background
// processes call it. Dev builds never check: they get no hints anyway.
func RefreshIfStale(ctx context.Context) error {
	if IsDevBuild(version.Version) {
		return nil
	}
	// A missing or corrupt cache just means "stale"; the write below
	// replaces it.
	if c, err := readCheck(); err == nil && c.fresh() {
		return nil
	}
	tag, err := LatestTag(ctx)
	if errors.Is(err, ErrNoRelease) {
		return RecordLatest("")
	}
	if err != nil {
		return err
	}
	return RecordLatest(tag)
}

// Hint prints one line to w when the cached update check knows a release
// newer than this binary. It reads the cache only (never the network) and
// prints nothing for dev builds.
func Hint(w io.Writer) {
	cur := version.Version
	if IsDevBuild(cur) {
		return
	}
	// Read errors (missing, unreadable or corrupt cache) are ignored on
	// purpose: the hint is purely cosmetic and must never fail or clutter an
	// interactive command. The next background refresh rewrites the file.
	c, err := readCheck()
	if err != nil || c.Latest == "" {
		return
	}
	if cmp, err := CompareVersions(c.Latest, cur); err != nil || cmp <= 0 {
		return
	}
	fmt.Fprintf(w, "julienning %s is available (you have %s): julienning update\n", DisplayVersion(c.Latest), DisplayVersion(cur))
}
