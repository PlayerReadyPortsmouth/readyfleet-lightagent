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
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venuepolicy"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// notifier is the subset of *trayicon.Tray runShowNotification needs —
// narrowed so tests can fake it without a real tray/window.
type notifier interface {
	Notify(title, body string, severity trayicon.NotifySeverity, onClick func()) error
}

// collectLightInventoryFn is the one collection seam used by both explicit
// refreshes and the state-changing SolarBeam handlers. Tests replace it with a
// privacy-safe fixture; production always uses lightinventory.Collect.
var collectLightInventoryFn = lightinventory.Collect

// lightInventoryProvider is the hello-triggered auto-refresh hook
// (transport.Config.InventoryProvider): when the C2's hello_ack says
// needs_inventory, the transport sends whatever this returns as a
// MsgInventory. It must be the same LightInventoryData the
// CmdInventoryRefresh handler sends, because the C2 strict-decodes every
// MsgInventory from a byod_solarbeam machine into that one type and rejects
// any other shape outright (dispatch: strict-decode BYOD inventory).
//
// Until 9 Nov 2026 this hook returned an empty proto.InventoryData on the
// theory that the strict path "doesn't exist yet". It did: the zero-valued
// "hostname" field made the C2 drop every hello-time inventory with
// `json: unknown field "hostname"`, so a Mac's SolarBeam host report (and its
// agent version, OS and architecture) never landed after a reconnect.
func lightInventoryProvider(agentVersion string, manager *solarbeam.Manager) func(context.Context) (any, error) {
	return func(ctx context.Context) (any, error) {
		return collectLightInventoryFn(ctx, agentVersion, manager)
	}
}

// LightHandlers builds the light entrypoint's complete, exhaustive handler
// map — exactly the narrow command kinds byod_solarbeam's capability
// allowlist (server/internal/capability/policy.go) permits. This IS the
// enforcement seam Task 4's design relies on: there is no handler for any
// other kind because this binary never compiles one in, not because
// something filters requests at runtime.
//
// indicator may be nil — that is the shipped default, and every call site
// below treats nil as "no on-screen indicator on this machine".
//
// policy is the C2's authenticated venue policy for this connection. A nil
// or still-unknown policy refuses every start_solarbeam, exactly as the
// managed agent does (venuepolicy.State.StartAllowed). ownerActorID is the
// enrolled owner (config); a C2 requester_is_owner marker is only honoured
// when the requester matches it.
func LightHandlers(manager *solarbeam.Manager, updater *update.Manager, notify notifier, consentGate gate, indicator *sessionIndicatorDriver, policy *venuepolicy.State, ownerActorID string) map[proto.CommandKind]runtime.Handler {
	return map[proto.CommandKind]runtime.Handler{
		proto.CmdInventoryRefresh: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runInventoryRefresh(ctx, env, send, manager)
		},
		proto.CmdStartSolarbeam: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runStartSolarbeam(ctx, env, send, manager, indicator, policy, ownerActorID)
		},
		proto.CmdSetSolarbeamAvailability: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			runSetSolarbeamAvailability(ctx, env, send, manager)
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
			runShowNotification(ctx, env, send, notify, consentGate, indicator)
		},
	}
}

func runInventoryRefresh(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	if err := sendLightInventory(ctx, cmdEnv.ID, send, manager); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0}, start)
}

// sendLightInventory publishes the current privacy-safe BYOD snapshot. Start
// and stop use it immediately after changing the engine state so ReadyApp does
// not keep showing the last periodic value (for example "stopped" while a
// session is already connected).
func sendLightInventory(ctx context.Context, correlationID uuid.UUID, send transport.SendFunc, manager *solarbeam.Manager) error {
	inv, err := collectLightInventoryFn(ctx, agentVersion, manager)
	if err != nil {
		return fmt.Errorf("inventory collect: %w", err)
	}
	invEnv, err := proto.NewEnvelope(proto.MsgInventory, uuid.Nil, &correlationID, inv)
	if err != nil {
		return fmt.Errorf("build inventory envelope: %w", err)
	}
	if err := send(ctx, invEnv); err != nil {
		return fmt.Errorf("send inventory: %w", err)
	}
	return nil
}

func runStartSolarbeam(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager, indicator *sessionIndicatorDriver, policy *venuepolicy.State, ownerActorID string) {
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
	// Core Team (require_connection_approval) venues: the agent-side half of
	// the C2's per-session approval gate, the same check the managed agent
	// makes. Before the indicator and the engine, so a refused start never
	// streams a frame.
	decision := ""
	if policy != nil {
		decision = policy.StartDecision(args, ownerActorID)
	}
	if decision == "" {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{
			ExitCode: -1, Error: "connection approval required or venue policy unavailable",
		}, start)
		return
	}
	if decision == venuepolicy.StartOwnerSelf || decision == venuepolicy.StartAssigned {
		// No prompt was shown; the session indicator below still runs for
		// the whole session.
		slog.Info("owner connection, no prompt", "decision", decision, "session_id", args.SessionID, "requester_id", args.RequesterID)
	}
	// The one place the require_session_indicator policy is applied.
	// Before the engine is started, not after: a session refused here has
	// never streamed a frame. Returns nil unless the operator set that
	// key — see sessionIndicatorDriver.GateSessionStart.
	if err := indicator.GateSessionStart(); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if err := manager.Start(ctx, args); err != nil {
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	if err := sendLightInventory(ctx, cmdEnv.ID, send, manager); err != nil {
		// The stream is already live. Do not turn a reporting failure into a
		// false launch failure; the next refresh can reconcile it.
		slog.Warn("solarbeam started but live inventory update failed", "error", err)
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
	if err := sendLightInventory(ctx, cmdEnv.ID, send, manager); err != nil {
		// The session is already stopped; keep the command result truthful and
		// let a later refresh repair the displayed state.
		slog.Warn("solarbeam stopped but live inventory update failed", "error", err)
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

func runShowNotification(ctx context.Context, cmdEnv proto.Envelope, send transport.SendFunc, notify notifier, consentGate gate, indicator *sessionIndicatorDriver) {
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
	// A connection request is a question somebody has to answer, so it goes
	// through the consent gate: unattended for this device's own owner, a
	// prompt that waits for anyone else, a decline for every path where the
	// question could not be put and answered. Every other kind stays a
	// plain notice — they are statements, not questions.
	if args.Kind == connectionRequestKind && consentGate != nil {
		showConnectionRequest(consentGate, args, send)
		emitResult(ctx, send, cmdEnv.ID, proto.ResultData{ExitCode: 0, Duration: time.Since(start)}, start)
		return
	}
	// session_started additionally names the viewer for the on-screen
	// bar: for that kind Body IS the display name (see
	// sessionStartedKind). It stays a normal notice as well — the
	// notification is still shown below, unchanged — so a machine with no
	// indicator loses nothing by this.
	if args.Kind == sessionStartedKind && indicator != nil {
		indicator.SetViewer(args.Body)
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

func runSetSolarbeamAvailability(ctx context.Context, env proto.Envelope, send transport.SendFunc, manager *solarbeam.Manager) {
	start := time.Now()
	var args proto.SetSolarbeamAvailabilityArgs
	err := decodeArgs(env, &args, proto.CmdSetSolarbeamAvailability)
	if err == nil {
		err = manager.SetRemoteHostingEnabled(ctx, args.Enabled)
	}
	if err != nil {
		emitResult(ctx, send, env.ID, proto.ResultData{ExitCode: -1, Error: err.Error()}, start)
		return
	}
	emitResult(ctx, send, env.ID, proto.ResultData{ExitCode: 0}, start)
}
