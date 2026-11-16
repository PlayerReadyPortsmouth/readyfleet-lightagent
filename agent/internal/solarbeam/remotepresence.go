package solarbeam

import (
	"context"
	"regexp"
	"strings"
	"unicode"
)

// Keys are opaque and for local toast deduplication only, never displayed.
type RemoteViewer struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}
type RemotePresence struct {
	Known   bool           `json:"known"`
	Count   int            `json:"count"`
	Viewers []RemoteViewer `json:"viewers,omitempty"`
}

var remoteKey = regexp.MustCompile(`^[0-9a-f]{32}$`)
var remoteIdentifier = regexp.MustCompile(`(?i)^(?:(?:mentor|viewer|driver|participant|guest|cyp|PA|RM|sess)[_-]|[0-9a-f]{8}-[0-9a-f-]{27}$)`)

func remoteName(raw string) string {
	for _, c := range raw {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return "Remote viewer"
		}
	}
	name := strings.TrimSpace(raw)
	if name == "" || len([]rune(name)) > 80 || remoteIdentifier.MatchString(name) {
		return "Remote viewer"
	}
	letter := false
	for _, c := range name {
		if unicode.IsLetter(c) {
			letter = true
			continue
		}
		if unicode.IsMark(c) || c == ' ' || strings.ContainsRune(".’'()-", c) {
			continue
		}
		return "Remote viewer"
	}
	if !letter {
		return "Remote viewer"
	}
	return name
}

// RemotePresence answers who is connected now; unlike the update interlock,
// uncertainty clears the indicator rather than inventing a live connection.
// Older count-only bridges remain useful without manufacturing viewer names.
func (m *Manager) RemotePresence(ctx context.Context) RemotePresence {
	if !m.bridgeRunningFn() {
		return RemotePresence{Known: true}
	}
	st, err := m.readBridgeStatus(ctx)
	if err != nil || !st.LiveKitConnected || (st.PresenceKnown != nil && !*st.PresenceKnown) || *st.Viewers > 10000 {
		return RemotePresence{}
	}
	out := RemotePresence{Known: true, Count: *st.Viewers}
	if len(st.Remote) > 64 || len(st.Remote) > out.Count {
		return RemotePresence{}
	}
	seen := map[string]bool{}
	for _, viewer := range st.Remote {
		if !remoteKey.MatchString(viewer.Key) || seen[viewer.Key] {
			return RemotePresence{}
		}
		seen[viewer.Key] = true
		out.Viewers = append(out.Viewers, RemoteViewer{Key: viewer.Key, Name: remoteName(viewer.Name)})
	}
	return out
}
