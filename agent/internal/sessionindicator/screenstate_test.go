package sessionindicator

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecide(t *testing.T) {
	tests := []struct {
		name       string
		state      ScreenState
		wantSkip   bool
		wantReason string // substring
	}{
		{
			name:       "locked ⇒ skip",
			state:      ScreenState{Locked: Yes, LockedSource: "org.freedesktop.ScreenSaver", DisplaysOff: No},
			wantSkip:   true,
			wantReason: "the session is locked (org.freedesktop.ScreenSaver)",
		},
		{
			name:       "displays off ⇒ skip",
			state:      ScreenState{Locked: No, DisplaysOff: Yes, DisplaysOffSource: "DRM connector dpms"},
			wantSkip:   true,
			wantReason: "the displays are asleep (DRM connector dpms)",
		},
		{
			name:       "locked AND displays off ⇒ skip, saying both",
			state:      ScreenState{Locked: Yes, DisplaysOff: Yes},
			wantSkip:   true,
			wantReason: "the session is locked and the displays are asleep",
		},
		{
			name:     "awake and unlocked ⇒ show",
			state:    ScreenState{Locked: No, DisplaysOff: No},
			wantSkip: false,
		},
		{
			// The whole point of Tri: a probe that could not reach the
			// compositor must not be able to silence the indicator.
			name:     "nothing could be determined ⇒ show",
			state:    ScreenState{},
			wantSkip: false,
		},
		{
			name:     "lock unknown, displays definitely on ⇒ show",
			state:    ScreenState{Locked: Unknown, DisplaysOff: No},
			wantSkip: false,
		},
		{
			name:       "lock unknown, displays definitely off ⇒ skip",
			state:      ScreenState{Locked: Unknown, DisplaysOff: Yes},
			wantSkip:   true,
			wantReason: "the displays are asleep",
		},
		{
			name:       "locked, display state unknown ⇒ skip",
			state:      ScreenState{Locked: Yes, DisplaysOff: Unknown},
			wantSkip:   true,
			wantReason: "the session is locked",
		},
		{
			name:       "a source-less reading still gives a usable reason",
			state:      ScreenState{Locked: Yes},
			wantSkip:   true,
			wantReason: "the session is locked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := Decide(tt.state)
			if skip != tt.wantSkip {
				t.Fatalf("Decide(%+v) skip = %v, want %v (reason %q)", tt.state, skip, tt.wantSkip, reason)
			}
			if !tt.wantSkip {
				if reason != "" {
					t.Errorf("a show decision carried a reason %q", reason)
				}
				return
			}
			if !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", reason, tt.wantReason)
			}
		})
	}
}

func TestSkippedIsDistinguishableFromAFailure(t *testing.T) {
	skipped := skippedFor("the session is locked", ScreenState{Locked: Yes})
	failure := errors.New("no zwlr_layer_shell_v1")

	// This is the property require_session_indicator depends on: a
	// deliberate skip must never be mistaken for an inability to show.
	if !errors.Is(skipped, ErrSkipped) {
		t.Error("a skip does not match ErrSkipped")
	}
	if errors.Is(failure, ErrSkipped) {
		t.Error("a real failure matched ErrSkipped")
	}

	var sk *Skipped
	if !errors.As(skipped, &sk) {
		t.Fatal("errors.As could not recover the *Skipped")
	}
	if sk.Reason != "the session is locked" {
		t.Errorf("Reason = %q", sk.Reason)
	}
	if sk.State.Locked != Yes {
		t.Errorf("State was not carried through: %+v", sk.State)
	}
}

func TestSkippedSurvivesWrapping(t *testing.T) {
	// Show wraps, callers unwrap; the sentinel has to survive the round
	// trip or the policy check silently stops working.
	wrapped := errors.Join(errors.New("context"), skippedFor("the displays are asleep", ScreenState{}))
	if !errors.Is(wrapped, ErrSkipped) {
		t.Error("ErrSkipped did not survive wrapping")
	}
}

func TestTriString(t *testing.T) {
	for _, tt := range []struct {
		in   Tri
		want string
	}{
		{Unknown, "unknown"},
		{No, "no"},
		{Yes, "yes"},
		{Tri(99), "unknown"},
	} {
		if got := tt.in.String(); got != tt.want {
			t.Errorf("Tri(%d).String() = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestReadScreenStateBoundedReturnsUnknownWhenTheProbeHangs(t *testing.T) {
	// A wedged session bus must not hold up start_solarbeam. Unknown is
	// the right answer, and Unknown means show.
	blocked := make(chan struct{})
	defer close(blocked)

	got := readScreenStateBounded(func() ScreenState {
		<-blocked
		return ScreenState{Locked: Yes}
	}, 20*time.Millisecond)

	if got.Locked != Unknown || got.DisplaysOff != Unknown {
		t.Fatalf("a timed-out probe returned %+v, want everything Unknown", got)
	}
	if skip, _ := Decide(got); skip {
		t.Error("a timed-out probe suppressed the indicator")
	}
}

func TestReadScreenStateBoundedPassesTheProbeThrough(t *testing.T) {
	want := ScreenState{Locked: Yes, LockedSource: "test", DisplaysOff: No}
	got := readScreenStateBounded(func() ScreenState { return want }, time.Second)
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
