//go:build linux || darwin

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
	"path/filepath"
	"strings"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/enroll"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// Install steps shared by the Linux (systemd) and macOS (launchd) installers:
// everything except how the agent is registered and started.

func verifyBundledAgent(path string) error {
	if !verify.ValidLowerHex256(expectedAgentSHA256) {
		return fmt.Errorf("build is not properly pinned (expected_agent_sha256 unset)")
	}
	f, err := os.Open(path)
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
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("bundled agent is not executable")
	}
	return nil
}

func ProtectKeyACL(path string) error {
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod private key: %w", err)
	}
	return nil
}

func enrollLight(ctx context.Context, cfg Config, payload Payload, onPending func()) error {
	var pending func(enroll.Pending)
	if onPending != nil {
		pending = func(enroll.Pending) { onPending() }
	}
	res, err := enroll.Enroll(ctx, enroll.Options{EnrollmentURL: linuxEnrollmentURL(cfg.ServerURL), BearerSecret: payload.EnrollmentToken, MaterialDir: cfg.Paths.Root, OnPending: pending, ExpectInstallProfile: cfg.ExpectInstallProfile})
	if err != nil {
		return err
	}
	c := config.Config{ServerURL: cfg.ServerURL, MachineID: res.MachineID, VenueID: res.VenueID, CertPath: res.CertPath, KeyPath: res.KeyPath, CAPath: res.CAPath, OwnerActorID: res.OwnerActorID}
	if res.AgentListenURL != "" {
		c.ServerURL = res.AgentListenURL
	}
	return config.Save(cfg.Paths.ConfigPath, c)
}

func linuxEnrollmentURL(serverURL string) string {
	rest := serverURL
	for _, prefix := range []string{"wss://", "ws://", "https://", "http://"} {
		if strings.HasPrefix(rest, prefix) {
			rest = strings.TrimPrefix(rest, prefix)
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return "https://" + rest + "/enroll/v1/redeem"
}

func waitForAgentConnected(ctx context.Context, logPath string, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(logPath); err == nil && bytes.Contains(data, []byte(`"msg":"agent connected"`)) {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("agent did not connect within %s", timeout)
		case <-ticker.C:
		}
	}
}

func unlinkOnline(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, path, "unlink").CombinedOutput()
	if err != nil {
		return fmt.Errorf("lightagent unlink: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeInstallDir(root string) error { return os.RemoveAll(root) }

func writeTombstone(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "pending-unlink.tombstone"), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}
