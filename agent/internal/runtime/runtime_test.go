package runtime

import (
	"context"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func mustEnvelope(t *testing.T, kind proto.CommandKind) proto.Envelope {
	t.Helper()
	env, err := proto.NewEnvelope(proto.MsgCommand, uuid.Nil, nil, proto.CommandData{Kind: kind})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	return env
}

func TestDispatch_RoutesToMatchingHandler(t *testing.T) {
	called := false
	handlers := map[proto.CommandKind]Handler{
		proto.CmdInventoryRefresh: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			called = true
		},
	}
	Dispatch(context.Background(), nil, handlers, mustEnvelope(t, proto.CmdInventoryRefresh), func(context.Context, proto.Envelope) error { return nil })
	if !called {
		t.Fatal("Dispatch did not invoke the matching handler")
	}
}

// TestDispatch_UnsupportedKindMatchesInlineSwitchBehavior is a parity check
// against cmd/agent/main.go's pre-extraction inline switch: an unmapped kind
// must produce the exact same ExitCode:-1 / "command kind not supported: "
// result shape, since ReadyApp and any operator tooling may match on that
// error string.
func TestDispatch_UnsupportedKindMatchesInlineSwitchBehavior(t *testing.T) {
	var got proto.Envelope
	send := func(_ context.Context, env proto.Envelope) error {
		got = env
		return nil
	}
	env := mustEnvelope(t, proto.CommandKind("future_unknown_kind"))
	Dispatch(context.Background(), nil, map[proto.CommandKind]Handler{}, env, send)

	if got.Type != proto.MsgResult {
		t.Fatalf("result type = %q, want %q", got.Type, proto.MsgResult)
	}
	if got.InReplyTo == nil || *got.InReplyTo != env.ID {
		t.Fatalf("result InReplyTo = %v, want %v", got.InReplyTo, env.ID)
	}
	var result proto.ResultData
	if err := got.DecodeData(&result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	wantErr := "command kind not supported: future_unknown_kind"
	if result.Error != wantErr {
		t.Errorf("Error = %q, want %q", result.Error, wantErr)
	}
}

func TestDispatch_DecodeFailureDoesNotPanicOrSend(t *testing.T) {
	sendCalled := false
	send := func(context.Context, proto.Envelope) error {
		sendCalled = true
		return nil
	}
	// An envelope with no Data at all fails DecodeData, same as a
	// malformed command.
	badEnv := proto.Envelope{ID: uuid.New(), Type: proto.MsgCommand}
	Dispatch(context.Background(), slog.Default(), map[proto.CommandKind]Handler{}, badEnv, send)
	if sendCalled {
		t.Fatal("Dispatch sent a reply for an envelope it could not decode")
	}
}

func TestDispatch_NilLoggerDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Dispatch panicked with a nil logger: %v", r)
		}
	}()
	Dispatch(context.Background(), nil, map[proto.CommandKind]Handler{}, mustEnvelope(t, proto.CmdInventoryRefresh), func(context.Context, proto.Envelope) error { return nil })
}
