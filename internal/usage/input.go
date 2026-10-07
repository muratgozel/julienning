// Package usage parses the Claude Code status line payload, renders the status
// line and the usage columns, and owns the local diagnostics log and the
// send-usage debounce cache. Every function here is pure with respect to the
// clock and the timezone: callers pass `now` and `loc` so tests can freeze both.
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// MaxInputBytes bounds how much stdin the status line reads; the real payload
// is a few hundred bytes and this path must never block Claude Code.
const MaxInputBytes = 1 << 20

// WindowState says whether a rate-limit window can be used.
type WindowState int

const (
	// WindowAbsent means the key was missing or null: normal before the first
	// reply and right after a reset, so it is not an error.
	WindowAbsent WindowState = iota
	// WindowOK means both fields were present, typed and in range.
	WindowOK
	// WindowInvalid means the shape was unexpected: report it so we learn about it.
	WindowInvalid
)

// Window is one parsed rate-limit window.
type Window struct {
	State WindowState
	// Used is the percentage rounded to one decimal; only set when State is OK.
	Used float64
	// ResetsAt is unix epoch seconds; only set when State is OK.
	ResetsAt int64
}

// Input is the validated status line payload.
type Input struct {
	Model   string // "" when absent or not a string
	Ctx     float64
	HasCtx  bool
	Session Window // rate_limits.five_hour
	Week    Window // rate_limits.seven_day

	rateLimits any
	hasLimits  bool
}

// ErrInvalidInput is returned by Parse for anything that is not a JSON object.
var ErrInvalidInput = errors.New("status line input is not valid JSON")

// Parse reads one JSON object from r. Only a structurally broken payload is an
// error; malformed rate limit windows are reported through Window.State so the
// caller can still print a status line.
func Parse(r io.Reader) (Input, error) {
	dec := json.NewDecoder(io.LimitReader(r, MaxInputBytes))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return Input{}, ErrInvalidInput
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return Input{}, ErrInvalidInput
	}
	in := Input{Model: cleanString(field(root, "model", "display_name"))}
	if n, ok := asFloat(field(root, "context_window", "used_percentage")); ok {
		in.Ctx, in.HasCtx = n, true
	}
	rl, present := root["rate_limits"]
	in.rateLimits, in.hasLimits = rl, present
	switch {
	case !present || rl == nil:
		in.Session, in.Week = Window{State: WindowAbsent}, Window{State: WindowAbsent}
	default:
		obj, ok := rl.(map[string]any)
		if !ok {
			in.Session, in.Week = Window{State: WindowInvalid}, Window{State: WindowInvalid}
			break
		}
		in.Session = parseWindow(obj, "five_hour")
		in.Week = parseWindow(obj, "seven_day")
	}
	return in, nil
}

// Invalid reports whether either window has an unexpected shape.
func (in Input) Invalid() bool {
	return in.Session.State == WindowInvalid || in.Week.State == WindowInvalid
}

// Complete reports whether both windows are usable.
func (in Input) Complete() bool {
	return in.Session.State == WindowOK && in.Week.State == WindowOK
}

func parseWindow(rl map[string]any, key string) Window {
	v, present := rl[key]
	if !present || v == nil {
		return Window{State: WindowAbsent}
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return Window{State: WindowInvalid}
	}
	used, ok := asFloat(obj["used_percentage"])
	if !ok || math.IsNaN(used) || used < 0 || used > 100 {
		return Window{State: WindowInvalid}
	}
	reset, ok := asFloat(obj["resets_at"])
	if !ok || !validReset(reset) {
		return Window{State: WindowInvalid}
	}
	return Window{State: WindowOK, Used: round1(used), ResetsAt: int64(reset)}
}

// maxEpochExclusive bounds resets_at to a plausible epoch (2^40 s is the year
// 36812). float64(math.MaxInt64) rounds up to exactly 2^63, so the old
// `reset > math.MaxInt64` test accepted 9223372036854775808 and int64(reset)
// then overflowed to a negative timestamp: compare against the float 2^63 too.
const (
	maxEpochExclusive = float64(1 << 40)
	int64OverflowAt   = float64(1 << 63)
)

// validReset reports whether a resets_at value is a usable epoch in seconds.
func validReset(reset float64) bool {
	if math.IsNaN(reset) || math.IsInf(reset, 0) {
		return false
	}
	if reset != math.Trunc(reset) {
		return false
	}
	return reset > 0 && reset < maxEpochExclusive && reset < int64OverflowAt
}

// RateLimitsSignature describes the shape (not the values) of the rate_limits
// payload, so errors.log can teach us about shapes we have not seen without
// ever recording usage numbers or the account.
func (in Input) RateLimitsSignature() string {
	if !in.hasLimits {
		return "rate_limits=absent"
	}
	obj, ok := in.rateLimits.(map[string]any)
	if !ok {
		return "rate_limits=" + typeName(in.rateLimits)
	}
	return fmt.Sprintf("five_hour=%s seven_day=%s", windowSignature(obj, "five_hour"), windowSignature(obj, "seven_day"))
}

func windowSignature(rl map[string]any, key string) string {
	v, present := rl[key]
	if !present {
		return "absent"
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return typeName(v)
	}
	// Known fields first, in spec order, so signatures are comparable at a glance.
	keys := []string{"used_percentage", "resets_at"}
	rest := make([]string, 0, len(obj))
	for k := range obj {
		if k != "used_percentage" && k != "resets_at" {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	parts := make([]string, 0, len(obj))
	for _, k := range append(keys, rest...) {
		if fv, ok := obj[k]; ok {
			parts = append(parts, k+":"+typeName(fv))
		}
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}

// field walks nested objects, returning nil when any step is missing or is not
// an object (mirrors jq's `try .a.b catch null`).
func field(root map[string]any, path ...string) any {
	var cur any = root
	for _, k := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[k]
	}
	return cur
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return f, true
	case float64:
		return n, true
	}
	return 0, false
}

// cleanString drops C0 controls and DEL so a hostile model name cannot inject
// escape sequences into the terminal (ported from the bash status line).
func cleanString(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// FormatPercent renders a usage percentage with at most one decimal and no
// trailing ".0" (23.54 -> "23.5", 41 -> "41").
func FormatPercent(v float64) string {
	return strconv.FormatFloat(round1(v), 'f', -1, 64)
}
