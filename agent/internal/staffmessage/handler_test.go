package staffmessage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

type fakeDeliverer struct {
	ack   Ack
	err   error
	calls []Message
}

func (f *fakeDeliverer) Deliver(_ context.Context, m Message) (Ack, error) {
	f.calls = append(f.calls, m)
	return f.ack, f.err
}

func command(t *testing.T, kind proto.CommandKind, args any) proto.Envelope {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.NewEnvelope(proto.MsgCommand, uuid.New(), nil, proto.CommandData{Kind: kind, Args: raw})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

var validArgs = proto.ShowNotificationArgs{Kind: "staff_message", Title: "Hello", Body: "Five minutes left", Severity: "info"}

func TestHandleOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		args      proto.ShowNotificationArgs
		deliverer Deliverer
		wantCode  string
		wantCalls int
	}{
		{"displayed", validArgs, &fakeDeliverer{ack: Ack{Displayed: true, Via: "banner+tray"}}, proto.NotifyDisplayed, 1},
		{"no companion", validArgs, &fakeDeliverer{err: ErrNoCompanion}, proto.NotifyNoSession, 1},
		{"nil deliverer is no session", validArgs, nil, proto.NotifyNoSession, 0},
		{"not collected", validArgs, &fakeDeliverer{err: ErrNotClaimed}, proto.NotifyNoSession, 1},
		{"locked", validArgs, &fakeDeliverer{ack: Ack{Reason: ReasonLocked}}, proto.NotifyNoSession, 1},
		{"session changed", validArgs, &fakeDeliverer{ack: Ack{Reason: ReasonSessionChanged}}, proto.NotifyNoSession, 1},
		{"no ack", validArgs, &fakeDeliverer{err: ErrNoAck}, proto.NotifyNotDisplayed, 1},
		{"busy", validArgs, &fakeDeliverer{err: ErrBusy}, proto.NotifyNotDisplayed, 1},
		{"window failed", validArgs, &fakeDeliverer{ack: Ack{Reason: ReasonDisplayFailed}}, proto.NotifyNotDisplayed, 1},
		{"interrupted", validArgs, &fakeDeliverer{err: context.Canceled}, proto.NotifyNotDisplayed, 1},
		{"connection request refused before delivery", proto.ShowNotificationArgs{Kind: "connection_request", Title: "t", Body: "b"}, &fakeDeliverer{}, proto.NotifyRefused, 0},
		{"url refused before delivery", proto.ShowNotificationArgs{Kind: "k", Title: "t", Body: "b", URL: "https://example.com"}, &fakeDeliverer{}, proto.NotifyRefused, 0},
		{"invalid before delivery", proto.ShowNotificationArgs{Kind: "k", Title: strings.Repeat("x", 81), Body: "b"}, &fakeDeliverer{}, proto.NotifyInvalid, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := Handle(context.Background(), command(t, proto.CmdShowNotification, tc.args), tc.deliverer)
			if rep.Code != tc.wantCode {
				t.Fatalf("code = %q (%s), want %q", rep.Code, rep.Detail, tc.wantCode)
			}
			if f, ok := tc.deliverer.(*fakeDeliverer); ok && len(f.calls) != tc.wantCalls {
				t.Fatalf("deliveries = %d, want %d", len(f.calls), tc.wantCalls)
			}
		})
	}
}

func TestHandleWrongKind(t *testing.T) {
	rep := Handle(context.Background(), command(t, proto.CommandKind("shell"), validArgs), &fakeDeliverer{})
	if rep.Code != proto.NotifyInvalid {
		t.Fatalf("code = %q", rep.Code)
	}
}

// TestRunReportsOnStdoutAndPrefixesError pins the contract ReadyApp maps:
// stdout JSON report, exit code 0 only for displayed, and Error "<code>: …".
func TestRunReportsOnStdoutAndPrefixesError(t *testing.T) {
	for _, tc := range []struct {
		d        Deliverer
		wantCode string
		wantExit int
	}{
		{&fakeDeliverer{ack: Ack{Displayed: true, Via: "banner"}}, proto.NotifyDisplayed, 0},
		{&fakeDeliverer{err: ErrNoCompanion}, proto.NotifyNoSession, 1},
	} {
		var sent []proto.Envelope
		env := command(t, proto.CmdShowNotification, validArgs)
		Run(context.Background(), env, func(_ context.Context, e proto.Envelope) error { sent = append(sent, e); return nil }, tc.d)
		if len(sent) != 2 || sent[0].Type != proto.MsgOutput || sent[1].Type != proto.MsgResult {
			t.Fatalf("sent %d envelopes", len(sent))
		}
		for _, e := range sent {
			if e.InReplyTo == nil || *e.InReplyTo != env.ID {
				t.Fatal("envelope not in reply to the command")
			}
		}
		var out proto.OutputData
		if err := sent[0].DecodeData(&out); err != nil {
			t.Fatal(err)
		}
		var rep proto.ShowNotificationReport
		if err := json.Unmarshal([]byte(out.Chunk), &rep); err != nil || rep.Code != tc.wantCode {
			t.Fatalf("stdout report = %q (%v)", out.Chunk, err)
		}
		var res proto.ResultData
		if err := sent[1].DecodeData(&res); err != nil {
			t.Fatal(err)
		}
		if res.ExitCode != tc.wantExit {
			t.Fatalf("exit = %d, want %d", res.ExitCode, tc.wantExit)
		}
		if tc.wantExit == 0 && res.Error != "" {
			t.Fatalf("success carried error %q", res.Error)
		}
		if tc.wantExit != 0 && !strings.HasPrefix(res.Error, tc.wantCode+": ") {
			t.Fatalf("error = %q, want prefix %q", res.Error, tc.wantCode)
		}
	}
}

// TestEndToEndWithBrokerAndFakeCompanion drives Run through a real Broker
// with a fake companion that collects and acks.
func TestEndToEndWithBrokerAndFakeCompanion(t *testing.T) {
	b := fastBroker()
	b.AckTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			m, ok := b.Next(ctx)
			if ok {
				b.Acknowledge(Ack{ID: m.ID, Displayed: true, Via: "banner"})
			}
		}
	}()
	waitPresent(t, b)
	rep := Handle(ctx, command(t, proto.CmdShowNotification, validArgs), b)
	if rep.Code != proto.NotifyDisplayed || rep.Via != "banner" {
		t.Fatalf("report = %+v", rep)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("context ended early")
	}
}
