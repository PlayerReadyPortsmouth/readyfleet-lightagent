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
// Preload/Start/Stop/Update are mutex-serialized against each other: an
// engine update swaps the binary on disk, so it must never race a start
// (which could launch a half-written file) or another update.
//
// # Warm engine (ModeManaged only)
//
// On a managed venue PC the engine is kept resident rather than killed and
// respawned per session: Preload brings it up at agent start, Start reuses
// a healthy one instead of restarting it, and Stop leaves it running.
// Together that removes the ~2.8s service spawn and the ~2.0s cold encoder
// probe from every launch. See warmEngineEnabled for why the gate is
// Paths.Mode rather than a feature flag, and SessionActive (sessionactive.go)
// for the interlock change that makes it safe to ship — without it a
// permanently-running engine would block every future agent update and
// engine install on the machine.
//
// ⚠ Deployment order: this half must not reach a venue PC before an engine
// build that resets its input-authorisation gate on WS disconnect.
// Otherwise a warm engine can carry a previous session's gate state into
// the next one.
package solarbeam

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

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
	// StateDir is the existing agent config directory, outside engine updates.
	StateDir string
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
	// DisableWarmEngine turns warm-engine behaviour OFF for this machine.
	//
	// The zero value (false) leaves it ON, which is the shipped default
	// for ModeManaged — warm-engine is the whole point of the 2026-08-20
	// design, so "not configured" must mean "warm". Set true only to
	// revert one venue PC to the pre-warm start/stop lifecycle without a
	// redeploy; the managed agent maps it from config.json's
	// `warm_engine` key (config.Config.WarmEngineEnabled).
	//
	// Ignored in ModeInteractive, which never goes warm regardless — see
	// warmEngineEnabled.
	DisableWarmEngine bool
	// LinkAuthMode is a local per-machine rollout setting: warn (default) or enforce.
	LinkAuthMode string
	// RestrictedBridge launches the Windows managed bridge as a restricted,
	// Low-integrity LocalService process instead of LocalSystem (see
	// bridge_restricted_windows.go). Default false keeps today's behaviour;
	// config.json `solarbeam_restricted_bridge`. Ignored in ModeInteractive and
	// on Linux, where the bridge already runs as the signed-in user.
	RestrictedBridge bool
	// TrustedPublicKeys contains Linux release-signing keys keyed by the
	// lowercase SHA-256 fingerprint of their raw Ed25519 public key. Windows
	// uses Authenticode and leaves this empty.
	TrustedPublicKeys map[string]ed25519.PublicKey
	// DarwinHostEnabled is config.json `solarbeam_host` on a Mac. False (the
	// default) keeps the Mac watch-only: prepare refuses every start with a
	// clear reason and nothing is ever launched, even with the engine files
	// present. Read only by the darwin manager (manager_darwin.go); Windows
	// and Linux agents exist to host and ignore it.
	DarwinHostEnabled bool
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
	CurrentSessionID string
	HostingRevision  string
	HostingEnabled   bool
	HostingError     string
	Running          bool
	EngineVersion    string
}

// Manager handles SolarBeam session start/stop and engine updates.
type Manager struct {
	paths                  Paths
	linkCredential         *linkCredential
	recoveredLinkCleanupFn func() error
	linkCapabilityFn       func(string) ([]byte, error)
	linkEngineMatchesFn    func(Paths) bool
	linkEngineVerified     bool // managed engine restarted after this agent's gate

	mu                    sync.Mutex
	availabilityMu        sync.Mutex
	hostingOff            atomic.Bool
	hostingEpoch          string
	hostingGeneration     uint64
	hostingError          string
	availabilityLoadError error
	disableHostFn         func() error
	// currentSessionID is destructive-stop authority, not viewer telemetry.
	// It is bound only after this Manager successfully starts a session and
	// protected by mu through the stop itself. An agent restart loses this
	// authority; an unauthenticated loopback observation cannot recreate it.
	currentSessionID     string
	lastStoppedSessionID string
	// bridgeGoneCount counts consecutive reaper passes that found no bridge
	// for the bound session (see ReapSession). Guarded by mu.
	bridgeGoneCount int

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
	// preloadFn brings the engine up at agent start so the first session
	// of the day is already warm (warm-engine design §5.1). ModeManaged
	// only; defaults to preloadEngine, tests swap it.
	preloadFn func(machineID string) error
	// restartFn stops (if running) and starts the engine service, the
	// cold path both a non-warm session start and a preload of an
	// unhealthy engine take. Defaults to restartEngineService; tests swap
	// it so no test ever drives the real SCM.
	restartFn func() error
	// stopServiceFn stops the engine service at session end on the
	// non-warm path. Defaults to stopEngineService; tests swap it.
	stopServiceFn func() error
	// healthyFn reports whether the already-running engine is healthy
	// enough to reuse instead of restarting it — warm-engine design §4.3
	// option 1, "liveness only". Defaults to engineHealthy; tests swap it.
	healthyFn func() bool
	// bridgeRunningFn reports whether the agent-launched bridge process
	// exists at all. The outer gate of the viewer-aware SessionActive:
	// with no bridge there is no session, which is the only way to tell
	// "no session" apart from "bridge wedged" (design §5.2.1). Defaults
	// to bridgeProcessRunning; tests swap it.
	bridgeRunningFn func() bool
	// bridgeStatusURL is the bridge's loopback status endpoint. A field
	// rather than a bare constant so tests can point it at an httptest
	// server; production always uses defaultBridgeStatusURL.
	bridgeStatusURL string
	// statusClient probes bridgeStatusURL. Short timeout on purpose — see
	// bridgeViewers.
	statusClient *http.Client

	// TrustedSigners is this entrypoint's own local, hardcoded set of
	// Authenticode signer fingerprints (lowercase hex) Update will ever
	// accept for an engine release — never server-supplied, mirroring
	// update.ProductPolicy.Signers for the agent's own self-update (see
	// that type's doc comment for the gap this closes). It is a dedicated
	// field rather than an embedded ProductPolicy because
	// SolarbeamEngineUpdateArgs — a deliberately narrower payload than
	// UpdateArgs, see its doc comment in proto/messages.go — has no
	// AgentProfile field, so ProductPolicy.Validate's signature doesn't
	// fit. There is no default: callers must set it before Update can
	// ever succeed, same as ApplyFn above. A caller that never calls
	// Update (e.g. the managed agent's exec.SolarbeamManager, which only
	// wires Start/Stop today) can safely leave this nil — validateUpdateArgs
	// then rejects every fingerprint, which is the safe failure mode.
	TrustedSigners map[string]struct{}

	// lastUpdatedVersion is set by a successful Update; see Status doc.
	lastUpdatedVersion string

	// interactiveEngineProc tracks the engine process ModeInteractive
	// launched directly (nil in ModeManaged, where the SCM tracks it
	// instead). Guarded by the same mu Start/Stop/Update already share.
	interactiveEngineProc *os.Process
	interactiveEngineDone <-chan struct{}
	// interactiveBridgeProc is the per-session bridge child in interactive
	// mode. Linux cannot safely kill processes globally by basename (and doing
	// so could terminate another user's bridge), so the BYOD manager owns the
	// exact child it started.
	interactiveBridgeProc *os.Process
	interactiveBridgeDone <-chan struct{}
	// fileVersionFn reads the engine binary's own file version resource —
	// real at any time, unlike lastUpdatedVersion. Defaults to
	// engineFileVersion; tests swap it.
	fileVersionFn func(path string) (string, error)
	// sessionListener is told about every session start/stop (see
	// SetSessionListener). Guarded by mu; nil when nobody is listening.
	sessionListener func()
	// darwin is the macOS host state (manager_darwin.go): the per-session
	// engine supervisor and the last permission fault. Guarded by mu.
	darwin darwinHostState
}

// New returns a Manager wired to the real prepare/launch/engine/stop/status
// implementations for paths, chosen by paths.Mode.
func New(paths Paths) *Manager {
	m := &Manager{paths: paths, hostingEpoch: rand.Text()}
	m.recoveredLinkCleanupFn = m.removeRecoveredLinkAuth
	m.linkCapabilityFn = probeLinkCapability
	m.linkEngineMatchesFn = linkEngineMatches
	m.loadHostingAvailability()
	m.disableHostFn = m.disableConfiguredHost
	m.launchFn = m.launchBridge
	m.applyUpdateFn = applyEngineUpdate
	m.fileVersionFn = engineFileVersion
	m.preloadFn = m.preloadEngine
	m.restartFn = restartEngineService
	m.stopServiceFn = stopEngineService
	m.healthyFn = m.engineHealthy
	m.bridgeRunningFn = m.bridgeProcessRunning
	m.bridgeStatusURL = defaultBridgeStatusURL
	m.statusClient = &http.Client{Timeout: bridgeStatusTimeout}
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

// warmEngineEnabled reports whether this Manager keeps the engine warm
// across sessions.
//
// The gate is Paths.Mode, not a feature flag, because the two agent
// profiles genuinely diverge here (warm-engine design §5.1):
//
//   - ModeManaged — a company-owned venue PC whose whole job is to be
//     streamable. Keeping the engine resident removes the ~2.8s service
//     spawn and the ~2.0s encoder probe from every launch.
//   - ModeInteractive — the BYOD light agent on a mentor's PERSONAL
//     machine. An always-running capture engine is too intrusive a
//     default there, so BYOD keeps today's start-on-session /
//     stop-on-session behaviour and takes its saving from the engine-side
//     persisted encoder-probe cache instead.
//
// Paths.DisableWarmEngine is the per-machine revert lever on top of that
// (config.json `warm_engine: false`), not a second profile.
func (m *Manager) warmEngineEnabled() bool {
	return m.paths.Mode == ModeManaged && !m.paths.DisableWarmEngine
}

// Preload brings the engine up at agent start, before any session
// arrives, so the FIRST launch of the day is already warm — the benefit
// of a warm engine is precisely the launch that would otherwise pay the
// cold start (warm-engine design §5.1).
//
// It writes bridge.env (whose contents are machine-constant: the fixed
// loopback bridge URL plus this machine's id — design §2) and starts the
// engine service if it is not already healthy. The engine then sits in
// its own 2s reconnect loop against 127.0.0.1:9092 until a bridge
// appears.
//
// A no-op unless warm-engine is enabled, so the BYOD light agent and any
// reverted venue PC keep today's behaviour. Errors are returned rather
// than swallowed, but callers should treat a preload failure as a
// warning: the next session start will fall back to the normal
// prepare→launch→restart path.
func (m *Manager) Preload(ctx context.Context, machineID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hostingOff.Load() {
		return m.disableConfiguredHostLocked()
	}
	if !m.warmEngineEnabled() {
		return nil
	}
	if _, err := m.linkAuthMode(); err != nil {
		return err
	}
	if machineID == "" {
		return fmt.Errorf("engine preload: machine id required")
	}

	if err := m.prepareFn(); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if m.hostingOff.Load() {
		return nil
	}
	if err := m.preloadFn(machineID); err != nil {
		return fmt.Errorf("engine preload: %w", err)
	}
	slog.Info("solarbeam: engine preloaded", "phase", "engine_preloaded", "machine_id", machineID)
	return nil
}

// Start prepares the engine, checks both binaries before provisioning a launch
// credential, then starts or reuses the engine. A listening bridge is only a
// liveness hint; provisioned children authenticate before application traffic.
// Wrapped error prefixes ("engine: ", "bridge launch: ") are part of the contract.
func (m *Manager) Start(ctx context.Context, args proto.SolarbeamStartArgs) (err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.hostingOff.Load() {
		return ErrHostingDisabled
	}
	if _, err := m.linkAuthMode(); err != nil {
		return err
	}
	if args.SessionID == "" {
		return fmt.Errorf("engine: session id required")
	}

	slog.Info("solarbeam: launch dispatch received", "phase", "start_received", "session_id", args.SessionID, "machine_id", args.MachineID)

	// Check the engine can host BEFORE launching the bridge, so a machine
	// with no interactive session fails cleanly without leaving a bridge
	// connected to LiveKit.
	if err := m.prepareFn(); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	if m.hostingOff.Load() {
		return ErrHostingDisabled
	}
	previousSessionID := m.currentSessionID
	if previousSessionID == "" {
		previousSessionID = m.loadSessionRecord()
	}
	// launchFn may replace the old bridge even when it subsequently fails.
	// From this point, an old queued stop must not touch the new/partial
	// launch. A preparation failure above leaves the old session unchanged.
	m.currentSessionID = ""
	m.bridgeGoneCount = 0
	m.hostingGeneration++
	// Record ownership BEFORE the bridge exists, so an agent killed at any
	// point from here on can still honour the matching stop after it
	// restarts (sessionowner.go).
	m.saveSessionRecordLocked(args.SessionID)
	defer func() {
		m.forgetLinkEnvironment()
		if err != nil {
			if errors.Is(err, errPreviousBridgeStillRunning) {
				m.currentSessionID = previousSessionID
				if previousSessionID != "" {
					m.saveSessionRecordLocked(previousSessionID)
				} else {
					m.clearSessionRecordLocked()
				}
				return
			}
			if m.linkCredential != nil {
				if stopErr := m.stopFn(); stopErr != nil {
					m.currentSessionID = args.SessionID
					err = fmt.Errorf("%w; session cleanup requires retry", err)
					slog.Error("solarbeam: failed launch cleanup failed; session ownership retained")
					return
				}
				if cleanupErr := m.cleanupLinkAuth(); cleanupErr != nil {
					m.currentSessionID = args.SessionID
					err = fmt.Errorf("%w; credential cleanup requires retry", err)
					slog.Error("solarbeam: link credential cleanup failed; session ownership retained")
					return
				}
			}
			m.currentSessionID = ""
			m.clearSessionRecordLocked()
		}
	}()
	// Bridge first: it joins LiveKit and starts listening on the local WS
	// the engine connects to.
	if err := m.launchFn(args); err != nil {
		return fmt.Errorf("bridge launch: %w", err)
	}
	if m.hostingOff.Load() {
		return ErrHostingDisabled
	}
	// Engine last: (re)launch it in bridge mode pointed at the local WS.
	if err := m.engineFn(args); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	m.currentSessionID = args.SessionID
	m.hostingGeneration++
	m.lastStoppedSessionID = ""
	if m.hostingOff.Load() {
		return ErrHostingDisabled
	}
	m.notifySessionChangeLocked()
	slog.Info("solarbeam: launch complete", "phase", "start_complete", "session_id", args.SessionID)
	return nil
}

// Stop tears down only the session successfully started by this Manager.
// Stale queued commands and unknown ownership after restart fail closed. The
// platform cleanup routines remain available to their existing local callers.
func (m *Manager) Stop(ctx context.Context, args proto.StopSolarbeamArgs) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked(ctx, args)
}

// StopIfCurrent binds a local reviewed disconnect to the displayed generation,
// including same-session relaunches that the session ID alone cannot distinguish.
func (m *Manager) StopIfCurrent(ctx context.Context, args proto.StopSolarbeamArgs, expectedRevision string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedRevision == "" || expectedRevision != m.hostingRevisionLocked() {
		return fmt.Errorf("hosting changed; review the current session again")
	}
	return m.stopLocked(ctx, args)
}

func (m *Manager) stopLocked(ctx context.Context, args proto.StopSolarbeamArgs) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if args.SessionID == "" {
		return fmt.Errorf("stop: session id required")
	}
	// After an agent restart the binding lives only in the session record;
	// adopt it (if its bridge is still running) before judging the stop.
	m.recoverSessionLocked()
	if m.currentSessionID == "" {
		if m.lastStoppedSessionID == args.SessionID {
			return nil
		}
		return fmt.Errorf("stop: current session identity unknown; refusing destructive cleanup")
	}
	if args.SessionID != m.currentSessionID {
		return fmt.Errorf("stop: requested session does not match current session")
	}
	if args.RequireEmpty {
		if err := m.confirmEmptyForExpiry(ctx, args.SessionID); err != nil {
			return fmt.Errorf("stop: automatic expiry refused: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	m.hostingGeneration++
	if err := m.stopFn(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	if err := m.cleanupLinkAuth(); err != nil {
		return err
	}
	m.currentSessionID = ""
	m.lastStoppedSessionID = args.SessionID
	m.clearSessionRecordLocked()
	m.notifySessionChangeLocked()
	return nil
}

// Called only while mu is held and destructive session ownership matched.
// The bridge supplies monotonic continuous-empty evidence; a fresh zero count
// alone cannot detect a viewer who joined and left while this command queued.
// This is the final host check immediately before cleanup, not an atomic
// LiveKit membership lock: admission can still race the following stopFn call.
func (m *Manager) confirmEmptyForExpiry(ctx context.Context, sessionID string) error {
	if !m.bridgeRunningFn() {
		return nil
	}
	st, err := m.readBridgeStatus(ctx)
	if err != nil {
		return fmt.Errorf("fresh bridge status unavailable: %w", err)
	}
	if st.SessionID != sessionID {
		return fmt.Errorf("bridge session identity differs")
	}
	if !st.LiveKitConnected || st.PresenceKnown == nil || !*st.PresenceKnown {
		return fmt.Errorf("bridge viewer presence is not confirmed")
	}
	if st.Viewers == nil || *st.Viewers != 0 || len(st.Remote) != 0 {
		return fmt.Errorf("a remote viewer is present or membership is inconsistent")
	}
	if st.ConfirmedEmptySeconds == nil || *st.ConfirmedEmptySeconds < 900 {
		return fmt.Errorf("fifteen continuous empty minutes are not confirmed")
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
		CurrentSessionID: m.currentSessionID,
		HostingRevision:  m.hostingRevisionLocked(),
		HostingEnabled:   !m.hostingOff.Load(),
		HostingError:     m.hostingError,
		Running:          m.statusFn(),
		EngineVersion:    version,
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
	m.linkEngineVerified = false
	if err := m.applyUpdateFn(ctx, m.paths, args); err != nil {
		return err
	}
	m.lastUpdatedVersion = args.Version
	if m.hostingOff.Load() {
		return m.disableConfiguredHostLocked()
	}
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
	// Hex-shape alone only proves the server sent something that LOOKS
	// like a fingerprint, not that it's one this agent has ever agreed to
	// trust — a compromised or confused C2 could otherwise name any
	// signer it likes and applyUpdateFn (VerifyAuthenticode) would happily
	// accept a binary carrying that signature. TrustedSigners is this
	// entrypoint's own local allowlist, checked before applyUpdateFn ever
	// runs, exactly like update.ProductPolicy.Validate gates self-update.
	if _, ok := m.TrustedSigners[a.SignerFingerprint]; !ok {
		return fmt.Errorf("solarbeam engine update args: signer_fingerprint is not in the trusted set")
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
