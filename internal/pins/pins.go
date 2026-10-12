// Package pins keeps directory pins in $JULIENNING_HOME/pins.json: plain
// `claude` (through the shell wrapper and `resolve-dir --pin`) and
// `julienning use` without a target, run in a pinned directory or anywhere
// below it, open the pinned account instead of following the selection.
//
// A pin names an account by email, never a config dir: logins move between
// dirs, so the dir is resolved, and its login checked, on every use
// (resolve.Account). Pins are local to this machine and never leave it.
package pins

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/muratgozel/julienning/internal/config"
)

// Pin is one pinned directory.
type Pin struct {
	Dir   string `json:"dir"`   // absolute and cleaned, as the working directory was spelled when pinned
	Email string `json:"email"` // the account, lowercased
}

// SchemaVersion bumps when pins.json changes incompatibly.
const SchemaVersion = 1

type file struct {
	Version int   `json:"version"`
	Pins    []Pin `json:"pins"`
}

// Path is pins.json in the state dir.
func Path() (string, error) { return config.Path(config.PinsFile) }

// Load returns the pins, sorted by dir; none when the file does not exist.
func Load() ([]Pin, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix it, or delete it to drop every pin)", p, err)
	}
	if f.Version != SchemaVersion {
		return nil, fmt.Errorf("%s has schema version %d, this binary expects %d (update julienning, or delete the file to drop every pin)", p, f.Version, SchemaVersion)
	}
	if err := validate(f.Pins); err != nil {
		return nil, fmt.Errorf("%s: %w (fix it, or delete the file to drop every pin)", p, err)
	}
	sortPins(f.Pins)
	return f.Pins, nil
}

// Save writes the pins atomically. With no pins left the file is removed:
// the claude wrapper only asks julienning about pins while the file exists,
// so a machine without pins never pays for the extra process.
func Save(list []Pin) error {
	if err := validate(list); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if len(list) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		return nil
	}
	sorted := append([]Pin(nil), list...)
	sortPins(sorted)
	raw, err := json.MarshalIndent(file{Version: SchemaVersion, Pins: sorted}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode pins: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	return config.WriteFileAtomic(p, append(raw, '\n'), 0o600)
}

// Set pins dir to email, replacing a pin on the same dir. prev is that
// replaced pin; created reports whether pins.json did not exist before.
func Set(dir, email string) (prev Pin, replaced, created bool, err error) {
	list, err := Load()
	if err != nil {
		return Pin{}, false, false, err
	}
	created = len(list) == 0
	pin := Pin{Dir: filepath.Clean(dir), Email: strings.ToLower(email)}
	for i, p := range list {
		if p.Dir == pin.Dir {
			list[i] = pin
			return p, true, false, Save(list)
		}
	}
	return Pin{}, false, created, Save(append(list, pin))
}

// Remove deletes the pin on dir itself (not one on a parent). The dir may
// be spelled through a symlink; it need not exist any more.
func Remove(dir string) (Pin, bool, error) {
	list, err := Load()
	if err != nil {
		return Pin{}, false, err
	}
	for i, p := range list {
		if sameDir(p.Dir, dir) {
			return p, true, Save(append(list[:i], list[i+1:]...))
		}
	}
	return Pin{}, false, nil
}

// Match returns the pin that applies in cwd: the deepest pinned directory
// that is cwd or contains it. Both paths are compared as written and with
// symlinks resolved, because the shell's $PWD and the pinned spelling may
// reach the same directory through different links.
func Match(list []Pin, cwd string) (Pin, bool) {
	var best Pin
	found := false
	for _, p := range list {
		if Within(cwd, p.Dir) && (!found || len(p.Dir) > len(best.Dir)) {
			best, found = p, true
		}
	}
	return best, found
}

// Within reports whether path is dir or below it.
func Within(path, dir string) bool {
	for _, a := range spellings(path) {
		for _, b := range spellings(dir) {
			if a == b || strings.HasPrefix(a, strings.TrimSuffix(b, string(filepath.Separator))+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func sameDir(a, b string) bool {
	for _, x := range spellings(a) {
		for _, y := range spellings(b) {
			if x == y {
				return true
			}
		}
	}
	return false
}

// spellings are p cleaned and, when it exists, with symlinks resolved.
func spellings(p string) []string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		return []string{p, r}
	}
	return []string{p}
}

func validate(list []Pin) error {
	seen := map[string]bool{}
	for _, p := range list {
		if !filepath.IsAbs(p.Dir) || filepath.Clean(p.Dir) != p.Dir {
			return fmt.Errorf("pin dir must be an absolute, clean path, got %q", p.Dir)
		}
		at := strings.Index(p.Email, "@")
		if p.Email != strings.ToLower(p.Email) || at < 1 || at == len(p.Email)-1 || strings.ContainsAny(p.Email, " \t\r\n") {
			return fmt.Errorf("pin on %s: invalid email %q", p.Dir, p.Email)
		}
		if seen[p.Dir] {
			return fmt.Errorf("duplicate pin on %s", p.Dir)
		}
		seen[p.Dir] = true
	}
	return nil
}

func sortPins(list []Pin) {
	sort.Slice(list, func(i, j int) bool { return list[i].Dir < list[j].Dir })
}

// UseError explains why the pin on p cannot be used right now. Callers must
// stop rather than fall back to the selection: a pinned project must never
// open silently on another account.
func UseError(p Pin, err error) error {
	d := ShortHome(p.Dir)
	return fmt.Errorf("%s is pinned to %s, which cannot be opened: %w (or remove the pin: julienning unpin %s)", d, p.Email, err, d)
}

// ShortHome replaces the home prefix with "~" for display.
func ShortHome(p string) string {
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return p
	}
	if p == h {
		return "~"
	}
	if strings.HasPrefix(p, h+string(filepath.Separator)) {
		return "~" + p[len(h):]
	}
	return p
}
