// Package hosts owns what this machine knows about other machines running
// julienning (SPEC "Hosts"): hosts.json, the agent protocol both ends speak,
// the version rule, the probe of a new host and locating a project's
// checkout on another machine.
package hosts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/muratgozel/julienning/internal/config"
)

// File is hosts.json in the state dir.
const File = "hosts.json"

// SchemaVersion of hosts.json.
const SchemaVersion = 1

// Local names this machine wherever a host name is taken (--host local,
// transfer --to local); it can never name a host.
const Local = "local"

var (
	nameRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)
	machineIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// NameRule describes a valid host name for error messages.
const NameRule = "1-32 characters: lowercase letters, digits, dot, underscore or dash, starting with a letter or digit; local is reserved"

// ValidName reports whether s can name a host (the nickname rule, minus
// "local").
func ValidName(s string) bool { return nameRe.MatchString(s) && s != Local }

// ValidSSH checks an ssh destination (an ~/.ssh/config alias or
// user@host). It is passed after `--`, and must not start with "-" anyway:
// ssh would read "-oProxyCommand=…" as an option.
func ValidSSH(s string) error {
	switch {
	case s == "":
		return errors.New("the ssh destination is empty")
	case len(s) > 255:
		return errors.New("the ssh destination is longer than 255 characters")
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("ssh destination %q starts with -, which ssh would read as an option", s)
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("ssh destination %q contains whitespace or control characters", s)
		}
	}
	return nil
}

// Paths are absolute paths on the host, found when it was added: a
// non-interactive ssh command gets a minimal PATH, so nothing is looked up
// through PATH there.
type Paths struct {
	Julienning string  `json:"julienning"`
	Claude     *string `json:"claude"`
	Tmux       *string `json:"tmux"`

	extra map[string]json.RawMessage
}

// Host is one entry of hosts.json.
type Host struct {
	Name      string    `json:"name"`
	SSH       string    `json:"ssh"`
	MachineID string    `json:"machine_id"`
	Home      string    `json:"home"`
	OS        string    `json:"os"`
	Arch      string    `json:"arch"`
	Paths     Paths     `json:"paths"`
	AddedAt   time.Time `json:"added_at"`
	CheckedAt time.Time `json:"checked_at"`

	// extra holds fields a newer julienning wrote; Save writes them back.
	extra map[string]json.RawMessage
}

// Hosts is hosts.json.
type Hosts struct {
	Version int    `json:"version"`
	Hosts   []Host `json:"hosts"`

	extra map[string]json.RawMessage
}

// Validate checks one entry; called on load and save.
func (h Host) Validate() error {
	if !ValidName(h.Name) {
		return fmt.Errorf("invalid host name %q (%s)", h.Name, NameRule)
	}
	if err := ValidSSH(h.SSH); err != nil {
		return fmt.Errorf("host %s: %w", h.Name, err)
	}
	if !machineIDRe.MatchString(h.MachineID) {
		return fmt.Errorf("host %s: invalid machine id %q", h.Name, h.MachineID)
	}
	if !filepath.IsAbs(h.Home) {
		return fmt.Errorf("host %s: home %q is not an absolute path", h.Name, h.Home)
	}
	if h.OS != "darwin" && h.OS != "linux" {
		return fmt.Errorf("host %s: unsupported os %q", h.Name, h.OS)
	}
	if h.Arch != "amd64" && h.Arch != "arm64" {
		return fmt.Errorf("host %s: unsupported arch %q", h.Name, h.Arch)
	}
	// The julienning path goes on the remote command line in single quotes.
	if p := h.Paths.Julienning; !filepath.IsAbs(p) || strings.ContainsAny(p, `'\`) || strings.ContainsFunc(p, unicode.IsControl) {
		return fmt.Errorf("host %s: julienning path %q must be absolute, without quotes, backslashes or control characters", h.Name, p)
	}
	for _, p := range []*string{h.Paths.Claude, h.Paths.Tmux} {
		if p != nil && !filepath.IsAbs(*p) {
			return fmt.Errorf("host %s: path %q is not absolute", h.Name, *p)
		}
	}
	return nil
}

// Validate checks the whole file: every entry, unique names and machine ids.
func (f *Hosts) Validate() error {
	names, ids := map[string]bool{}, map[string]string{}
	for _, h := range f.Hosts {
		if err := h.Validate(); err != nil {
			return err
		}
		if names[h.Name] {
			return fmt.Errorf("duplicate host %q", h.Name)
		}
		if other, dup := ids[h.MachineID]; dup {
			return fmt.Errorf("hosts %s and %s have the same machine id %s", other, h.Name, h.MachineID)
		}
		names[h.Name], ids[h.MachineID] = true, h.Name
	}
	return nil
}

// Path is hosts.json's path.
func Path() (string, error) { return config.Path(File) }

// Load reads hosts.json; a missing file is an empty list.
func Load() (*Hosts, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return &Hosts{Version: SchemaVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var f Hosts
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if f.Version != SchemaVersion {
		return nil, fmt.Errorf("%s has version %d, this julienning reads %d (update julienning)", p, f.Version, SchemaVersion)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &f, nil
}

// Save writes hosts.json atomically (0600), sorted by name.
func (f *Hosts) Save() error {
	f.Version = SchemaVersion
	sort.Slice(f.Hosts, func(i, j int) bool { return f.Hosts[i].Name < f.Hosts[j].Name })
	if err := f.Validate(); err != nil {
		return err
	}
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(p), err)
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode hosts: %w", err)
	}
	return config.WriteFileAtomic(p, append(raw, '\n'), 0o600)
}

// Find returns the host named name.
func (f *Hosts) Find(name string) (Host, bool) {
	for _, h := range f.Hosts {
		if h.Name == name {
			return h, true
		}
	}
	return Host{}, false
}

// ByMachineID returns the host with machine id id.
func (f *Hosts) ByMachineID(id string) (Host, bool) {
	for _, h := range f.Hosts {
		if h.MachineID == id {
			return h, true
		}
	}
	return Host{}, false
}

// Put adds h or replaces the entry of the same name, keeping the fields a
// newer julienning added to it.
func (f *Hosts) Put(h Host) {
	for i := range f.Hosts {
		if f.Hosts[i].Name == h.Name {
			if h.extra == nil {
				h.extra = f.Hosts[i].extra
			}
			if h.Paths.extra == nil {
				h.Paths.extra = f.Hosts[i].Paths.extra
			}
			f.Hosts[i] = h
			return
		}
	}
	f.Hosts = append(f.Hosts, h)
}

// Remove drops the host named name; false when there is none.
func (f *Hosts) Remove(name string) bool {
	for i, h := range f.Hosts {
		if h.Name == name {
			f.Hosts = append(f.Hosts[:i], f.Hosts[i+1:]...)
			return true
		}
	}
	return false
}

// --- unknown fields survive a load and save ---

func splitKnown(raw []byte, known []string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(all, k)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

func mergeExtra(known []byte, extra map[string]json.RawMessage) ([]byte, error) {
	if len(extra) == 0 {
		return known, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(known, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		if _, ok := m[k]; !ok {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

func (p *Paths) UnmarshalJSON(raw []byte) error {
	type plain Paths
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	extra, err := splitKnown(raw, []string{"julienning", "claude", "tmux"})
	if err != nil {
		return err
	}
	*p = Paths(v)
	p.extra = extra
	return nil
}

func (p Paths) MarshalJSON() ([]byte, error) {
	type plain Paths
	known, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	return mergeExtra(known, p.extra)
}

func (h *Host) UnmarshalJSON(raw []byte) error {
	type plain Host
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	extra, err := splitKnown(raw, []string{"name", "ssh", "machine_id", "home", "os", "arch", "paths", "added_at", "checked_at"})
	if err != nil {
		return err
	}
	*h = Host(v)
	h.extra = extra
	return nil
}

func (h Host) MarshalJSON() ([]byte, error) {
	type plain Host
	known, err := json.Marshal(plain(h))
	if err != nil {
		return nil, err
	}
	return mergeExtra(known, h.extra)
}

func (f *Hosts) UnmarshalJSON(raw []byte) error {
	type plain Hosts
	var v plain
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	extra, err := splitKnown(raw, []string{"version", "hosts"})
	if err != nil {
		return err
	}
	*f = Hosts(v)
	f.extra = extra
	return nil
}

func (f Hosts) MarshalJSON() ([]byte, error) {
	type plain Hosts
	if f.Hosts == nil {
		f.Hosts = []Host{}
	}
	known, err := json.Marshal(plain(f))
	if err != nil {
		return nil, err
	}
	return mergeExtra(known, f.extra)
}

// ReceivedDir holds receipts of sessions this machine received by
// transfer, one <session id>.json each.
const ReceivedDir = "received"
