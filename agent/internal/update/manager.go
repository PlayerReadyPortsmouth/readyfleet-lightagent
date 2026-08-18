package update

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	maxDownloadBytes = 200 * 1024 * 1024
	downloadTimeout  = 5 * time.Minute
)

// Config configures a Manager.
type Config struct {
	// AllowedHost, when non-empty, restricts the update URL host.
	AllowedHost string
	// DownloadDir is where the update is staged. Empty = os.TempDir().
	DownloadDir string
	// Policy is this entrypoint's fixed ProductPolicy — see the type doc.
	Policy ProductPolicy
}

// Manager downloads, verifies, and applies a self-update. It is
// single-flight: a second Update while one is applying is rejected.
type Manager struct {
	cfg Config

	verify.SingleFlight

	// ApplyFn performs the entrypoint-specific binary swap + restart of
	// the verified download. There is no default — callers must set it,
	// since "restart" means something different for a Windows service
	// (managed) than a Task-Scheduler process (lite).
	ApplyFn func(downloadedPath, version, signerFingerprint string) error

	// SessionActiveFn, when set, reports whether a SolarBeam session is
	// currently active on this machine. Update refuses to run while it
	// returns true: ApplyFn's restart would kill the process serving that
	// session (the managed agent's own process, or — for the light agent
	// specifically — the engine it launched directly as a child in
	// ModeInteractive), with no coordination on the other end to resume
	// it. Mirrors solarbeam.Manager.Update's own "engine is running"
	// guard, which protects the engine binary itself the same way; this
	// protects the agent binary. Nil means no check (tests that don't
	// care about this leave it unset).
	SessionActiveFn func(ctx context.Context) bool

	httpClient *http.Client
}

// New returns a Manager for cfg. Callers must set the returned Manager's
// ApplyFn before calling Update.
func New(cfg Config) *Manager {
	return &Manager{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: downloadTimeout},
	}
}

// Update handles a self-update: validate args (including this entrypoint's
// ProductPolicy), download+verify against SHA256, then call ApplyFn. On
// success ApplyFn is expected to have applied the update (and, depending
// on the entrypoint, initiated its own restart) before returning.
func (m *Manager) Update(ctx context.Context, args proto.UpdateArgs) error {
	if !m.TryBegin() {
		return errors.New("update already in progress")
	}
	defer m.End()

	if err := m.validate(args); err != nil {
		return err
	}

	if m.SessionActiveFn != nil && m.SessionActiveFn(ctx) {
		return errors.New("update: a SolarBeam session is currently active on this machine (refusing to restart mid-session)")
	}

	path, err := verify.DownloadVerified(ctx, verify.DownloadParams{
		URL:         args.URL,
		SHA256:      args.SHA256,
		DownloadDir: m.cfg.DownloadDir,
		TempPattern: "fleet-agent-update-*.exe",
		MaxBytes:    maxDownloadBytes,
		Client:      m.httpClient,
	})
	if err != nil {
		return err
	}

	if m.ApplyFn == nil {
		_ = os.Remove(path)
		return errors.New("update: no ApplyFn configured")
	}
	// Re-check right before the actual restart: the download above can take
	// a while, and a session may have started since the earlier check.
	if m.SessionActiveFn != nil && m.SessionActiveFn(ctx) {
		_ = os.Remove(path)
		return errors.New("update: a SolarBeam session is currently active on this machine (refusing to restart mid-session)")
	}
	if err := m.ApplyFn(path, args.Version, args.SignerFingerprint); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("apply: %w", err)
	}
	return nil
}

func (m *Manager) validate(a proto.UpdateArgs) error {
	if a.ReleaseID == "" || a.Version == "" || a.URL == "" || a.SHA256 == "" ||
		a.AgentProfile == "" || a.SignerFingerprint == "" || a.ProductID == "" {
		return errors.New("update args: release_id, version, url, sha256, agent_profile, signer_fingerprint and product_id are required")
	}
	if !verify.ValidLowerHex256(a.SHA256) {
		return errors.New("update args: sha256 must be 64 lowercase hexadecimal characters")
	}
	if !verify.ValidLowerHex256(a.SignerFingerprint) {
		return errors.New("update args: signer_fingerprint must be 64 lowercase hexadecimal characters")
	}
	if err := m.cfg.Policy.Validate(a); err != nil {
		return err
	}
	u, err := url.Parse(a.URL)
	if err != nil {
		return fmt.Errorf("update args: bad url: %w", err)
	}
	if m.cfg.AllowedHost != "" && u.Hostname() != m.cfg.AllowedHost {
		return fmt.Errorf("update args: url host %q not allowed", u.Hostname())
	}
	return nil
}
