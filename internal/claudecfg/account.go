// Package claudecfg reads and patches files inside a Claude Code config dir
// (the directory CLAUDE_CONFIG_DIR points at). It never touches credentials:
// the only account field read is the email address.
package claudecfg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// AccountFile is the file Claude Code writes its login profile to.
const AccountFile = ".claude.json"

// DefaultDir is Claude Code's config dir when CLAUDE_CONFIG_DIR is unset.
//
// CRITICAL: the default dir is not equivalent to CLAUDE_CONFIG_DIR=~/.claude.
// Unset, Claude Code reads ~/.claude.json and the unsuffixed Keychain entry;
// set explicitly, it reads ~/.claude/.claude.json and a different credential
// key, so the account looks logged out. Launchers must unset the variable for
// this dir, and account lookups must use AccountFilePath.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// IsDefaultDir reports whether dir is Claude Code's default config dir.
func IsDefaultDir(dir string) bool {
	d, err := DefaultDir()
	return err == nil && filepath.Clean(dir) == d
}

// ActiveDir returns the config dir a Claude process with the given
// CLAUDE_CONFIG_DIR value uses ("" means unset → DefaultDir).
func ActiveDir(envValue string) (string, error) {
	if envValue == "" {
		return DefaultDir()
	}
	return filepath.Clean(envValue), nil
}

// AccountFileForEnv returns the account file a Claude process uses given its
// CLAUDE_CONFIG_DIR value: unset → ~/.claude.json; set (even to ~/.claude) →
// $CLAUDE_CONFIG_DIR/.claude.json. Hooks and the status line must use this,
// not AccountFilePath, because they see the real env of the Claude process.
func AccountFileForEnv(envValue string) (string, error) {
	if envValue == "" {
		d, err := DefaultDir()
		if err != nil {
			return "", err
		}
		return AccountFilePath(d), nil
	}
	return filepath.Join(filepath.Clean(envValue), AccountFile), nil
}

// AccountFilePath returns the login profile file for a config dir:
// ~/.claude.json for the default dir, <dir>/.claude.json otherwise.
func AccountFilePath(dir string) string {
	if IsDefaultDir(dir) {
		return filepath.Dir(filepath.Clean(dir)) + string(filepath.Separator) + AccountFile
	}
	return filepath.Join(dir, AccountFile)
}

// MaxEmailLen mirrors RFC 5321's practical limit.
const MaxEmailLen = 254

var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// ValidEmail reports whether s looks like an email address the Worker accepts.
func ValidEmail(s string) bool {
	return s != "" && len(s) <= MaxEmailLen && emailRe.MatchString(s)
}

// ErrNotLoggedIn means the account file exists but carries no email (fresh dir).
var ErrNotLoggedIn = errors.New("not logged in")

// ReadEmail returns oauthAccount.emailAddress from AccountFilePath(dir),
// lowercased. Missing file or missing/empty email → ErrNotLoggedIn. Other
// failures are wrapped with the path so callers can print them as-is.
func ReadEmail(dir string) (string, error) {
	p := AccountFilePath(dir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotLoggedIn
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	var doc struct {
		OAuthAccount struct {
			EmailAddress any `json:"emailAddress"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", p, err)
	}
	email, _ := doc.OAuthAccount.EmailAddress.(string)
	if email == "" {
		return "", ErrNotLoggedIn
	}
	if !ValidEmail(email) {
		return "", fmt.Errorf("%s: oauthAccount.emailAddress is not a valid email", p)
	}
	return strings.ToLower(email), nil
}
