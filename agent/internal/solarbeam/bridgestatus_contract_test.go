package solarbeam

import (
	"context"
	"net/http"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Wire bodies exactly as solarbeam-livekit-bridge serves GET /status since
// bridge PR #16 (feat/bridge-status-20260925), byte for byte from its
// wsbridge.Status marshalling. Before #16 bridge main had no /status route at
// all, so every running bridge read as "session active" and blocked engine
// installs (Ports-Admin, 24–25 Sep 2026). If the bridge changes a field name,
// update both halves together.
const (
	bridgeBodyWatched      = `{"engine_connected":true,"livekit_connected":true,"viewers":2,"session_id":"session","uptime_seconds":137,"viewer_presence_known":true,"confirmed_empty_seconds":0}`
	bridgeBodyEmptyShort   = `{"engine_connected":true,"livekit_connected":true,"viewers":0,"session_id":"session","uptime_seconds":137,"viewer_presence_known":true,"confirmed_empty_seconds":120}`
	bridgeBodyEmptyLong    = `{"engine_connected":true,"livekit_connected":true,"viewers":0,"session_id":"session","uptime_seconds":1000,"viewer_presence_known":true,"confirmed_empty_seconds":960}`
	bridgeBodyResuming     = `{"engine_connected":true,"livekit_connected":true,"viewers":1,"session_id":"session","uptime_seconds":137,"viewer_presence_known":false,"confirmed_empty_seconds":0}`
	bridgeBodyDisconnected = `{"engine_connected":false,"livekit_connected":false,"viewers":0,"session_id":"session","uptime_seconds":900,"viewer_presence_known":false,"confirmed_empty_seconds":0}`
	// handleStatus's marshal-failure fallback: must read as watched.
	bridgeBodyFallback = `{"engine_connected":false,"livekit_connected":true,"viewers":1,"session_id":"","uptime_seconds":0,"viewer_presence_known":false,"confirmed_empty_seconds":0}`
)

func TestBridgeStatusContractSessionActive(t *testing.T) {
	cases := []struct {
		name, body string
		active     bool
	}{
		{"watched", bridgeBodyWatched, true},
		{"empty", bridgeBodyEmptyShort, false},
		{"resuming keeps its viewer", bridgeBodyResuming, true},
		// The room is gone (the bridge exits by itself shortly after): nobody
		// can be watching, so installs may proceed.
		{"livekit disconnected", bridgeBodyDisconnected, false},
		{"marshal fallback", bridgeBodyFallback, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			m.paths.Mode = ModeManaged // warm engine: the /status branch of SessionActive
			if got := m.SessionActive(context.Background()); got != tc.active {
				t.Fatalf("SessionActive = %v, want %v", got, tc.active)
			}
		})
	}
}

func TestBridgeStatusContractPresenceAndExpiry(t *testing.T) {
	cases := []struct {
		name, body  string
		known       bool
		count       int
		allowExpiry bool
	}{
		{"watched", bridgeBodyWatched, true, 2, false},
		{"empty, not long enough", bridgeBodyEmptyShort, true, 0, false},
		{"empty fifteen minutes", bridgeBodyEmptyLong, true, 0, true},
		{"resuming", bridgeBodyResuming, false, 0, false},
		{"livekit disconnected", bridgeBodyDisconnected, false, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			if p := m.RemotePresence(context.Background()); p.Known != tc.known || p.Count != tc.count || len(p.Viewers) != 0 {
				t.Fatalf("RemotePresence = %+v, want known=%v count=%d, no names", p, tc.known, tc.count)
			}
			startBoundSession(t, m, "session")
			stopped := false
			m.stopFn = func() error { stopped = true; return nil }
			err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session", RequireEmpty: true})
			if (err == nil) != tc.allowExpiry || stopped != tc.allowExpiry {
				t.Fatalf("expiry stopped=%v err=%v, want allowed=%v", stopped, err, tc.allowExpiry)
			}
		})
	}
}
