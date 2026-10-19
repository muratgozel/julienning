package sessions

import (
	"strings"
	"testing"
)

func TestRewriteJSONPaths(t *testing.T) {
	r := NewRewriter([]Pair{
		{From: "/Users/murat", To: "/home/dev"},
		{From: "/Users/murat/Code/shop", To: "/home/dev/src/shop"},
		{From: "/Users/murat/.claude-a", To: "/home/dev/.claude-x"},
		{From: "/same", To: "/same"},
		{From: "", To: "/x"},
	})
	in := `{"cwd":"/Users/murat/Code/shop/web","x":"cd /Users/murat/Code/shop2 && ls /mnt/Users/murat/a","m":"/Users/murat/.claude-a/projects/p/memory/","h":"/Users/murat/notes.txt","s":"/same/path","p":"/Users/murat/Code/shop."}` + "\n" + `not json /Users/murat/Code/shop` + "\n"
	got, n := r.JSON([]byte(in))
	want := `{"cwd":"/home/dev/src/shop/web","x":"cd /home/dev/Code/shop2 && ls /mnt/Users/murat/a","m":"/home/dev/.claude-x/projects/p/memory/","h":"/home/dev/notes.txt","s":"/same/path","p":"/home/dev/Code/shop."}` + "\n" + `not json /home/dev/src/shop` + "\n"
	if string(got) != want || n != 6 {
		t.Fatalf("got %d:\n%s\nwant:\n%s", n, got, want)
	}
}

// Replaced text is never matched again, so a pair whose target contains
// another pair's source cannot cascade.
func TestRewriteIsOnePass(t *testing.T) {
	r := NewRewriter([]Pair{{From: "/a", To: "/b/a"}, {From: "/b", To: "/c"}})
	got, n := r.Text([]byte("/a /b"))
	if string(got) != "/b/a /c" || n != 2 {
		t.Fatalf("%q %d", got, n)
	}
}

func TestRewriteEscapedAndNonASCIIPaths(t *testing.T) {
	r := NewRewriter([]Pair{{From: `/Users/Ömer/Code/"q"\x`, To: "/home/ömer/src/q"}})
	got, n := r.JSON([]byte(`{"cwd":"/Users/Ömer/Code/\"q\"\\x/web"}`))
	if string(got) != `{"cwd":"/home/ömer/src/q/web"}` || n != 1 {
		t.Fatalf("%s %d", got, n)
	}
	if jsonEscape("a\x01\tb<&\u2028") != `a\u0001\tb<&`+"\u2028" {
		t.Fatalf("escape: %q", jsonEscape("a\x01\tb<&\u2028"))
	}
}

func TestRewriteUntouched(t *testing.T) {
	r := NewRewriter([]Pair{{From: "/Users/murat", To: "/home/dev"}})
	in := []byte(strings.Repeat("nothing here ", 1000))
	got, n := r.JSON(in)
	if n != 0 || &got[0] != &in[0] {
		t.Fatal("unchanged input must come back as is")
	}
}
