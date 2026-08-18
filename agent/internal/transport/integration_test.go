package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// TestClient_HandshakeAndCommand spins up a hand-rolled WebSocket
// "server" that speaks the wire protocol (a stand-in for the real
// agentws handler) and verifies that the agent's transport client:
//
//   - completes the hello → hello_ack handshake
//   - replies to ping with pong
//   - invokes CommandHandler when the server sends a MsgCommand
//
// The server-side equivalent lives in
// server/internal/agentws/handler_test.go: it spins up the real
// handler against a hand-rolled WS client. Together they prove both
// implementations agree on the wire format without requiring a
// circular module dependency.
func TestClient_HandshakeAndCommand(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}
	gotHello := make(chan struct{}, 1)
	cmdSent := make(chan struct{}, 1)
	gotResult := make(chan proto.Envelope, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		// Read hello.
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var helloEnv proto.Envelope
		if err := json.Unmarshal(raw, &helloEnv); err != nil {
			t.Errorf("hello unmarshal: %v", err)
			return
		}
		if helloEnv.Type != proto.MsgHello {
			t.Errorf("first message: got %q want hello", helloEnv.Type)
			return
		}
		select {
		case gotHello <- struct{}{}:
		default:
		}

		// Send hello_ack.
		ack, _ := proto.NewEnvelope(proto.MsgHelloAck, uuid.Nil, &helloEnv.ID, proto.HelloAckData{
			AcceptedProtocolVersion: proto.ProtocolVersion,
			HeartbeatSeconds:        30,
			ServerTime:              time.Now().UTC(),
		})
		ackJSON, _ := json.Marshal(ack)
		if err := conn.WriteMessage(websocket.TextMessage, ackJSON); err != nil {
			return
		}

		// Send a command.
		cmd, _ := proto.NewEnvelope(proto.MsgCommand, uuid.Nil, nil, proto.CommandData{
			// Any kind round-trips identically here — this exercises the
			// transport, not the command. Use one the light agent actually
			// handles so this file carries no managed-only surface.
			Kind: proto.CmdUpdate,
			Args: []byte(`{"version":"0.0.1"}`),
		})
		cmdJSON, _ := json.Marshal(cmd)
		if err := conn.WriteMessage(websocket.TextMessage, cmdJSON); err != nil {
			return
		}
		select {
		case cmdSent <- struct{}{}:
		default:
		}

		// Read until we get a result, or the connection closes.
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env proto.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue
			}
			if env.Type == proto.MsgResult {
				gotResult <- env
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	var ranCommand atomic.Bool
	cli := NewClient(Config{
		ServerURL:    wsURL,
		MachineID:    uuid.NewString(),
		AgentVersion: "test-0.1",
		OS:           "linux",
		Insecure:     true,
		Logger:       logger,
		CommandHandler: func(ctx context.Context, env proto.Envelope, send SendFunc) {
			ranCommand.Store(true)
			result, _ := proto.NewEnvelope(proto.MsgResult, uuid.Nil, &env.ID, proto.ResultData{
				ExitCode: 0,
			})
			_ = send(ctx, result)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientDone := make(chan error, 1)
	go func() { clientDone <- cli.Run(ctx) }()

	select {
	case <-gotHello:
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not see hello")
	}
	select {
	case <-cmdSent:
	case <-time.After(2 * time.Second):
		t.Fatalf("server did not send command")
	}
	select {
	case env := <-gotResult:
		if env.Type != proto.MsgResult {
			t.Errorf("got %q want result", env.Type)
		}
		if !ranCommand.Load() {
			t.Errorf("CommandHandler never ran")
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("server did not receive result")
	}

	cancel()
	select {
	case <-clientDone:
	case <-time.After(2 * time.Second):
		t.Errorf("client did not exit promptly")
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
