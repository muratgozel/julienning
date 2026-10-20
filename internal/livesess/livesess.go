// Package livesess reads Claude Code's live-session registry:
// <config dir>/sessions/<pid>.json, one file per running claude process.
//
// CRITICAL for future agents: this registry is an undocumented Claude Code
// internal (observed in 2.1.28x to 2.1.304). Treat everything here as best
// effort: RegistryExists lets callers fall back when a Claude version stops
// writing it, and Alive answers "alive" whenever it cannot tell, because a
// live session taken for dead gets moved while Claude still writes it.
// procStart differs by OS and pidDomain scopes the pid; see Alive. Never
// read the sibling `<pid>.<hash>.key` files; they hold a peer token and are
// none of our business.
package livesess

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	PIDDomain string `json:"pidDomain"` // pid namespace of PID, may be empty (older versions)

	ConfigDir string `json:"-"` // filled by List
}

var registryName = regexp.MustCompile(`^[0-9]+\.json$`)

// ProbeSessionName is the name julienning's usage probe gives its `claude -p
// "/usage"` process (internal/usageprobe). Claude Code registers that process
// as kind "interactive" for the second or so it runs; CountsAsSession leaves
// it out, or a claim-sync running meanwhile would claim the account for a
// session nobody has.
const ProbeSessionName = "julienning-usage-probe"

// CountsAsSession reports whether a registry entry is a user-facing session
// for claim purposes. Claude also registers daemon, daemon-worker and
// pre-spawned spare processes; counting those would hold claims forever.
// Unknown/empty kinds (older versions) count, to stay conservative. Move
// safety must NOT use this: any live entry with a matching session id blocks.
func CountsAsSession(s Session) bool {
	if s.Spare || s.Name == ProbeSessionName {
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
// wrote the entry (pid reuse is caught by comparing start times when ps or
// /proc can tell us). A var so tests can stub process inspection.
var Alive = func(s Session) bool {
	if s.PID <= 0 {
		return false
	}
	// A pid from another machine or pid namespace (a dir shared with a
	// container, or claude in a Flatpak terminal) says nothing about this
	// namespace's processes, so the entry cannot be proven dead; Claude Code
	// treats it the same way.
	if s.PIDDomain != "" && !ownDomain(s.PIDDomain) {
		return true
	}
	err := syscall.Kill(s.PID, 0)
	if err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if s.ProcStart == "" {
		return true
	}
	// On Linux and WSL Claude Code writes procStart as the starttime field
	// of /proc/<pid>/stat (clock ticks since boot, e.g. "12098273"), not
	// the ps ctime string it writes on macOS (2.1.303, 2.1.304).
	if want, err := strconv.ParseUint(s.ProcStart, 10, 64); err == nil {
		st, ok := readStat(s.PID)
		if !ok {
			return true
		}
		if st.start == want {
			return st.state != "Z" && st.state != "X" // exited, not yet reaped
		}
		// The process that wrote an entry we read started before we did, so
		// a larger number is no tick count we understand (say, the format
		// moved to epoch milliseconds): unknown, rather than every live
		// session reported gone.
		own, ok := readStat(os.Getpid())
		return !ok || want > own.start
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

// localDomain is this process's pid namespace in the shape of Claude Code's
// pidDomain: "darwin" on macOS, ":<machine-id>:<readlink
// /proc/self/ns/pid>" on Linux (the "<platform>" prefix Claude adds is left
// out). A read error yields "" for that part, as it does in Claude Code, so
// the two still agree. A var so tests can pick a namespace.
var localDomain = sync.OnceValue(func() string {
	if runtime.GOOS != "linux" {
		return "darwin"
	}
	id, _ := os.ReadFile("/etc/machine-id")
	ns, _ := os.Readlink("/proc/self/ns/pid")
	return ":" + strings.TrimSpace(string(id)) + ":" + ns
})

// ownDomain reports whether d, an entry's pidDomain, is this process's.
func ownDomain(d string) bool {
	local := localDomain()
	if strings.HasPrefix(local, ":") {
		return strings.HasSuffix(d, local)
	}
	return d == local
}

// procRoot is where readStat reads; a var so tests can use a fake tree.
var procRoot = "/proc"

type procStat struct {
	state string // field 3: R, S, Z (zombie), X (dead), ...
	start uint64 // field 22: starttime, clock ticks since boot
}

// readStat reads /proc/<pid>/stat. Field 2 is the command name in
// parentheses and may itself hold spaces and ")", so fields are counted from
// the last ")", as Claude Code does.
func readStat(pid int) (procStat, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, false
	}
	s := string(raw)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procStat{}, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return procStat{}, false
	}
	n, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procStat{}, false
	}
	return procStat{state: fields[0], start: n}, true
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
