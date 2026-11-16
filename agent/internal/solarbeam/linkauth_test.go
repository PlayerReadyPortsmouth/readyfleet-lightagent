package solarbeam

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func supportLinkAuthForTest(m *Manager) {
	m.linkEngineMatchesFn = func(Paths) bool { return true }
	if m.paths.Engine == "" {
		m.paths.Engine = "engine"
	}
	if m.paths.Bridge == "" {
		m.paths.Bridge = "bridge"
	}
	m.linkCapabilityFn = func(path string) ([]byte, error) {
		if path == m.paths.Engine {
			return []byte(linkCapabilityPrefix + "engine provisioned-v1\n"), nil
		}
		if path == m.paths.Bridge {
			return []byte(linkCapabilityPrefix + "bridge provisioned-v1\n"), nil
		}
		return nil, errors.New("unexpected binary path")
	}
	m.recoveredLinkCleanupFn = func() error { return nil }
}

func TestLinkAuthMissingBinariesRemainLegacy(t *testing.T) {
	m := New(Paths{Engine: "missing-engine", Bridge: "missing-bridge", LinkAuthMode: "warn"})
	m.recoveredLinkCleanupFn = func() error { return nil }
	if err := m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
		t.Fatal("must not provision before both installed binaries report support")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.linkAuthEnvironment([]string{"PATH=safe", "SOLARBEAM_LINK_AUTH_MODE=enforce", "SOLARBEAM_LINK_SECRET=stale"}); !reflect.DeepEqual(got, []string{"PATH=safe"}) {
		t.Fatalf("legacy environment = %v", got)
	}
}

func TestLinkAuthLaunchSharesRotatesAndForgetsCredential(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	m := New(Paths{LinkAuthMode: "enforce"})
	supportLinkAuthForTest(m)
	m.prepareFn = func() error { return nil }
	var secrets []string
	var bridgeEnv []string
	cleaned := 0
	m.launchFn = func(proto.SolarbeamStartArgs) error {
		if err := m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
			if raw, err := hex.DecodeString(string(secret)); err != nil || len(raw) != 32 {
				t.Fatal("invalid random credential")
			}
			secrets = append(secrets, string(secret))
			return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET=" + string(secret)}, cleanup: func() error { cleaned++; return nil }}, nil
		}); err != nil {
			return err
		}
		bridgeEnv = m.linkAuthEnvironment([]string{"PATH=safe", "SOLARBEAM_LINK_SECRET=stale", "solarbeam_link_secret_file=stale", "SOLARBEAM_LINK_AUTH_MODE=off"})
		return nil
	}
	m.engineFn = func(proto.SolarbeamStartArgs) error {
		if engineEnv := m.linkAuthEnvironment([]string{"PATH=safe"}); !reflect.DeepEqual(bridgeEnv, engineEnv) {
			t.Fatalf("children received different environments")
		}
		return nil
	}
	m.stopFn = func() error { return nil }
	args := proto.SolarbeamStartArgs{SessionID: "same-session", MachineID: "machine"}
	for range 2 {
		if err := m.Start(context.Background(), args); err != nil {
			t.Fatal(err)
		}
		if len(m.linkCredential.env) != 0 {
			t.Fatal("launch credential retained after children started")
		}
	}
	if secrets[0] == secrets[1] {
		t.Fatal("same-session relaunch reused credential")
	}
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: args.SessionID}); err != nil {
		t.Fatal(err)
	}
	if cleaned != 2 || m.linkCredential != nil {
		t.Fatal("launch generations not cleaned")
	}
	for _, secret := range secrets {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("secret in logs")
		}
	}
}

func TestLinkAuthFailureCleanup(t *testing.T) {
	for _, stage := range []string{"bridge", "engine"} {
		t.Run(stage, func(t *testing.T) {
			m := New(Paths{})
			supportLinkAuthForTest(m)
			m.prepareFn = func() error { return nil }
			cleaned, stopped := false, false
			m.launchFn = func(proto.SolarbeamStartArgs) error {
				if err := m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
					return &linkCredential{cleanup: func() error { cleaned = true; return nil }}, nil
				}); err != nil {
					return err
				}
				if stage == "bridge" {
					return errors.New("spawn failed")
				}
				return nil
			}
			m.engineFn = func(proto.SolarbeamStartArgs) error { return errors.New("engine failed") }
			m.stopFn = func() error { stopped = true; return nil }
			if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "s"}); err == nil {
				t.Fatal("failed launch accepted")
			}
			if !cleaned || !stopped || m.linkCredential != nil || m.currentSessionID != "" {
				t.Fatal("failed launch left state or credential")
			}
		})
	}
}

func TestLinkAuthModeAndServiceConfig(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
		bad        bool
	}{{"", "warn", false}, {"warn", "warn", false}, {"enforce", "enforce", false}, {"off", "", true}, {"ENFORCE", "", true}, {"unknown", "", true}} {
		m := New(Paths{LinkAuthMode: tc.mode})
		got, err := m.linkAuthMode()
		if (err != nil) != tc.bad || got != tc.want {
			t.Fatalf("mode %q: %q %v", tc.mode, got, err)
		}
	}
	body, err := linkBridgeEnvironment("machine", `C:\ProgramData\SolarBeam\link-auth\secret`, "enforce")
	if err != nil || strings.Contains(body, "SOLARBEAM_LINK_SECRET=") || !strings.Contains(body, "SOLARBEAM_LINK_AUTH_MODE=enforce\n") {
		t.Fatal("service configuration must contain path and explicit mode only")
	}
	if _, err := linkBridgeEnvironment("machine\nSOLARBEAM_LINK_SECRET=injected", "path", "warn"); err == nil {
		t.Fatal("service environment line injection accepted")
	}
}

func TestLinkAuthFailedStopRetainsSessionForRetry(t *testing.T) {
	m := New(Paths{StateDir: t.TempDir()})
	supportLinkAuthForTest(m)
	m.prepareFn = func() error { return nil }
	cleaned := false
	m.launchFn = func(proto.SolarbeamStartArgs) error {
		return m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
			return &linkCredential{cleanup: func() error { cleaned = true; return nil }}, nil
		})
	}
	m.engineFn = func(proto.SolarbeamStartArgs) error { return errors.New("engine failed") }
	m.stopFn = func() error { return errors.New("bridge still running") }
	if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "survivor"}); err == nil {
		t.Fatal("launch failure accepted")
	}
	if cleaned || m.linkCredential == nil || m.currentSessionID != "survivor" || !recordExists(m.paths.StateDir) {
		t.Fatal("surviving bridge lost credential or cleanup ownership")
	}
	m.stopFn = func() error { return nil }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "survivor"}); err != nil {
		t.Fatal(err)
	}
	if !cleaned || recordExists(m.paths.StateDir) {
		t.Fatal("stop retry did not finish cleanup")
	}
}

func TestLinkAuthPreviousBridgeStopFailurePreservesOldOwnership(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "bound manager"
		if recovered {
			name = "agent restarted with unavailable process snapshot"
		}
		t.Run(name, func(t *testing.T) {
			m := New(Paths{StateDir: t.TempDir()})
			supportLinkAuthForTest(m)
			m.prepareFn = func() error { return nil }
			if !recovered {
				m.currentSessionID = "old"
			}
			m.bridgeRunningFn = func() bool { return false }
			m.saveSessionRecordLocked("old")
			m.launchFn = func(proto.SolarbeamStartArgs) error { return errPreviousBridgeStillRunning }
			if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "new"}); err == nil {
				t.Fatal("old bridge still running accepted")
			}
			if m.currentSessionID != "old" || m.loadSessionRecord() != "old" {
				t.Fatal("old bridge stop authority lost")
			}
		})
	}
}

func TestLegacyLinkAuthLaunchFailureStillStopsBridge(t *testing.T) {
	m := New(Paths{StateDir: t.TempDir()})
	m.prepareFn = func() error { return nil }
	m.linkCapabilityFn = func(string) ([]byte, error) { return nil, nil }
	m.recoveredLinkCleanupFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error {
		return m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
			t.Fatal("legacy launch provisioned a credential")
			return nil, nil
		})
	}
	m.engineFn = func(proto.SolarbeamStartArgs) error { return errors.New("service restart failed") }
	stopped := false
	m.stopFn = func() error { stopped = true; return nil }
	if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "legacy"}); err == nil {
		t.Fatal("failed engine startup accepted")
	}
	if !stopped || m.linkCredential != nil || m.currentSessionID != "" || recordExists(m.paths.StateDir) {
		t.Fatal("legacy bridge left behind after engine failure")
	}
}

func TestLinkAuthUnverifiedServicePathRemainsLegacy(t *testing.T) {
	m := New(Paths{LinkAuthMode: "warn"})
	supportLinkAuthForTest(m)
	m.linkEngineMatchesFn = func(Paths) bool { return false }
	if err := m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
		t.Fatal("unverified service engine provisioned")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(m.linkAuthEnvironment(nil)) != 0 || m.linkCredential.mode != "" {
		t.Fatal("service mismatch retained auth settings")
	}
}
