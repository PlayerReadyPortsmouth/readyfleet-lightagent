package transport

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// hangUpServer accepts TCP connections and closes each one immediately, so
// every WebSocket handshake fails while the test can still count dials. A
// closed port would fail too, but then the reconnect loop is unobservable —
// and the dial count is the whole symptom being guarded here.
func hangUpServer(t *testing.T, dialed chan<- struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
			select {
			case dialed <- struct{}{}:
			default:
			}
		}
	}()
	return ln.Addr().String()
}

// TestClient_Run_FailedDialTakesBackoff guards the reconnect hot-loop
// regression: connectAndServe's deferred block used to test attemptCtx.Err()
// AFTER calling cancel(), where it is always non-nil, so every genuine
// failure looked like a SetPaused abort and Run took the no-delay pause path.
// Agents redialled as fast as the OS could refuse them (4867 dials for 28
// connects observed in prod). A failing dial under a live outer ctx must walk
// the documented 1s..60s schedule instead.
func TestClient_Run_FailedDialTakesBackoff(t *testing.T) {
	dialed := make(chan struct{}, 64)
	addr := hangUpServer(t, dialed)

	cli := NewClient(Config{
		ServerURL: "ws://" + addr,
		MachineID: uuid.NewString(),
		Insecure:  true,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})

	// Record the schedule instead of sleeping it, and stop the loop once
	// we have enough samples so the test never waits on real time.
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	delays := make(chan time.Duration, len(want))
	cli.sleepFn = func(_ context.Context, d time.Duration) bool {
		select {
		case delays <- d:
			return true
		default:
			return false
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Run never took a backoff delay after %d failed dials — "+
			"reconnect is hot-looping", len(dialed))
	}

	if got := len(delays); got != len(want) {
		t.Fatalf("recorded %d backoff delays, want %d", got, len(want))
	}
	for i, w := range want {
		if got := <-delays; got != w {
			t.Errorf("backoff delay %d: got %v want %v", i, got, w)
		}
	}

	// One dial per delay, plus the one that ended the loop. Anything more
	// means a dial slipped through without its delay.
	if got := len(dialed); got != len(want)+1 {
		t.Errorf("server saw %d dials, want %d", got, len(want)+1)
	}

	if last := cli.Stats().LastError; !strings.Contains(last, "dial") {
		t.Errorf("Stats().LastError = %q, want the underlying dial failure", last)
	}
}

// TestClient_SetPaused_SkipsBackoff is the other half of the classification:
// a connection dropped by SetPaused is not a failure, so it must still take
// the no-delay path straight back to Run's pause check.
func TestClient_SetPaused_SkipsBackoff(t *testing.T) {
	connected := make(chan struct{}, 4)
	srv := helloAckServer(t, connected)
	defer srv.Close()

	cli := NewClient(Config{
		ServerURL: "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID: uuid.NewString(),
		Insecure:  true,
		Logger:    slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})

	delays := make(chan time.Duration, 4)
	cli.sleepFn = func(ctx context.Context, d time.Duration) bool {
		select {
		case delays <- d:
		default:
		}
		return sleepBackoff(ctx, d)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cli.Run(ctx) }()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("server never saw a connection")
	}

	cli.SetPaused(true)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && cli.Stats().Connected {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.Stats().Connected {
		t.Fatal("Stats().Connected still true a second after SetPaused(true)")
	}

	select {
	case d := <-delays:
		t.Fatalf("pause-triggered drop slept %v, want no backoff", d)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	<-done
}
