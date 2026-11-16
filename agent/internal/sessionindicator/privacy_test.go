package sessionindicator

import (
	"strings"
	"testing"
)

func TestPrivacyLinesCarryTheRequiredSentence(t *testing.T) {
	got := strings.Join(PrivacyNotice{Viewer: "Aurora N.", More: 2}.Lines(), "\n")
	for _, want := range []string{"Computer is in use by a session. Do not use", "Connected: Aurora N. + 2 more"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q missing %q", got, want)
		}
	}
}

func TestPrivacyLinesSanitiseTheName(t *testing.T) {
	got := strings.Join(PrivacyNotice{Viewer: "Ev\x00il\n\u202eName"}.Lines(), "|")
	if strings.ContainsAny(got, "\x00\n\u202e") {
		t.Fatalf("control/format characters reached the screen text: %q", got)
	}
	if !strings.Contains(strings.Join(PrivacyNotice{}.Lines(), "|"), "Connected: a member of staff") {
		t.Fatal("empty name must fall back to a fixed phrase")
	}
}

func TestPrivacyCountdownReplacesSteadyNotice(t *testing.T) {
	got := strings.Join(PrivacyNotice{Viewer: "A B.", Countdown: 7}.Lines(), "|")
	if !strings.Contains(got, "goes blank in 7") || strings.Contains(got, "Connected") {
		t.Fatalf("countdown text wrong: %q", got)
	}
}

func TestPlanOverlays(t *testing.T) {
	mons := []MonitorRect{
		{Left: 1920, Right: 3840, Bottom: 1080, Name: `\\.\DISPLAY2`},
		{Right: 1920, Bottom: 1080, Primary: true, Name: `\\.\DISPLAY1`},
		{Right: 1920, Bottom: 1080, Name: `\\.\DISPLAY3`},    // mirror of 1
		{Left: 5000, Right: 5000, Bottom: 10, Name: "empty"}, // empty
		{Left: 9000, Right: 10920, Bottom: 1080, Name: "SudoVDA Virtual Display"},
	}
	got := PlanOverlays(mons, IsVirtualDisplayName)
	if len(got) != 2 || !got[0].Primary || got[1].Left != 1920 {
		t.Fatalf("plan = %+v, want primary then second head only", got)
	}
	if mons[0].Primary {
		t.Fatal("input was reordered")
	}
}

func TestRenderPrivacyIsOpaqueAndInked(t *testing.T) {
	img := renderPrivacy(1280, 720, PrivacyNotice{Viewer: "A B."})
	var amber, white int
	for y := 0; y < 720; y++ {
		for x := 0; x < 1280; x++ {
			c := img.RGBAAt(x, y)
			if c.A != 0xFF {
				t.Fatalf("pixel %d,%d not opaque", x, y)
			}
			switch c {
			case privTitle:
				amber++
			case privInk:
				white++
			}
		}
	}
	if amber == 0 || white == 0 {
		t.Fatalf("text not drawn (amber=%d white=%d)", amber, white)
	}
}
