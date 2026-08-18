//go:build windows

// Windows implementations of the lightinstall seams.
//
// registerLogonStartup/removeLogonStartup were originally a schtasks.exe
// ONLOGON task (/RL LIMITED, i.e. explicitly not elevated) — verified
// live this session to genuinely need administrator elevation to create,
// at least for an Administrators-group account running under UAC's
// filtered non-elevated token (the common case: the first account on a
// personal Windows PC is an admin, and normally runs unelevated day to
// day). That directly contradicts the BYOD design's core promise — no
// admin rights required — so logon startup now uses the standard
// per-user mechanism that every ordinary consumer app relies on for
// exactly this (Spotify, Discord, Dropbox, ...): a
// HKEY_CURRENT_USER\...\Run registry value. Purely per-user state, never
// requires elevation, same "runs once at next logon" semantics as the
// task it replaces.
package lightinstall

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/enroll"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// runKeyPath is HKCU's standard per-user "run at logon" registry key.
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// runValueName is this value's name under runKeyPath. A fixed,
// recognisable name (not per-install-unique) — Install/Uninstall/unlink
// all need to agree on the same identifier, and a mentor inspecting
// Task Manager's Startup tab or msconfig should see something legible.
const runValueName = "ReadyFleet Light Agent"

// verifyBundledAgent checks the bundled lightagent.exe against the
// ldflags-baked SHA-256 + Authenticode signer pin before it's ever
// enrolled or scheduled to run. Refuses outright if either pin is unset
// (an unpinned build must never install anything) — mirrors
// bootstrap.Install's identical refusal.
func verifyBundledAgent(exePath string) error {
	if !verify.ValidLowerHex256(expectedAgentSHA256) || !verify.ValidLowerHex256(expectedSignerFingerprint) {
		return fmt.Errorf("build is not properly signed/pinned (expected_agent_sha256/expected_signer_fingerprint unset)")
	}
	f, err := os.Open(exePath)
	if err != nil {
		return fmt.Errorf("open bundled agent: %w", err)
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, f)
	_ = f.Close()
	if copyErr != nil {
		return fmt.Errorf("hash bundled agent: %w", copyErr)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != expectedAgentSHA256 {
		return fmt.Errorf("bundled agent sha256 mismatch: got %s want %s", got, expectedAgentSHA256)
	}
	if err := verify.VerifyAuthenticode(exePath, expectedSignerFingerprint); err != nil {
		return fmt.Errorf("authenticode: %w", err)
	}
	return nil
}

// protectKeyACL restricts keyPath to the current user only, via
// icacls.exe — the design doc's "current-user ACLs" alternative to DPAPI
// (simpler: no CryptProtectData/CryptUnprotectData binding needed, and
// the file stays a plain PEM readable by config.Load without an extra
// decrypt step). Shelling to icacls.exe matches this codebase's existing
// convention of shelling to system tools (sc.exe, msiexec.exe) rather
// than binding Win32 APIs directly.
func protectKeyACL(keyPath string) error {
	u, err := user.Current()
	if err != nil {
		return fmt.Errorf("resolve current user: %w", err)
	}
	cmd := exec.Command("icacls.exe", keyPath, "/inheritance:r", "/grant:r", u.Username+":F")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("icacls: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// registerLogonStartup writes agentExePath (quoted, since the per-user
// install path contains spaces) into HKCU's Run key so it launches at
// the next logon — current-user, no elevation, no service, matching the
// BYOD design's non-admin requirement exactly (see package doc comment
// for why this replaced a schtasks.exe task).
func registerLogonStartup(agentExePath string) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("open Run key: %w", err)
	}
	defer key.Close()
	if err := key.SetStringValue(runValueName, `"`+agentExePath+`"`); err != nil {
		return fmt.Errorf("set Run value: %w", err)
	}
	return nil
}

// removeLogonStartup deletes the Run value. A missing value (already
// removed, or install never completed registration) is not an error.
func removeLogonStartup() error {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
	if err != nil {
		if err == registry.ErrNotExist {
			return nil
		}
		return fmt.Errorf("open Run key: %w", err)
	}
	defer key.Close()
	if err := key.DeleteValue(runValueName); err != nil && err != registry.ErrNotExist {
		return fmt.Errorf("delete Run value: %w", err)
	}
	return nil
}

// enrollLight redeems payload's token and writes config.json + cert
// material under cfg.Paths.Root. Mirrors agent/cmd/lightagent/main.go's
// runLightEnrollment (which handles re-enrollment on an already-installed
// machine); this is the first-install path.
func enrollLight(ctx context.Context, cfg Config, payload Payload) error {
	res, err := enroll.Enroll(ctx, enroll.Options{
		EnrollmentURL: deriveEnrollmentURL(cfg.ServerURL),
		BearerSecret:  payload.EnrollmentToken,
		MaterialDir:   cfg.Paths.Root,
	})
	if err != nil {
		return err
	}
	c := config.Config{
		ServerURL: cfg.ServerURL,
		MachineID: res.MachineID,
		VenueID:   res.VenueID,
		CertPath:  res.CertPath,
		KeyPath:   res.KeyPath,
		CAPath:    res.CAPath,
	}
	if res.AgentListenURL != "" {
		c.ServerURL = res.AgentListenURL
	}
	return config.Save(cfg.Paths.ConfigPath, c)
}

func deriveEnrollmentURL(serverURL string) string {
	rest := serverURL
	for _, p := range []string{"wss://", "ws://", "https://", "http://"} {
		if strings.HasPrefix(rest, p) {
			rest = rest[len(p):]
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return "https://" + rest + "/enroll/v1/redeem"
}

// connectedLogMarker matches transport/client.go's exact "agent connected"
// log line (JSON-encoded by slog, so this substring always appears intact
// regardless of surrounding fields or their order).
const connectedLogMarker = `"msg":"agent connected"`

// startAgentOnce launches the agent immediately, using the exact same
// command line the Run key launches at logon — so Install's healthcheck
// proves the real startup path works, not a special-cased test invocation.
// Left running afterwards: this becomes the agent's actual first live
// session, not a throwaway probe. A stale already-running instance from a
// prior install is a known, accepted rough edge for this pass — not
// addressed here.
func startAgentOnce(agentExePath string) error {
	cmd := exec.Command(agentExePath)
	cmd.Dir = filepath.Dir(agentExePath)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start agent: %w", err)
	}
	return nil
}

// waitForAgentConnected polls logPath for the "agent connected" line
// transport.Client writes on a successful WSS handshake — the one signal
// that actually proves the machine can reach the C2, as opposed to just
// verifying the enrollment HTTP call worked (a separate hop; a firewall or
// proxy can allow one and block the other).
func waitForAgentConnected(ctx context.Context, logPath string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(logPath); err == nil && bytes.Contains(data, []byte(connectedLogMarker)) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("agent did not report a successful connection within %s (check %s)", timeout, logPath)
		case <-ticker.C:
		}
	}
}

// unlinkOnline shells out to the installed agent's own `unlink`
// subcommand — the agent already knows how to revoke itself server-side
// and remove its local cert material; reusing it here avoids duplicating
// that logic in the installer.
func unlinkOnline(ctx context.Context, agentExePath string) error {
	cmd := exec.CommandContext(ctx, agentExePath, "unlink")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("lightagent unlink: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// removeInstallDir deletes the entire install root.
func removeInstallDir(root string) error {
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove %s: %w", root, err)
	}
	return nil
}

// writeTombstone records that an uninstall happened while unable to reach
// the C2, so a future online moment could still complete the revoke.
// There is no background process reading this file yet — a real
// retry-on-reconnect mechanism is Task 8+ territory; this just makes the
// gap visible on disk instead of silently dropping it.
func writeTombstone(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("ensure %s: %w", root, err)
	}
	path := filepath.Join(root, "pending-unlink.tombstone")
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}
