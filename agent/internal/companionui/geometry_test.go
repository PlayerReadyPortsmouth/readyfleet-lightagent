package companionui

import (
	"fmt"
	"strings"
	"testing"
)

func TestShortScreenKeepsTextScaleAndAllActionsReachable(t *testing.T) {
	// Includes 720px/150% and 768px/125%, with taskbar excluded from work area.
	for _, height := range []int32{680, 728, 960} {
		for _, dpi := range []int32{96, 120, 144, 192} {
			t.Run(fmt.Sprintf("height%d_dpi%d", height, dpi), func(t *testing.T) {
				outer := Place(Rect{0, 0, 1280, height}, dpi)
				// Account for borders and a conservative scrollbar width. Actual native
				// code reads GetClientRect after ShowScrollBar instead of guessing these.
				v := PanelViewport{Width: outer.Right - outer.Left - 2 - 24*dpi/96, Height: outer.Bottom - outer.Top - 2, DPI: dpi}
				if v.Bounds(Rect{20, 0, 400, 35}).Bottom != 35*dpi/96 {
					t.Fatal("controls/text were shrunk to fit the work area")
				}
				// Every control on every page and tab, including the bottom controls
				// that once fell below the clamped outer window.
				var controls []Rect
				for _, st := range panelStates() {
					for _, c := range panelControls {
						if r, ok := controlRect(c.ID, st); ok {
							controls = append(controls, r)
						}
					}
				}
				for _, c := range controls {
					for _, initial := range []int32{0, v.MaxOffset()} {
						v.Offset = initial
						v.Offset = v.Reveal(c)
						r := v.Bounds(c)
						if r.Left < 0 || r.Right > v.Width || r.Top < 0 || r.Bottom > v.Height {
							t.Fatalf("control %+v unreachable at %+v (viewport %+v)", c, r, v)
						}
					}
				}
				v.Offset = v.MaxOffset()
				if v.Bounds(Rect{0, 0, 420, PanelContentHeight}).Bottom > v.Height {
					t.Fatal("scrollbar cannot reach bottom of content")
				}
			})
		}
	}
}
func TestViewportScrollClampsAndOversizeTextUsesNativeScrolling(t *testing.T) {
	v := PanelViewport{Width: 500, Height: 400, DPI: 192}
	if v.Clamp(-100) != 0 || v.Clamp(99999) != v.MaxOffset() {
		t.Fatal("invalid scrollbar range")
	}
	v.Offset = v.MaxOffset()
	text := Rect{20, 133, 400, 470}
	v.Offset = v.Reveal(text)
	if v.Bounds(text).Top != 0 {
		t.Fatal("oversized text should reveal its top; its native scrollbar exposes the rest")
	}
	v = PanelViewport{Width: 420, Height: PanelContentHeight + 8, DPI: 96}
	if v.MaxOffset() != 0 {
		t.Fatal("original size should not need a scrollbar")
	}
}

func panelStates() []panelState {
	var out []panelState
	for page := range 4 {
		for tab := range 2 {
			for _, active := range []bool{false, true} {
				out = append(out, panelState{Page: page, Tab: tab, HostingActive: active})
			}
		}
	}
	return out
}

func TestPanelControlsNeverOverlapAndStayInsideTheDesign(t *testing.T) {
	ids := map[int]bool{}
	for _, c := range panelControls {
		if ids[c.ID] {
			t.Fatalf("control id %d defined twice", c.ID)
		}
		ids[c.ID] = true
	}
	for _, st := range panelStates() {
		var shown []panelControl
		for _, c := range panelControls {
			r, ok := controlRect(c.ID, st)
			if !ok {
				continue
			}
			if r.Left < 20 || r.Right > 400 || r.Top < 0 || r.Bottom > PanelContentHeight || r.Right <= r.Left || r.Bottom <= r.Top {
				t.Fatalf("%+v: control %d at %+v is outside the 20..400 × 0..%d design", st, c.ID, r, PanelContentHeight)
			}
			for _, o := range shown {
				q, _ := controlRect(o.ID, st)
				if r.Left < q.Right && q.Left < r.Right && r.Top < q.Bottom && q.Top < r.Bottom {
					t.Fatalf("%+v: controls %d %+v and %d %+v overlap", st, c.ID, r, o.ID, q)
				}
			}
			shown = append(shown, c)
		}
	}
}

// Each page keeps the controls it had before the layout was tightened.
func TestPanelPagesShowTheirControls(t *testing.T) {
	want := map[panelState][]int{
		{Page: 0, Tab: 0}:                      {1, 2, 3, 10, 11, 20, 21, 25, 30, 31, 32, 33, 34, 35, 36, 22, 23, 37},
		{Page: 0, Tab: 0, HostingActive: true}: {24},
		{Page: 0, Tab: 1}:                      {45, 20, 21, 40, 41, 42, 43, 44, 34, 35, 36},
		{Page: 1}:                              {60, 61, 62, 63},
		{Page: 2}:                              {64, 63},
		{Page: 3}:                              {70, 71, 72, 73, 74, 75, 76, 77, 78, 79, 63},
	}
	for st, ids := range want {
		for _, id := range ids {
			if _, ok := controlRect(id, st); !ok {
				t.Fatalf("%+v hides control %d", st, id)
			}
		}
	}
	for _, id := range []int{30, 40, 45, 60, 70, 24, 34} {
		if _, ok := controlRect(id, panelState{Page: 2}); ok {
			t.Fatalf("details page shows control %d", id)
		}
	}
	if _, ok := controlRect(24, panelState{}); ok {
		t.Fatal("stream warning shown with no active session")
	}
}

func TestVersionTextHidesUnknownUpdateState(t *testing.T) {
	cases := map[Device]string{
		{AgentVersion: "0.41.0", EngineVersion: "2.1.1.126", UpdateStatus: "unknown"}:   "Fleet 0.41.0 · SolarBeam 2.1.1.126",
		{AgentVersion: "0.41.0", EngineVersion: "2.1.1.126", UpdateStatus: " Unknown "}: "Fleet 0.41.0 · SolarBeam 2.1.1.126",
		{AgentVersion: "0.41.0", EngineVersion: "2.1.1.126"}:                            "Fleet 0.41.0 · SolarBeam 2.1.1.126",
		{AgentVersion: "0.41.0", UpdateStatus: "Update available"}:                      "Fleet 0.41.0\r\nUpdate available",
		{}: "Installed versions unavailable",
	}
	for d, want := range cases {
		if got := VersionText(d); got != want {
			t.Fatalf("VersionText(%+v) = %q, want %q", d, got, want)
		}
		if strings.Contains(strings.ToLower(VersionText(d)), "unknown") {
			t.Fatalf("raw unknown shown for %+v", d)
		}
	}
}

func TestEditTextUsesCRLF(t *testing.T) {
	if got := editText("a\nb\r\nc"); got != "a\r\nb\r\nc" {
		t.Fatalf("editText = %q", got)
	}
}
