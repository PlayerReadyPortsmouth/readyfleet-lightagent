package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venuepolicy"
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

func TestSendLightInventory_PublishesCurrentSolarbeamState(t *testing.T) {
	original := collectLightInventoryFn
	t.Cleanup(func() { collectLightInventoryFn = original })
	collectLightInventoryFn = func(context.Context, string, *solarbeam.Manager) (proto.LightInventoryData, error) {
		return proto.LightInventoryData{
			OSFamily:         "linux",
			AgentVersion:     "test",
			SolarbeamVersion: "0.8.8",
			SolarbeamStatus:  "running",
		}, nil
	}

	correlationID := uuid.New()
	var got []proto.Envelope
	err := sendLightInventory(context.Background(), correlationID, func(_ context.Context, env proto.Envelope) error {
		got = append(got, env)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("sendLightInventory: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sent %d envelopes, want 1", len(got))
	}
	if got[0].Type != proto.MsgInventory {
		t.Fatalf("type = %q, want %q", got[0].Type, proto.MsgInventory)
	}
	if got[0].InReplyTo == nil || *got[0].InReplyTo != correlationID {
		t.Fatalf("reply id = %v, want %s", got[0].InReplyTo, correlationID)
	}
	var inv proto.LightInventoryData
	if err := got[0].DecodeData(&inv); err != nil {
		t.Fatalf("decode inventory: %v", err)
	}
	if inv.SolarbeamStatus != "running" || inv.SolarbeamVersion != "0.8.8" {
		t.Fatalf("solarbeam inventory = status %q version %q", inv.SolarbeamStatus, inv.SolarbeamVersion)
	}
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
		runShowNotification(context.Background(), env, send, notify, &recordingGate{}, nil)
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
		runShowNotification(context.Background(), withURL, send, notify, &recordingGate{}, nil)
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
		runShowNotification(context.Background(), withoutURL, send, notify2, &recordingGate{}, nil)
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
			runShowNotification(context.Background(), env, send, notify, &recordingGate{}, nil)
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
		runShowNotification(context.Background(), env, send, notify, &recordingGate{}, nil)
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
	prompt := &recordingGate{}

	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, prompt, nil)
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
	prompt := &recordingGate{}

	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, prompt, nil)
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

// recordingGate stands in for the consent decision: the real one talks to
// a desktop notification daemon or a TaskDialog and blocks on a person.
type recordingGate struct {
	mu      sync.Mutex
	calls   []promptCall
	outcome consent.Outcome
}

type promptCall struct{ title, body, requesterID string }

func (p *recordingGate) Decide(_ context.Context, req consent.Request) consent.Outcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, promptCall{req.Title, req.Body, req.RequesterID})
	if p.outcome == "" {
		return consent.OutcomeDeclined
	}
	return p.outcome
}

func (p *recordingGate) seen() []promptCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]promptCall(nil), p.calls...)
}

// TestRunStartSolarbeam_CoreTeamNeedsApproval: on a Core Team venue the
// light agent refuses a start the C2 did not mark approved, and refuses
// every start while the venue policy is unknown, before touching the engine
// (manager is nil here: reaching it would panic).
func TestRunStartSolarbeam_CoreTeamNeedsApproval(t *testing.T) {
	var core venuepolicy.State
	core.Set(&proto.VenuePolicy{RequireConnectionApproval: true, ScreenMonitorLockedOff: true})
	var unknown venuepolicy.State
	cases := map[string]struct {
		policy *venuepolicy.State
		args   proto.SolarbeamStartArgs
	}{
		"core team, unapproved":                   {&core, proto.SolarbeamStartArgs{RequireConnectionApproval: true}},
		"core team, approval without policy flag": {&core, proto.SolarbeamStartArgs{ConnectionApproved: true}},
		"unknown policy":                          {&unknown, proto.SolarbeamStartArgs{RequireConnectionApproval: true, ConnectionApproved: true}},
		"no policy":                               {nil, proto.SolarbeamStartArgs{}},
		// Forged/insufficient owner markers: another requester, the wrong
		// session type, or the marker on a policy-less connection.
		"owner marker, other requester": {&core, proto.SolarbeamStartArgs{RequireConnectionApproval: true, RequesterIsOwner: true, SessionType: proto.SessionTypeOwnDevice, RequesterID: "someone-else"}},
		"owner marker, cyp session":     {&core, proto.SolarbeamStartArgs{RequireConnectionApproval: true, RequesterIsOwner: true, RequesterID: "owner-oid"}},
		"owner marker, unknown policy":  {&unknown, proto.SolarbeamStartArgs{RequireConnectionApproval: true, RequesterIsOwner: true, SessionType: proto.SessionTypeOwnDevice, RequesterID: "owner-oid"}},
		"owner, no C2 marker":           {&core, proto.SolarbeamStartArgs{RequireConnectionApproval: true, SessionType: proto.SessionTypeOwnDevice, RequesterID: "owner-oid"}},
	}
	for name, tc := range cases {
		tc.args.SessionID, tc.args.MachineID, tc.args.BundleToken = "s1", uuid.NewString(), "bt"
		raw, _ := json.Marshal(tc.args)
		env, _ := proto.NewEnvelope(proto.MsgCommand, uuid.New(), nil, proto.CommandData{Kind: proto.CmdStartSolarbeam, Args: raw})
		var got proto.ResultData
		send := func(_ context.Context, e proto.Envelope) error { return e.DecodeData(&got) }
		runStartSolarbeam(context.Background(), env, send, nil, nil, tc.policy, "owner-oid")
		if got.ExitCode == 0 || got.Error != "connection approval required or venue policy unavailable" {
			t.Errorf("%s: result %+v, want refusal", name, got)
		}
	}
}
