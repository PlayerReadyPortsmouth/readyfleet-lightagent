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

// helloAckServer is a minimal test WS server: accepts one connection at a
// time, replies to hello with hello_ack, and reports each new connection
// on connected. Used by both tests below in place of
// TestClient_HandshakeAndCommand's fuller (command-dispatching) harness —
// stats/pause only need the handshake itself.
func helloAckServer(t *testing.T, connected chan<- struct{}) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var helloEnv proto.Envelope
		if err := json.Unmarshal(raw, &helloEnv); err != nil {
			return
		}
		ack, _ := proto.NewEnvelope(proto.MsgHelloAck, uuid.Nil, &helloEnv.ID, proto.HelloAckData{
			AcceptedProtocolVersion: proto.ProtocolVersion,
			HeartbeatSeconds:        30,
			ServerTime:              time.Now().UTC(),
		})
		ackJSON, _ := json.Marshal(ack)
		if err := conn.WriteMessage(websocket.TextMessage, ackJSON); err != nil {
			return
		}
		select {
		case connected <- struct{}{}:
		default:
		}
		// Hold the connection open until the client closes it (pause) or
		// the test server shuts down.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
}

func TestClient_Stats_ReflectsConnection(t *testing.T) {
	connected := make(chan struct{}, 4)
	srv := helloAckServer(t, connected)
	defer srv.Close()

	cli := NewClient(Config{
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID: uuid.NewString(),
		Insecure:  true,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx) }()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw a connection")
	}

	// Stats() is polled asynchronously by the caller in this test just
	// like a tray icon would — give the client a moment to record the
	// connect before asserting.
	var stats Stats
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats = cli.Stats()
		if stats.Connected {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !stats.Connected {
		t.Fatal("Stats().Connected = false, want true after handshake")
	}
	if stats.ConnectedSince.IsZero() {
		t.Error("Stats().ConnectedSince is zero, want a real timestamp")
	}
	if stats.ConnectCount != 1 {
		t.Errorf("Stats().ConnectCount = %d, want 1", stats.ConnectCount)
	}

	cancel()
	<-done
}

func TestClient_SetPaused_DropsAndBlocksReconnect(t *testing.T) {
	connected := make(chan struct{}, 4)
	srv := helloAckServer(t, connected)
	defer srv.Close()

	cli := NewClient(Config{
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID: uuid.NewString(),
		Insecure:  true,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx) }()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw the first connection")
	}

	cli.SetPaused(true)
	if !cli.Paused() {
		t.Fatal("Paused() = false right after SetPaused(true)")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && cli.Stats().Connected {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.Stats().Connected {
		t.Fatal("Stats().Connected still true a second after SetPaused(true)")
	}

	// Must NOT reconnect while paused.
	select {
	case <-connected:
		t.Fatal("client reconnected while paused")
	case <-time.After(500 * time.Millisecond):
	}

	cli.SetPaused(false)
	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not reconnect after SetPaused(false)")
	}
	if cli.Stats().ConnectCount != 2 {
		t.Errorf("Stats().ConnectCount = %d, want 2 after resuming", cli.Stats().ConnectCount)
	}

	cancel()
	<-done
}
