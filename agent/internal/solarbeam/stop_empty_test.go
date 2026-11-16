package solarbeam

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestAutomaticStopRequiresFreshContinuousEmptyEvidence(t *testing.T) {
	cases := []struct {
		name, body string
		allow      bool
	}{
		{"confirmed", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":900}`, true},
		{"occupied", `{"session_id":"session","viewers":1,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"reconnected", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":0}`, false},
		{"not long enough", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":899}`, false},
		{"old bridge", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true}`, false},
		{"unknown", `{"session_id":"session","viewers":0,"viewer_presence_known":false,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"legacy presence", `{"session_id":"session","viewers":0,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"livekit disconnected", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":false,"confirmed_empty_seconds":900}`, false},
		{"replacement bridge", `{"session_id":"replacement","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"unidentified bridge", `{"viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"inconsistent roster", `{"session_id":"session","viewers":0,"remote_viewers":[{"key":"arrival","name":"Viewer"}],"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":900}`, false},
		{"malformed interval", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":"900"}`, false},
		{"negative interval", `{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":-1}`, false},
		{"malformed status", `{`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) })
			startBoundSession(t, m, "session")
			stopped := false
			m.stopFn = func() error { stopped = true; return nil }
			err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session", RequireEmpty: true})
			if (err == nil) != tc.allow || stopped != tc.allow {
				t.Fatalf("stopped=%v err=%v allow=%v", stopped, err, tc.allow)
			}
			if !tc.allow && m.currentSessionID != "session" {
				t.Fatal("deferred expiry lost its session binding")
			}
		})
	}
}

func TestAutomaticStopNoBridgeStillRequiresSessionOwnership(t *testing.T) {
	m := New(Paths{})
	probes := 0
	stops := 0
	m.bridgeRunningFn = func() bool { probes++; return false }
	m.stopFn = func() error { stops++; return nil }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session", RequireEmpty: true}); err == nil {
		t.Fatal("unknown ownership accepted")
	}
	if probes != 0 || stops != 0 {
		t.Fatal("unknown stop reached presence or cleanup")
	}
	startBoundSession(t, m, "session")
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session", RequireEmpty: true}); err != nil {
		t.Fatal(err)
	}
	if probes != 1 || stops != 1 {
		t.Fatalf("probes=%d stops=%d", probes, stops)
	}
}

func TestOperatorStopDoesNotRequireEmptyRoom(t *testing.T) {
	m := New(Paths{})
	startBoundSession(t, m, "session")
	m.bridgeRunningFn = func() bool { t.Error("operator stop should not use automatic expiry probe"); return true }
	stopped := false
	m.stopFn = func() error { stopped = true; return nil }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session"}); err != nil || !stopped {
		t.Fatalf("stopped=%v err=%v", stopped, err)
	}
}

func TestQueuedAutomaticStopRechecksAfterJoinAndLeave(t *testing.T) {
	var emptyFor atomic.Int64
	emptyFor.Store(1200)
	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		if emptyFor.Load() == 1200 {
			_, _ = w.Write([]byte(`{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":1200}`))
		} else {
			_, _ = w.Write([]byte(`{"session_id":"session","viewers":0,"viewer_presence_known":true,"livekit_connected":true,"confirmed_empty_seconds":1}`))
		}
	})
	startBoundSession(t, m, "session")
	if p := m.RemotePresence(context.Background()); !p.Known || p.Count != 0 {
		t.Fatal("initial snapshot should be empty")
	}
	// A viewer joins and leaves while the command is queued. Bridge evidence
	// resets; the current zero count alone would have allowed an unsafe stop.
	emptyFor.Store(1)
	m.stopFn = func() error { t.Error("expired queue snapshot reached cleanup"); return nil }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "session", RequireEmpty: true}); err == nil {
		t.Fatal("stale queued expiry accepted")
	}
}
