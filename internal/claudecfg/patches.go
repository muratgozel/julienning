package claudecfg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/muratgozel/julienning/internal/config"
	"github.com/muratgozel/julienning/internal/jsonedit"
	"github.com/muratgozel/julienning/internal/paths"
)

// PatchesFile (under config.Dir) records, per resolved settings.json path,
// what Patch needs to undo later: the statusLine it replaced and which
// containers it created.
const PatchesFile = "patches.json"

// Settings keys and the julienning subcommands wired into them.
const (
	keyStatusLine = "statusLine"
	keyHooks      = "hooks"

	subStatusLine   = "statusline"
	subSessionStart = "hook session-start"
	subSessionEnd   = "hook session-end"
	subStopFailure  = "hook stop-failure"
)

// hookEvent is one hook julienning wires: the event, the julienning
// subcommand it runs and the matcher of the group Patch appends ("" for
// none).
type hookEvent struct{ name, sub, matcher string }

// hookEvents is every hook julienning owns, in the order a fresh hooks
// object lists them (hooksValue must agree). StopFailure fires when a turn
// ends on an API error; the rate_limit matcher keeps it to usage-limit
// refusals, which it reports as an exhausted account.
var hookEvents = []hookEvent{
	{EntrySessionStart, subSessionStart, ""},
	{EntrySessionEnd, subSessionEnd, ""},
	{EntryStopFailure, subStopFailure, "rate_limit"},
}

// Action is what Patch or Unpatch did to one settings.json.
type Action int

const (
	Unchanged Action = iota
	Added
	Updated
	Removed
)

func (a Action) String() string {
	switch a {
	case Added:
		return "added"
	case Updated:
		return "updated"
	case Removed:
		return "removed"
	default:
		return "unchanged"
	}
}

// Result reports one Patch/Unpatch.
type Result struct {
	Action Action
	Path   string // <dir>/settings.json
	// ReplacedStatusLine describes a non-julienning statusLine Patch replaced
	// (its raw value is saved in patches.json for Unpatch). Empty otherwise.
	ReplacedStatusLine string
	// Changes lists what Unpatch removed or restored, in order.
	Changes []string
}

func (r Result) String() string { return r.Action.String() }

type statusLineValue struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type hookGroup struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []statusLineValue `json:"hooks"`
}

// hooksValue keeps the events in hookEvents order (a map would sort them).
type hooksValue struct {
	SessionStart []hookGroup `json:"SessionStart"`
	SessionEnd   []hookGroup `json:"SessionEnd"`
	StopFailure  []hookGroup `json:"StopFailure"`
}

// group is the matcher group Patch adds for ev, running exe.
func (ev hookEvent) group(exe string) hookGroup {
	return hookGroup{Matcher: ev.matcher, Hooks: []statusLineValue{{Type: "command", Command: Command(exe, ev.sub)}}}
}

// Command is the settings.json command running `exe sub`. exe is quoted for
// sh when needed: Claude runs these through a shell, and an unquoted path with
// a space would also defeat the ownership test.
func Command(exe, sub string) string { return shellQuote(exe) + " " + sub }

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// IsJulienningCommand is the SPEC ownership test: the command ends with
// " statusline", " hook session-start", " hook session-end" or " hook
// stop-failure" and its first shell word (unquoted) either has the basename
// "julienning" or is an installed version file (.../julienning/versions/<ver>),
// which is what a dev build or a missing install symlink writes
// (paths.StableCommand, stable=false). Without the second form a re-run would
// not recognise its own entries, append duplicate hooks and record its own
// statusLine as the user's.
func IsJulienningCommand(cmd string) bool {
	return isJulienningCommandFor(cmd, "")
}

// isJulienningCommandFor is IsJulienningCommand limited to one subcommand
// (sub "" accepts any of julienning's).
func isJulienningCommandFor(cmd, sub string) bool {
	cmd = strings.TrimSpace(cmd)
	subs := []string{sub}
	if sub == "" {
		subs = []string{subStatusLine}
		for _, ev := range hookEvents {
			subs = append(subs, ev.sub)
		}
	}
	if !slices.ContainsFunc(subs, func(s string) bool { return strings.HasSuffix(cmd, " "+s) }) {
		return false
	}
	first := firstWord(cmd)
	if first == "" {
		return false
	}
	return filepath.Base(first) == "julienning" || paths.IsVersionFile(first)
}

// firstWord returns the first sh word of s with quotes removed.
func firstWord(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch ch := s[i]; ch {
		case ' ', '\t', '\n':
			return b.String()
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return b.String() + s[i+1:]
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
			}
		case '\\':
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// Patch wires julienning into <dir>/settings.json: statusLine plus one
// SessionStart, one SessionEnd and one StopFailure (matcher rate_limit) hook
// entry, all running exe (normally paths.StableCommand). Every other byte of
// the file is preserved; the file is written atomically, through symlinks,
// and only when it changes. A non-julienning statusLine is recorded in
// patches.json before it is replaced so Unpatch can restore it. On a file an
// older julienning patched, the entries it lacks are added (and recorded as
// created) and the action is Updated.
func Patch(dir, exe string) (Result, error) {
	path := SettingsPath(dir)
	res := Result{Path: path}
	if strings.TrimSpace(exe) == "" {
		return res, errors.New("julienning executable path must not be empty")
	}
	doc, err := openSettings(path)
	if err != nil {
		return res, err
	}
	store, err := loadPatches()
	if err != nil {
		return res, err
	}
	rec := store.record(doc.key)
	rec.dropOwnPrior()
	if !doc.exists {
		rec.CreatedFile = true
	}
	obj := doc.obj
	present := 0 // julienning entries found before patching

	// statusLine
	want := Command(exe, subStatusLine)
	cur, has := obj.Get(keyStatusLine)
	switch {
	case !has:
		if err := obj.SetValue(keyStatusLine, statusLineValue{Type: "command", Command: want}); err != nil {
			return res, editErr(path, err)
		}
	case ownedStatusLine(cur):
		present++
		if err := updateCommand(obj, keyStatusLine, want); err != nil {
			return res, editErr(path, err)
		}
	default:
		prior := string(cur)
		rec.StatusLine = &prior
		res.ReplacedStatusLine = describe(cur)
		if err := obj.SetValue(keyStatusLine, statusLineValue{Type: "command", Command: want}); err != nil {
			return res, editErr(path, err)
		}
	}

	// hooks
	hooks, has, err := obj.Object(keyHooks)
	switch {
	case err != nil:
		return res, fmt.Errorf("%s: %q is not a JSON object; leaving the file alone", path, keyHooks)
	case !has:
		v := hooksValue{
			SessionStart: []hookGroup{hookEvents[0].group(exe)},
			SessionEnd:   []hookGroup{hookEvents[1].group(exe)},
			StopFailure:  []hookGroup{hookEvents[2].group(exe)},
		}
		if err := obj.SetValue(keyHooks, v); err != nil {
			return res, editErr(path, err)
		}
		rec.CreatedHooks = true
		for _, ev := range hookEvents {
			rec.addEvent(ev.name)
		}
	default:
		for _, ev := range hookEvents {
			arr, has, err := hooks.Array(ev.name)
			if err != nil {
				return res, fmt.Errorf("%s: hooks.%s is not a JSON array; leaving the file alone", path, ev.name)
			}
			if !has {
				// Also how an older julienning's file gains a newer event:
				// recorded as created, so Unpatch removes the container too.
				if err := hooks.SetValue(ev.name, []hookGroup{ev.group(exe)}); err != nil {
					return res, editErr(path, err)
				}
				rec.addEvent(ev.name)
				continue
			}
			found, err := ensureHook(arr, ev.group(exe))
			if err != nil {
				return res, editErr(path, err)
			}
			if found {
				present++
			}
			if err := hooks.Set(ev.name, arr.Bytes()); err != nil {
				return res, editErr(path, err)
			}
		}
		if err := obj.Set(keyHooks, hooks.Bytes()); err != nil {
			return res, editErr(path, err)
		}
	}

	if !doc.changed() {
		res.Action = Unchanged
		return res, nil
	}
	res.Action = Added
	if present > 0 {
		res.Action = Updated
	}
	// The undo record must be durable before the statusLine it describes is
	// overwritten; a crash in between must never lose the user's statusLine.
	if err := store.save(); err != nil {
		return res, err
	}
	if err := doc.save(); err != nil {
		return res, err
	}
	return res, nil
}

// Unpatch removes what Patch added to <dir>/settings.json: julienning's
// statusLine (restoring the recorded prior value), julienning hook entries,
// and hook containers Patch created that are now empty. A settings.json
// Patch created and that ends up empty is deleted. Safe to call repeatedly.
func Unpatch(dir string) (Result, error) {
	path := SettingsPath(dir)
	res := Result{Path: path}
	doc, err := openSettings(path)
	if err != nil {
		return res, err
	}
	store, err := loadPatches()
	if err != nil {
		return res, err
	}
	rec := store.record(doc.key)
	rec.dropOwnPrior()
	obj := doc.obj

	res.Changes, err = stripOwned(obj, path, rec.plan(path, false))
	if err != nil {
		return res, err
	}

	if doc.changed() {
		res.Action = Removed
		if rec.CreatedFile && obj.Len() == 0 && isRegularFile(path) {
			if err := os.Remove(path); err != nil {
				return res, fmt.Errorf("remove %s: %w", path, err)
			}
			res.Changes = append(res.Changes, "deleted the settings.json julienning created")
		} else if err := doc.save(); err != nil {
			return res, err
		}
	}
	// The record is spent either way: nothing of julienning's is left to undo.
	store.forget(doc.key)
	if err := store.save(); err != nil {
		return res, err
	}
	return res, nil
}

// SettingsWithoutJulienning returns <dir>/settings.json as it would read had
// julienning never patched it: julienning's statusLine and hook entries are
// removed, the statusLine they replaced is restored from patches.json, and
// hook containers left empty by the removal are dropped. nil means there is
// nothing to copy (no file, or only what julienning created). new-config
// --copy-settings-from uses it so the copy is patched like a fresh file and
// forget/uninstall leave no empty "hooks" arrays behind. Nothing is written.
func SettingsWithoutJulienning(dir string) ([]byte, error) {
	path := SettingsPath(dir)
	doc, err := openSettings(path)
	if err != nil {
		return nil, err
	}
	if !doc.exists {
		return nil, nil
	}
	store, err := loadPatches()
	if err != nil {
		return nil, err
	}
	rec := &patchRecord{}
	if r, ok := store.Files[doc.key]; ok {
		cp := *r
		rec = &cp
	}
	rec.dropOwnPrior()
	if _, err := stripOwned(doc.obj, path, rec.plan(path, true)); err != nil {
		return nil, err
	}
	if rec.CreatedFile && doc.obj.Len() == 0 {
		return nil, nil
	}
	return doc.obj.Bytes(), nil
}

// stripPlan says how stripOwned undoes julienning's entries.
type stripPlan struct {
	prior         []byte // statusLine to put back; nil deletes julienning's
	priorErr      error  // the recorded statusLine is unusable; reported only if needed
	createdEvents []string
	createdHooks  bool
	// dropEmptied also drops hook containers that held only julienning
	// entries, whoever created them (used for copies, where the record of
	// who created what belongs to another file).
	dropEmptied bool
}

// stripOwned removes julienning's statusLine and hook entries from obj and
// returns what it changed, in order.
func stripOwned(obj *jsonedit.Object, path string, plan stripPlan) ([]string, error) {
	var changes []string
	if cur, has := obj.Get(keyStatusLine); has && ownedStatusLine(cur) {
		switch {
		case plan.priorErr != nil:
			return changes, plan.priorErr
		case plan.prior != nil:
			if err := obj.Set(keyStatusLine, plan.prior); err != nil {
				return changes, editErr(path, err)
			}
			changes = append(changes, "restored previous statusLine")
		default:
			if _, err := obj.Delete(keyStatusLine); err != nil {
				return changes, editErr(path, err)
			}
			changes = append(changes, "removed statusLine")
		}
	}

	hooks, has, err := obj.Object(keyHooks)
	if err != nil || !has {
		return changes, nil
	}
	removedAny := false
	seen := map[string]bool{}
	for _, event := range hooks.Keys() {
		if seen[event] {
			continue
		}
		seen[event] = true
		arr, ok, err := hooks.Array(event)
		if err != nil || !ok {
			continue
		}
		n, err := removeOwned(arr)
		if err != nil {
			return changes, editErr(path, err)
		}
		if n == 0 {
			continue
		}
		removedAny = true
		changes = append(changes, "removed "+event+" hook")
		if arr.Len() == 0 && (plan.dropEmptied || slices.Contains(plan.createdEvents, event)) {
			_, err = hooks.Delete(event)
		} else {
			err = hooks.Set(event, arr.Bytes())
		}
		if err != nil {
			return changes, editErr(path, err)
		}
	}
	if hooks.Len() == 0 && (plan.createdHooks || plan.dropEmptied && removedAny) {
		if _, err := obj.Delete(keyHooks); err != nil {
			return changes, editErr(path, err)
		}
		changes = append(changes, "removed empty hooks")
	} else if err := obj.Set(keyHooks, hooks.Bytes()); err != nil {
		return changes, editErr(path, err)
	}
	return changes, nil
}

// ensureHook makes arr (one hook event's matcher groups) hold exactly one
// julienning entry running want's command: the first one found, in any
// group, is updated in place (its group's matcher is left as it is), later
// duplicates are removed, and want (with its matcher) is appended when none
// exists.
func ensureHook(arr *jsonedit.Array, want hookGroup) (found bool, err error) {
	wantCmd := want.Hooks[0].Command
	for gi := 0; gi < arr.Len(); gi++ {
		group, err := arr.Object(gi)
		if err != nil {
			continue // not a matcher group; not ours to judge
		}
		inner, ok, err := group.Array(keyHooks)
		if err != nil || !ok {
			continue
		}
		removed := false
		for hi := 0; hi < inner.Len(); hi++ {
			typ, cmd, isObj := decodeHook(inner.Get(hi))
			if !isObj || !IsJulienningCommand(cmd) {
				continue
			}
			if found {
				if err := inner.Delete(hi); err != nil {
					return found, err
				}
				hi--
				removed = true
				continue
			}
			found = true
			if typ == "command" && cmd == wantCmd {
				continue
			}
			h, err := inner.Object(hi)
			if err != nil {
				return found, err
			}
			if err := updateCommand(h, "", wantCmd); err != nil {
				return found, err
			}
			if err := inner.Set(hi, h.Bytes()); err != nil {
				return found, err
			}
		}
		if removed && inner.Len() == 0 {
			if err := arr.Delete(gi); err != nil {
				return found, err
			}
			gi--
			continue
		}
		if err := group.Set(keyHooks, inner.Bytes()); err != nil {
			return found, err
		}
		if err := arr.Set(gi, group.Bytes()); err != nil {
			return found, err
		}
	}
	if !found {
		if err := arr.AppendValue(want); err != nil {
			return false, err
		}
	}
	return found, nil
}

// removeOwned deletes every julienning hook entry from an event's groups and
// drops groups that only held julienning entries. It returns how many
// entries it removed.
func removeOwned(arr *jsonedit.Array) (int, error) {
	total := 0
	for gi := 0; gi < arr.Len(); gi++ {
		group, err := arr.Object(gi)
		if err != nil {
			continue
		}
		inner, ok, err := group.Array(keyHooks)
		if err != nil || !ok {
			continue
		}
		n := 0
		for hi := 0; hi < inner.Len(); hi++ {
			if _, cmd, isObj := decodeHook(inner.Get(hi)); isObj && IsJulienningCommand(cmd) {
				if err := inner.Delete(hi); err != nil {
					return total, err
				}
				hi--
				n++
			}
		}
		if n == 0 {
			continue
		}
		total += n
		if inner.Len() == 0 {
			if err := arr.Delete(gi); err != nil {
				return total, err
			}
			gi--
			continue
		}
		if err := group.Set(keyHooks, inner.Bytes()); err != nil {
			return total, err
		}
		if err := arr.Set(gi, group.Bytes()); err != nil {
			return total, err
		}
	}
	return total, nil
}

// updateCommand sets type/command on the object at key (or on obj itself when
// key is ""), touching only those two members.
func updateCommand(obj *jsonedit.Object, key, want string) error {
	target := obj
	if key != "" {
		child, _, err := obj.Object(key)
		if err != nil {
			return err
		}
		target = child
	}
	if raw, _ := target.Get("type"); string(raw) != `"command"` {
		if err := target.SetValue("type", "command"); err != nil {
			return err
		}
	}
	var cur string
	if raw, ok := target.Get("command"); ok {
		_ = json.Unmarshal(raw, &cur) // a non-string command is simply replaced
	}
	if cur != want {
		if err := target.SetValue("command", want); err != nil {
			return err
		}
	}
	if key != "" {
		return obj.Set(key, target.Bytes())
	}
	return nil
}

// decodeHook reads type/command of one hook entry; ok is false when the
// entry is not an object.
func decodeHook(raw []byte) (typ, cmd string, ok bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return "", "", false
	}
	_ = json.Unmarshal(m["type"], &typ)    // absent or non-string → ""
	_ = json.Unmarshal(m["command"], &cmd) // absent or non-string → ""
	return typ, cmd, true
}

func ownedStatusLine(raw []byte) bool {
	_, cmd, ok := decodeHook(raw)
	return ok && IsJulienningCommand(cmd)
}

// describe renders a replaced statusLine for the setup report.
func describe(raw []byte) string {
	if _, cmd, ok := decodeHook(raw); ok && cmd != "" {
		return cmd
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "(unreadable value)"
	}
	s := buf.String()
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}

func editErr(path string, err error) error {
	return fmt.Errorf("edit %s: %w", path, err)
}

func isRegularFile(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// --- patches.json ---

type patchRecord struct {
	// StatusLine holds the exact bytes of the replaced statusLine value, as a
	// string so a restore is byte-identical (RawMessage would be re-indented).
	StatusLine    *string  `json:"status_line,omitempty"`
	CreatedFile   bool     `json:"created_file,omitempty"`
	CreatedHooks  bool     `json:"created_hooks,omitempty"`
	CreatedEvents []string `json:"created_events,omitempty"`
}

func (r *patchRecord) addEvent(name string) {
	if !slices.Contains(r.CreatedEvents, name) {
		r.CreatedEvents = append(r.CreatedEvents, name)
		sort.Strings(r.CreatedEvents)
	}
}

// dropOwnPrior forgets a recorded statusLine that is julienning's own. Builds
// before the versions-dir ownership rule recorded their own statusLine as
// "the user's" on a re-run; restoring it would leave julienning wired in.
func (r *patchRecord) dropOwnPrior() {
	if r.StatusLine != nil && ownedStatusLine([]byte(*r.StatusLine)) {
		r.StatusLine = nil
	}
}

// plan turns the record into stripOwned's instructions for the file at path.
func (r *patchRecord) plan(path string, dropEmptied bool) stripPlan {
	p := stripPlan{createdEvents: r.CreatedEvents, createdHooks: r.CreatedHooks, dropEmptied: dropEmptied}
	if r.StatusLine != nil {
		prior := []byte(*r.StatusLine)
		if json.Valid(prior) {
			p.prior = prior
		} else {
			p.priorErr = fmt.Errorf("the statusLine saved for %s in %s is not valid JSON; fix or delete that entry, then retry", path, patchesPathForMessage())
		}
	}
	return p
}

func (r *patchRecord) empty() bool {
	return r.StatusLine == nil && !r.CreatedFile && !r.CreatedHooks && len(r.CreatedEvents) == 0
}

type patchStore struct {
	path  string
	raw   []byte // as read, to skip no-op writes
	Files map[string]*patchRecord
}

type patchFileFormat struct {
	Version int                     `json:"version"`
	Files   map[string]*patchRecord `json:"files"`
}

const patchesVersion = 1

func loadPatches() (*patchStore, error) {
	p, err := config.Path(PatchesFile)
	if err != nil {
		return nil, err
	}
	s := &patchStore{path: p, Files: map[string]*patchRecord{}}
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	s.raw = raw
	var f patchFileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w (it holds statusLine values to restore; fix it, or delete it to forget them)", p, err)
	}
	for k, r := range f.Files {
		if r != nil {
			s.Files[k] = r
		}
	}
	return s, nil
}

// record returns the (possibly new) record for key; it is stored on save
// only if it carries something.
func (s *patchStore) record(key string) *patchRecord {
	r, ok := s.Files[key]
	if !ok {
		r = &patchRecord{}
		s.Files[key] = r
	}
	return r
}

func (s *patchStore) forget(key string) { delete(s.Files, key) }

func (s *patchStore) save() error {
	files := map[string]*patchRecord{}
	for k, r := range s.Files {
		if !r.empty() {
			files[k] = r
		}
	}
	if len(files) == 0 && s.raw == nil {
		return nil
	}
	data, err := json.MarshalIndent(patchFileFormat{Version: patchesVersion, Files: files}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", s.path, err)
	}
	data = append(data, '\n')
	if bytes.Equal(data, s.raw) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(s.path), err)
	}
	if err := config.WriteFileAtomic(s.path, data, 0o600); err != nil {
		return err
	}
	s.raw = data
	return nil
}

func patchesPathForMessage() string {
	if p, err := config.Path(PatchesFile); err == nil {
		return p
	}
	return PatchesFile
}
