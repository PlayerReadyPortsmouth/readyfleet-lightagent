package main

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinventory"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/runtime"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/update"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// notifier is the subset of *trayicon.Tray runShowNotification needs —
// narrowed so tests can fake it without a real tray/window.
type notifier interface {
	Notify(title, body string, severity trayicon.NotifySeverity, onClick func()) error
}

// LightHandlers builds the light entrypoint's complete, exhaustive handler
// map — exactly the six command kinds byod_solarbeam's capability
// allowlist (server/internal/capability/policy.go) permits. This IS the
// enforcement seam Task 4's design relies on: there is no handler for any
// other kind because this binary never compiles one in, not because
// something filters requests at runtime.
func LightHandlers(manager *solarbeam.Manager, updater *update.Manager, notify notifier, prompt prompter) map[proto.CommandKind]runtime.Handler {
	return map[proto.CommandKind]runtime.Handler{
		proto.CmdInventoryRefresh: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runInventoryRefresh(ctx, env, send, manager)
		},
		proto.CmdStartSolarbeam: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runStartSolarbeam(ctx, env, send, manager)
		},
		proto.CmdStopSolarbeam: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runStopSolarbeam(ctx, env, send, manager)
		},
		proto.CmdSolarbeamEngineUpdate: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runSolarbeamEngineUpdate(ctx, env, send, manager)
		},
		proto.CmdUpdate: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runSelfUpdate(ctx, env, send, updater)
		},
		proto.CmdShowNotification: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runShowNotification(ctx, env, send, notify, prompt)
		},
	}
}

func runInventoryRefresh(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	inv, err := lightinventory.Collect(ctx, agentVersion, manager)
	if err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: "inventory collect: " + err.Error()}, start)
		return
	}
	invEnv, err := proto.NewEnvelope(proto.MsgInventory, uuid.Nil, &cmdEnv.ID, inv)
	if err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: "build inventory envelope: " + err.Error()}, start)
		return
	}
	if err := send(ctx, invEnv); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: "send inventory: " + err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0}, start)
}

func runStartSolarbeam(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	var args proto.SolarbeamStartArgs
	if err := decodeArgs(cmdEnv, &args, proto.CmdStartSolarbeam); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if args.SessionID == "" || args.MachineID == "" || args.BundleToken == "" {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{
			ExitCode: -1, Error: "start_solarbeam args: session_id, machine_id and bundle_token are required",
		}, start)
		return
	}
	if err := manager.Start(ctx, args); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
}

func runStopSolarbeam(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	var args proto.StopSolarbeamArgs
	if err := decodeArgs(cmdEnv, &args, proto.CmdStopSolarbeam); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if args.SessionID == "" {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{
			ExitCode: -1, Error: "stop_solarbeam args: session_id is required",
		}, start)
		return
	}
	if err := manager.Stop(ctx, args); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
}

func runSolarbeamEngineUpdate(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	var args proto.SolarbeamEngineUpdateArgs
	if err := decodeArgs(cmdEnv, &args, proto.CmdSolarbeamEngineUpdate); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if err := manager.Update(ctx, args); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
}

// severityFromWire maps proto.ShowNotificationArgs.Severity to a
// trayicon.NotifySeverity, defaulting unrecognised/empty values to Info —
// see ShowNotificationArgs's doc comment for why this fails open rather
// than rejecting the command.
func severityFromWire(s string) trayicon.NotifySeverity {
	switch s {
	case "warning":
		return trayicon.NotifyWarning
	case "error":
		return trayicon.NotifyError
	default:
		return trayicon.NotifyInfo
	}
}

func runShowNotification(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, notify notifier, prompt prompter) {
	start := time.Now()
	var args proto.ShowNotificationArgs
	if err := decodeArgs(cmdEnv, &args, proto.CmdShowNotification); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if args.Kind == "" || args.Title == "" || args.Body == "" {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{
			ExitCode: -1, Error: "show_notification args: kind, title and body are required",
		}, start)
		return
	}
	// A connection request is a question the mentor has to answer, so it gets
	// a dialog that waits for them rather than a balloon that fades. Every
	// other kind stays a balloon: they are notices, not questions.
	if args.Kind == connectionRequestKind && prompt != nil {
		showConnectionRequest(prompt, args.Title, args.Body, args.URL)
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
		return
	}

	var onClick func()
	if args.URL != "" {
		url := args.URL // capture: args is this call's own local, but be explicit
		onClick = func() {
			if err := trayicon.OpenURL(url); err != nil {
				slog.Default().Warn("notification click: open url", "err", err)
			}
		}
	}
	if err := notify.Notify(args.Title, args.Body, severityFromWire(args.Severity), onClick); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
}

func runSelfUpdate(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, updater *update.Manager) {
	start := time.Now()
	var args proto.UpdateArgs
	if err := decodeArgs(cmdEnv, &args, proto.CmdUpdate); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if err := updater.Update(ctx, args); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
}

// decodeArgs and emitResult are small, package-local copies of exec's
// helpers of the same name (agent/internal/exec/decode.go, shell.go) —
// this package must not import exec (see main.go's boundary doc comment),
// and these are ~15 lines total, simple enough that duplicating them is
// the proportionate choice over a new shared package.
func decodeArgs[T any](env proto.Envelope, out *T, want ...proto.CommandKind) error {
	var cmd proto.CommandData
	if err := env.DecodeData(&cmd); err != nil {
		return fmt.Errorf("decode command: %w", err)
	}
	if len(want) > 0 && !slices.Contains(want, cmd.Kind) {
		return fmt.Errorf("expected kind %v, got %q", want, cmd.Kind)
	}
	if err := cmd.DecodeArgs(out); err != nil {
		return fmt.Errorf("decode %s args: %w", cmd.Kind, err)
	}
	return nil
}

func emitResult(ctx context.Context, send transport.SendFunc, parentID uuid.UUID, data proto.ResultData, start time.Time) {
	if data.Duration == 0 {
		data.Duration = time.Since(start)
	}
	env, err := proto.NewEnvelope(proto.MsgResult, uuid.Nil, &parentID, data)
	if err != nil {
		return
	}
	_ = send(ctx, env)
}
