package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// dialTestConn upgrades a WebSocket against handler and returns the client
// side as a persistentConn with a short read deadline so the liveness paths
// are provable in milliseconds rather than the production 75s window.
func dialTestConn(t *testing.T, handler http.HandlerFunc, readDL time.Duration) (*persistentConn, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
	pc := newPersistentConn(conn, logger)
	pc.readDeadline = readDL
	cleanup := func() {
		_ = pc.Close()
		srv.Close()
	}
	return pc, cleanup
}

// TestReadLoop_StalledReaderReturnsWithinDeadline proves the core 3.3 fix: a
// server that upgrades then goes completely silent — never sending a message
// and never reading (so it never auto-replies to our control pings) — models a
// silently-dropped TCP path. Before the fix ReadMessage blocked forever here
// and the reconnect loop never fired. With a read deadline, readLoop must
// return an error within roughly p.readDeadline.
//
// Real network-partition behaviour (a black-holed route with no RST/FIN) can
// only be proven on a real venue host; this test exercises the timeout that
// bounds detection of it.
func TestReadLoop_StalledReaderReturnsWithinDeadline(t *testing.T) {
	hold := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	pc, cleanup := dialTestConn(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		<-hold // hold the connection open but silent; never read, never write
	}, 200*time.Millisecond)
	defer cleanup()
	defer close(hold)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- pc.readLoop(context.Background(),
			func(context.Context, *persistentConn, proto.Envelope) error { return nil })
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readLoop returned nil; expected a read-deadline timeout error")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("readLoop took %v to surface the dead link; expected ~readDeadline (200ms)", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readLoop blocked past the read deadline; dead link never surfaced")
	}
}

// TestReadLoop_HealthyIdleLinkSurvives proves the other half of the contract:
// an idle-but-alive link must not be torn down. The server sends nothing but
// application data of its own; instead a steady trickle of inbound messages
// (standing in for the server's 30s heartbeat, sped up here) keeps refreshing
// the read deadline, so readLoop stays blocked in ReadMessage until the peer
// actually goes away.
func TestReadLoop_HealthyIdleLinkSurvives(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	stop := make(chan struct{})
	pc, cleanup := dialTestConn(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Trickle a ping every 50ms — comfortably under the 200ms deadline —
		// for ~600ms, then stop and let the read deadline finally trip.
		ping, _ := proto.NewEnvelope(proto.MsgPing, uuid.Nil, nil, nil)
		b, _ := json.Marshal(ping)
		t := time.NewTicker(50 * time.Millisecond)
		defer t.Stop()
		deadline := time.After(600 * time.Millisecond)
		for {
			select {
			case <-stop:
				return
			case <-deadline:
				<-stop
				return
			case <-t.C:
				if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
					return
				}
			}
		}
	}, 200*time.Millisecond)
	defer cleanup()
	defer close(stop)

	done := make(chan error, 1)
	go func() {
		done <- pc.readLoop(context.Background(),
			func(context.Context, *persistentConn, proto.Envelope) error { return nil })
	}()

	// While the server is trickling messages the link must stay up well past
	// the 200ms deadline.
	select {
	case err := <-done:
		t.Fatalf("readLoop returned early on a healthy link: %v", err)
	case <-time.After(500 * time.Millisecond):
		// Still alive after 2.5× the deadline — the refresh-on-inbound works.
	}
}
