// Package config owns the local state under $JULIENNING_HOME (default
// ~/.julienning): config.json (identity, remote, registered config dirs) and
// the `current` file read by the shell wrapper.
//
// Nothing about a team is compiled in: the Worker URL and token come from
// setup (or JULIENNING_REMOTE_URL / JULIENNING_TOKEN). Registered config dirs
// are the ones julienning manages; whether a dir is *shared* is decided at
// runtime by its current login email being on the team allowlist (see
// internal/sharedcache), because logins move between dirs.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/muratgozel/julienning/internal/shell"
)

// SchemaVersion bumps when config.json changes incompatibly.
const SchemaVersion = 1

// File names inside Dir().
const (
	ConfigFile  = "config.json"
	CurrentFile = "current"    // absolute path of the selected config dir, or empty
	ErrorLog    = "errors.log" // statusline/send-usage diagnostics, never contains emails
	SentDir     = "sent"       // send-usage debounce cache, one file per email
)

// EnvHome overrides the state directory (tests, multiple profiles).
const EnvHome = "JULIENNING_HOME"

// EnvAutoUpdate turns background auto-updates off at runtime when false
// (0, false, f; see strconv.ParseBool). A true value does not override
// "auto_update": false in config.json: the variable can only switch off.
const EnvAutoUpdate = "JULIENNING_AUTO_UPDATE"

// Remote is how the CLI reaches the Worker.
type Remote struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// ConfigDir is one registered (shared) Claude config directory.
type ConfigDir struct {
	Name string `json:"name"` // internal label, e.g. "julienning1" or "default"; users address accounts by nickname
	Dir  string `json:"dir"`  // absolute path, e.g. /Users/x/.claude-julienning1
	// ShareOnLogin is set by new-config (a dir for a team account): the
	// first account the dir is signed into is shared with the team
	// automatically (claims.ResolvePendingShares), then the mark is cleared.
	// nil means nothing is pending.
	ShareOnLogin *ShareOnLogin `json:"share_on_login,omitempty"`
}

// ShareOnLogin is a pending share of a registered dir's future login.
type ShareOnLogin struct {
	// Nickname is the team nickname to share under; "" means the email's
	// local part (DefaultNickname), decided at share time.
	Nickname string `json:"nickname,omitempty"`
}

// NicknameFor is the nickname the pending share uses for email: Nickname,
// else DefaultNickname(email) ("" when neither gives a valid one).
func (s *ShareOnLogin) NicknameFor(email string) string {
	if s != nil && s.Nickname != "" {
		return s.Nickname
	}
	return DefaultNickname(email)
}

// DefaultNickname is the suggested team nickname for an email: its local
// part (claude1@x.io → claude1). Characters a nickname cannot hold become
// dashes (john+team@x.io → john-team), leading dots, underscores and dashes
// are dropped and it is cut to 32; "" when nothing valid is left. setup,
// share, new-config and the pending share on login all use this one rule.
func DefaultNickname(email string) string {
	local := strings.ToLower(email)
	if i := strings.LastIndex(local, "@"); i >= 0 {
		local = local[:i]
	}
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	n := strings.TrimLeft(b.String(), "._-")
	if len(n) > 32 {
		n = n[:32]
	}
	if !shell.ValidNickname(n) {
		return ""
	}
	return n
}

// Config is config.json.
type Config struct {
	Version   int         `json:"version"`
	Dev       string      `json:"dev"`        // human username, e.g. "murat"
	MachineID string      `json:"machine_id"` // random hex, generated at setup
	Remote    Remote      `json:"remote"`
	Configs   []ConfigDir `json:"configs"`
	// DeclinedEmails are accounts this machine's user said are personal, so
	// setup does not offer to share them again. Lowercased.
	DeclinedEmails []string `json:"declined_emails,omitempty"`
	// SendMinIntervalSec throttles usage reports per account (KV write budget).
	SendMinIntervalSec int `json:"send_min_interval_sec,omitempty"`
	// NamePrefix names the dirs julienning registers without an explicit
	// name: <prefix><N> (SPEC "Config names"). Empty means
	// DefaultNamePrefix; read it through Prefix. Changing it never renames
	// configs that are already registered.
	NamePrefix string `json:"name_prefix,omitempty"`
	// AutoUpdate lets detached background processes install new releases
	// (SPEC "Install layout and updates"). nil means on; read it through
	// AutoUpdateEnabled, which also honours EnvAutoUpdate.
	AutoUpdate *bool `json:"auto_update,omitempty"`

	// fileRemote is Remote as stored on disk and loadedRemote is Remote after
	// env overrides. Save writes fileRemote unless the caller changed Remote,
	// so JULIENNING_REMOTE_URL / JULIENNING_TOKEN never leak into config.json.
	fileRemote   Remote
	loadedRemote Remote
}

// DefaultSendMinIntervalSec keeps five devs under Cloudflare's KV free tier.
const DefaultSendMinIntervalSec = 300

// DefaultNamePrefix is the NamePrefix used when config.json sets none:
// auto-named configs are julienning1, julienning2, …. Deliberately generic:
// config names are internal labels and must not leak anything about the dirs
// or the team; users address accounts by nickname.
const DefaultNamePrefix = "julienning"

var (
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	devRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

	machineIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// ErrNotSetup is returned by Load when config.json does not exist.
var ErrNotSetup = errors.New("julienning is not set up on this machine (run `julienning setup`)")

// Dir returns the state directory ($JULIENNING_HOME or ~/.julienning).
func Dir() (string, error) {
	if h := os.Getenv(EnvHome); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".julienning"), nil
}

// Path returns Dir()/name.
func Path(name string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, name), nil
}

// New returns a Config with defaults applied and a fresh machine id.
func New(dev string) (*Config, error) {
	id, err := NewMachineID()
	if err != nil {
		return nil, err
	}
	return &Config{
		Version:            SchemaVersion,
		Dev:                dev,
		MachineID:          id,
		SendMinIntervalSec: DefaultSendMinIntervalSec,
	}, nil
}

// NewMachineID returns 12 random hex chars.
func NewMachineID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate machine id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Load reads config.json. Returns ErrNotSetup when missing. Env overrides for
// the remote are applied here so every command sees the same effective value.
func Load() (*Config, error) {
	p, err := Path(ConfigFile)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotSetup
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if c.Version != SchemaVersion {
		return nil, fmt.Errorf("%s has schema version %d, this binary expects %d (re-run `julienning setup`)", p, c.Version, SchemaVersion)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c.fileRemote = c.Remote
	if v := os.Getenv("JULIENNING_REMOTE_URL"); v != "" {
		c.Remote.URL = v
	}
	if v := os.Getenv("JULIENNING_TOKEN"); v != "" {
		c.Remote.Token = v
	}
	c.loadedRemote = c.Remote
	if c.SendMinIntervalSec <= 0 {
		c.SendMinIntervalSec = DefaultSendMinIntervalSec
	}
	return &c, nil
}

// Save writes config.json atomically (0600, dir 0700).
func (c *Config) Save() error {
	if err := c.Validate(); err != nil {
		return err
	}
	p, err := Path(ConfigFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	// Per field: an unchanged value is written back as it was on disk, so an
	// env override for one field never leaks when the other field changes.
	out := *c
	if c.Remote.URL == c.loadedRemote.URL {
		out.Remote.URL = c.fileRemote.URL
	}
	if c.Remote.Token == c.loadedRemote.Token {
		out.Remote.Token = c.fileRemote.Token
	}
	raw, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	if err := writeAtomic(p, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	c.fileRemote = out.Remote
	c.loadedRemote = c.Remote
	return nil
}

// Validate checks invariants; called on Load and Save.
func (c *Config) Validate() error {
	if !devRe.MatchString(c.Dev) {
		return fmt.Errorf("invalid dev name %q (lowercase letters, digits, . _ - ; max 32)", c.Dev)
	}
	if !machineIDRe.MatchString(c.MachineID) {
		return fmt.Errorf("invalid machine id %q (12 lowercase hex characters; delete it from config.json and re-run setup to regenerate)", c.MachineID)
	}
	if c.NamePrefix != "" && !ValidNamePrefix(c.NamePrefix) {
		return fmt.Errorf("invalid name_prefix %q (%s; fix or remove it)", c.NamePrefix, NamePrefixRule)
	}
	seenName := map[string]bool{}
	seenDir := map[string]bool{}
	for _, cd := range c.Configs {
		if !nameRe.MatchString(cd.Name) {
			return fmt.Errorf("invalid config name %q", cd.Name)
		}
		if !filepath.IsAbs(cd.Dir) {
			return fmt.Errorf("config %q: dir must be absolute, got %q", cd.Name, cd.Dir)
		}
		if seenName[cd.Name] {
			return fmt.Errorf("duplicate config name %q", cd.Name)
		}
		if seenDir[cd.Dir] {
			return fmt.Errorf("duplicate config dir %q", cd.Dir)
		}
		if s := cd.ShareOnLogin; s != nil && s.Nickname != "" && !shell.ValidNickname(s.Nickname) {
			return fmt.Errorf("config %q: invalid share_on_login nickname %q (%s; fix it, or remove the nickname to use the email's local part)", cd.Name, s.Nickname, shell.NicknameRule)
		}
		seenName[cd.Name] = true
		seenDir[cd.Dir] = true
	}
	return nil
}

// ErrRemoteNotConfigured is returned by RequireRemote when the Worker URL or
// token is missing.
var ErrRemoteNotConfigured = errors.New("Worker URL or token not configured (run: julienning setup, or set JULIENNING_REMOTE_URL and JULIENNING_TOKEN)")

// RequireRemote fails when the remote is not usable.
func (c *Config) RequireRemote() error {
	if c.Remote.URL == "" || c.Remote.Token == "" {
		return ErrRemoteNotConfigured
	}
	return nil
}

// AutoUpdateEnabled reports whether background auto-updates may run: the
// auto_update field (absent = on), switched off by a false EnvAutoUpdate. A
// value of EnvAutoUpdate that is not a boolean is an error and counts as off:
// whoever set it meant to configure updates, so installing anyway would be
// the surprising choice. A nil receiver (not set up) consults the env only.
func (c *Config) AutoUpdateEnabled() (bool, error) {
	if v := strings.TrimSpace(os.Getenv(EnvAutoUpdate)); v != "" {
		on, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("%s=%q is not a boolean (use 0 or false to turn auto-update off, or unset it); auto-update stays off until it is fixed", EnvAutoUpdate, v)
		}
		if !on {
			return false, nil
		}
	}
	return c == nil || c.AutoUpdate == nil || *c.AutoUpdate, nil
}

// Declined reports whether email was marked personal on this machine.
func (c *Config) Declined(email string) bool {
	email = strings.ToLower(email)
	for _, e := range c.DeclinedEmails {
		if e == email {
			return true
		}
	}
	return false
}

// Undecline forgets that email was marked personal (idempotent).
func (c *Config) Undecline(email string) {
	email = strings.ToLower(email)
	kept := c.DeclinedEmails[:0]
	for _, e := range c.DeclinedEmails {
		if e != email {
			kept = append(kept, e)
		}
	}
	c.DeclinedEmails = kept
}

// Decline records email as personal (idempotent).
func (c *Config) Decline(email string) {
	if !c.Declined(email) {
		c.DeclinedEmails = append(c.DeclinedEmails, strings.ToLower(email))
		sort.Strings(c.DeclinedEmails)
	}
}

// ValidDevName reports whether s is an acceptable dev username.
func ValidDevName(s string) bool { return devRe.MatchString(s) }

// ValidConfigName reports whether s is an acceptable config name.
func ValidConfigName(s string) bool { return nameRe.MatchString(s) }

// NamePrefixRule describes ValidNamePrefix for error messages.
const NamePrefixRule = "letters, digits, underscore or dash, starting with a letter or digit and not ending in a digit"

// ValidNamePrefix reports whether s can prefix auto-numbered config names.
// It must itself be a valid config name and must not end in a digit, so
// <prefix><N> always splits back into the same prefix and number ("team1"
// followed by 2 would read as team12).
func ValidNamePrefix(s string) bool {
	return nameRe.MatchString(s) && !isDigit(s[len(s)-1])
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// Prefix returns the effective NamePrefix.
func (c *Config) Prefix() string {
	if c.NamePrefix == "" {
		return DefaultNamePrefix
	}
	return c.NamePrefix
}

// Find returns the config dir with the given name.
func (c *Config) Find(name string) (ConfigDir, bool) {
	for _, cd := range c.Configs {
		if cd.Name == name {
			return cd, true
		}
	}
	return ConfigDir{}, false
}

// FindByDir returns the registered config dir for an absolute path.
// Paths are cleaned before comparison; symlinks are not resolved.
func (c *Config) FindByDir(dir string) (ConfigDir, bool) {
	dir = filepath.Clean(dir)
	for _, cd := range c.Configs {
		if filepath.Clean(cd.Dir) == dir {
			return cd, true
		}
	}
	return ConfigDir{}, false
}

// Add registers a config dir, keeping Configs sorted by name.
func (c *Config) Add(cd ConfigDir) error {
	if _, dup := c.Find(cd.Name); dup {
		return fmt.Errorf("config %q already registered", cd.Name)
	}
	if existing, dup := c.FindByDir(cd.Dir); dup {
		return fmt.Errorf("%s already registered as %q", cd.Dir, existing.Name)
	}
	c.Configs = append(c.Configs, cd)
	c.sortConfigs()
	return nil
}

// Rename changes the internal name of a registered config. The dir
// is unchanged, and with it the `current` selection, which stores the path.
func (c *Config) Rename(oldName, newName string) error {
	if !nameRe.MatchString(newName) {
		return fmt.Errorf("invalid config name %q", newName)
	}
	if other, taken := c.Find(newName); taken {
		return fmt.Errorf("config name %q is already used by %s", newName, other.Dir)
	}
	for i := range c.Configs {
		if c.Configs[i].Name == oldName {
			c.Configs[i].Name = newName
			c.sortConfigs()
			return nil
		}
	}
	return fmt.Errorf("config %q is not registered", oldName)
}

func (c *Config) sortConfigs() {
	sort.Slice(c.Configs, func(i, j int) bool { return c.Configs[i].Name < c.Configs[j].Name })
}

// Remove unregisters a config dir by name (does not delete files).
func (c *Config) Remove(name string) bool {
	for i, cd := range c.Configs {
		if cd.Name == name {
			c.Configs = append(c.Configs[:i], c.Configs[i+1:]...)
			return true
		}
	}
	return false
}

// UpdateDir re-reads config.json, applies mutate to the registered dir at
// path dir and saves. Long-running processes (claim-sync) use it instead of
// saving the Config they loaded at start: Save rewrites the whole file, so
// that would undo whatever another command saved in the meantime. found is
// false, and nothing is saved, when the dir is no longer registered. mutate
// must not change Dir.
func UpdateDir(dir string, mutate func(*ConfigDir)) (found bool, err error) {
	c, err := Load()
	if err != nil {
		return false, err
	}
	dir = filepath.Clean(dir)
	for i := range c.Configs {
		if filepath.Clean(c.Configs[i].Dir) == dir {
			mutate(&c.Configs[i])
			return true, c.Save()
		}
	}
	return false, nil
}

// Current returns the selected config dir, or ok=false when none is selected.
// A selection pointing at an unregistered dir is treated as none.
func (c *Config) Current() (ConfigDir, bool, error) {
	p, err := Path(CurrentFile)
	if err != nil {
		return ConfigDir{}, false, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return ConfigDir{}, false, nil
	}
	if err != nil {
		return ConfigDir{}, false, fmt.Errorf("read %s: %w", p, err)
	}
	dir := strings.TrimSpace(string(raw))
	if dir == "" {
		return ConfigDir{}, false, nil
	}
	cd, ok := c.FindByDir(dir)
	return cd, ok, nil
}

// SetCurrent writes the `current` file. The shell wrapper reads this file on
// every `claude` invocation, so it holds the absolute dir path, not the name.
func SetCurrent(cd ConfigDir) error {
	p, err := Path(CurrentFile)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	return writeAtomic(p, []byte(cd.Dir+"\n"), 0o600)
}

// ClearCurrent removes the selection so plain `claude` uses the default dir.
func ClearCurrent() error {
	p, err := Path(CurrentFile)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	return nil
}

// writeAtomic writes via a temp file + rename in the same directory.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename to %s: %w", path, err)
	}
	return nil
}
