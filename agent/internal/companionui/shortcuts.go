package companionui

import (
	"fmt"
	"strings"
	"time"
)

type OpenRequest uint8

const (
	OpenPanel OpenRequest = iota
	ReviewAdmission
	ReviewControl
	RestoreSharedInput
)

type Shortcut struct {
	Request   OpenRequest
	Modifiers uint32
	Key       uint32
}

const (
	Alt     uint32 = 1
	Control uint32 = 2
	Shift   uint32 = 4
)

func DefaultShortcuts() [4]Shortcut {
	return [4]Shortcut{{OpenPanel, Control | Alt, 'R'}, {ReviewAdmission, Control | Alt, 'A'}, {ReviewControl, Control | Alt, 'C'}, {RestoreSharedInput, Control | Alt, 'L'}}
}
func (s Shortcut) String() string {
	if s.Key == 0 {
		return ""
	}
	parts := []string{}
	if s.Modifiers&Control != 0 {
		parts = append(parts, "Ctrl")
	}
	if s.Modifiers&Alt != 0 {
		parts = append(parts, "Alt")
	}
	if s.Modifiers&Shift != 0 {
		parts = append(parts, "Shift")
	}
	return strings.Join(append(parts, string(rune(s.Key))), "+")
}
func ParseShortcut(request OpenRequest, text string) (Shortcut, error) {
	s := Shortcut{Request: request}
	text = strings.TrimSpace(text)
	if text == "" {
		return s, nil
	}
	p := strings.Split(strings.ToUpper(text), "+")
	for i, v := range p {
		v = strings.TrimSpace(v)
		if i == len(p)-1 {
			if len(v) != 1 || v[0] < 'A' || v[0] > 'Z' {
				return s, fmt.Errorf("Use a letter A–Z with Ctrl and Alt")
			}
			s.Key = uint32(v[0])
			continue
		}
		switch v {
		case "CTRL":
			s.Modifiers |= Control
		case "ALT":
			s.Modifiers |= Alt
		case "SHIFT":
			s.Modifiers |= Shift
		default:
			return s, fmt.Errorf("Unknown shortcut modifier %s", v)
		}
	}
	if s.Modifiers&(Control|Alt) != Control|Alt {
		return s, fmt.Errorf("Shortcuts require Ctrl+Alt")
	}
	return s, nil
}
func ValidateShortcuts(bindings [4]Shortcut) error {
	seen := map[string]bool{}
	for i, b := range bindings {
		if b.Request != OpenRequest(i) {
			return fmt.Errorf("Shortcut request order differs")
		}
		if b.Key == 0 {
			continue
		}
		parsed, err := ParseShortcut(b.Request, b.String())
		if err != nil || parsed != b {
			return fmt.Errorf("Invalid shortcut")
		}
		key := b.String()
		if seen[key] {
			return fmt.Errorf("Shortcut %s is assigned twice", key)
		}
		seen[key] = true
	}
	return nil
}

// ShortcutEnabled uses the same current authority and exact-session checks as
// buttons. A global registration never guesses among browser tabs.
//
// Shortcuts are off while a SolarBeam viewer could be driving this PC. Windows
// delivers a hotkey for injected keystrokes exactly as for the physical
// keyboard, so a remote viewer could otherwise open the capture-excluded panel
// they cannot see. The tray icon still opens it for someone at the PC, and
// every action still needs its review confirmation.
func ShortcutEnabled(request OpenRequest, s Snapshot, now time.Time) bool {
	if !s.Fresh(now) || s.Authority.Locked || s.RemoteInputPossible() {
		return false
	}
	switch request {
	case OpenPanel:
		return s.Authority.HostingAuthorized || s.Authority.ViewerAuthorized
	case ReviewAdmission:
		return s.Allow(AdmitGuest, now) == nil
	case ReviewControl:
		return s.Allow(TakeExclusive, now) == nil
	case RestoreSharedInput:
		return s.Allow(ReleaseExclusive, now) == nil
	}
	return false
}

// RemoteInputPossible reports whether a SolarBeam viewer may currently be
// injecting keyboard or mouse input into this PC. Unknown hosting state counts
// as possible: the venue snapshot already folds unknown room membership into
// Active, and an unreadable hosting state says nothing either way.
func (s Snapshot) RemoteInputPossible() bool {
	return !s.Hosting.Known || s.Hosting.Active
}
