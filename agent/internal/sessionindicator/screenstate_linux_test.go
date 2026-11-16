//go:build linux

package sessionindicator

import (
	"os"
	"path/filepath"
	"testing"
)

// connector describes one fixture directory under a fake /sys/class/drm.
type connector struct {
	name    string
	status  string
	enabled string
	dpms    string
}

// drmFixture builds a temporary tree shaped like /sys/class/drm. An empty
// field means the attribute file is absent, which is a real case: not
// every DRM node carries every attribute.
func drmFixture(t *testing.T, conns ...connector) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range conns {
		dir := filepath.Join(root, c.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for file, content := range map[string]string{
			"status": c.status, "enabled": c.enabled, "dpms": c.dpms,
		} {
			if content == "" {
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, file), []byte(content+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return root
}

func TestDisplaysOffUnder(t *testing.T) {
	tests := []struct {
		name  string
		conns []connector
		want  Tri
	}{
		{
			name: "one connected display, powered on",
			conns: []connector{
				{"card1-DP-1", "connected", "enabled", "On"},
			},
			want: No,
		},
		{
			name: "one connected display, asleep",
			conns: []connector{
				{"card1-DP-1", "connected", "enabled", "Off"},
			},
			want: Yes,
		},
		{
			// This is the pilot CachyOS box, and the reason the filter
			// exists at all: card0's four unused connectors read Off
			// permanently. Counting them would declare the machine asleep
			// forever and the indicator would never appear again.
			name: "unused connectors on a second GPU are ignored",
			conns: []connector{
				{"card0-DP-4", "disconnected", "disabled", "Off"},
				{"card0-DP-5", "disconnected", "disabled", "Off"},
				{"card0-DP-6", "disconnected", "disabled", "Off"},
				{"card0-HDMI-A-2", "disconnected", "disabled", "Off"},
				{"card1-DP-1", "connected", "enabled", "On"},
				{"card1-HDMI-A-1", "connected", "enabled", "On"},
			},
			want: No,
		},
		{
			name: "every real display asleep, with junk connectors alongside",
			conns: []connector{
				{"card0-DP-4", "disconnected", "disabled", "Off"},
				{"card1-DP-1", "connected", "enabled", "Off"},
				{"card1-HDMI-A-1", "connected", "enabled", "Off"},
			},
			want: Yes,
		},
		{
			// Two screens, one asleep: somebody may still be looking at
			// the other one, so the bar goes up.
			name: "one of two displays asleep is not asleep",
			conns: []connector{
				{"card1-DP-1", "connected", "enabled", "Off"},
				{"card1-HDMI-A-1", "connected", "enabled", "On"},
			},
			want: No,
		},
		{
			// A monitor plugged in but switched off in the display
			// settings is not driving anything, so it cannot vote.
			name: "connected but not enabled does not count",
			conns: []connector{
				{"card1-DP-1", "connected", "disabled", "Off"},
				{"card1-HDMI-A-1", "connected", "enabled", "On"},
			},
			want: No,
		},
		{
			name: "a connector with no dpms attribute cannot answer",
			conns: []connector{
				{"card1-DP-1", "connected", "enabled", ""},
			},
			want: Unknown,
		},
		{
			name:  "nothing to judge is Unknown, not No",
			conns: []connector{},
			want:  Unknown,
		},
		{
			name: "no connected connectors at all is Unknown",
			conns: []connector{
				{"card0-DP-4", "disconnected", "disabled", "Off"},
			},
			want: Unknown,
		},
		{
			// The kernel's own capitalisation is "On"/"Off"; tolerate a
			// driver that differs rather than reading it as asleep.
			name: "dpms case is not load-bearing",
			conns: []connector{
				{"card1-DP-1", "connected", "enabled", "on"},
			},
			want: No,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := displaysOffUnder(drmFixture(t, tt.conns...))
			if got != tt.want {
				t.Fatalf("displaysOffUnder = %v, want %v", got, tt.want)
			}
			if tt.want == Unknown && source != "" {
				t.Errorf("an Unknown reading named a source %q", source)
			}
			if tt.want != Unknown && source == "" {
				t.Error("a definite reading named no source")
			}
		})
	}
}

func TestDisplaysOffUnderMissingRootIsUnknown(t *testing.T) {
	// A container, or a kernel without the sysfs nodes. Not "no".
	got, _ := displaysOffUnder(filepath.Join(t.TempDir(), "does-not-exist"))
	if got != Unknown {
		t.Fatalf("a missing %s gave %v, want Unknown", drmRoot, got)
	}
}

// TestManualScreenState prints what the real probes say on this machine.
//
// A manual test for the same reason TestManualBar is one: it needs a live
// session bus and a real desktop, so it must not run on CI or on a venue
// PC. Lock the screen, or turn the displays off, and run it again — the
// reading should follow.
//
//	READYFLEET_INDICATOR_MANUAL=1 \
//	  go test ./internal/sessionindicator/ -run TestManualScreenState -v
func TestManualScreenState(t *testing.T) {
	if os.Getenv("READYFLEET_INDICATOR_MANUAL") != "1" {
		t.Skip("set READYFLEET_INDICATOR_MANUAL=1 to probe the real desktop")
	}
	st := platformScreenState()
	skip, reason := Decide(st)
	t.Logf("locked=%s (%s) displays_off=%s (%s)",
		st.Locked, st.LockedSource, st.DisplaysOff, st.DisplaysOffSource)
	if skip {
		t.Logf("decision: SKIP - %s", reason)
	} else {
		t.Log("decision: SHOW")
	}
}
