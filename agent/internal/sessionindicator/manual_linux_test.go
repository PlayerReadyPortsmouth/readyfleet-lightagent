//go:build linux

package sessionindicator

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// TestManualBar is the reproducible version of the on-screen proof: it
// puts the real bar on the real compositor and leaves it there.
//
// Skipped unless READYFLEET_INDICATOR_MANUAL=1, because it needs a live
// Wayland session with zwlr_layer_shell_v1 and would otherwise fail on
// every CI runner and every venue PC's test run. It is a test rather than
// a demo command so the mechanism has exactly one home and no extra
// binary ships with the agent.
//
//	READYFLEET_INDICATOR_MANUAL=1 READYFLEET_INDICATOR_SECONDS=30 \
//	  go test ./internal/sessionindicator/ -run TestManualBar -v
//
// Set READYFLEET_INDICATOR_VIEWER to exercise the named-viewer text.
func TestManualBar(t *testing.T) {
	if os.Getenv("READYFLEET_INDICATOR_MANUAL") != "1" {
		t.Skip("set READYFLEET_INDICATOR_MANUAL=1 to put a real bar on a real screen")
	}
	seconds := 20
	if v := os.Getenv("READYFLEET_INDICATOR_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			seconds = n
		}
	}

	ind, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer ind.Close()

	if err := ind.Show(Notice{Viewer: os.Getenv("READYFLEET_INDICATOR_VIEWER")}); err != nil {
		t.Fatalf("Show: %v", err)
	}
	t.Logf("bar is up on every output for %ds", seconds)
	time.Sleep(time.Duration(seconds) * time.Second)

	if err := ind.Hide(); err != nil {
		t.Fatalf("Hide: %v", err)
	}
	t.Log("bar taken down")
}
