// Package resolve maps what a user types — a team nickname, an email, or a
// registered config name — to a local config dir. Nicknames and emails are
// resolved at call time against the live logins, because accounts move
// between dirs.
package resolve

import (
	"errors"
	"fmt"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
	"github.com/muratgozel/julienning/internal/config"
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
}

func (e *NotLocalError) Error() string {
	const hint = "sign in with `julienning login <config>` or `julienning new-config --login`"
	if e.Nickname != "" {
		return fmt.Sprintf("%s (%s) is shared but not logged in on this machine; %s", e.Nickname, e.Email, hint)
	}
	return fmt.Sprintf("no registered config dir on this machine is logged in as %s; %s", e.Email, hint)
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
			return byEmail(cfg, email, lower, ViaNickname, current)
		}
	}
	if strings.Contains(lower, "@") {
		nick := ""
		if cache != nil {
			nick = cache.Nickname(lower)
		}
		return byEmail(cfg, lower, nick, ViaEmail, current)
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

func byEmail(cfg *config.Config, email, nick, via string, current *config.ConfigDir) (Match, error) {
	dirs := Dirs(cfg, email, current)
	if len(dirs) == 0 {
		return Match{}, &NotLocalError{Nickname: nick, Email: email}
	}
	return Match{Dir: dirs[0], Email: email, Nickname: nick, Via: via}, nil
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
