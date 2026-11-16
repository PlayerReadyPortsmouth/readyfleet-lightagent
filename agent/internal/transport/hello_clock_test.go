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

// TestHelloAck_CarriesServerTimeToTheObserver is the wire half of the clock
// posture: the agent's only reference clock is the ServerTime the C2 stamps
// into hello_ack, so if that field does not survive the frame — dropped by a
// decode, truncated to a date, silently zero — the offset is measured against
// nothing and a drifted machine reports as healthy.
//
// The server here stamps a deliberately skewed time (the C2 is "ahead" of
// this test process by a fixed amount) so a decode that lost the field, or
// substituted the local clock for it, cannot pass by coincidence.
func TestHelloAck_CarriesServerTimeToTheObserver(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	// A fixed instant, not time.Now(): the assertion is on the exact value
	// making it across, and RFC3339 JSON round-tripping is only lossless to
	// the precision actually encoded.
	stamped := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if json.Unmarshal(raw, &helloEnv) != nil || helloEnv.Type != proto.MsgHello {
			return
		}
		ack, _ := proto.NewEnvelope(proto.MsgHelloAck, uuid.Nil, &helloEnv.ID, proto.HelloAckData{
			AcceptedProtocolVersion: proto.ProtocolVersion,
			HeartbeatSeconds:        30,
			ServerTime:              stamped,
		})
		ackJSON, _ := json.Marshal(ack)
		_ = conn.WriteMessage(websocket.TextMessage, ackJSON)
		// Hold the connection open so the client does not reconnect and
		// deliver a second ack mid-assertion.
		conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	acks := make(chan proto.HelloAckData, 1)
	cli := NewClient(Config{
		ServerURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID:    uuid.NewString(),
		AgentVersion: "test-0.1",
		OS:           "linux",
		Insecure:     true,
		Logger:       logger,
		HelloAckObserver: func(ack proto.HelloAckData) {
			select {
			case acks <- ack:
			default:
			}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = cli.Run(ctx) }()

	select {
	case ack := <-acks:
		if ack.ServerTime.IsZero() {
			t.Fatal("server_time did not survive the hello_ack frame")
		}
		if !ack.ServerTime.Equal(stamped) {
			t.Fatalf("server_time = %v, want %v", ack.ServerTime.UTC(), stamped)
		}
		// And the value is usable as a reference: a local clock 40s behind
		// this stamp must read as -40s, the PC-7 shape.
		wantOffset := -40 * time.Second
		if got := stamped.Add(wantOffset).Sub(ack.ServerTime); got != wantOffset {
			t.Fatalf("offset from the decoded stamp = %v, want %v", got, wantOffset)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HelloAckObserver never fired")
	}
}
