// Package resolve maps what a user types — a team nickname, an email, or a
// registered config name — to a local config dir. Nicknames and emails are
// resolved at call time against the live logins, because accounts move
// between dirs.
package resolve

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/discover"
	"github.com/muratgozel/julienning/internal/sharedcache"
)

// Via values.
const (
	ViaNickname = "nickname"
	ViaEmail    = "email"
	ViaDir      = "dir"
)

// Match is a resolved target.
type Match struct {
	Dir      config.ConfigDir
	Email    string // "" when the dir is not logged in
	Nickname string // "" when the account has no team nickname
	Via      string
}

// NotLocalError means the account is shared but no registered dir on this
// machine is logged into it.
type NotLocalError struct {
	Nickname, Email string
	// Shared is true when the account is known to be on the team allowlist,
	// so `julienning login` can sign it in here; any other email is pointed
	// at new-config, which decides whether it is shared.
	Shared bool
	// SignedOut are the registered dirs that held the account last
	// (config.ConfigDir.LastEmail) and have no login now: Claude Code signed
	// them out. Their sessions are still there; `julienning login` signs in
	// there again.
	SignedOut []config.ConfigDir
}

func (e *NotLocalError) Error() string {
	if len(e.SignedOut) > 0 {
		who := e.Email
		if e.Nickname != "" {
			who = e.Nickname
		}
		label := who
		if e.Nickname != "" {
			label += " (" + e.Email + ")"
		}
		return fmt.Sprintf("%s is signed out on this machine (%s held it last and has no login now); sign in again with: julienning login %s",
			label, shortHome(e.SignedOut[0].Dir), who)
	}
	if e.Nickname != "" {
		return fmt.Sprintf("%s (%s) is shared but not logged in on this machine; sign it in here with: julienning login %s", e.Nickname, e.Email, e.Nickname)
	}
	if e.Shared {
		return fmt.Sprintf("no registered config dir on this machine is logged in as %s; sign it in here with: julienning login %s", e.Email, e.Email)
	}
	return fmt.Sprintf("no registered config dir on this machine is logged in as %s; sign it into a new dir with: julienning new-config (`--no-share` keeps it personal)", e.Email)
}

// Dirs returns the registered dirs currently logged into email, the current
// selection first, then by name. Unreadable account files count as not
// logged in.
func Dirs(cfg *config.Config, email string, current *config.ConfigDir) []config.ConfigDir {
	email = strings.ToLower(email)
	var out []config.ConfigDir
	for _, cd := range cfg.Configs {
		if e, err := claudecfg.ReadEmail(cd.Dir); err == nil && e == email {
			if current != nil && cd.Dir == current.Dir {
				out = append([]config.ConfigDir{cd}, out...)
			} else {
				out = append(out, cd)
			}
		}
	}
	return out
}

// SignedOutDirs returns the registered dirs, by name, whose last recorded
// login was email and that have no login now (the account file is missing
// or names nobody). An unreadable account file is not "no login".
func SignedOutDirs(cfg *config.Config, email string) []config.ConfigDir {
	email = strings.ToLower(email)
	var out []config.ConfigDir
	for _, cd := range cfg.Configs {
		if cd.LastEmail != email {
			continue
		}
		if _, err := claudecfg.ReadEmail(cd.Dir); errors.Is(err, claudecfg.ErrNotLoggedIn) {
			out = append(out, cd)
		}
	}
	return out
}

// Target resolves arg: a nickname first, then an email, then a config name.
// cache may be nil (no nicknames known).
func Target(cfg *config.Config, cache *sharedcache.Cache, arg string, current *config.ConfigDir) (Match, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Match{}, errors.New("empty target")
	}
	lower := strings.ToLower(arg)
	if cache != nil {
		if email, ok := cache.ByNickname(lower); ok {
			return byEmail(cfg, email, lower, ViaNickname, current, true)
		}
	}
	if strings.Contains(lower, "@") {
		nick := ""
		if cache != nil {
			nick = cache.Nickname(lower)
		}
		return byEmail(cfg, lower, nick, ViaEmail, current, cache != nil && cache.Contains(lower))
	}
	if cd, ok := cfg.Find(arg); ok {
		m := Match{Dir: cd, Via: ViaDir}
		if e, err := claudecfg.ReadEmail(cd.Dir); err == nil {
			m.Email = e
			if cache != nil {
				m.Nickname = cache.Nickname(e)
			}
		}
		return m, nil
	}
	return Match{}, fmt.Errorf("unknown target %q: not a team nickname, an email, or a config name (see `julienning accounts` and `julienning configs`)", arg)
}

func byEmail(cfg *config.Config, email, nick, via string, current *config.ConfigDir, shared bool) (Match, error) {
	dirs := Dirs(cfg, email, current)
	if len(dirs) == 0 {
		return Match{}, &NotLocalError{Nickname: nick, Email: email, Shared: shared, SignedOut: SignedOutDirs(cfg, email)}
	}
	return Match{Dir: dirs[0], Email: email, Nickname: nick, Via: via}, nil
}

// Unregistered returns the Claude config dirs setup would discover that are
// logged into email but not registered, default dir first, then by path. An
// unreadable login is skipped: it cannot be told to hold the account.
func Unregistered(cfg *config.Config, email string) ([]string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory (set $HOME): %w", err)
	}
	found, err := discover.Find(cfg.Configs, cfg.Prefix())
	if err != nil {
		return nil, fmt.Errorf("search for Claude config dirs: %w", err)
	}
	var out []string
	for _, c := range found {
		if !c.Registered && c.LoggedIn && c.Email == email && !discover.IsUnsafeConfigDir(c.Dir, home) {
			out = append(out, c.Dir)
		}
	}
	return out, nil
}

// Label is how a dir is shown to users: the account nickname when the dir
// is logged into a nicknamed account, else its config name.
func Label(cache *sharedcache.Cache, cd config.ConfigDir, email string) string {
	if cache != nil && email != "" {
		if n := cache.Nickname(email); n != "" {
			return n
		}
	}
	return cd.Name
}
