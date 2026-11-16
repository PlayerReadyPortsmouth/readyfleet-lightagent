package solarbeam

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestLinkAuthCapabilityProvisioningMatrix(t *testing.T) {
	for _, mode := range []string{"", "warn", "enforce"} {
		for _, pair := range []struct {
			name           string
			engine, bridge bool
		}{
			{"old-old", false, false},
			{"new-old", true, false},
			{"old-new", false, true},
			{"new-new", true, true},
		} {
			t.Run(mode+"/"+pair.name, func(t *testing.T) {
				m := New(Paths{Engine: "engine", Bridge: "bridge", LinkAuthMode: mode})
				m.linkEngineMatchesFn = func(Paths) bool { return true }
				m.recoveredLinkCleanupFn = func() error { return nil }
				var checked []string
				m.linkCapabilityFn = func(path string) ([]byte, error) {
					checked = append(checked, path)
					if (path == "engine" && pair.engine) || (path == "bridge" && pair.bridge) {
						return []byte(linkCapabilityPrefix + path + " provisioned-v1\n"), nil
					}
					return []byte("legacy binary help\n"), nil
				}
				writes := 0
				refuse := mode == "enforce" && !(pair.engine && pair.bridge)
				err := m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
					writes++
					if !reflect.DeepEqual(checked, []string{"engine", "bridge"}) {
						t.Fatal("credential written before checking both installed binaries")
					}
					return &linkCredential{env: []string{"SOLARBEAM_LINK_SECRET=" + string(secret)}}, nil
				})
				if refuse {
					// enforce never degrades to an unauthenticated legacy pair.
					if !errors.Is(err, errLinkAuthEnforceUnsupported) || writes != 0 {
						t.Fatalf("enforce with %s: err = %v, writes = %d; want refusal", pair.name, err, writes)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(checked, []string{"engine", "bridge"}) {
					t.Fatalf("checked binaries = %v", checked)
				}
				wantMode := ""
				if pair.engine && pair.bridge {
					wantMode = mode
					if wantMode == "" {
						wantMode = "warn"
					}
				}
				if (writes == 1) != (wantMode != "") || writes > 1 || m.linkCredential.mode != wantMode {
					t.Fatalf("writes = %d, mode = %q; want mode %q", writes, m.linkCredential.mode, wantMode)
				}
				// Both child environments discard inherited credentials and mode,
				// including differently cased Windows environment names.
				engineEnv := m.linkAuthEnvironment([]string{"PATH=safe", "SOLARBEAM_LINK_SECRET=engine-stale", "solarbeam_link_secret_file=old-file", "SOLARBEAM_LINK_AUTH_MODE=enforce"})
				bridgeEnv := m.linkAuthEnvironment([]string{"PATH=safe", "solarbeam_link_secret=bridge-stale", "SOLARBEAM_LINK_SECRET_FILE=old-file", "solarbeam_link_auth_mode=warn"})
				if !reflect.DeepEqual(engineEnv, bridgeEnv) {
					t.Fatal("engine and bridge received different link authentication settings")
				}
				if wantMode == "" {
					if !reflect.DeepEqual(engineEnv, []string{"PATH=safe"}) {
						t.Fatal("legacy launch inherited authentication settings")
					}
				} else if len(engineEnv) != 3 || engineEnv[2] != "SOLARBEAM_LINK_AUTH_MODE="+wantMode || strings.Contains(strings.Join(engineEnv, "\n"), "stale") {
					t.Fatal("supported launch did not receive only the fresh credential and selected mode")
				}
			})
		}
	}
}

func TestLinkAuthCapabilityRecheckedOnEveryLaunch(t *testing.T) {
	m := New(Paths{Engine: "engine", Bridge: "bridge", StateDir: t.TempDir(), LinkAuthMode: "warn"})
	m.prepareFn = func() error { return nil }
	m.stopFn = func() error { return nil }
	m.linkEngineMatchesFn = func(Paths) bool { return true }
	m.recoveredLinkCleanupFn = func() error { return nil }
	stage := 0
	stages := []struct {
		name                  string
		engine, bridge        bool
		probeError            bool
		wantWrites, wantClean int
	}{
		{"legacy", false, false, false, 0, 0},
		{"engine upgrade only", true, false, false, 0, 0},
		{"bridge upgrade completes pair", true, true, false, 1, 0},
		{"engine downgrade", false, true, false, 1, 1},
		{"engine upgrade restores pair", true, true, false, 2, 1},
		{"probe fails after authenticated launch", true, true, true, 2, 2},
	}
	probes, writes, cleaned := 0, 0, 0
	m.linkCapabilityFn = func(path string) ([]byte, error) {
		probes++
		if path == "engine" && stages[stage].probeError {
			return []byte(linkCapabilityPrefix + "engine provisioned-v1"), errors.New("probe failed")
		}
		if (path == "engine" && stages[stage].engine) || (path == "bridge" && stages[stage].bridge) {
			return []byte(linkCapabilityPrefix + path + " provisioned-v1"), nil
		}
		return nil, nil
	}
	var bridgeEnv []string
	m.launchFn = func(proto.SolarbeamStartArgs) error {
		if err := m.provisionLinkAuth(func(secret []byte) (*linkCredential, error) {
			writes++
			return &linkCredential{
				env:     []string{"SOLARBEAM_LINK_SECRET=" + string(secret)},
				cleanup: func() error { cleaned++; return nil },
			}, nil
		}); err != nil {
			return err
		}
		bridgeEnv = m.linkAuthEnvironment([]string{"PATH=safe", "SOLARBEAM_LINK_SECRET=stale", "SOLARBEAM_LINK_AUTH_MODE=enforce"})
		return nil
	}
	m.engineFn = func(proto.SolarbeamStartArgs) error {
		engineEnv := m.linkAuthEnvironment([]string{"PATH=safe", "solarbeam_link_secret_file=stale", "solarbeam_link_auth_mode=warn"})
		if !reflect.DeepEqual(engineEnv, bridgeEnv) {
			t.Fatal("launch configured only one endpoint for authentication")
		}
		if !stages[stage].engine || !stages[stage].bridge || stages[stage].probeError {
			if !reflect.DeepEqual(engineEnv, []string{"PATH=safe"}) {
				t.Fatal("fallback launch retained authentication settings")
			}
		} else if len(engineEnv) != 3 || engineEnv[2] != "SOLARBEAM_LINK_AUTH_MODE=warn" {
			t.Fatal("fully upgraded launch did not enable authentication on both endpoints")
		}
		return nil
	}
	args := proto.SolarbeamStartArgs{SessionID: "same-session", MachineID: "machine"}
	for stage = range stages {
		if err := m.Start(context.Background(), args); err != nil {
			t.Fatalf("%s: %v", stages[stage].name, err)
		}
		if probes != 2*(stage+1) || writes != stages[stage].wantWrites || cleaned != stages[stage].wantClean {
			t.Fatalf("%s: probes=%d writes=%d cleaned=%d", stages[stage].name, probes, writes, cleaned)
		}
		if len(m.linkCredential.env) != 0 {
			t.Fatal("launch retained child credentials")
		}
	}
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: args.SessionID}); err != nil {
		t.Fatal(err)
	}
	if m.linkCredential != nil || cleaned != 2 {
		t.Fatal("last legacy launch retained credential ownership")
	}
}

func TestLinkAuthCapabilityFailureReportsOnlySafeStatus(t *testing.T) {
	for _, failure := range []struct {
		name, output, status string
		err                  error
	}{
		{"missing", "", "unsupported", nil},
		{"malformed", linkCapabilityPrefix + "engine provisioned-v999\nraw-sensitive-help", "unsupported", nil},
		{"duplicate", strings.Repeat(linkCapabilityPrefix+"engine provisioned-v1\n", 2), "unsupported", nil},
		{"wrong role", linkCapabilityPrefix + "bridge provisioned-v1", "unsupported", nil},
		{"failed probe", "raw-sensitive-help", "unavailable", errors.New("raw-sensitive-error")},
	} {
		t.Run(failure.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(old) })
			m := New(Paths{Engine: "engine", Bridge: "bridge", LinkAuthMode: "warn"})
			m.linkEngineMatchesFn = func(Paths) bool { return true }
			m.recoveredLinkCleanupFn = func() error { return nil }
			cleaned := false
			m.linkCredential = &linkCredential{
				env:     []string{"SOLARBEAM_LINK_SECRET=old-sensitive-secret", "SOLARBEAM_LINK_AUTH_MODE=enforce"},
				cleanup: func() error { cleaned = true; return nil },
			}
			m.linkCapabilityFn = func(path string) ([]byte, error) {
				if path == "engine" {
					return []byte(failure.output), failure.err
				}
				return []byte(linkCapabilityPrefix + "bridge provisioned-v1"), nil
			}
			if err := m.provisionLinkAuth(func([]byte) (*linkCredential, error) {
				t.Fatal("failed capability check wrote a credential")
				return nil, nil
			}); err != nil {
				t.Fatal(err)
			}
			if !cleaned || len(m.linkAuthEnvironment(nil)) != 0 || m.linkCredential.mode != "" {
				t.Fatal("failed capability check retained previous authentication configuration")
			}
			report := logs.String()
			if !strings.Contains(report, "phase=link_auth_legacy") || !strings.Contains(report, "engine="+failure.status) || !strings.Contains(report, "bridge=supported") {
				t.Fatalf("missing capability fallback report: %s", report)
			}
			for _, sensitive := range []string{"raw-sensitive-help", "raw-sensitive-error", "old-sensitive-secret", linkCapabilityPrefix} {
				if strings.Contains(report, sensitive) {
					t.Fatal("capability report exposed raw process output, error, or credentials")
				}
			}
		})
	}
}

func TestLinkCapabilityStatus(t *testing.T) {
	marker := linkCapabilityPrefix + "engine provisioned-v1"
	for _, tc := range []struct {
		name, output, endpoint, want string
		err                          error
	}{
		{"exact", marker, "engine", "supported", nil},
		{"help and CRLF", "usage\r\n" + marker + "\r\noptions\r\n", "engine", "supported", nil},
		{"bridge", linkCapabilityPrefix + "bridge provisioned-v1", "bridge", "supported", nil},
		{"absent", "legacy help", "engine", "unsupported", nil},
		{"wrong binary", marker, "bridge", "unsupported", nil},
		{"duplicate", marker + "\n" + marker, "engine", "unsupported", nil},
		{"conflicting version", marker + "\n" + linkCapabilityPrefix + "engine provisioned-v2", "engine", "unsupported", nil},
		{"extra field", marker + " extra", "engine", "unsupported", nil},
		{"leading whitespace", " " + marker, "engine", "unsupported", nil},
		{"reflected flag default", "flag default: " + marker, "engine", "unsupported", nil},
		{"unsuccessful process", marker, "engine", "unavailable", errors.New("failed")},
		{"exact output limit", marker + "\n" + strings.Repeat("x", linkCapabilityLimit-len(marker)-1), "engine", "supported", nil},
		{"output beyond limit", marker + "\n" + strings.Repeat("x", linkCapabilityLimit), "engine", "unavailable", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := linkCapabilityStatus([]byte(tc.output), tc.endpoint, tc.err); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLinkCapabilityOutputBound(t *testing.T) {
	var output linkCapabilityOutput
	chunk := bytes.Repeat([]byte{'x'}, linkCapabilityLimit/2)
	for range 2 {
		if n, err := output.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("bounded output write: n=%d err=%v", n, err)
		}
	}
	if n, err := output.Write([]byte{'x'}); n != 0 || !errors.Is(err, errLinkCapabilityProbe) || len(output.data) != linkCapabilityLimit {
		t.Fatal("output beyond capability probe limit was retained")
	}
	var oversized linkCapabilityOutput
	if n, err := oversized.Write(make([]byte, linkCapabilityLimit+1)); n != 0 || !errors.Is(err, errLinkCapabilityProbe) || len(oversized.data) != 0 {
		t.Fatal("oversized first write was retained")
	}
}

func TestConfigureLinkCapabilityCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine")
	cmd := exec.Command(path)
	allowed := []string{"PATH=safe", "SystemRoot=system", "WINDIR=windows", "HOME=home", "USERPROFILE=profile", "TEMP=temp", "TMP=tmp"}
	env := append(append([]string(nil), allowed...),
		"SOLARBEAM_LINK_SECRET=secret", "solarbeam_link_secret_file=secret-path", "SOLARBEAM_LINK_AUTH_MODE=enforce",
		"SOLARBEAM_BRIDGE_WS=ws://secret", "SOLARBEAM_MACHINE_ID=machine\n"+linkCapabilityPrefix+"engine provisioned-v1",
		"LIVEKIT_TOKEN=secret", "LD_PRELOAD=untrusted", "UNRELATED=untrusted")
	configureLinkCapabilityCommand(cmd, env)
	if !reflect.DeepEqual(cmd.Args, []string{path, "--help"}) || cmd.Dir != filepath.Dir(path) || cmd.WaitDelay != time.Second {
		t.Fatalf("unexpected probe configuration: args=%v dir=%q wait=%v", cmd.Args, cmd.Dir, cmd.WaitDelay)
	}
	if !reflect.DeepEqual(cmd.Env, allowed) {
		t.Fatal("probe environment retained credentials or reflective help defaults")
	}
	if output, err := probeLinkCapability("relative-engine"); !errors.Is(err, errLinkCapabilityProbe) || output != nil {
		t.Fatal("relative binary path accepted")
	}
}
