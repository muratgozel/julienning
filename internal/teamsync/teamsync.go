// Package teamsync decides what `julienning sync` offers and changes: the
// Claude Code settings it covers, the choices for each one, and the edits
// that make every shared account hold the chosen value. It reads and writes
// nothing itself; settings.json and CLAUDE.md go through internal/claudecfg.
package teamsync

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/muratgozel/julienning/internal/claudecfg"
)

// Setting is one top-level settings.json key sync covers. Values are JSON
// literals: Fixed are always offered (the team's usual answers), Default is
// the one the picker opens on.
type Setting struct {
	Key     string
	Summary string
	Fixed   []string
	Default string
}

// Settings is sync's scope, chosen with the team lead: only keys a teammate
// changes and wants the same on every shared account. julienning's own
// statusLine and hooks, secrets (env, auth helpers) and per-account state
// (dialog acceptances) are never here. Keep docs/SPEC.md and
// docs/REFERENCE.md ("sync") in step with this list.
var Settings = []Setting{
	{Key: "model", Summary: "Default model (opus is the latest Opus)", Fixed: []string{`"opus"`}, Default: `"opus"`},
	{Key: "effortLevel", Summary: "Effort level for supported models", Fixed: []string{`"xhigh"`}, Default: `"xhigh"`},
	{Key: "promptCacheTtl", Summary: "Prompt cache TTL of the main conversation", Fixed: []string{`"5m"`, `"1h"`}, Default: `"5m"`},
	{Key: "subagentPromptCacheTtl", Summary: "Prompt cache TTL of subagents, workflows and background requests", Fixed: []string{`"5m"`, `"1h"`}, Default: `"5m"`},
	{Key: "autoContinueAtUsageLimit", Summary: "When a usage limit stops a session, wait for the reset and continue", Fixed: []string{`true`, `false`}, Default: `true`},
	{Key: "theme", Summary: "Color theme", Fixed: []string{`"dark"`, `"light"`, `"dark-daltonized"`, `"light-daltonized"`, `"dark-ansi"`, `"light-ansi"`, `"auto"`}, Default: `"dark"`},
	{Key: "editorMode", Summary: "Key bindings of the prompt input", Fixed: []string{`"normal"`, `"vim"`}, Default: `"normal"`},
}

// Keys lists the keys of Settings.
func Keys() []string {
	out := make([]string, len(Settings))
	for i, s := range Settings {
		out[i] = s.Key
	}
	return out
}

// Unset is the value of a key an account does not set (Claude Code's
// default applies); choosing it removes the key.
const Unset = ""

// Canonical is raw JSON in a comparable form: insignificant whitespace and
// object key order do not matter, number literals are kept. Unparseable
// input is returned trimmed.
func Canonical(raw []byte) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Unset
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return strings.TrimRight(buf.String(), "\n")
}

// Display renders a canonical value for people: strings without quotes,
// anything else as compact JSON.
func Display(value string) string {
	if value == Unset {
		return "not set (Claude Code's default)"
	}
	var s string
	if json.Unmarshal([]byte(value), &s) == nil {
		return s
	}
	return value
}

// Account is one shared account's view for sync: its label and the
// canonical value of each key in Settings it sets.
type Account struct {
	Label  string
	Values map[string]string
}

// Choice is one answer to a setting's question.
type Choice struct {
	Value    string   // canonical JSON, Unset to remove the key
	Accounts []string // labels of the accounts holding it now
	Default  bool
}

// Choices are the answers for s, the default first: the default, the other
// fixed values in order, values accounts hold that are not fixed (most
// accounts first), then Unset when some account does not set the key. Every
// value is listed once; sync adds "Don't sync" after these.
func Choices(s Setting, accounts []Account) []Choice {
	holders := map[string][]string{}
	for _, a := range accounts {
		v := a.Values[s.Key]
		holders[v] = append(holders[v], a.Label)
	}
	var out []Choice
	seen := map[string]bool{}
	add := func(v string) {
		if seen[v] {
			return
		}
		seen[v] = true
		out = append(out, Choice{Value: v, Accounts: holders[v], Default: v == Canonical([]byte(s.Default))})
	}
	add(Canonical([]byte(s.Default)))
	for _, f := range s.Fixed {
		add(Canonical([]byte(f)))
	}
	var found []string
	for v := range holders {
		if v != Unset && !seen[v] {
			found = append(found, v)
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if n, m := len(holders[found[i]]), len(holders[found[j]]); n != m {
			return n > m
		}
		return found[i] < found[j]
	})
	for _, v := range found {
		add(v)
	}
	if _, ok := holders[Unset]; ok {
		add(Unset)
	}
	return out
}

// Same reports whether every account holds the same value for key, and
// which.
func Same(key string, accounts []Account) (string, bool) {
	if len(accounts) == 0 {
		return Unset, true
	}
	first := accounts[0].Values[key]
	for _, a := range accounts[1:] {
		if a.Values[key] != first {
			return Unset, false
		}
	}
	return first, true
}

// Edit is the settings.json change that gives an account value for key:
// nil when the account already holds it.
func Edit(key, value string, a Account) *claudecfg.SettingEdit {
	if a.Values[key] == value {
		return nil
	}
	e := claudecfg.SettingEdit{Key: key}
	if value != Unset {
		e.Value = json.RawMessage(value)
	}
	return &e
}
