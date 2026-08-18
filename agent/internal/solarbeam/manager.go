// Package solarbeam owns SolarBeam engine/bridge lifecycle: starting a
// session, stopping it, and (new) updating the engine binary in place. It
// is the shared core both the managed agent's exec.SolarbeamManager adapter
// and a future restricted "lite"/BYOD entrypoint
// (docs/superpowers/plans/2026-07-26-solarbeam-byod-light-agent.md) call
// into — this package knows nothing about which profile is running or
// about the WS command-envelope protocol; it takes decoded args and
// returns errors, same as any other manager, with the envelope
// decode/validate/emitResult wrapping left to each caller's adapter layer.
//
// Start/Stop/Update are mutex-serialized against each other: an engine
// update swaps the binary on disk, so it must never race a start (which
// could launch a half-written file) or another update.
package solarbeam

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Mode selects how Start/Stop/Status manage the engine process. The zero
// value is ModeManaged, so every existing caller/test that builds a Paths
// without setting Mode keeps today's SYSTEM-service behavior unchanged.
type Mode int

const (
	// ModeManaged (zero value) launches the engine via a SYSTEM service
	// (SunshineService) that follows the console session across logons
	// and captures the secure desktop — the venue/managed-agent behavior.
	// Requires SC_MANAGER_CREATE_SERVICE (administrator).
	ModeManaged Mode = iota
	// ModeInteractive launches the engine directly as a normal child
	// process in the caller's own session — no service, no admin rights,
	// no secure-desktop capture. Correct for the BYOD light agent: the
	// mentor is already interactively logged in, so session-following and
	// secure-desktop capture (built for an unattended venue PC) are
	// neither needed nor obtainable without rights the light agent
	// deliberately doesn't have. Verified live this session: Start()
	// unconditionally calling into the managed (ModeManaged) path from
	// the light agent failed with an admin-rights error trying to
	// register SunshineService — this Mode is the fix.
	ModeInteractive
)

// Paths locates the local SolarBeam binaries and their install root.
type Paths struct {
	// Engine is the engine (sunshine.exe) binary. In ModeManaged it's
	// launched per session by its bundled SunshineService (session 0 →
	// console session, as SYSTEM, in WebRTC bridge mode), which Start
	// installs and restarts; Engine locates both the binary and (via its
	// directory) the bundled sunshinesvc.exe service host. In
	// ModeInteractive it's launched directly as this process's own child.
	Engine string
	// Bridge is the path to the bridge executable launched per session.
	Bridge string
	// Root is the SolarBeam install directory, if the caller has it
	// separately from Engine's directory. Unused by Start/Stop (which
	// derive everything from Engine/Bridge); Update's apply step extracts
	// the downloaded engine package here if set, else derives the install
	// directory from filepath.Dir(Engine) — see engineInstallDir.
	Root string
	// Mode selects the launch strategy — see the Mode type doc.
	Mode Mode
}

// Status reports what this Manager currently knows about the local engine.
// Running is a live query (SCM in ModeManaged, process liveness in
// ModeInteractive), always accurate. EngineVersion is read live from the
// binary's own Win32 file version resource (fileVersionFn) whenever
// possible — accurate regardless of how the binary arrived (Update,
// manual copy, MSI/rollout CmdInstall) or whether this Manager instance
// has ever run Update. Falls back to lastUpdatedVersion (the in-memory
// value set by a successful Update on this instance) only if the file
// itself can't be read, e.g. the engine isn't installed at all.
type Status struct {
	Running       bool
	EngineVersion string
}

// Manager handles SolarBeam session start/stop and engine updates.
type Manager struct {
	paths Paths

	mu sync.Mutex

	// prepareFn validates the engine's preconditions (the engine binary is
	// installed and the bundled SunshineService is installed, installing it
	// if missing) WITHOUT starting the engine, so a machine that cannot
	// host fails before the bridge touches LiveKit. Defaults to
	// prepareEngine; tests swap it.
	prepareFn func() error
	// launchFn launches the bridge with the given session context and
	// blocks until it is listening for the engine. Defaults to
	// launchBridge; tests swap it.
	launchFn func(args proto.SolarbeamStartArgs) error
	// engineFn (re)launches the engine in WebRTC bridge mode for this
	// session, pointed at the bridge's local WS. Runs after launchFn so the
	// bridge is already listening (the engine's WS client does not retry).
	// Defaults to startEngine; tests swap it.
	engineFn func(args proto.SolarbeamStartArgs) error
	// stopFn tears the session down: stops the service-managed engine,
	// kills the agent-launched bridge, and clears bridge.env. Defaults to
	// stopSolarbeam; tests swap it.
	stopFn func() error
	// statusFn reports whether the engine service is currently running.
	// Defaults to engineServiceRunning; tests swap it.
	statusFn func() bool
	// applyUpdateFn performs the platform-specific download+verify+swap for
	// Update. Defaults to applyEngineUpdate; tests swap it.
	applyUpdateFn func(ctx context.Context, paths Paths, args proto.SolarbeamEngineUpdateArgs) error

	// lastUpdatedVersion is set by a successful Update; see Status doc.
	lastUpdatedVersion string

	// interactiveEngineProc tracks the engine process ModeInteractive
	// launched directly (nil in ModeManaged, where the SCM tracks it
	// instead). Guarded by the same mu Start/Stop/Update already share.
	interactiveEngineProc *os.Process
	// fileVersionFn reads the engine binary's own file version resource —
	// real at any time, unlike lastUpdatedVersion. Defaults to
	// engineFileVersion; tests swap it.
	fileVersionFn func(path string) (string, error)
}

// New returns a Manager wired to the real prepare/launch/engine/stop/status
// implementations for paths, chosen by paths.Mode.
func New(paths Paths) *Manager {
	m := &Manager{paths: paths}
	m.launchFn = m.launchBridge
	m.applyUpdateFn = applyEngineUpdate
	m.fileVersionFn = engineFileVersion
	if paths.Mode == ModeInteractive {
		m.prepareFn = m.prepareEngineInteractive
		m.engineFn = m.startEngineInteractive
		m.stopFn = m.stopSolarbeamInteractive
		m.statusFn = m.engineProcessRunning
	} else {
		m.prepareFn = m.prepareEngine
		m.engineFn = m.startEngine
		m.stopFn = m.stopSolarbeam
		m.statusFn = m.engineServiceRunning
	}
	return m
}

// Start prepares the engine, launches the bridge, then launches the engine
// in bridge mode pointed at it — in that order, because the engine's WS
// client does not retry the connect, so the bridge must already be
// listening. Wrapped error prefixes ("engine: ", "bridge launch: ") are
// part of this Manager's contract; callers/tests key off them.
func (m *Manager) Start(ctx context.Context, args proto.SolarbeamStartArgs) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check the engine can host BEFORE launching the bridge, so a machine
	// with no interactive session fails cleanly without leaving a bridge
	// connected to LiveKit.
	if err := m.prepareFn(); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	// Bridge first: it joins LiveKit and starts listening on the local WS
	// the engine connects to.
	if err := m.launchFn(args); err != nil {
		return fmt.Errorf("bridge launch: %w", err)
	}
	// Engine last: (re)launch it in bridge mode pointed at the local WS.
	if err := m.engineFn(args); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}

// Stop tears the session down: stops the service-managed engine, kills the
// agent-launched bridge, and clears bridge.env.
func (m *Manager) Stop(ctx context.Context, args proto.StopSolarbeamArgs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.stopFn(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}

// Status reports whether the engine is currently running and the last
// engine version this Manager applied (see the Status type doc for its
// limits).
func (m *Manager) Status(ctx context.Context) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	version := m.lastUpdatedVersion
	if v, err := m.fileVersionFn(m.paths.Engine); err == nil && v != "" {
		version = v
	}
	return Status{
		Running:       m.statusFn(),
		EngineVersion: version,
	}
}

const solarbeamEngineProductID = "solarbeam-engine"

// Update downloads, verifies, and extracts the engine package — both
// installing it fresh (nothing at paths.Engine yet, the light agent's
// normal starting state on a personally-owned device) and updating an
// existing install go through this same call; see applyEngineUpdate's doc
// comment. Refuses while the engine service is running rather than
// silently stopping it — an active session could be using it, and this
// Manager has no way to know whether stopping it now is safe. Callers that
// need to update a running engine are responsible for stopping the session
// first (Stop) and sequencing accordingly.
func (m *Manager) Update(ctx context.Context, args proto.SolarbeamEngineUpdateArgs) error {
	if err := m.validateUpdateArgs(args); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.statusFn() {
		return fmt.Errorf("solarbeam engine update: engine is running (stop the session first)")
	}
	if err := m.applyUpdateFn(ctx, m.paths, args); err != nil {
		return err
	}
	m.lastUpdatedVersion = args.Version
	return nil
}

func (m *Manager) validateUpdateArgs(a proto.SolarbeamEngineUpdateArgs) error {
	if a.ReleaseID == "" || a.Version == "" || a.URL == "" ||
		a.SHA256 == "" || a.SignerFingerprint == "" || a.ProductID == "" {
		return fmt.Errorf("solarbeam engine update args: release_id, version, url, sha256, signer_fingerprint and product_id are required")
	}
	if !verify.ValidLowerHex256(a.SHA256) {
		return fmt.Errorf("solarbeam engine update args: sha256 must be 64 lowercase hexadecimal characters")
	}
	if !verify.ValidLowerHex256(a.SignerFingerprint) {
		return fmt.Errorf("solarbeam engine update args: signer_fingerprint must be 64 lowercase hexadecimal characters")
	}
	if a.ProductID != solarbeamEngineProductID {
		return fmt.Errorf("solarbeam engine update args: product_id must be %q", solarbeamEngineProductID)
	}
	return nil
}

// engineBinaryExists is a small shared guard the _windows/_other apply
// implementations both use before touching the filesystem.
func engineBinaryExists(path string) error {
	if path == "" {
		return fmt.Errorf("engine path not configured")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("engine not installed at %s: %w", path, err)
	}
	return nil
}
