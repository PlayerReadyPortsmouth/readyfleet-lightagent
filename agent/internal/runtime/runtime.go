// Package runtime is the profile-neutral agent lifecycle shared by every
// agent entrypoint (the managed agent under cmd/agent today; a future
// restricted "lite"/BYOD entrypoint per
// docs/superpowers/plans/2026-07-26-solarbeam-byod-light-agent.md).
//
// It owns exactly two things: dispatching an incoming command envelope to a
// caller-supplied handler map ([Dispatch]), and wiring that dispatch plus
// the caller's terminal/screen/inventory hooks into a [transport.Client]
// and running it ([Run]). Config loading and enrollment stay with each
// entrypoint's main package — they differ enough between profiles (and are
// already well-tested there) that folding them in here would widen this
// package's blast radius for no shared benefit yet. What differs between
// profiles is which managers exist and therefore which command kinds are in
// the Handlers map; this package never itself knows or cares which profile
// is running.
package runtime

import (
	"context"
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Handler runs one command kind. Implementations stream MsgOutput via send
// and must emit a final MsgResult — the same contract transport.CommandHandler
// documents, just keyed by kind instead of being one big switch.
type Handler func(ctx context.Context, env proto.Envelope, send transport.SendFunc)

// Config configures Run. Fields not documented here are passed straight
// through to the transport.Client they configure.
type Config struct {
	ServerURL                 string
	CertFile, KeyFile, CAFile string
	MachineID                 string
	AgentVersion              string
	OS, OSVersion             string
	Insecure                  bool
	Logger                    *slog.Logger
	InventoryProvider         func(ctx context.Context) (proto.InventoryData, error)

	// TelemetryProvider, if non-nil, is passed straight through to
	// transport.Config — see that field's doc for the push-cadence
	// contract. A restricted entrypoint with no telemetry capability
	// simply leaves this nil.
	TelemetryProvider func(ctx context.Context) (proto.TelemetryData, error)

	// Handlers maps each command kind this entrypoint supports to its
	// implementation. A kind absent from this map is reported to the
	// server as unsupported (see [Dispatch]) rather than silently ignored
	// — this is the enforcement seam a restricted entrypoint relies on:
	// it simply never populates the map with anything outside its
	// profile's capability set.
	Handlers map[proto.CommandKind]Handler

	// TerminalHandler, ScreenHandler are optional and passed through to
	// transport.Config unchanged. A restricted entrypoint that has no
	// terminal/screen capability at all simply leaves these nil.
	TerminalHandler transport.TerminalHandler
	ScreenHandler   transport.ScreenHandler
}

// NewClient builds the transport.Client Run would otherwise construct and
// immediately consume internally. Exported so a caller that needs to reach
// past Run's blocking call — e.g. the light agent's tray icon polling
// [transport.Client.Stats] and driving [transport.Client.SetPaused] for its
// disconnect toggle — can hold onto the same Client instance Run ends up
// driving, rather than Run hiding it entirely.
func NewClient(cfg Config) *transport.Client {
	return transport.NewClient(transport.Config{
		ServerURL:         cfg.ServerURL,
		CertFile:          cfg.CertFile,
		KeyFile:           cfg.KeyFile,
		CAFile:            cfg.CAFile,
		MachineID:         cfg.MachineID,
		AgentVersion:      cfg.AgentVersion,
		OS:                cfg.OS,
		OSVersion:         cfg.OSVersion,
		Insecure:          cfg.Insecure,
		Logger:            cfg.Logger,
		InventoryProvider: cfg.InventoryProvider,
		TelemetryProvider: cfg.TelemetryProvider,
		CommandHandler: func(ctx context.Context, env proto.Envelope, send transport.SendFunc) {
			Dispatch(ctx, cfg.Logger, cfg.Handlers, env, send)
		},
		TerminalHandler: cfg.TerminalHandler,
		ScreenHandler:   cfg.ScreenHandler,
	})
}

// Run builds a transport.Client from cfg and runs it until ctx is
// cancelled. It assumes cfg's connection material (certs, machine ID) is
// already valid — callers enroll first, same as today.
func Run(ctx context.Context, cfg Config) error {
	return NewClient(cfg).Run(ctx)
}

// Dispatch decodes env as a command and runs the matching entry in
// handlers, or — for a kind not in the map — replies with the same
// ExitCode:-1 "command kind not supported" result the managed agent's
// inline switch always has. logger may be nil (defaults to slog.Default(),
// matching transport.NewClient's own default).
func Dispatch(ctx context.Context, logger *slog.Logger, handlers map[proto.CommandKind]Handler, env proto.Envelope, send transport.SendFunc) {
	if logger == nil {
		logger = slog.Default()
	}
	var cmd proto.CommandData
	if err := env.DecodeData(&cmd); err != nil {
		logger.Warn("decode command", "err", err)
		return
	}
	if h, ok := handlers[cmd.Kind]; ok {
		h(ctx, env, send)
		return
	}
	logger.Warn("unsupported command kind", "kind", cmd.Kind)
	result, _ := proto.NewEnvelope(proto.MsgResult, env.ID, &env.ID, proto.ResultData{
		ExitCode: -1,
		Error:    "command kind not supported: " + string(cmd.Kind),
	})
	_ = send(ctx, result)
}
