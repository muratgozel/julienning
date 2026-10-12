package teamsync

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCanonicalIgnoresLayoutAndKeyOrder(t *testing.T) {
	cases := map[string]string{
		`"dark"`:              `"dark"`,
		" true \n":            `true`,
		`{"b": 1, "a": 1.50}`: `{"a":1.50,"b":1}`,
		`"a&b"`:               `"a&b"`,
		"":                    Unset,
		`{"broken"`:           `{"broken"`,
	}
	for in, want := range cases {
		if got := Canonical([]byte(in)); got != want {
			t.Errorf("Canonical(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplay(t *testing.T) {
	for in, want := range map[string]string{`"1h"`: "1h", `true`: "true", Unset: "not set (Claude Code's default)", `{"a":1}`: `{"a":1}`} {
		if got := Display(in); got != want {
			t.Errorf("Display(%q) = %q, want %q", in, got, want)
		}
	}
}

func setting(key string) Setting {
	for _, s := range Settings {
		if s.Key == key {
			return s
		}
	}
	panic(key)
}

func TestChoicesOrderDefaultFixedFoundUnset(t *testing.T) {
	accounts := []Account{
		{Label: "alpha", Values: map[string]string{"model": `"opus[1m]"`}},
		{Label: "beta", Values: map[string]string{"model": `"sonnet"`}},
		{Label: "gamma", Values: map[string]string{"model": `"opus[1m]"`}},
		{Label: "delta", Values: map[string]string{}},
		{Label: "eps", Values: map[string]string{"model": `"opus"`}},
	}
	got := Choices(setting("model"), accounts)
	want := []Choice{
		{Value: `"opus"`, Accounts: []string{"eps"}, Default: true},
		{Value: `"opus[1m]"`, Accounts: []string{"alpha", "gamma"}},
		{Value: `"sonnet"`, Accounts: []string{"beta"}},
		{Value: Unset, Accounts: []string{"delta"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

// Fixed values are offered even when no account holds them, and "not set"
// only when an account leaves the key unset.
func TestChoicesOfferFixedValuesNobodyHas(t *testing.T) {
	accounts := []Account{
		{Label: "alpha", Values: map[string]string{"promptCacheTtl": `"1h"`}},
		{Label: "beta", Values: map[string]string{"promptCacheTtl": `"1h"`}},
	}
	got := Choices(setting("promptCacheTtl"), accounts)
	want := []Choice{
		{Value: `"5m"`, Default: true},
		{Value: `"1h"`, Accounts: []string{"alpha", "beta"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
	if got := Choices(setting("autoContinueAtUsageLimit"), []Account{{Label: "a"}}); len(got) != 3 || got[0].Value != `true` || got[2].Value != Unset {
		t.Fatalf("bool choices: %+v", got)
	}
}

func TestSameAndEdit(t *testing.T) {
	a := Account{Label: "a", Values: map[string]string{"theme": `"dark"`}}
	b := Account{Label: "b", Values: map[string]string{}}
	if _, same := Same("theme", []Account{a, b}); same {
		t.Fatal("dark and unset are the same")
	}
	if v, same := Same("theme", []Account{a, a}); !same || v != `"dark"` {
		t.Fatalf("Same = %q, %v", v, same)
	}
	if e := Edit("theme", `"dark"`, a); e != nil {
		t.Fatalf("edit for a value already held: %+v", e)
	}
	if e := Edit("theme", Unset, a); e == nil || e.Value != nil {
		t.Fatalf("unset edit: %+v", e)
	}
	if e := Edit("theme", `"vim"`, b); e == nil || string(e.Value) != `"vim"` {
		t.Fatalf("set edit: %+v", e)
	}
}

func TestEverySettingIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Settings {
		if seen[s.Key] || s.Summary == "" {
			t.Errorf("%q: duplicate or no summary", s.Key)
		}
		seen[s.Key] = true
		found := false
		for _, f := range s.Fixed {
			if !json.Valid([]byte(f)) {
				t.Errorf("%s: fixed %q is not JSON", s.Key, f)
			}
			found = found || f == s.Default
		}
		if !found {
			t.Errorf("%s: default %q is not a fixed value", s.Key, s.Default)
		}
	}
}
