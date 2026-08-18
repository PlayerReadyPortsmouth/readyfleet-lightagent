package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
	"testing"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// recordingNotifier records every Notify call so tests can assert on what
// the handler actually asked the tray to show, and can be told to fail to
// exercise the error path.
type recordingNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
	err   error
}

type notifyCall struct {
	title, body string
	severity    trayicon.NotifySeverity
	onClick     func()
}

func (n *recordingNotifier) Notify(title, body string, severity trayicon.NotifySeverity, onClick func()) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, notifyCall{title, body, severity, onClick})
	return n.err
}

func makeShowNotificationEnv(t *testing.T, args proto.ShowNotificationArgs) proto.Envelope {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	env, err := proto.NewEnvelope(proto.MsgCommand, uuid.New(), nil, proto.CommandData{
		Kind: proto.CmdShowNotification, Args: raw,
	})
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}
	return env
}

// collectResult runs a handler and returns the single MsgResult it sent.
func collectResult(t *testing.T, run func(send func(ctx context.Context, env proto.Envelope) error)) proto.ResultData {
	t.Helper()
	var got *proto.Envelope
	send := func(ctx context.Context, env proto.Envelope) error {
		e := env
		got = &e
		return nil
	}
	run(send)
	if got == nil {
		t.Fatal("handler never sent a result")
	}
	var res proto.ResultData
	if err := got.DecodeData(&res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return res
}

func TestRunShowNotification_CallsNotifyWithDecodedFields(t *testing.T) {
	// A notice kind, deliberately: connection_request now routes to the
	// persistent dialog instead of a balloon, and this test is about decoding
	// the fields, not about which surface they land on.
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "agent_updated", Title: "Connection request", Body: "Aurora wants to connect",
		Severity: "warning",
	})
	notify := &recordingNotifier{}
	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, &recordingPrompter{})
	})
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d err=%s", res.ExitCode, res.Error)
	}
	notify.mu.Lock()
	defer notify.mu.Unlock()
	if len(notify.calls) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notify.calls))
	}
	got := notify.calls[0]
	if got.title != "Connection request" || got.body != "Aurora wants to connect" {
		t.Fatalf("Notify(%q, %q), want the decoded title/body", got.title, got.body)
	}
	if got.severity != trayicon.NotifyWarning {
		t.Fatalf("severity = %v, want NotifyWarning", got.severity)
	}
}

func TestRunShowNotification_URLPresenceControlsOnClick(t *testing.T) {
	// Notice kinds, not connection_request: that kind now routes to the
	// persistent dialog and never reaches Notify, so there would be no
	// onClick to inspect.
	withURL := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "agent_updated", Title: "t", Body: "b", URL: "https://readyapp.player-ready.co.uk/x",
	})
	notify := &recordingNotifier{}
	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), withURL, send, notify, &recordingPrompter{})
	})
	notify.mu.Lock()
	gotOnClick := notify.calls[0].onClick
	notify.mu.Unlock()
	if gotOnClick == nil {
		t.Fatal("args.URL set but onClick passed to Notify was nil")
	}

	withoutURL := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "agent_updated", Title: "t", Body: "b",
	})
	notify2 := &recordingNotifier{}
	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), withoutURL, send, notify2, &recordingPrompter{})
	})
	notify2.mu.Lock()
	gotOnClick2 := notify2.calls[0].onClick
	notify2.mu.Unlock()
	if gotOnClick2 != nil {
		t.Fatal("args.URL unset but onClick passed to Notify was non-nil")
	}
}

func TestRunShowNotification_MissingFieldsRejectedBeforeNotify(t *testing.T) {
	cases := []proto.ShowNotificationArgs{
		{Title: "t", Body: "b"}, // missing kind
		{Kind: "k", Body: "b"},  // missing title
		{Kind: "k", Title: "t"}, // missing body
	}
	for _, args := range cases {
		env := makeShowNotificationEnv(t, args)
		notify := &recordingNotifier{}
		res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
			runShowNotification(context.Background(), env, send, notify, &recordingPrompter{})
		})
		if res.ExitCode == 0 {
			t.Errorf("args %+v: expected non-zero exit for missing required field", args)
		}
		if len(notify.calls) != 0 {
			t.Errorf("args %+v: Notify must not run when validation fails", args)
		}
	}
}

func TestRunShowNotification_NotifyErrorSurfacedVerbatim(t *testing.T) {
	// A notice kind: this asserts the balloon's failure is reported, and
	// connection_request no longer takes the balloon path.
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "agent_updated", Title: "t", Body: "b",
	})
	notify := &recordingNotifier{err: errors.New("trayicon: tray not running")}
	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, &recordingPrompter{})
	})
	if res.ExitCode == 0 {
		t.Fatal("expected non-zero exit when Notify fails")
	}
	if res.Error != "trayicon: tray not running" {
		t.Fatalf("result error = %q, want the notifier's error verbatim", res.Error)
	}
}

func TestSeverityFromWire(t *testing.T) {
	cases := map[string]trayicon.NotifySeverity{
		"warning":      trayicon.NotifyWarning,
		"error":        trayicon.NotifyError,
		"info":         trayicon.NotifyInfo,
		"":             trayicon.NotifyInfo,
		"unrecognised": trayicon.NotifyInfo, // fails open, not closed — see ShowNotificationArgs's doc comment
	}
	for in, want := range cases {
		if got := severityFromWire(in); got != want {
			t.Errorf("severityFromWire(%q) = %v, want %v", in, got, want)
		}
	}
}



// A connection request is a question, not a notice: a tray balloon fades
// after a few seconds whether or not the mentor saw it, and a missed request
// simply expires 30 minutes later with nobody knowing they were asked. So
// this kind — and only this kind — goes to the dialog that waits.
func TestRunShowNotification_ConnectionRequestUsesThePersistentPrompt(t *testing.T) {
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind:  "connection_request",
		Title: "Alex Staff wants to connect to your PC",
		Body:  "For a lesson at 4pm",
		URL:   "https://readyapp.player-ready.co.uk/settings/solarbeam-devices?request=req-1",
	})
	notify := &recordingNotifier{}
	prompt := &recordingPrompter{}

	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, prompt)
	})
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d err=%s", res.ExitCode, res.Error)
	}

	// The dialog is raised on its own goroutine so a handler is never held
	// open waiting on a person.
	deadline := time.Now().Add(2 * time.Second)
	for len(prompt.seen()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	seen := prompt.seen()
	if len(seen) != 1 {
		t.Fatalf("prompt raised %d times, want 1", len(seen))
	}
	if seen[0].title != "Alex Staff wants to connect to your PC" {
		t.Errorf("prompt title = %q", seen[0].title)
	}

	notify.mu.Lock()
	defer notify.mu.Unlock()
	if len(notify.calls) != 0 {
		t.Errorf("a connection request must not also raise a fading balloon, got %d", len(notify.calls))
	}
}

// Everything else stays a balloon. Those are notices, and a dialog demanding
// dismissal for each would train mentors to click through the one that matters.
func TestRunShowNotification_OtherKindsStayBalloons(t *testing.T) {
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "agent_updated", Title: "Updated", Body: "Now on 0.20.1",
	})
	notify := &recordingNotifier{}
	prompt := &recordingPrompter{}

	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, prompt)
	})

	notify.mu.Lock()
	defer notify.mu.Unlock()
	if len(notify.calls) != 1 {
		t.Errorf("expected a balloon, got %d", len(notify.calls))
	}
	if len(prompt.seen()) != 0 {
		t.Errorf("notice kinds must not raise a blocking dialog")
	}
}

// recordingPrompter stands in for the persistent connection-request dialog:
// the real one blocks on a person and needs a window.
type recordingPrompter struct {
	mu     sync.Mutex
	calls  []promptCall
	answer bool
}

type promptCall struct{ title, body string }

func (p *recordingPrompter) Prompt(title, body string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, promptCall{title, body})
	return p.answer
}

func (p *recordingPrompter) seen() []promptCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]promptCall(nil), p.calls...)
}
