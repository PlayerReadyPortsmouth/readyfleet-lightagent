package companionui

import (
	"testing"
	"time"
)

// A viewer's injected Ctrl+Alt chord arrives as an ordinary WM_HOTKEY. While a
// SolarBeam session could be sending input, no shortcut may open the panel.
func TestShortcutsRefusedWhileRemoteInputPossible(t *testing.T) {
	idle := fixture()
	idle.Hosting.Active = false
	idle.Hosting.SessionID = ""
	now := time.Now()
	for request, want := range map[OpenRequest]bool{OpenPanel: true, ReviewAdmission: true, ReviewControl: true, RestoreSharedInput: true} {
		if got := ShortcutEnabled(request, idle, now); got != want {
			t.Fatalf("idle PC shortcut %d enabled = %v, want %v", request, got, want)
		}
	}
	cases := map[string]func(*Snapshot){
		"active incoming session":        func(s *Snapshot) { s.Hosting.Active = true; s.Hosting.SessionID = "hosting" },
		"viewer present without session": func(s *Snapshot) { s.Hosting.Active = true },
		"hosting state unknown":          func(s *Snapshot) { s.Hosting.Known = false },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := idle.Clone()
			mutate(&s)
			if !s.RemoteInputPossible() {
				t.Fatal("remote input not considered possible")
			}
			for request := OpenPanel; request <= RestoreSharedInput; request++ {
				if ShortcutEnabled(request, s, now) {
					t.Fatalf("shortcut %d enabled while a viewer may inject input", request)
				}
			}
		})
	}
	// The guard only gates shortcut activation; buttons (with their review
	// confirmations) remain available to someone physically at the PC.
	active := fixture()
	if _, err := active.Prepare(DisconnectHosting, now); err != nil {
		t.Fatalf("disconnect review unavailable during a session: %v", err)
	}
}
