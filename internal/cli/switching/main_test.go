package switching

import (
	"context"
	"os"
	"testing"

	"github.com/muratgozel/julienning/internal/usageprobe"
)

// TestMain stubs the usage refresh out: it runs claude, and tests never run
// the real claude. A test that needs it installs its own (see stubRefresh).
func TestMain(m *testing.M) {
	refreshUsage = func(_ context.Context, o usageprobe.Options) usageprobe.Report {
		return usageprobe.Report{Listing: o.Listing}
	}
	os.Exit(m.Run())
}

// stubRefresh replaces the refresh for one test and records its options.
func stubRefresh(t *testing.T, f func(usageprobe.Options) usageprobe.Report) *[]usageprobe.Options {
	t.Helper()
	var calls []usageprobe.Options
	prev := refreshUsage
	refreshUsage = func(_ context.Context, o usageprobe.Options) usageprobe.Report {
		calls = append(calls, o)
		return f(o)
	}
	t.Cleanup(func() { refreshUsage = prev })
	return &calls
}
