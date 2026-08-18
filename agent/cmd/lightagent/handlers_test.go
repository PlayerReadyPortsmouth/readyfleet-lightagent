package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "connection_request", Title: "Connection request", Body: "Aurora wants to connect",
		Severity: "warning",
	})
	notify := &recordingNotifier{}
	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify)
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
	withURL := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "connection_request", Title: "t", Body: "b", URL: "https://readyapp.player-ready.co.uk/x",
	})
	notify := &recordingNotifier{}
	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), withURL, send, notify)
	})
	notify.mu.Lock()
	gotOnClick := notify.calls[0].onClick
	notify.mu.Unlock()
	if gotOnClick == nil {
		t.Fatal("args.URL set but onClick passed to Notify was nil")
	}

	withoutURL := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "connection_request", Title: "t", Body: "b",
	})
	notify2 := &recordingNotifier{}
	collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), withoutURL, send, notify2)
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
			runShowNotification(context.Background(), env, send, notify)
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
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: "connection_request", Title: "t", Body: "b",
	})
	notify := &recordingNotifier{err: errors.New("trayicon: tray not running")}
	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify)
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
