package launch

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestEnvSetsOrUnsets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	base := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/old"}

	got := Env(base, filepath.Join(home, ".claude-x"))
	if !slices.Contains(got, "CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude-x")) || slices.Contains(got, "CLAUDE_CONFIG_DIR=/old") {
		t.Fatalf("custom dir: %v", got)
	}
	got = Env(base, filepath.Join(home, ".claude"))
	for _, kv := range got {
		if len(kv) >= 17 && kv[:17] == "CLAUDE_CONFIG_DIR" {
			t.Fatalf("default dir must unset the variable: %v", got)
		}
	}
}
