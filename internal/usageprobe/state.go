package usageprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/usage"
)

// State is what this machine last learned by probing one registered dir:
// ~/.julienning/probes/<hash of the dir>.json. It is the local cache window
// (a login is not probed twice within CacheWindow), the numbers re-sent to
// the Worker while its listing lags, and the signed-out mark.
type State struct {
	Dir      string    `json:"dir"`
	Email    string    `json:"email"` // the dir's login when probed, lowercased
	Outcome  Outcome   `json:"outcome"`
	ProbedAt time.Time `json:"probed_at"`
	Session  *Window   `json:"session,omitempty"` // OK only
	Week     *Window   `json:"week,omitempty"`    // OK only
}

// StatePath is the state file of a config dir. Named by a hash like the
// sent/ cache: ~/.julienning ends up in bug reports, and a dir path can name
// a person or a team.
func StatePath(dir string) (string, error) {
	d, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, config.ProbesDir, usage.KeyHash(filepath.Clean(dir))+".json"), nil
}

// LoadState reads dir's state. A missing, corrupt or foreign file reads as
// nil: it is a cache, and the worst case is one extra probe.
func LoadState(dir string) *State {
	p, err := StatePath(dir)
	if err != nil {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var s State
	if json.Unmarshal(raw, &s) != nil || filepath.Clean(s.Dir) != filepath.Clean(dir) {
		return nil
	}
	switch s.Outcome {
	case OK:
		if s.Session == nil || s.Week == nil {
			return nil
		}
	case SignedOut, Failed:
	default:
		return nil
	}
	return &s
}

// SaveState writes s atomically (0600), so a concurrent LoadState never sees
// half a file.
func SaveState(s State) error {
	p, err := StatePath(s.Dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode probe state: %w", err)
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
	return nil
}

// stateOf turns a probe result into the state saved for it.
func stateOf(r Result, email string) State {
	s := State{Dir: filepath.Clean(r.Dir), Email: strings.ToLower(email), Outcome: r.Outcome, ProbedAt: r.At.UTC()}
	if r.Outcome == OK {
		session, week := r.Session, r.Week
		s.Session, s.Week = &session, &week
	}
	return s
}

// IsSignedOut reports whether the last probe of dir found it signed out
// while it was logged into email, the dir's login now. A different login
// (someone signed in again, as another account or the same one through
// `julienning login`) is not covered by an old mark.
func IsSignedOut(dir, email string) bool {
	s := LoadState(dir)
	return s != nil && s.Outcome == SignedOut && s.Email == strings.ToLower(email)
}

// ClearSignedOut forgets dir's signed-out mark: something proved the login
// works (a status line report from the dir, a verified `julienning login`).
// Other states are kept.
func ClearSignedOut(dir string) error {
	s := LoadState(dir)
	if s == nil || s.Outcome != SignedOut {
		return nil
	}
	p, err := StatePath(dir)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}
