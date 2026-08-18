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

func TestClient_PushesTelemetryOnInterval(t *testing.T) {
	var calls int32
	provider := func(ctx context.Context) (proto.TelemetryData, error) {
		atomic.AddInt32(&calls, 1)
		return proto.TelemetryData{CollectedAt: time.Now().UTC(), CPUPercent: 5}, nil
	}

	received := make(chan proto.Envelope, 4)

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

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

		// Read until we get telemetry or the connection closes.
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env proto.Envelope
			if err := json.Unmarshal(raw, &env); err != nil {
				continue
			}
			if env.Type == proto.MsgTelemetry {
				received <- env
				// Don't return immediately, continue reading in case there are more messages
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	logger := slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))

	client := NewClient(Config{
		ServerURL:         wsURL,
		MachineID:         "test-machine",
		Insecure:          true,
		Logger:            logger,
		TelemetryProvider: provider,
		TelemetryInterval: 20 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = client.Run(ctx) }()

	select {
	case env := <-received:
		var data proto.TelemetryData
		if err := env.DecodeData(&data); err != nil {
			t.Fatalf("decode telemetry: %v", err)
		}
		if data.CPUPercent != 5 {
			t.Errorf("CPUPercent = %v, want 5", data.CPUPercent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for MsgTelemetry")
	}
}
