package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/config"
)

const (
	// RepeatWindow is how long an unchanged payload stays debounced, on top of
	// the configured minimum interval.
	RepeatWindow = 30 * time.Minute

	// InflightTTL is how long an in-flight marker is trusted. It must be longer
	// than the report timeout (5 s) and short enough that a killed process
	// cannot block reporting for a whole session.
	InflightTTL = 30 * time.Second
)

// Payload is the set of numbers a report carries; equality drives the debounce.
type Payload struct {
	SessionUsed   float64 `json:"session_used"`
	SessionResets int64   `json:"session_resets"`
	WeekUsed      float64 `json:"week_used"`
	WeekResets    int64   `json:"week_resets"`
}

// Sent is one entry of the sent/<hash>.json debounce cache. It records the
// last *attempted* payload: AttemptedAt is stamped before the request so an
// unreachable Worker cannot fan out one process per status line render, while
// SentAt only moves on a 2xx (zero until the first success).
type Sent struct {
	Payload
	AttemptedAt time.Time `json:"attempted_at"`
	SentAt      time.Time `json:"sent_at"`
}

// SentPath is the cache file for an email.
func SentPath(dir, email string) string {
	return filepath.Join(dir, config.SentDir, cacheBase(email)+".json")
}

// InflightPath is the O_EXCL marker held while a report is in flight.
func InflightPath(dir, email string) string {
	return filepath.Join(dir, config.SentDir, cacheBase(email)+".inflight")
}

// cacheBase names an account's files by KeyHash, like the throttle markers:
// ~/.julienning ends up in bug reports, so no file name may carry an address.
// A hash also cannot contain a path separator.
func cacheBase(email string) string {
	return KeyHash(strings.ToLower(email))
}

// removeLegacy deletes sent/ entries named after an email address (the
// pre-hash layout). Best effort by design: it runs on every SaveSent, so a
// file that cannot be removed now is retried on the next write, and a
// leftover is never read (lookups only use hashed names).
func removeLegacy(sentDir string) {
	entries, err := os.ReadDir(sentDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.Type().IsRegular() && strings.Contains(name, "@") &&
			(strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".inflight")) {
			_ = os.Remove(filepath.Join(sentDir, name))
		}
	}
}

// LoadSent reads the cache entry. A missing, empty or corrupt file yields
// (nil, nil): the worst case is one extra report, and nothing about the
// account may be logged from here.
func LoadSent(dir, email string) (*Sent, error) {
	raw, err := os.ReadFile(SentPath(dir, email))
	if err != nil || len(raw) == 0 {
		return nil, nil
	}
	var s Sent
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil
	}
	return &s, nil
}

// SaveSent writes the cache entry atomically: a concurrent LoadSent must never
// observe a half-written (or 0-byte) file.
func SaveSent(dir, email string, s Sent) error {
	p := SentPath(dir, email)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode cache: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeded
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	removeLegacy(filepath.Dir(p))
	return nil
}

// AcquireInflight takes the per-account send lock. ok=false means another
// send-usage started less than InflightTTL ago and this process must exit
// quietly. A marker older than that is assumed to belong to a crashed process
// and is replaced. The returned release is always safe to defer.
func AcquireInflight(dir, email string, now time.Time) (release func(), ok bool, err error) {
	p := InflightPath(dir, email)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return func() {}, false, fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case err == nil:
			f.Close()
			// The marker's mtime is the lock clock, so a frozen
			// JULIENNING_NOW_EPOCH must move it or tests could never observe it.
			_ = os.Chtimes(p, now, now)
			return func() { os.Remove(p) }, true, nil
		case !errors.Is(err, os.ErrExist):
			return func() {}, false, fmt.Errorf("create %s: %w", p, err)
		}
		fi, statErr := os.Stat(p)
		if statErr != nil {
			continue // it vanished between the two calls: retry once
		}
		if now.Sub(fi.ModTime()) < InflightTTL {
			return func() {}, false, nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return func() {}, false, fmt.Errorf("remove stale %s: %w", p, err)
		}
	}
	// Lost both races: another process is reporting, which is the point.
	return func() {}, false, nil
}

// ShouldSend applies the debounce: a new rate-limit window always goes out
// immediately; a payload identical to the last successfully sent one waits
// RepeatWindow; anything else waits minInterval since the last *attempt* for
// this account, so an unreachable Worker is retried at a fixed rate instead of
// once per status line render.
func ShouldSend(prev *Sent, cur Payload, now time.Time, minInterval time.Duration) bool {
	if prev == nil {
		return true
	}
	if cur.SessionResets != prev.SessionResets || cur.WeekResets != prev.WeekResets {
		return true
	}
	if cur == prev.Payload && !prev.SentAt.IsZero() && since(now, prev.SentAt) < RepeatWindow {
		return false
	}
	return since(now, prev.AttemptedAt) >= minInterval
}

// since is now-t, clamped at 0: clock skew or a frozen test clock must read as
// "just now", never as "long ago".
func since(now, t time.Time) time.Duration {
	if t.IsZero() {
		return 1<<63 - 1
	}
	if d := now.Sub(t); d > 0 {
		return d
	}
	return 0
}
