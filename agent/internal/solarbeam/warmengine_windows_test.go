//go:build windows

package solarbeam

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// redirectBridgeEnv points bridge.env at a temp dir for the duration of a
// test, so nothing here writes to the real %ProgramData%\SolarBeam.
// Writing it re-owns the folder to SYSTEM, which needs SeRestorePrivilege:
// run these as SYSTEM or an elevated administrator (the build VM).
func redirectBridgeEnv(t *testing.T) string {
	t.Helper()
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("bridge.env lock-down needs SYSTEM or an elevated administrator")
	}
	dir := t.TempDir()
	oldDir, oldFile := bridgeEnvDir, bridgeEnvFile
	bridgeEnvDir = dir
	bridgeEnvFile = filepath.Join(dir, "bridge.env")
	t.Cleanup(func() { bridgeEnvDir, bridgeEnvFile = oldDir, oldFile })
	return bridgeEnvFile
}

var testStartArgs = proto.SolarbeamStartArgs{
	SessionID: "sess-1", MachineID: "uuid-1", BundleToken: "btok",
}

// The launch-time half of the saving: ~2.8s of service spawn plus ~2.0s of
// cold encoder probe, skipped whenever the resident engine is already
// healthy.
func TestStartEngine_HealthyWarmEngineSkipsTheRestart(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "warn"); err != nil {
		t.Fatal(err)
	}
	if err := markBridgeEnvLoaded(); err != nil {
		t.Fatal(err)
	}
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	m.healthyFn = func() bool { return true }
	m.restartFn = func() error { t.Fatal("a healthy warm engine must not be restarted"); return nil }

	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatalf("startEngine: %v", err)
	}
	// bridge.env is still written every session: same bytes, and it is
	// what makes the file survive a machine that was never preloaded.
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("bridge.env must still be written: %v", err)
	}
	want, _ := linkBridgeEnvironment("uuid-1", windowsLinkSecretPath(), "warn")
	if string(b) != want {
		t.Fatalf("bridge.env = %q, want %q", string(b), want)
	}
}

// §4.3 option 1: a crash-looping engine (service Running, no process)
// reports unhealthy, and the next launch restarts it. That is the only
// self-heal left once the unconditional restart is gone.
func TestStartEngine_UnhealthyEngineIsRestarted(t *testing.T) {
	redirectBridgeEnv(t)
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	restarted := false
	m.healthyFn = func() bool { return false }
	m.restartFn = func() error { restarted = true; return nil }

	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatalf("startEngine: %v", err)
	}
	if !restarted {
		t.Fatal("an unhealthy engine must be restarted, not reused")
	}
}

// The kill switch restores the pre-warm launch path exactly: restart every
// session, healthy or not.
func TestStartEngine_WarmDisabledAlwaysRestarts(t *testing.T) {
	redirectBridgeEnv(t)
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`, DisableWarmEngine: true})
	restarted := false
	m.healthyFn = func() bool { t.Fatal("a reverted machine must not consult engine health"); return true }
	m.restartFn = func() error { restarted = true; return nil }

	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatalf("startEngine: %v", err)
	}
	if !restarted {
		t.Fatal("with warm-engine disabled every session must restart the engine, as before")
	}
}

// Session end must leave the engine up — and must leave bridge.env in
// place, or an engine respawned by sunshinesvc would come back STOCK
// instead of in bridge mode and silently un-warm the machine.
func TestStopSolarbeam_WarmLeavesEngineRunningAndBridgeEnvInPlace(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "warn"); err != nil {
		t.Fatalf("seed bridge.env: %v", err)
	}
	m := New(Paths{})
	m.stopServiceFn = func() error { t.Fatal("warm engine must not be stopped at session end"); return nil }

	if err := m.stopSolarbeam(); err != nil {
		t.Fatalf("stopSolarbeam: %v", err)
	}
	if _, err := os.Stat(envFile); err != nil {
		t.Fatalf("bridge.env must survive a warm session end: %v", err)
	}
}

// Reverted machine: today's teardown, unchanged.
func TestStopSolarbeam_WarmDisabledStopsEngineAndClearsBridgeEnv(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "warn"); err != nil {
		t.Fatalf("seed bridge.env: %v", err)
	}
	m := New(Paths{DisableWarmEngine: true})
	stopped := false
	m.stopServiceFn = func() error { stopped = true; return nil }

	if err := m.stopSolarbeam(); err != nil {
		t.Fatalf("stopSolarbeam: %v", err)
	}
	if !stopped {
		t.Fatal("with warm-engine disabled the engine service must still be stopped at session end")
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("bridge.env must be removed on the non-warm path, stat err = %v", err)
	}
}

func TestPreloadEngine_WritesBridgeEnvAndStartsAColdEngine(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	started := false
	m.healthyFn = func() bool { return false }
	m.restartFn = func() error { started = true; return nil }

	if err := m.preloadEngine("uuid-1"); err != nil {
		t.Fatalf("preloadEngine: %v", err)
	}
	if !started {
		t.Fatal("preload must bring a cold engine up")
	}
	b, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("preload must write bridge.env: %v", err)
	}
	want, _ := linkBridgeEnvironment("uuid-1", windowsLinkSecretPath(), "warn")
	if string(b) != want {
		t.Fatalf("bridge.env = %q, want %q", string(b), want)
	}
}

// Preload after an agent restart, with the engine already warm from the
// previous run: nothing to do, and in particular no restart that would
// drop a session in progress.
func TestPreloadEngine_HealthyEngineIsLeftAlone(t *testing.T) {
	redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "warn"); err != nil {
		t.Fatal(err)
	}
	if err := markBridgeEnvLoaded(); err != nil {
		t.Fatal(err)
	}
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	m.healthyFn = func() bool { return true }
	m.restartFn = func() error { t.Fatal("preload must not restart an already-healthy engine"); return nil }

	if err := m.preloadEngine("uuid-1"); err != nil {
		t.Fatalf("preloadEngine: %v", err)
	}
}

// engineHealthy is deliberately AND-ed: an unconfigured engine path can
// never be reported healthy, so a misconfigured machine restarts rather
// than assuming warmth it cannot see.
func TestEngineHealthy_UnconfiguredEnginePathIsNotHealthy(t *testing.T) {
	m := New(Paths{})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	if m.engineHealthy() {
		t.Fatal("an empty engine path must never report healthy")
	}
}

// SessionActive's outer gate cannot claim a bridge exists when none is
// configured.
func TestBridgeProcessRunning_UnconfiguredBridgePathIsFalse(t *testing.T) {
	m := New(Paths{})
	if m.bridgeProcessRunning() {
		t.Fatal("an empty bridge path must never report a running bridge")
	}
}

// processExistsByName underpins both engineHealthy and the SessionActive
// outer gate, so prove it against a process that genuinely exists (this
// test binary) and one that cannot.
func TestProcessExistsByName_FindsThisTestBinary(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !processExistsByName(filepath.Base(self)) {
		t.Fatalf("processExistsByName(%q) = false for the running test binary", filepath.Base(self))
	}
	if processExistsByName("readyfleet-no-such-process-32f0a1.exe") {
		t.Fatal("processExistsByName must be false for a name nothing is running under")
	}
	if processExistsByName("") {
		t.Fatal("processExistsByName(\"\") must be false, not match everything")
	}
}

// The warm-engine work added a caller that arrives at an unknown moment:
// the re-preload straight after an engine MSI install, which may land while
// the installer is still stopping or starting SunshineService. Sending a
// Stop control to a service in a transitional state fails with
// ERROR_SERVICE_CANNOT_ACCEPT_CTRL, so stopServiceIfRunning lets it settle
// first — this pins which states count as transitional.
func TestServiceStateIsPending_CoversEveryTransitionalState(t *testing.T) {
	tests := []struct {
		name  string
		state svc.State
		want  bool
	}{
		{"start pending", svc.StartPending, true},
		{"stop pending", svc.StopPending, true},
		{"continue pending", svc.ContinuePending, true},
		{"pause pending", svc.PausePending, true},
		{"stopped", svc.Stopped, false},
		{"running", svc.Running, false},
		{"paused", svc.Paused, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceStateIsPending(tc.state); got != tc.want {
				t.Fatalf("serviceStateIsPending(%v) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

func TestLinkAuthServiceConfigChangesRestartOnceAndRetryFailure(t *testing.T) {
	redirectBridgeEnv(t)
	m := New(Paths{})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	m.linkCredential = &linkCredential{mode: "warn"}
	m.healthyFn = func() bool { return true }
	calls := 0
	m.restartFn = func() error { calls++; return nil }
	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatal(err)
	}
	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("initial provision restart count=%d", calls)
	}
	m.paths.LinkAuthMode = "enforce"
	m.linkCredential.mode = "enforce"
	m.restartFn = func() error { calls++; return os.ErrPermission }
	if err := m.startEngine(testStartArgs); err == nil {
		t.Fatal("failed restart accepted")
	}
	// A new agent cannot mistake the newly-written config for an applied mode.
	restarted := New(Paths{LinkAuthMode: "enforce"})
	restarted.linkCredential = &linkCredential{mode: "enforce"}
	restarted.healthyFn = func() bool { return true }
	restarted.restartFn = func() error { calls++; return nil }
	if err := restarted.startEngine(testStartArgs); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("mode update was not retried: %d", calls)
	}
	if err := restarted.startEngine(testStartArgs); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatal("unchanged warm configuration restarted again")
	}
}

func TestLinkAuthPreloadDefersModeChangeDuringActiveSession(t *testing.T) {
	redirectBridgeEnv(t)
	m := New(Paths{LinkAuthMode: "enforce"})
	supportLinkAuthForTest(m)
	m.bridgeRunningFn = func() bool { return true }
	m.restartFn = func() error { t.Fatal("active session must survive preload"); return nil }
	if err := m.preloadEngine("machine"); err == nil {
		t.Fatal("pending service configuration must be reported")
	}
}

func TestInteractiveLinkSecretOnlyInExplicitChildren(t *testing.T) {
	m := New(Paths{Mode: ModeInteractive, LinkAuthMode: "enforce"})
	supportLinkAuthForTest(m)
	m.linkEngineVerified = true
	if err := m.provisionPlatformLinkAuth(); err != nil {
		t.Fatal(err)
	}
	bridge := m.linkAuthEnvironment(nil)
	cmd := buildEngineCmd(`C:\SolarBeam\sunshine.exe`, testStartArgs)
	cmd.Env = m.linkAuthEnvironment(cmd.Env)
	var credential string
	for _, entry := range bridge {
		if strings.HasPrefix(entry, "SOLARBEAM_LINK_SECRET=") {
			credential = entry
		}
	}
	if len(credential) != len("SOLARBEAM_LINK_SECRET=")+64 {
		t.Fatal("BYOD credential absent")
	}
	if !slices.Contains(cmd.Env, credential) {
		t.Fatal("interactive children received different keys")
	}
	if strings.Contains(strings.Join(cmd.Args, " "), strings.TrimPrefix(credential, "SOLARBEAM_LINK_SECRET=")) {
		t.Fatal("secret in process arguments")
	}
	if slices.Contains(os.Environ(), credential) {
		t.Fatal("agent environment mutated")
	}
	cmd.Env = withoutLinkAuth(cmd.Env)
	m.forgetLinkEnvironment()
	if slices.Contains(cmd.Env, credential) || len(m.linkCredential.env) != 0 {
		t.Fatal("launcher retained child secret")
	}
}

func TestLinkAuthManagedFallbackClearsConfigAndRestartsWarmEngine(t *testing.T) {
	for _, preload := range []bool{false, true} {
		t.Run(fmt.Sprintf("preload=%v", preload), func(t *testing.T) {
			envFile := redirectBridgeEnv(t)
			if _, err := writeBridgeEnv("uuid-1", "enforce"); err != nil {
				t.Fatal(err)
			}
			if err := markBridgeEnvLoaded(); err != nil {
				t.Fatal(err)
			}
			m := New(Paths{Engine: "engine", Bridge: "bridge", LinkAuthMode: "warn"})
			m.linkCapabilityFn = func(string) ([]byte, error) { return nil, nil }
			m.recoveredLinkCleanupFn = func() error { return nil }
			m.bridgeRunningFn = func() bool { return false }
			m.healthyFn = func() bool { return true }
			restarts := 0
			m.restartFn = func() error { restarts++; return nil }
			if preload {
				if err := m.preloadEngine("uuid-1"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := m.provisionLinkAuth(func([]byte) (*linkCredential, error) { t.Fatal("mixed pair provisioned"); return nil, nil }); err != nil {
					t.Fatal(err)
				}
				if err := m.startEngine(testStartArgs); err != nil {
					t.Fatal(err)
				}
			}
			want, _ := linkBridgeEnvironment("uuid-1", "", "")
			for _, path := range []string{envFile, envFile + ".applied"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("legacy service config = %q, %v", got, err)
				}
			}
			if restarts != 1 {
				t.Fatalf("warm engine fallback restarts = %d", restarts)
			}
		})
	}
}

func TestLinkAuthPreloadDefersFallbackWithoutChangingActiveConfig(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "enforce"); err != nil {
		t.Fatal(err)
	}
	if err := markBridgeEnvLoaded(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	m := New(Paths{LinkAuthMode: "warn"})
	m.linkCapabilityFn = func(string) ([]byte, error) { return nil, nil }
	m.bridgeRunningFn = func() bool { return true }
	m.restartFn = func() error { t.Fatal("must not restart an active session"); return nil }
	if err := m.preloadEngine("uuid-1"); err == nil {
		t.Fatal("active downgrade not deferred")
	}
	for _, path := range []string{envFile, envFile + ".applied"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(before) {
			t.Fatal("active service config changed")
		}
	}
}

func TestLinkAuthManagedEngineUsesBridgeLaunchDecision(t *testing.T) {
	envFile := redirectBridgeEnv(t)
	m := New(Paths{LinkAuthMode: "enforce"})
	m.linkCredential = &linkCredential{mode: "warn"}
	m.linkCapabilityFn = func(string) ([]byte, error) { t.Fatal("engine must use the bridge's launch decision"); return nil, nil }
	m.restartFn = func() error { return nil }
	if err := m.startEngine(testStartArgs); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(envFile)
	if err != nil || !strings.Contains(string(got), "SOLARBEAM_LINK_AUTH_MODE=warn\n") {
		t.Fatal("engine mode diverged from bridge")
	}
}

func TestLinkAuthFirstSupportedLaunchRestartsUnverifiedWarmEngine(t *testing.T) {
	redirectBridgeEnv(t)
	if _, err := writeBridgeEnv("uuid-1", "warn"); err != nil {
		t.Fatal(err)
	}
	if err := markBridgeEnvLoaded(); err != nil {
		t.Fatal(err)
	}
	m := New(Paths{})
	m.linkCredential = &linkCredential{mode: "warn"}
	m.healthyFn = func() bool { return true }
	restarts := 0
	m.restartFn = func() error { restarts++; return nil }
	for range 2 {
		if err := m.startEngine(testStartArgs); err != nil {
			t.Fatal(err)
		}
	}
	if restarts != 1 || !m.linkEngineVerified {
		t.Fatalf("unverified engine reused or restarted twice: %d", restarts)
	}
}

func TestLinkServicePathMatchesInstalledEngine(t *testing.T) {
	engine := `C:\Program Files\SolarBeam\sunshine.exe`
	service := engineServicePath(engine)
	for _, tc := range []struct {
		path string
		want bool
	}{
		{service, false}, {`"` + service + `"`, true}, {`"` + strings.ToUpper(service) + `"`, true},
		{`C:\Old\tools\sunshinesvc.exe`, false}, {`"` + service + `" --other`, false},
		{service + ` --other`, false}, {`"` + service, false}, {"", false},
	} {
		if got := linkServicePathMatches(engine, tc.path); got != tc.want {
			t.Fatalf("service path %q match = %v", tc.path, got)
		}
	}
	if linkServicePathMatches(`C:\Program Files\SolarBeam\unrelated.exe`, service) {
		t.Fatal("wrong checked engine basename accepted")
	}
}
