package claudecfg

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/muratgozel/julienning/internal/config"
)

// Entry names MissingEntries reports: the statusLine key and the hook events.
const (
	EntryStatusLine   = keyStatusLine
	EntrySessionStart = "SessionStart"
	EntrySessionEnd   = "SessionEnd"
	EntryStopFailure  = "StopFailure"
)

// MissingEntries reports which of julienning's entries <dir>/settings.json
// lacks, in Patch's order: EntryStatusLine, then one name per hook event
// without a julienning entry for its subcommand (ownership as in
// IsJulienningCommand). A missing file lacks all of them. An unreadable or
// non-object file is an error; nothing is written.
func MissingEntries(dir string) ([]string, error) {
	raw, err := ReadSettings(dir)
	if err != nil {
		return nil, err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("%s is not a JSON object", SettingsPath(dir))
	}
	var missing []string
	if _, cmd, ok := decodeHook(doc[keyStatusLine]); !ok || !isJulienningCommandFor(cmd, subStatusLine) {
		missing = append(missing, EntryStatusLine)
	}
	var hooks map[string]json.RawMessage
	_ = json.Unmarshal(doc[keyHooks], &hooks) // absent or not an object: no hooks at all
	for _, ev := range hookEvents {
		if !hasOwnHook(hooks[ev.name], ev.sub) {
			missing = append(missing, ev.name)
		}
	}
	return missing, nil
}

// hasOwnHook reports whether an event's matcher groups hold a julienning
// entry running sub.
func hasOwnHook(raw json.RawMessage, sub string) bool {
	var groups []json.RawMessage
	if json.Unmarshal(raw, &groups) != nil {
		return false
	}
	for _, g := range groups {
		var group struct {
			Hooks []json.RawMessage `json:"hooks"`
		}
		if json.Unmarshal(g, &group) != nil {
			continue
		}
		for _, h := range group.Hooks {
			if _, cmd, ok := decodeHook(h); ok && isJulienningCommandFor(cmd, sub) {
				return true
			}
		}
	}
	return false
}

// SetupWarning is the one line accounts, next and use print when a
// registered dir's settings.json lacks an entry this julienning adds, as
// after an upgrade that wires a new hook; "" when nothing is missing. Dirs
// that do not exist, and settings.json files that cannot be read or parsed,
// are skipped: setup reports those itself.
func SetupWarning(dirs []config.ConfigDir) string {
	var names []string
	seen := map[string]bool{}
	var union []string
	for _, cd := range dirs {
		if fi, err := os.Stat(cd.Dir); err != nil || !fi.IsDir() {
			continue
		}
		missing, err := MissingEntries(cd.Dir)
		if err != nil || len(missing) == 0 {
			continue
		}
		names = append(names, cd.Name)
		for _, m := range missing {
			if !seen[m] {
				seen[m] = true
				union = append(union, m)
			}
		}
	}
	if len(names) == 0 {
		return ""
	}
	where := "settings.json of " + joinAnd(names)
	if len(union) == 1 && union[0] == EntryStopFailure {
		return "this julienning adds a rate-limit hook that " + where + " does not have yet; run: julienning setup"
	}
	// Generic on purpose: it has to stay true for entries a later version adds.
	return where + " lacks entries this julienning adds (" + describeEntries(union) + "); run: julienning setup"
}

// describeEntries renders MissingEntries names for a message: "statusLine,
// SessionStart hook, StopFailure hook". Entries keep Patch's order.
func describeEntries(entries []string) string {
	out := make([]string, 0, len(entries))
	for _, e := range allEntries() {
		for _, m := range entries {
			if m != e {
				continue
			}
			if e != EntryStatusLine {
				e += " hook"
			}
			out = append(out, e)
		}
	}
	return strings.Join(out, ", ")
}

// allEntries lists every entry Patch adds, in its order.
func allEntries() []string {
	out := []string{EntryStatusLine}
	for _, ev := range hookEvents {
		out = append(out, ev.name)
	}
	return out
}

// joinAnd renders "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}
