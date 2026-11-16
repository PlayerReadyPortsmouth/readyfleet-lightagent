package sessionindicator

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Privacy mode: a full-screen, opaque notice on every PHYSICAL monitor of a
// managed Windows venue PC, excluded from capture (WDA_EXCLUDEFROMCAPTURE) so
// the remote viewer keeps seeing the real desktop. Same exclusion idiom and
// failure rule as the bar: never show a window that could not be excluded.
//
// This file is the platform-neutral half: what the notice says, which
// monitors get covered, and the Privacy contract. Nothing here is wired into
// the agent or companion; that is a separate change.

// PrivacyNotice is what the privacy screen says. Every field is
// server-built/sanitised: Viewer is already "First I." from ReadyApp and is
// reduced to printable ASCII here again; More is a count from bridge
// presence, never extra names.
type PrivacyNotice struct {
	Viewer string
	More   int
	// Countdown > 0 shows the "about to take over" warning instead of the
	// steady notice; Countdown is the whole seconds left.
	Countdown int
}

// Lines is the fixed-template text, one entry per line.
func (n PrivacyNotice) Lines() []string {
	if n.Countdown > 0 {
		return []string{
			"Staff are about to take over this computer",
			fmt.Sprintf("This screen goes blank in %d", n.Countdown),
			"Please save your work and step away",
		}
	}
	who := sanitiseViewer(n.Viewer)
	if who == "" {
		who = "a member of staff"
	}
	if n.More > 0 {
		who = fmt.Sprintf("%s + %d more", who, n.More)
	}
	return []string{
		"This computer is in use by a remote session",
		"Computer is in use by a session. Do not use",
		"Connected: " + who,
	}
}

// PrivacyStatus says how much of the machine the overlay covers. Covered <
// Monitors is a partial result: some head is showing the desktop and the
// caller must report it ("Partial: ..."), never fail the session.
type PrivacyStatus struct {
	Monitors int
	Covered  int
}

// Privacy is the privacy overlay. Show is idempotent for a given notice.
// An error means NOTHING is covered. Hide removes everything; Close ends it.
type Privacy interface {
	Show(PrivacyNotice) (PrivacyStatus, error)
	Hide() error
	Close() error
}

// ErrNoMonitors is returned when no coverable physical monitor exists.
var ErrNoMonitors = errors.New("session indicator: no physical monitor to cover")

// MonitorRect is one display head in virtual-screen pixels.
type MonitorRect struct {
	Left, Top, Right, Bottom int32
	Primary                  bool
	Name                     string // device name, e.g. \\.\DISPLAY1
}

func (m MonitorRect) W() int { return int(m.Right - m.Left) }
func (m MonitorRect) H() int { return int(m.Bottom - m.Top) }

// PlanOverlays picks the heads to cover: drops empty rects and heads the
// caller flags as virtual (a streaming-only virtual display is not a
// physical monitor), collapses identical rects (mirrored heads share one
// window) and orders primary first. The input is not modified.
func PlanOverlays(mons []MonitorRect, isVirtual func(name string) bool) []MonitorRect {
	var out []MonitorRect
	seen := map[[4]int32]bool{}
	for _, m := range mons {
		key := [4]int32{m.Left, m.Top, m.Right, m.Bottom}
		if m.W() <= 0 || m.H() <= 0 || seen[key] {
			continue
		}
		if isVirtual != nil && isVirtual(m.Name) {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Primary && !out[j].Primary })
	return out
}

// IsVirtualDisplayName matches the streaming virtual-display drivers
// (SudoVDA / Vibeshine) by device or monitor description.
func IsVirtualDisplayName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "sudovda") || strings.Contains(n, "vibeshine") || strings.Contains(n, "virtual display")
}
