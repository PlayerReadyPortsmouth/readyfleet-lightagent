package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
)

// fakeRedeemServer stands in for the C2's /enroll/v1/redeem endpoint so
// runLightEnrollment's own HTTP call (built internally by enroll.Enroll,
// which supports no client-injection seam here) can be exercised without
// reaching a real server, while protectKeyACLFn — the thing this test is
// actually about — is faked via its package-level seam so no real Windows
// ACL command ever runs.
func fakeRedeemServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"machine_id": "machine-1",
			"venue_id":   "venue-1",
			"cert_pem":   "-----BEGIN CERTIFICATE-----\ndGVzdA==\n-----END CERTIFICATE-----\n",
			"ca_pem":     "-----BEGIN CERTIFICATE-----\ndGVzdA==\n-----END CERTIFICATE-----\n",
		})
	}))
}

// TestRunLightEnrollment_ProtectsKeyACL is Finding 2's regression: the
// direct-run re-enrolment path (not through light-installer.exe) writes a
// fresh private key via enroll.Enroll and, before this fix, never locked
// it down the way lightinstall.Manager.Install already does for the
// installer flow. It must now call ProtectKeyACL on the freshly written
// key, and — matching Install's own severity for the same failure — abort
// before persisting config.json if that lockdown fails.
func TestRunLightEnrollment_ProtectsKeyACL(t *testing.T) {
	server := fakeRedeemServer(t)
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	newCfg := func() *config.Config {
		return &config.Config{
			EnrollmentURL:    server.URL,
			EnrollmentSecret: "rft_test-secret",
			ServerURL:        "wss://fleet.example.invalid",
		}
	}

	t.Run("success protects the fresh key and persists config", func(t *testing.T) {
		orig := protectKeyACLFn
		defer func() { protectKeyACLFn = orig }()
		var gotKeyPath string
		protectKeyACLFn = func(keyPath string) error {
			gotKeyPath = keyPath
			return nil
		}

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		cfg := newCfg()

		if err := runLightEnrollment(context.Background(), configPath, cfg, logger, false); err != nil {
			t.Fatalf("runLightEnrollment: %v", err)
		}
		if gotKeyPath == "" {
			t.Fatal("ProtectKeyACL (via protectKeyACLFn) was never called")
		}
		if gotKeyPath != cfg.KeyPath {
			t.Fatalf("ProtectKeyACL called with %q, want cfg.KeyPath %q", gotKeyPath, cfg.KeyPath)
		}
		if _, err := os.Stat(configPath); err != nil {
			t.Fatalf("config.json was not persisted on success: %v", err)
		}
	})

	t.Run("ACL failure aborts before persisting config or clearing the secret", func(t *testing.T) {
		orig := protectKeyACLFn
		defer func() { protectKeyACLFn = orig }()
		acErr := errors.New("icacls: access denied")
		protectKeyACLFn = func(keyPath string) error { return acErr }

		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.json")
		cfg := newCfg()

		err := runLightEnrollment(context.Background(), configPath, cfg, logger, false)
		if err == nil {
			t.Fatal("runLightEnrollment must fail when ProtectKeyACL fails")
		}
		if !errors.Is(err, acErr) {
			t.Fatalf("err = %v, want wrapping %v", err, acErr)
		}
		if cfg.EnrollmentSecret == "" {
			t.Fatal("enrollment_secret must not be cleared when the key lockdown aborts enrolment")
		}
		if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
			t.Fatal("config.json must not be persisted when ProtectKeyACL fails")
		}
	})
}
