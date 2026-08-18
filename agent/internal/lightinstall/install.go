package lightinstall

import (
	"context"
	"fmt"
	"time"
)

// connectHealthcheckTimeout bounds how long Install waits for the just-
// started agent to prove it can actually reach the C2, not just that the
// installer's own enrollment HTTP call succeeded (enrollment and the
// agent's ongoing WSS connection are separate hops — a firewall or proxy
// can easily allow one and block the other).
const connectHealthcheckTimeout = 20 * time.Second

// expectedAgentSHA256 and expectedSignerFingerprint pin the bundled
// lightagent.exe. Empty in source; overwritten at build time via
// -ldflags "-X .../lightinstall.expectedAgentSHA256=... -X
// .../lightinstall.expectedSignerFingerprint=..." — mirrors
// agent/internal/bootstrap's identical pattern for the managed installer.
// Install refuses to run unless both are set (see verifyBundledAgent).
var (
	expectedAgentSHA256       string
	expectedSignerFingerprint string
)

// Paths locates the light install's on-disk layout, all under
// %LocalAppData%\ReadyFleet Light — current-user, no admin rights needed.
type Paths struct {
	Root       string
	AgentExe   string
	ConfigPath string
	KeyPath    string
	// LogPath is where the running agent writes its JSON log lines —
	// polled after startup to confirm a real "agent connected" line
	// appears, not just that the process launched.
	LogPath string
}

// Config configures a Manager.
type Config struct {
	Paths Paths
	// ServerURL is baked in at build time (by light-installer's own
	// caller), never taken from the payload — a compromised payload must
	// not be able to redirect enrollment to an attacker-controlled host.
	ServerURL string
}

// Manager installs and uninstalls the light agent. The Windows-specific
// steps (Authenticode/SHA-256 verification, ACL protection, scheduled-task
// registration) are injectable seams so the orchestration itself — order
// of operations, error wrapping, the tombstone-on-offline-uninstall path —
// is unit-testable without touching real Windows APIs. The seams'
// concrete _windows.go implementations are NOT verified in this
// environment (no admin rights, no interactive session) — see
// install_windows.go's doc comment.
type Manager struct {
	cfg Config

	// Progress, if non-nil, is called with a short human-readable label at
	// each Install stage boundary — the seam a UI (light-installer's
	// TaskDialog wizard) hooks to show real, live progress instead of a
	// console window that just sits there. Nil-safe: never called if unset,
	// which is what every existing test and the CLI path both rely on.
	Progress func(step string)

	verifyAgentFn      func(exePath string) error
	protectKeyFn       func(keyPath string) error
	registerTaskFn     func(agentExePath string) error
	removeTaskFn       func() error
	enrollFn           func(ctx context.Context, cfg Config, payload Payload) error
	startAgentFn       func(agentExePath string) error
	waitForConnectedFn func(ctx context.Context, logPath string, timeout time.Duration) error
	unlinkOnlineFn     func(ctx context.Context, agentExePath string) error
	removeAllFn        func(root string) error
	writeTombstoneFn   func(root string) error
}

func (m *Manager) progress(step string) {
	if m.Progress != nil {
		m.Progress(step)
	}
}

// New returns a Manager wired to the real platform implementations.
func New(cfg Config) *Manager {
	m := &Manager{cfg: cfg}
	m.verifyAgentFn = verifyBundledAgent
	m.protectKeyFn = protectKeyACL
	m.registerTaskFn = registerLogonStartup
	m.removeTaskFn = removeLogonStartup
	m.enrollFn = enrollLight
	m.startAgentFn = startAgentOnce
	m.waitForConnectedFn = waitForAgentConnected
	m.unlinkOnlineFn = unlinkOnline
	m.removeAllFn = removeInstallDir
	m.writeTombstoneFn = writeTombstone
	return m
}

// Install verifies the bundled agent, enrolls it (writing config.json +
// cert material), ACL-protects the private key, registers the current-user
// logon startup entry, then starts the agent immediately and waits for it to prove
// a real, live connection to the C2 — rather than leaving that unverified
// until the mentor's next logon. Any failure after enrollment leaves
// enrollment material on disk (unlike the managed installer's
// full-transaction rollback) — a partial light install is safe to retry or
// manually clean up since nothing here holds elevated/shared-machine
// state; flagged as a simplification, not silently matched to bootstrap's
// transactional rigor.
func (m *Manager) Install(ctx context.Context, payload Payload) error {
	m.progress("Verifying the agent's signature and integrity…")
	if err := m.verifyAgentFn(m.cfg.Paths.AgentExe); err != nil {
		return fmt.Errorf("lightinstall: verify agent: %w", err)
	}
	m.progress("Enrolling with ReadyFleet…")
	if err := m.enrollFn(ctx, m.cfg, payload); err != nil {
		return fmt.Errorf("lightinstall: enroll: %w", err)
	}
	m.progress("Securing local credentials…")
	if err := m.protectKeyFn(m.cfg.Paths.KeyPath); err != nil {
		return fmt.Errorf("lightinstall: protect key: %w", err)
	}
	m.progress("Registering the startup entry…")
	if err := m.registerTaskFn(m.cfg.Paths.AgentExe); err != nil {
		return fmt.Errorf("lightinstall: register logon startup: %w", err)
	}
	m.progress("Starting the agent…")
	if err := m.startAgentFn(m.cfg.Paths.AgentExe); err != nil {
		return fmt.Errorf("lightinstall: start agent: %w", err)
	}
	m.progress("Confirming it can reach ReadyFleet…")
	if err := m.waitForConnectedFn(ctx, m.cfg.Paths.LogPath, connectHealthcheckTimeout); err != nil {
		return fmt.Errorf("lightinstall: confirm connection: %w", err)
	}
	return nil
}

// RemoveLogonStartup deletes the current-user logon startup entry, if
// present. Exported for lightagent's own unlink subcommand, which needs
// to clear it (so a signed-out machine stops silently retrying invalid
// credentials at every logon) without constructing a full Manager or
// having an enrollment payload in hand.
func RemoveLogonStartup() error {
	return removeLogonStartup()
}

// Uninstall attempts an online revoke first (best-effort — offline is not
// a hard failure, since a mentor must still be able to uninstall from a
// machine that can't currently reach the C2), removes the logon startup
// entry, then deletes the install directory.
func (m *Manager) Uninstall(ctx context.Context) error {
	if err := m.unlinkOnlineFn(ctx, m.cfg.Paths.AgentExe); err != nil {
		if tombErr := m.writeTombstoneFn(m.cfg.Paths.Root); tombErr != nil {
			return fmt.Errorf("lightinstall: unlink failed (%v) and could not record a pending tombstone: %w", err, tombErr)
		}
	}
	if err := m.removeTaskFn(); err != nil {
		return fmt.Errorf("lightinstall: remove logon startup: %w", err)
	}
	return m.removeAllFn(m.cfg.Paths.Root)
}
