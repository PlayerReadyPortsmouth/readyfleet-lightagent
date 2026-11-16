package solarbeam

import (
	"context"
	"net/http"
	"testing"
)

func TestRemotePresenceStates(t *testing.T) {
	cases := []struct {
		name, body string
		known      bool
		count      int
		nameWant   string
	}{
		{"named", `{"viewers":1,"livekit_connected":true,"viewer_presence_known":true,"remote_viewers":[{"key":"1234567890abcdef1234567890abcdef","name":"Aurora Nicholas"}]}`, true, 1, "Aurora Nicholas"},
		{"email suppressed", `{"viewers":1,"livekit_connected":true,"remote_viewers":[{"key":"1234567890abcdef1234567890abcdef","name":"private@example.org"}]}`, true, 1, "Remote viewer"},
		{"legacy count", `{"viewers":2,"livekit_connected":true}`, true, 2, ""},
		{"left", `{"viewers":0,"livekit_connected":true}`, true, 0, ""},
		{"reconnecting cached", `{"viewers":2,"livekit_connected":false}`, false, 0, ""},
		{"explicit unknown", `{"viewers":2,"livekit_connected":true,"viewer_presence_known":false}`, false, 0, ""},
		{"inconsistent roster", `{"viewers":0,"livekit_connected":true,"remote_viewers":[{"key":"1234567890abcdef1234567890abcdef","name":"Aurora"}]}`, false, 0, ""},
		{"raw identity key", `{"viewers":1,"livekit_connected":true,"remote_viewers":[{"key":"mentor-private-id","name":"Aurora"}]}`, false, 0, ""},
		{"malformed", `{`, false, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			got := m.RemotePresence(context.Background())
			if got.Known != tc.known || got.Count != tc.count {
				t.Fatalf("got %+v", got)
			}
			if tc.nameWant != "" && (len(got.Viewers) != 1 || got.Viewers[0].Name != tc.nameWant) {
				t.Fatalf("got %+v", got)
			}
		})
	}
}
func TestRemotePresenceNoProcess(t *testing.T) {
	m := New(Paths{})
	m.bridgeRunningFn = func() bool { return false }
	got := m.RemotePresence(context.Background())
	if !got.Known || got.Count != 0 {
		t.Fatal(got)
	}
}
func TestRemoteNamesRejectIdentifiers(t *testing.T) {
	for _, s := range []string{"person@example.org", "mentor-abc", "3ba7ae96-a32b-4f9b-90de-695d473e7d44", "PA_private", "000123", "Alice\u202eTest", "Alice\nConnected"} {
		if got := remoteName(s); got != "Remote viewer" {
			t.Fatalf("accepted %q", s)
		}
	}
}
