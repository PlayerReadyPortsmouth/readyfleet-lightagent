//go:build !windows

package menuhelper

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
)

// shortSocketDir keeps the path under macOS's 104-byte sun_path limit;
// t.TempDir() under /var/folders/... plus a test name can exceed it.
func shortSocketDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "rfm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type fakeHelper struct {
	conn net.Conn
	rd   *bufio.Reader
}

// dialHelper connects and waits until the server has accepted: Dial
// returns as soon as the kernel queues the connection, before Serve has
// made it the attached helper.
func dialHelper(t *testing.T, s *Server) *fakeHelper {
	t.Helper()
	c, err := net.Dial("unix", s.Path())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for i := 0; i < 200 && !s.Connected(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if !s.Connected() {
		t.Fatal("server never attached the helper")
	}
	return &fakeHelper{conn: c, rd: bufio.NewReader(c)}
}

func (h *fakeHelper) send(t *testing.T, msg any) {
	t.Helper()
	b, err := Encode(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.conn.Write(b); err != nil {
		t.Fatal(err)
	}
}

func (h *fakeHelper) next(t *testing.T) any {
	t.Helper()
	_ = h.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := h.rd.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	msg, err := Decode(line)
	if err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return msg
}

func startServer(t *testing.T) (*Server, context.CancelFunc) {
	t.Helper()
	path := filepath.Join(shortSocketDir(t), "menu.sock")
	s, err := Listen(path, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	s.Timeout = 500 * time.Millisecond
	s.NewID = func() string { return "prompt-1" }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return s, cancel
}

func TestListen_SocketIsOwnerOnlyAndStaleFileIsReplaced(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "menu.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("not a socket: %v", info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode %o, want 0600", perm)
	}
}

func TestServe_HelloGetsTheLastStatusAndPublishPushes(t *testing.T) {
	s, _ := startServer(t)
	s.Publish(Status{Online: true, Venue: "venue-1"})
	h := dialHelper(t, s)
	h.send(t, Hello{Version: "test"})
	st, ok := h.next(t).(Status)
	if !ok || !st.Online || st.Venue != "venue-1" {
		t.Fatalf("hello did not return the stored status: %+v", st)
	}
	s.Publish(Status{Online: false, Venue: "venue-1", SessionLive: true})
	st, ok = h.next(t).(Status)
	if !ok || st.Online || !st.SessionLive {
		t.Fatalf("publish did not reach the helper: %+v", st)
	}
	if !s.Connected() {
		t.Error("Connected() false with a helper attached")
	}
}

func TestAsk_WithoutAHelperIsADecline(t *testing.T) {
	s, _ := startServer(t)
	if out := s.Ask(context.Background(), consent.Request{Title: "x"}); out != consent.OutcomeNoPrompter || out.Allowed() {
		t.Fatalf("outcome %q", out)
	}
}

func TestAsk_ApproveAndDeclineComeBackAsPressed(t *testing.T) {
	s, _ := startServer(t)
	h := dialHelper(t, s)
	h.send(t, Hello{})
	for _, approve := range []bool{true, false} {
		got := make(chan consent.Outcome, 1)
		go func() {
			got <- s.Ask(context.Background(), consent.Request{Title: "Jo wants to connect", Body: "to your Mac", URL: "https://readyapp.player-ready.co.uk/devices", RequesterID: "secret-oid"})
		}()
		p, ok := h.next(t).(Prompt)
		if !ok || p.ID != "prompt-1" || p.Requester != "Jo wants to connect" || p.Detail != "to your Mac" {
			t.Fatalf("prompt: %+v", p)
		}
		if time.Until(p.ExpiresAt) <= 0 || time.Until(p.ExpiresAt) > time.Second {
			t.Errorf("expires_at %v not within the timeout", p.ExpiresAt)
		}
		h.send(t, Decision{ID: p.ID, Approve: approve})
		out := <-got
		if approve && out != consent.OutcomeAccepted {
			t.Errorf("approve → %q", out)
		}
		if !approve && out != consent.OutcomeDeclined {
			t.Errorf("decline → %q", out)
		}
	}
}

func TestAsk_PromptCarriesNoURLOrRequesterID(t *testing.T) {
	s, _ := startServer(t)
	h := dialHelper(t, s)
	go s.Ask(context.Background(), consent.Request{Title: "t", URL: "https://readyapp.player-ready.co.uk/r/1", RequesterID: "oid-123", RequesterEmail: "a@b"})
	_ = h.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := h.rd.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"url", "requester_id", "requester_email"} {
		if _, present := raw[k]; present {
			t.Errorf("prompt leaked %s: %s", k, line)
		}
	}
}

func TestAsk_SilenceTimesOutAndCancelsThePrompt(t *testing.T) {
	s, _ := startServer(t)
	h := dialHelper(t, s)
	got := make(chan consent.Outcome, 1)
	go func() { got <- s.Ask(context.Background(), consent.Request{Title: "t"}) }()
	if _, ok := h.next(t).(Prompt); !ok {
		t.Fatal("no prompt")
	}
	if out := <-got; out != consent.OutcomeTimedOut || out.Allowed() {
		t.Fatalf("outcome %q", out)
	}
	if c, ok := h.next(t).(Cancel); !ok || c.ID != "prompt-1" {
		t.Fatalf("no cancel after timeout: %+v", c)
	}
	// A late Approve must be ignored, not crash, not count.
	h.send(t, Decision{ID: "prompt-1", Approve: true})
	time.Sleep(50 * time.Millisecond)
}

func TestAsk_RemoteDismissCancelsThePromptInTheMenu(t *testing.T) {
	s, _ := startServer(t)
	h := dialHelper(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan consent.Outcome, 1)
	go func() { got <- s.Ask(ctx, consent.Request{Title: "t"}) }()
	if _, ok := h.next(t).(Prompt); !ok {
		t.Fatal("no prompt")
	}
	cancel()
	if out := <-got; out != consent.OutcomeCancelled {
		t.Fatalf("outcome %q", out)
	}
	if _, ok := h.next(t).(Cancel); !ok {
		t.Fatal("helper was not told to close the question")
	}
}

func TestAsk_HelperGoneMidQuestionIsADecline(t *testing.T) {
	s, _ := startServer(t)
	h := dialHelper(t, s)
	got := make(chan consent.Outcome, 1)
	go func() { got <- s.Ask(context.Background(), consent.Request{Title: "t"}) }()
	if _, ok := h.next(t).(Prompt); !ok {
		t.Fatal("no prompt")
	}
	_ = h.conn.Close()
	select {
	case out := <-got:
		if out.Allowed() || out != consent.OutcomeNoPrompter {
			t.Fatalf("outcome %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask kept waiting on a dead helper")
	}
}

func TestServe_NewerHelperReplacesTheOld(t *testing.T) {
	s, _ := startServer(t)
	s.Publish(Status{Online: false})
	old := dialHelper(t, s)
	old.send(t, Hello{})
	if st, ok := old.next(t).(Status); !ok || st.Online {
		t.Fatalf("old helper hello: %+v", st)
	}
	fresh := dialHelper(t, s) // Connected() is already true: wait for the hello reply instead
	fresh.send(t, Hello{})
	if st, ok := fresh.next(t).(Status); !ok || st.Online {
		t.Fatalf("new helper hello: %+v", st)
	}
	s.Publish(Status{Online: true})
	if st, ok := fresh.next(t).(Status); !ok || !st.Online {
		t.Fatalf("new helper did not get the status: %+v", st)
	}
	_ = old.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := old.rd.ReadByte(); err == nil {
		t.Fatal("old helper is still being written to")
	}
}

func TestServe_ContextEndRemovesTheSocket(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "menu.sock")
	s, err := Listen(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx); close(done) }()
	cancel()
	<-done
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket left behind: %v", err)
	}
}
