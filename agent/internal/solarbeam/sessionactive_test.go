package solarbeam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newStatusManager builds a warm-engine ModeManaged Manager whose bridge
// process is reported present and whose /status probe points at handler.
// Everything else is stubbed so no test touches the SCM, the process table
// or the real bridge.
func newStatusManager(t *testing.T, handler http.HandlerFunc) *Manager {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	m := New(Paths{})
	m.bridgeRunningFn = func() bool { return true }
	m.bridgeStatusURL = srv.URL
	return m
}

// The whole point of the redefinition: someone is watching ⇒ refuse the
// update. An update applied here cuts a live stream a young person is on.
func TestSessionActive_ViewersPresentIsActive(t *testing.T) {
	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"engine_connected":true,"livekit_connected":true,"viewers":2,"session_id":"sess-1","uptime_seconds":137}`))
	})
	if !m.SessionActive(context.Background()) {
		t.Fatal("viewers > 0 must report the session ACTIVE")
	}
}

// The only branch that may permit an update once a bridge is up: a clear,
// well-formed "nobody is watching".
func TestSessionActive_ZeroViewersIsInactive(t *testing.T) {
	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"engine_connected":true,"livekit_connected":true,"viewers":0,"session_id":"sess-1","uptime_seconds":9}`))
	})
	if m.SessionActive(context.Background()) {
		t.Fatal("a well-formed viewers == 0 must report the session INACTIVE")
	}
}

// The outer gate. Without it, "cannot reach /status" is indistinguishable
// from "there is no bridge because there is no session", and the fail-safe
// answer to the former would block fleet updates on this machine forever —
// exactly the silent stall this change exists to prevent.
func TestSessionActive_NoBridgeProcessIsInactiveWithoutProbing(t *testing.T) {
	probed := false
	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		probed = true
		w.Write([]byte(`{"viewers":5}`))
	})
	m.bridgeRunningFn = func() bool { return false }

	if m.SessionActive(context.Background()) {
		t.Fatal("no bridge process means no session; must report INACTIVE")
	}
	if probed {
		t.Fatal("must not probe /status when there is no bridge process at all")
	}
}

// Every ambiguous answer resolves to ACTIVE. A wedged bridge, an engine MSI
// predating the /status route, a truncated body — none of them may open an
// update window on a possibly-live stream.
func TestSessionActive_AmbiguousBridgeAnswersAreActive(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "malformed body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`this is not json`))
			},
		},
		{
			name: "well-formed JSON with no viewers field",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"session_id":"sess-1","uptime_seconds":12}`))
			},
		},
		{
			name:    "empty body",
			handler: func(w http.ResponseWriter, r *http.Request) {},
		},
		{
			name: "negative viewer count",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"viewers":-1}`))
			},
		},
		{
			name: "500 from a broken bridge",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
		},
		{
			name: "404 from a bridge predating the status route",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
		},
		{
			name: "non-JSON 200, e.g. an HTML error page",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.Write([]byte(`<html><body>gateway</body></html>`))
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newStatusManager(t, tc.handler)
			if !m.SessionActive(context.Background()) {
				t.Fatalf("%s must fail safe to ACTIVE", tc.name)
			}
		})
	}
}

// A bridge that is running but not listening (crashed after its process
// was seen, port closed, firewall) must not be read as "safe to update".
func TestSessionActive_BridgeUnreachableIsActive(t *testing.T) {
	m := New(Paths{})
	m.bridgeRunningFn = func() bool { return true }
	// Port 1 on loopback: nothing listens there, so the dial fails fast.
	m.bridgeStatusURL = "http://127.0.0.1:1/status"

	if !m.SessionActive(context.Background()) {
		t.Fatal("an unreachable bridge must fail safe to ACTIVE")
	}
}

// A hung bridge must neither stall the caller nor be read as inactive.
func TestSessionActive_TimeoutIsActive(t *testing.T) {
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })

	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		// Hang until the test ends; the probe must give up first.
		select {
		case <-released:
		case <-r.Context().Done():
		}
	})
	m.statusClient = &http.Client{Timeout: 50 * time.Millisecond}

	start := time.Now()
	active := m.SessionActive(context.Background())
	elapsed := time.Since(start)

	if !active {
		t.Fatal("a hung bridge must fail safe to ACTIVE")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("probe took %s; a hung bridge must not stall the caller", elapsed)
	}
}

// The default probe timeout must stay short for the same reason.
func TestBridgeStatusTimeout_IsShort(t *testing.T) {
	if bridgeStatusTimeout > 5*time.Second {
		t.Fatalf("bridgeStatusTimeout = %s; a hung bridge would stall update/install decisions", bridgeStatusTimeout)
	}
}

// BYOD keeps today's meaning of "session active": the light agent starts
// the engine on session start and kills it on session end, so engine
// liveness is still truthful there — and it must never consult a bridge
// status endpoint that its profile does not keep running.
func TestSessionActive_InteractiveModeUsesEngineLiveness(t *testing.T) {
	running := true
	m := New(Paths{Mode: ModeInteractive})
	m.statusFn = func() bool { return running }
	m.bridgeRunningFn = func() bool {
		t.Fatal("ModeInteractive must not consult the bridge process")
		return false
	}
	m.bridgeStatusURL = "http://127.0.0.1:1/status"

	if !m.SessionActive(context.Background()) {
		t.Fatal("ModeInteractive: a running engine must report ACTIVE, as today")
	}
	running = false
	if m.SessionActive(context.Background()) {
		t.Fatal("ModeInteractive: a stopped engine must report INACTIVE, as today")
	}
}

// The kill switch is one lever: reverting a venue PC to the pre-warm
// lifecycle reverts the interlock's definition with it, so the machine
// behaves exactly as it did before this change.
func TestSessionActive_WarmDisabledUsesEngineLiveness(t *testing.T) {
	running := true
	m := New(Paths{DisableWarmEngine: true})
	m.statusFn = func() bool { return running }
	m.bridgeRunningFn = func() bool {
		t.Fatal("warm-engine disabled must not consult the bridge process")
		return false
	}

	if !m.SessionActive(context.Background()) {
		t.Fatal("warm disabled: a running engine must report ACTIVE, as before the change")
	}
	running = false
	if m.SessionActive(context.Background()) {
		t.Fatal("warm disabled: a stopped engine must report INACTIVE, as before the change")
	}
}

// A body large enough to be a denial-of-service is truncated, and a
// truncated body is malformed, which fails safe.
func TestSessionActive_OversizedBodyIsActive(t *testing.T) {
	m := newStatusManager(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"viewers":0,"pad":"`))
		chunk := make([]byte, 8192)
		for i := range chunk {
			chunk[i] = 'a'
		}
		for i := 0; i < 16; i++ {
			w.Write(chunk)
		}
		w.Write([]byte(`"}`))
	})
	if !m.SessionActive(context.Background()) {
		t.Fatal("an oversized /status body must fail safe to ACTIVE")
	}
}
