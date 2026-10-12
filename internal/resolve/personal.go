package resolve

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/discover"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// ViaPersonal marks a Match found by Personal: an account that is not shared
// with the team, in a dir that need not be registered.
const ViaPersonal = "personal"

// PersonalMatch is where a personal account (an email not on the team
// allowlist) is logged in on this machine.
type PersonalMatch struct {
	Email string
	Dir   string
	// Name is the dir's config name when it happens to be registered, else "".
	Name string
	// Moved is the remembered dir that no longer holds the account; "" when
	// the remembered dir was right or nothing was remembered.
	Moved string
	// Others are further dirs logged into the same account; Dir is the first
	// in discovery order (the default dir first, then by path).
	Others []string
	// Changed is true when config.json does not remember Dir yet.
	Changed bool
}

// PersonalNotFoundError means no Claude config dir on this machine is logged
// into the account.
type PersonalNotFoundError struct {
	Email string
	Moved string // the remembered dir that lost the login, "" when none
}

const searched = "searched ~/.claude, the top level of your home directory, the CLAUDE_CONFIG_DIR settings in your shell startup files and the registered dirs"

func (e *PersonalNotFoundError) Error() string {
	if e.Moved != "" {
		return fmt.Sprintf("%s is no longer logged in at %s, and no other Claude config dir on this machine is (%s); sign in again with: %s",
			e.Email, shortHome(e.Moved), searched, signInHint(e.Moved))
	}
	name := config.DefaultNickname(e.Email)
	if name == "" {
		name = "personal"
	}
	home, _ := os.UserHomeDir()
	return fmt.Sprintf("no Claude config dir on this machine is logged in as %s (%s); sign in first, for example: %s",
		e.Email, searched, signInHint(filepath.Join(home, ".claude-"+name)))
}

// signInHint starts Claude in dir so the user can sign in, bypassing the
// claude wrapper. The default dir runs with CLAUDE_CONFIG_DIR unset (see
// claudecfg.DefaultDir).
func signInHint(dir string) string {
	if claudecfg.IsDefaultDir(dir) {
		return "command claude"
	}
	return "CLAUDE_CONFIG_DIR=" + shortHome(dir) + " command claude"
}

// Personal finds the dir logged into email, an account that is not shared:
// the dir config.json remembers when its login still matches, else the
// first logged into it among the dirs setup discovers plus the registered
// dirs (an adopted dir need not be discoverable). It only reads; callers
// save a changed result with Remember.
func Personal(cfg *config.Config, email string) (PersonalMatch, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	m := PersonalMatch{Email: email}
	home, err := os.UserHomeDir()
	if err != nil {
		return m, fmt.Errorf("resolve home directory (set $HOME): %w", err)
	}
	if dir, ok := cfg.PersonalDirOf(email); ok {
		holds, err := holdsLogin(dir, email, home)
		if err != nil {
			return m, err
		}
		if holds {
			m.Dir, m.Name = dir, registeredName(cfg, dir)
			return m, nil
		}
		m.Moved = dir
	}

	found, err := discover.Find(cfg.Configs, cfg.Prefix())
	if err != nil {
		return m, fmt.Errorf("search for Claude config dirs: %w", err)
	}
	var dirs []string
	seen := map[string]bool{}
	for _, c := range found {
		seen[c.Dir] = true
		if c.LoggedIn && c.Email == email {
			dirs = append(dirs, c.Dir)
		}
	}
	for _, cd := range cfg.Configs {
		d := filepath.Clean(cd.Dir)
		if seen[d] {
			continue
		}
		if e, err := claudecfg.ReadEmail(d); err == nil && e == email && !discover.IsUnsafeConfigDir(d, home) {
			dirs = append(dirs, d)
		}
	}
	if len(dirs) == 0 {
		return m, &PersonalNotFoundError{Email: email, Moved: m.Moved}
	}
	m.Dir, m.Others, m.Changed = dirs[0], dirs[1:], true
	m.Name = registeredName(cfg, m.Dir)
	return m, nil
}

// holdsLogin reports whether dir is a usable config dir logged into email.
// A missing dir or a different login is false; an account file that cannot
// be read is an error, since searching elsewhere would hide the problem.
func holdsLogin(dir, email, home string) (bool, error) {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() || discover.IsUnsafeConfigDir(dir, home) {
		return false, nil
	}
	got, err := claudecfg.ReadEmail(dir)
	if errors.Is(err, claudecfg.ErrNotLoggedIn) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot read the login in %s: %w", shortHome(dir), err)
	}
	return got == email, nil
}

func registeredName(cfg *config.Config, dir string) string {
	if cd, ok := cfg.FindByDir(dir); ok {
		return cd.Name
	}
	return ""
}

// Notes are the one-line explanations to print when the lookup had to
// search: where the account went, and which other dirs also hold it.
func (m PersonalMatch) Notes() []string {
	var out []string
	if m.Moved != "" {
		out = append(out, fmt.Sprintf("%s is no longer logged in at %s; found it at %s", m.Email, shortHome(m.Moved), shortHome(m.Dir)))
	}
	if len(m.Others) > 0 {
		others := make([]string, len(m.Others))
		for i, d := range m.Others {
			others[i] = shortHome(d)
		}
		out = append(out, fmt.Sprintf("%s is also logged in at %s; using %s", m.Email, strings.Join(others, ", "), shortHome(m.Dir)))
	}
	return out
}

// Remember saves Dir as the account's dir in config.json when it changed.
func (m PersonalMatch) Remember() error {
	if !m.Changed {
		return nil
	}
	if err := config.RememberPersonalDir(m.Email, m.Dir); err != nil {
		return fmt.Errorf("remember %s for %s: %w", shortHome(m.Dir), m.Email, err)
	}
	return nil
}

// Shared reports whether email is a team account by the cached allowlist.
// A nil cache knows no team accounts, so every email counts as personal:
// the personal path never registers, reports or moves anything, which is the
// safe side when the allowlist is unknown.
func Shared(cache *sharedcache.Cache, email string) bool {
	return cache != nil && cache.Contains(strings.ToLower(email))
}

// Account resolves an account by email to the dir a launch uses: a team
// account to the registered dir logged into it (Dirs: the current selection
// first), any other account through Personal. pm is non-nil exactly for a
// personal account, so callers can print its Notes and Remember it.
func Account(cfg *config.Config, cache *sharedcache.Cache, email string, current *config.ConfigDir) (m Match, pm *PersonalMatch, err error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if Shared(cache, email) {
		m, err = byEmail(cfg, email, cache.Nickname(email), ViaEmail, current)
		return m, nil, err
	}
	p, err := Personal(cfg, email)
	if err != nil {
		return Match{}, nil, err
	}
	return Match{Dir: config.ConfigDir{Name: p.Name, Dir: p.Dir}, Email: email, Via: ViaPersonal}, &p, nil
}

// shortHome replaces the home prefix with "~" for display.
func shortHome(p string) string {
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
