// Package sharedcache keeps a local copy of the team allowlist (the emails the
// Worker knows). The status line and hooks must never touch the network, so
// they decide "is this account shared?" from this file; network-capable
// processes (setup, accounts, next, use, and the detached reporters) refresh
// it. A missing or unreadable cache means "nothing is shared": personal
// usage must never leave the machine because of a stale guess.
package sharedcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/remote"
)

// File is the cache file name under config.Dir().
const File = "shared.json"

// MaxAge is how old the cache may get before background refreshes.
const MaxAge = time.Hour

// Cache is the on-disk allowlist snapshot.
type Cache struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Emails    []string          `json:"emails"`              // lowercased, sorted
	Nicknames map[string]string `json:"nicknames,omitempty"` // email → team nickname (may be missing for legacy records)
	// LocalAdds records when this machine shared an email. KV listings lag
	// writes by up to a minute, so a refresh within LocalAddGrace keeps these
	// entries even when the Worker's listing does not show them yet.
	LocalAdds map[string]time.Time `json:"local_adds,omitempty"`
}

// LocalAddGrace is how long a locally added email survives refreshes that
// do not list it.
const LocalAddGrace = 10 * time.Minute

// nowFunc is the wall clock used for the grace window (a seam for tests);
// the fetch stamp passed to Save* is the listing's time, not "now".
var nowFunc = time.Now

// Entry is one shared account as the Worker lists it.
type Entry struct {
	Email    string
	Nickname string // "" when the Worker record has none yet
}

// Nickname returns the team nickname of email, or "" when unknown.
func (c *Cache) Nickname(email string) string {
	return c.Nicknames[strings.ToLower(email)]
}

// ByNickname finds the email whose nickname is nick (case-insensitive).
func (c *Cache) ByNickname(nick string) (string, bool) {
	nick = strings.ToLower(nick)
	for e, n := range c.Nicknames {
		if n == nick {
			return e, true
		}
	}
	return "", false
}

// Entries lists the cached accounts sorted by email.
func (c *Cache) Entries() []Entry {
	out := make([]Entry, 0, len(c.Emails))
	for _, e := range c.Emails {
		out = append(out, Entry{Email: e, Nickname: c.Nicknames[e]})
	}
	return out
}

// Load reads the cache; a missing file is an empty cache, not an error.
func Load() (*Cache, error) {
	p, err := config.Path(File)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(raw) == 0) {
		return &Cache{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

// Contains reports whether email is on the cached allowlist.
func (c *Cache) Contains(email string) bool {
	email = strings.ToLower(email)
	i := sort.SearchStrings(c.Emails, email)
	return i < len(c.Emails) && c.Emails[i] == email
}

// Stale reports whether the cache is older than maxAge (or never fetched).
func (c *Cache) Stale(now time.Time, maxAge time.Duration) bool {
	return c.FetchedAt.IsZero() || now.Sub(c.FetchedAt) > maxAge
}

// Save replaces the cache atomically, keeping no nicknames.
func Save(emails []string, now time.Time) (*Cache, error) {
	entries := make([]Entry, 0, len(emails))
	for _, e := range emails {
		entries = append(entries, Entry{Email: e})
	}
	return SaveEntries(entries, now)
}

// SaveEntries replaces the cache atomically with emails and nicknames,
// keeping entries this machine added within LocalAddGrace that the new
// listing does not contain yet.
func SaveEntries(entries []Entry, now time.Time) (*Cache, error) {
	prev, err := Load()
	if err != nil {
		prev = &Cache{}
	}
	return saveEntries(entries, now, prev.LocalAdds, prev)
}

func saveEntries(entries []Entry, now time.Time, localAdds map[string]time.Time, prev *Cache) (*Cache, error) {
	norm := make([]string, 0, len(entries))
	nicks := map[string]string{}
	seen := map[string]bool{}
	keptAdds := map[string]time.Time{}
	wall := nowFunc().UTC()
	for e, at := range localAdds {
		if age := wall.Sub(at); age >= 0 && age < LocalAddGrace {
			keptAdds[e] = at
		}
	}
	for _, en := range entries {
		e := strings.ToLower(en.Email)
		if !seen[e] {
			seen[e] = true
			norm = append(norm, e)
		}
		if n := strings.ToLower(strings.TrimSpace(en.Nickname)); n != "" {
			nicks[e] = n
		}
	}
	for e := range keptAdds {
		if !seen[e] {
			seen[e] = true
			norm = append(norm, e)
			if n := prev.Nicknames[e]; n != "" && nicks[e] == "" {
				nicks[e] = n
			}
		}
	}
	sort.Strings(norm)
	if len(nicks) == 0 {
		nicks = nil
	}
	if len(keptAdds) == 0 {
		keptAdds = nil
	}
	c := &Cache{FetchedAt: now.UTC(), Emails: norm, Nicknames: nicks, LocalAdds: keptAdds}
	p, err := config.Path(File)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode shared cache: %w", err)
	}
	if err := config.WriteFileAtomic(p, append(raw, '\n'), 0o600); err != nil {
		return nil, err
	}
	return c, nil
}

// Add records email as shared without changing FetchedAt. Callers use it
// right after a successful Share because KV listings lag writes by up to a
// minute, so a refresh may not show the new email yet.
func Add(email string) (*Cache, error) { return AddWithNickname(email, "") }

// AddWithNickname is Add plus the nickname given at share time (or a rename).
func AddWithNickname(email, nickname string) (*Cache, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	entries := c.Entries()
	email = strings.ToLower(email)
	found := false
	for i := range entries {
		if entries[i].Email == email {
			found = true
			if nickname != "" {
				entries[i].Nickname = nickname
			}
		}
	}
	if !found {
		entries = append(entries, Entry{Email: email, Nickname: nickname})
	}
	adds := map[string]time.Time{}
	for e, at := range c.LocalAdds {
		adds[e] = at
	}
	adds[email] = nowFunc().UTC()
	fetched := c.FetchedAt
	if fetched.IsZero() {
		fetched = time.Unix(0, 0)
	}
	return saveEntries(entries, fetched, adds, c)
}

// Remove drops email without changing FetchedAt (after Unshare, or when the
// Worker answers "account is not shared").
func Remove(email string) (*Cache, error) {
	c, err := Load()
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(email)
	kept := make([]Entry, 0, len(c.Emails))
	for _, en := range c.Entries() {
		if en.Email != email {
			kept = append(kept, en)
		}
	}
	adds := map[string]time.Time{}
	for e, at := range c.LocalAdds {
		if e != email {
			adds[e] = at
		}
	}
	fetched := c.FetchedAt
	if fetched.IsZero() {
		// Never fetched: keep it stale so the next network process refreshes.
		fetched = time.Unix(0, 0)
	}
	return saveEntries(kept, fetched, adds, c)
}

// FromListing extracts the allowlist (emails and nicknames) from a Worker
// listing and saves it.
func FromListing(l *remote.Listing, now time.Time) (*Cache, error) {
	entries := make([]Entry, 0, len(l.Accounts))
	for _, a := range l.Accounts {
		entries = append(entries, Entry{Email: a.Email, Nickname: a.Nickname})
	}
	return SaveEntries(entries, now)
}

// Refresh fetches the allowlist from the Worker and saves it.
func Refresh(ctx context.Context, c remote.Client, dev string, now time.Time) (*Cache, *remote.Listing, error) {
	l, err := c.ListAccounts(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	cache, err := FromListing(l, now)
	return cache, l, err
}
