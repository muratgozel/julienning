package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// realistic mirrors the shape of a hand-edited Claude Code settings.json.
const realistic = `{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "model": "opus",
  "cleanupPeriodDays": 30,
  "ratio": 1.50,
  "big": 1e3,
  "neg": -0.0,
  "unicode": "çağrı — 日本語 \u00e9 \ud83d\ude00",
  "escaped": "say \"hi\" \\ back\\slash\n\ttab",
  "empty": {},
  "emptyArr": [],
  "permissions": {
    "allow": [
      "Bash(git status:*)",
      "Read(~/.zshrc)"
    ],
    "deny": [],
    "defaultMode": "acceptEdits"
  },
  "env": {"FOO": "1", "BAR": "two"},
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write|Edit",
        "hooks": [
          { "type": "command", "command": "prettier --write \"$FILE\"" }
        ]
      }
    ]
  },
  "nullish": null,
  "flag": true
}
`

var corpus = []string{
	realistic,
	`{}`,
	"{}\n",
	`{ }`,
	"  {\"a\":1}  \n",
	`{"a":{"b":{"c":[1,2,{"d":"}"}]}}}`,
	"{\r\n  \"a\": 1,\r\n  \"b\": [\r\n    true\r\n  ]\r\n}\r\n",
	"{\n\t\"tabbed\": {\n\t\t\"x\": 1\n\t}\n}\n",
	`{"k\"ey":"v\\","\u0041":"é"}`,
}

func TestRoundTripIsByteIdentical(t *testing.T) {
	for _, src := range corpus {
		o, err := Parse([]byte(src))
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		if got := string(o.Bytes()); got != src {
			t.Errorf("round trip changed bytes:\n got %q\nwant %q", got, src)
		}
		// Opening and splicing back every nested container must be a no-op too.
		for _, k := range o.Keys() {
			raw, _ := o.Get(k)
			switch raw[0] {
			case '{':
				ch, _, err := o.Object(k)
				if err != nil {
					t.Fatal(err)
				}
				if err := o.Set(k, ch.Bytes()); err != nil {
					t.Fatal(err)
				}
			case '[':
				ch, _, err := o.Array(k)
				if err != nil {
					t.Fatal(err)
				}
				if err := o.Set(k, ch.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := string(o.Bytes()); got != src {
			t.Errorf("child splice changed bytes:\n got %q\nwant %q", got, src)
		}
	}
}

func TestParseStripsBOM(t *testing.T) {
	o, err := Parse([]byte("\ufeff{\"a\":1}"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(o.Bytes()); got != `{"a":1}` {
		t.Fatalf("got %q", got)
	}
}

func TestParseRejects(t *testing.T) {
	for _, src := range []string{`[1,2]`, `"s"`, `42`, `null`} {
		if _, err := Parse([]byte(src)); !errors.Is(err, ErrNotObject) {
			t.Errorf("%s: got %v, want ErrNotObject", src, err)
		}
	}
	for _, src := range []string{``, `   `, `{`, `{"a":}`, `{"a":1,}`, `{"a":1} x`, `// c` + "\n{}"} {
		_, err := Parse([]byte(src))
		if err == nil || errors.Is(err, ErrNotObject) {
			t.Errorf("%q: got %v, want a syntax error", src, err)
		}
	}
}

func TestGetReturnsRawAndLastDuplicate(t *testing.T) {
	o := mustParse(t, `{"a": 1e3, "b": {"x" : [1, 2]}, "a": "second"}`)
	if raw, ok := o.Get("b"); !ok || string(raw) != `{"x" : [1, 2]}` {
		t.Fatalf("b = %q %v", raw, ok)
	}
	if raw, _ := o.Get("a"); string(raw) != `"second"` {
		t.Fatalf("a = %q, want the last duplicate", raw)
	}
	if _, ok := o.Get("zzz"); ok {
		t.Fatal("missing key reported present")
	}
	if !reflect.DeepEqual(o.Keys(), []string{"a", "b", "a"}) {
		t.Fatalf("keys = %v", o.Keys())
	}
}

func TestSetExistingChangesOnlyThatSpan(t *testing.T) {
	o := mustParse(t, realistic)
	if err := o.SetValue("model", "sonnet"); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(realistic, `"model": "opus"`, `"model": "sonnet"`, 1)
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestSetRawIsVerbatim(t *testing.T) {
	o := mustParse(t, "{\n  \"a\": 1,\n  \"b\": 2\n}\n")
	if err := o.Set("a", []byte(`{"keep":  "my spacing"}`)); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"a\": {\"keep\":  \"my spacing\"},\n  \"b\": 2\n}\n"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if err := o.Set("a", []byte(`{bad`)); err == nil {
		t.Fatal("invalid raw accepted")
	}
}

func TestInsertIntoEmptyObjectIsPretty(t *testing.T) {
	for _, src := range []string{"{}\n", "{ }\n", "{\n}\n"} {
		o := mustParse(t, src)
		if err := o.SetValue("statusLine", struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		}{"command", "/bin/julienning statusline"}); err != nil {
			t.Fatal(err)
		}
		want := "{\n  \"statusLine\": {\n    \"type\": \"command\",\n    \"command\": \"/bin/julienning statusline\"\n  }\n}\n"
		if got := string(o.Bytes()); got != want {
			t.Errorf("%q:\n got %q\nwant %q", src, got, want)
		}
	}
}

func TestInsertIntoMultilineUsesDetectedIndent(t *testing.T) {
	src := "{\n    \"a\": 1\n}\n"
	o := mustParse(t, src)
	if err := o.SetValue("b", map[string]any{"x": []int{1}}); err != nil {
		t.Fatal(err)
	}
	want := "{\n    \"a\": 1,\n    \"b\": {\n        \"x\": [\n            1\n        ]\n    }\n}\n"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestInsertIntoTabIndented(t *testing.T) {
	o := mustParse(t, "{\n\t\"a\": 1\n}")
	if err := o.SetValue("b", map[string]int{"c": 2}); err != nil {
		t.Fatal(err)
	}
	want := "{\n\t\"a\": 1,\n\t\"b\": {\n\t\t\"c\": 2\n\t}\n}"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestInsertIntoSingleLineStaysSingleLine(t *testing.T) {
	cases := []struct{ src, want string }{
		{`{"a":1}`, `{"a":1,"b":{"x":[1,2],"y":"a:b,c"}}`},
		{`{ "a": 1, "z": 2 }`, `{ "a": 1, "z": 2, "b": {"x": [1, 2], "y": "a:b,c"} }`},
	}
	for _, tc := range cases {
		o := mustParse(t, tc.src)
		if err := o.SetValue("b", struct {
			X []int  `json:"x"`
			Y string `json:"y"`
		}{[]int{1, 2}, "a:b,c"}); err != nil {
			t.Fatal(err)
		}
		if got := string(o.Bytes()); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.src, got, tc.want)
		}
	}
}

func TestInsertKeepsCRLF(t *testing.T) {
	o := mustParse(t, "{\r\n  \"a\": 1\r\n}\r\n")
	if err := o.SetValue("b", []int{1}); err != nil {
		t.Fatal(err)
	}
	want := "{\r\n  \"a\": 1,\r\n  \"b\": [\r\n    1\r\n  ]\r\n}\r\n"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestDeleteRestoresOriginal(t *testing.T) {
	for _, src := range corpus {
		o := mustParse(t, src)
		if err := o.SetValue("zz_added", map[string]string{"k": "v"}); err != nil {
			t.Fatal(err)
		}
		if ok, err := o.Delete("zz_added"); err != nil || !ok {
			t.Fatalf("delete: %v %v", ok, err)
		}
		got := string(o.Bytes())
		want := src
		// An empty container that grew and shrank again comes back compact.
		if strings.TrimSpace(src) == "{ }" {
			want = strings.Replace(src, "{ }", "{}", 1)
		}
		if got != want {
			t.Errorf("insert+delete not reversible:\n got %q\nwant %q", got, want)
		}
	}
}

func TestDeletePositions(t *testing.T) {
	src := "{\n  \"a\": 1,\n  \"b\": 2,\n  \"c\": 3\n}"
	cases := map[string]string{
		"a": "{\n  \"b\": 2,\n  \"c\": 3\n}",
		"b": "{\n  \"a\": 1,\n  \"c\": 3\n}",
		"c": "{\n  \"a\": 1,\n  \"b\": 2\n}",
	}
	for key, want := range cases {
		o := mustParse(t, src)
		if _, err := o.Delete(key); err != nil {
			t.Fatal(err)
		}
		if got := string(o.Bytes()); got != want {
			t.Errorf("delete %s:\n got %q\nwant %q", key, got, want)
		}
	}
	o := mustParse(t, "{\n  \"only\": 1\n}\n")
	if _, err := o.Delete("only"); err != nil {
		t.Fatal(err)
	}
	if got := string(o.Bytes()); got != "{}\n" {
		t.Fatalf("got %q", got)
	}
	if ok, _ := o.Delete("missing"); ok {
		t.Fatal("deleting a missing key reported true")
	}
	dup := mustParse(t, `{"a":1,"b":2,"a":3}`)
	if _, err := dup.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if got := string(dup.Bytes()); got != `{"b":2}` {
		t.Fatalf("duplicates not all removed: %s", got)
	}
}

func TestNestedArrayEdit(t *testing.T) {
	o := mustParse(t, realistic)
	hooks, ok, err := o.Object("hooks")
	if err != nil || !ok {
		t.Fatalf("hooks: %v %v", ok, err)
	}
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type group struct {
		Hooks []hook `json:"hooks"`
	}
	if err := hooks.SetValue("SessionStart", []group{{Hooks: []hook{{"command", "j hook session-start"}}}}); err != nil {
		t.Fatal(err)
	}
	post, _, err := hooks.Array("PostToolUse")
	if err != nil {
		t.Fatal(err)
	}
	if err := post.AppendValue(group{Hooks: []hook{{"command", "echo done"}}}); err != nil {
		t.Fatal(err)
	}
	if err := hooks.Set("PostToolUse", post.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := o.Set("hooks", hooks.Bytes()); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(realistic, `          { "type": "command", "command": "prettier --write \"$FILE\"" }
        ]
      }
    ]
  },`, `          { "type": "command", "command": "prettier --write \"$FILE\"" }
        ]
      },
      {
        "hooks": [
          {
            "type": "command",
            "command": "echo done"
          }
        ]
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "j hook session-start"
          }
        ]
      }
    ]
  },`, 1)
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestEmptyNestedContainerGrowsAtItsDepth(t *testing.T) {
	o := mustParse(t, "{\n  \"hooks\": {},\n  \"x\": 1\n}\n")
	h, _, err := o.Object("hooks")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.SetValue("E", []int{1}); err != nil {
		t.Fatal(err)
	}
	if err := o.Set("hooks", h.Bytes()); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"hooks\": {\n    \"E\": [\n      1\n    ]\n  },\n  \"x\": 1\n}\n"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestArrayOps(t *testing.T) {
	o := mustParse(t, "{\n  \"l\": [\n    1,\n    {\"a\": 2},\n    3\n  ]\n}")
	arr, _, err := o.Array("l")
	if err != nil {
		t.Fatal(err)
	}
	if arr.Len() != 3 || string(arr.Get(1)) != `{"a": 2}` {
		t.Fatalf("len=%d get(1)=%s", arr.Len(), arr.Get(1))
	}
	el, err := arr.Object(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := el.SetValue("a", 5); err != nil {
		t.Fatal(err)
	}
	if err := arr.Set(1, el.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := arr.Delete(0); err != nil {
		t.Fatal(err)
	}
	if err := arr.Append([]byte(`"x"`)); err != nil {
		t.Fatal(err)
	}
	if err := o.Set("l", arr.Bytes()); err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"l\": [\n    {\"a\": 5},\n    3,\n    \"x\"\n  ]\n}"
	if got := string(o.Bytes()); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if _, err := arr.Object(1); !errors.Is(err, ErrNotObject) {
		t.Errorf("element 1 is a number: got %v", err)
	}
	if err := arr.Delete(9); err == nil {
		t.Error("out of range delete accepted")
	}
}

func TestTypeMismatch(t *testing.T) {
	o := mustParse(t, `{"a":[1],"b":{}}`)
	if _, ok, err := o.Object("a"); !ok || !errors.Is(err, ErrNotObject) {
		t.Errorf("a: ok=%v err=%v", ok, err)
	}
	if _, ok, err := o.Array("b"); !ok || !errors.Is(err, ErrNotArray) {
		t.Errorf("b: ok=%v err=%v", ok, err)
	}
	if ch, ok, err := o.Object("missing"); ch != nil || ok || err != nil {
		t.Errorf("missing: %v %v %v", ch, ok, err)
	}
}

func TestNoHTMLEscaping(t *testing.T) {
	o := mustParse(t, `{}`)
	if err := o.SetValue("c", "a && b > c"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(o.Bytes()), `"a && b > c"`) {
		t.Fatalf("escaped: %s", o.Bytes())
	}
}

// TestRandomEditsMatchSemantics applies random sets and deletes to the corpus
// and checks two invariants after every step: the document decodes to what
// the same operations on a map produce, and members not touched keep their
// exact bytes.
func TestRandomEditsMatchSemantics(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	values := []any{1, "s", 1.5, true, nil, []any{1.0, "x"}, map[string]any{"n": map[string]any{"m": []any{}}}}
	for _, src := range corpus {
		for round := 0; round < 30; round++ {
			o := mustParse(t, src)
			var model map[string]any
			if err := json.Unmarshal([]byte(src), &model); err != nil {
				t.Fatal(err)
			}
			orig := map[string]string{}
			for _, k := range o.Keys() {
				raw, _ := o.Get(k)
				orig[k] = string(raw)
			}
			touched := map[string]bool{}
			for step := 0; step < 8; step++ {
				keys := o.Keys()
				key := fmt.Sprintf("k%d", rng.Intn(4))
				if len(keys) > 0 && rng.Intn(2) == 0 {
					key = keys[rng.Intn(len(keys))]
				}
				touched[key] = true
				if rng.Intn(3) == 0 {
					if _, err := o.Delete(key); err != nil {
						t.Fatal(err)
					}
					delete(model, key)
					continue
				}
				v := values[rng.Intn(len(values))]
				if err := o.SetValue(key, v); err != nil {
					t.Fatal(err)
				}
				var norm any
				b, _ := json.Marshal(v)
				_ = json.Unmarshal(b, &norm)
				model[key] = norm
			}
			var got map[string]any
			if err := json.Unmarshal(o.Bytes(), &got); err != nil {
				t.Fatalf("edited document invalid: %v\n%s", err, o.Bytes())
			}
			if !reflect.DeepEqual(got, model) {
				t.Fatalf("semantics diverged:\n got %v\nwant %v\n%s", got, model, o.Bytes())
			}
			for k, raw := range orig {
				if touched[k] {
					continue
				}
				now, ok := o.Get(k)
				if !ok || !bytes.Equal(now, []byte(raw)) {
					t.Fatalf("untouched %q changed: %q -> %q", k, raw, now)
				}
			}
		}
	}
}

func mustParse(t *testing.T, src string) *Object {
	t.Helper()
	o, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return o
}
