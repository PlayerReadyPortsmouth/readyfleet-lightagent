package staffmessage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// SendFunc is the transport callback (transport.SendFunc's shape).
type SendFunc func(ctx context.Context, env proto.Envelope) error

// Deliverer is the Broker seam Run uses.
type Deliverer interface {
	Deliver(ctx context.Context, msg Message) (Ack, error)
}

// Run is the managed agent's CmdShowNotification handler. deliverer may be
// nil (no companion support in this build) and then every valid message is
// no_session.
func Run(ctx context.Context, env proto.Envelope, send SendFunc, deliverer Deliverer) {
	start := time.Now()
	rep := Handle(ctx, env, deliverer)
	if out, err := json.Marshal(rep); err == nil {
		if e, err := proto.NewEnvelope(proto.MsgOutput, uuid.Nil, &env.ID, proto.OutputData{
			Stream: proto.StreamStdout, Chunk: string(out) + "\n",
		}); err == nil {
			_ = send(ctx, e)
		}
	}
	result := proto.ResultData{Duration: time.Since(start)}
	if rep.Code != proto.NotifyDisplayed {
		result.ExitCode = 1
		result.Error = rep.Code + ": " + rep.Detail
	}
	if e, err := proto.NewEnvelope(proto.MsgResult, uuid.Nil, &env.ID, result); err == nil {
		_ = send(ctx, e)
	}
}

// Handle decides the report for one command. Split from Run for tests.
func Handle(ctx context.Context, env proto.Envelope, deliverer Deliverer) proto.ShowNotificationReport {
	var cmd proto.CommandData
	if err := env.DecodeData(&cmd); err != nil {
		return report(proto.NotifyInvalid, "decode command: "+err.Error())
	}
	if cmd.Kind != proto.CmdShowNotification {
		return report(proto.NotifyInvalid, fmt.Sprintf("expected %s, got %q", proto.CmdShowNotification, cmd.Kind))
	}
	var args proto.ShowNotificationArgs
	if err := cmd.DecodeArgs(&args); err != nil {
		return report(proto.NotifyInvalid, "decode show_notification args: "+err.Error())
	}
	msg, f := Validate(args)
	if f != nil {
		return report(f.Code, f.Detail)
	}
	if deliverer == nil {
		return report(proto.NotifyNoSession, ErrNoCompanion.Error())
	}
	ack, err := deliverer.Deliver(ctx, msg)
	switch {
	case err == nil:
	case errors.Is(err, ErrNoCompanion), errors.Is(err, ErrNotClaimed):
		return report(proto.NotifyNoSession, err.Error())
	case errors.Is(err, ErrBusy), errors.Is(err, ErrNoAck):
		return report(proto.NotifyNotDisplayed, err.Error())
	default:
		return report(proto.NotifyNotDisplayed, "delivery interrupted: "+err.Error())
	}
	if ack.Displayed {
		return proto.ShowNotificationReport{Code: proto.NotifyDisplayed, Via: ack.Via}
	}
	switch ack.Reason {
	case ReasonLocked:
		return report(proto.NotifyNoSession, "the PC's screen is locked")
	case ReasonSessionChanged:
		return report(proto.NotifyNoSession, "the signed-in session changed before the message was shown")
	case ReasonInvalid:
		return report(proto.NotifyNotDisplayed, "the venue companion rejected the message text")
	default:
		return report(proto.NotifyNotDisplayed, "the venue companion could not show the message")
	}
}

func report(code, detail string) proto.ShowNotificationReport {
	return proto.ShowNotificationReport{Code: code, Detail: detail}
}

// RunConnectionRequest is only selected for an authenticated approval-required venue.
// The service transports the question; the interactive companion runs the BYOD gate.
func RunConnectionRequest(ctx context.Context, env proto.Envelope, send SendFunc, deliverer Deliverer) {
	var cmd proto.CommandData
	var args proto.ShowNotificationArgs
	result := proto.ResultData{ExitCode: 1, Error: "connection request refused"}
	if env.DecodeData(&cmd) == nil && json.Unmarshal(cmd.Args, &args) == nil {
		if msg, f := ConnectionMessage(args); f == nil && deliverer != nil {
			if ack, err := deliverer.Deliver(ctx, msg); err == nil && ack.Displayed {
				result = proto.ResultData{}
			}
		}
	}
	if e, err := proto.NewEnvelope(proto.MsgResult, uuid.Nil, &env.ID, result); err == nil {
		_ = send(ctx, e)
	}
}
