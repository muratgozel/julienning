// Package livesess reads Claude Code's live-session registry:
// <config dir>/sessions/<pid>.json, one file per running claude process.
//
// CRITICAL for future agents: this registry is an undocumented Claude Code
// internal (observed in 2.1.28x). Treat everything here as best effort:
// RegistryExists lets callers fall back when a Claude version stops writing
// it. Never read the sibling `<pid>.<hash>.key` files; they hold a peer
// token and are none of our business.
package livesess

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Session mirrors the fields julienning uses from <pid>.json.
type Session struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`   // /rename title, may be empty
	Status    string `json:"status"` // "busy" | "idle" | ...
	Kind      string `json:"kind"`   // "interactive" | ...
	ProcStart string `json:"procStart"`
	StartedAt int64  `json:"startedAt"` // unix ms
	UpdatedAt int64  `json:"updatedAt"` // unix ms
	Spare     bool   `json:"spare"`     // pre-spawned process not yet serving a session

	ConfigDir string `json:"-"` // filled by List
}

var registryName = regexp.MustCompile(`^[0-9]+\.json$`)

// CountsAsSession reports whether a registry entry is a user-facing session
// for claim purposes. Claude also registers daemon, daemon-worker and
// pre-spawned spare processes; counting those would hold claims forever.
// Unknown/empty kinds (older versions) count, to stay conservative. Move
// safety must NOT use this: any live entry with a matching session id blocks.
func CountsAsSession(s Session) bool {
	if s.Spare {
		return false
	}
	switch s.Kind {
	case "", "interactive", "bg":
		return true
	default:
		return false
	}
}

// RegistryExists reports whether dir has a sessions/ registry at all.
func RegistryExists(configDir string) bool {
	fi, err := os.Stat(filepath.Join(configDir, "sessions"))
	return err == nil && fi.IsDir()
}

// List returns the registry entries of configDir whose process is alive.
// A missing registry yields no sessions and no error; unreadable or
// malformed entries are skipped (the registry is written concurrently).
func List(configDir string) ([]Session, error) {
	dir := filepath.Join(configDir, "sessions")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []Session
	for _, e := range entries {
		if e.IsDir() || !registryName.MatchString(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(raw, &s) != nil || s.PID <= 0 || s.SessionID == "" {
			continue
		}
		if strconv.Itoa(s.PID)+".json" != e.Name() {
			continue
		}
		s.ConfigDir = configDir
		if Alive(s) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Alive reports whether s's process still runs and is the same process that
// wrote the entry (pid reuse is caught by comparing start times when ps can
// tell us). A var so tests can stub process inspection.
var Alive = func(s Session) bool {
	if s.PID <= 0 {
		return false
	}
	err := syscall.Kill(s.PID, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if s.ProcStart == "" {
		return true
	}
	want, ok := parseStart(s.ProcStart)
	if !ok {
		return true
	}
	got, ok := psStart(s.PID)
	if !ok {
		return true
	}
	return got.Equal(want)
}

// psStart asks ps for the process start time in UTC. macOS and procps print
// different field orders ("Tue Sep 29 ..." vs "Tue 29 Sep ..."), so both are
// parsed rather than compared as strings.
func psStart(pid int) (time.Time, bool) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, false
	}
	return parseStart(string(out))
}

func parseStart(s string) (time.Time, bool) {
	norm := strings.Join(strings.Fields(s), " ")
	for _, layout := range []string{"Mon Jan 2 15:04:05 2006", "Mon 2 Jan 15:04:05 2006"} {
		if t, err := time.ParseInLocation(layout, norm, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
