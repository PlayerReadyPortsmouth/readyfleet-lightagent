//go:build darwin

package solarbeam

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// writeFakeBundle lays out <dir>/Fake.app with an Info.plist and an executable
// script (never run by the unit tests below; the dry run runs it).
func writeFakeBundle(t *testing.T, dir, script string) (app, exe string) {
	t.Helper()
	app = filepath.Join(dir, "Fake.app")
	exe = DarwinEngineExecutable(app, "Fake")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	plist := strings.ReplaceAll(testInfoPlist, "Vibeshine", "Fake")
	if err := os.WriteFile(DarwinInfoPlistPath(app), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return app, exe
}

func writeFakeBridge(t *testing.T, dir, script string) string {
	t.Helper()
	bridge := filepath.Join(dir, DarwinBridgeName)
	if err := os.WriteFile(bridge, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bridge
}

func newDarwinManager(t *testing.T, dir string, enabled bool) *Manager {
	t.Helper()
	_, exe := writeFakeBundle(t, dir, "#!/bin/sh\nexit 0\n")
	bridge := writeFakeBridge(t, dir, "#!/bin/sh\nexit 0\n")
	return New(Paths{StateDir: dir, Root: dir, Engine: exe, Bridge: bridge, Mode: ModeInteractive, DarwinHostEnabled: enabled})
}

func TestDarwinPrepareRefusesUntilOptedIn(t *testing.T) {
	m := newDarwinManager(t, t.TempDir(), false)
	err := m.prepareEngineInteractive()
	if err == nil || !strings.Contains(err.Error(), "solarbeam_host") {
		t.Fatalf("watch-only Mac accepted a start: %v", err)
	}
	a := m.HostAvailability()
	if a.Enabled || a.Available || !strings.Contains(a.Reason, "solarbeam_host") {
		t.Fatalf("availability = %+v", a)
	}
}

func TestDarwinPrepareNeedsBundleAndBridge(t *testing.T) {
	dir := t.TempDir()
	m := newDarwinManager(t, dir, true)
	if err := m.prepareEngineInteractive(); err != nil {
		t.Fatalf("complete install refused: %v", err)
	}
	if a := m.HostAvailability(); !a.Available || !a.Enabled || a.Reason != "" {
		t.Fatalf("availability = %+v", a)
	}

	// A bare binary outside a bundle is not an engine app.
	bare := filepath.Join(dir, "sunshine")
	if err := os.WriteFile(bare, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.paths.Engine = bare
	if err := m.prepareEngineInteractive(); err == nil || !strings.Contains(err.Error(), ".app") {
		t.Fatalf("bare binary accepted: %v", err)
	}

	// Missing bridge names the path.
	m = newDarwinManager(t, dir, true)
	_ = os.Remove(m.paths.Bridge)
	if err := m.prepareEngineInteractive(); err == nil || !strings.Contains(err.Error(), "bridge not installed") {
		t.Fatalf("missing bridge: %v", err)
	}

	// A non-executable engine is refused.
	m = newDarwinManager(t, dir, true)
	if err := os.Chmod(m.paths.Engine, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareEngineInteractive(); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("non-executable engine: %v", err)
	}
}

func TestDarwinPrepareHonoursARecentPermissionFault(t *testing.T) {
	m := newDarwinManager(t, t.TempDir(), true)
	m.darwin.fault = hostFault{Reason: ReasonScreenRecording, At: time.Now()}
	err := m.prepareEngineInteractive()
	if err == nil || !strings.Contains(err.Error(), ReasonScreenRecording) || !strings.Contains(err.Error(), "Privacy & Security") {
		t.Fatalf("recent fault did not refuse with instructions: %v", err)
	}
	if a := m.HostAvailability(); a.Available || a.Reason != ReasonScreenRecording {
		t.Fatalf("availability = %+v", a)
	}
	// After the cooldown a start may try again: the grant may have been given.
	m.darwin.fault.At = time.Now().Add(-faultCooldown - time.Second)
	if err := m.prepareEngineInteractive(); err != nil {
		t.Fatalf("stale fault still refused: %v", err)
	}
}

func TestDarwinEngineFileVersionComesFromInfoPlist(t *testing.T) {
	dir := t.TempDir()
	_, exe := writeFakeBundle(t, dir, "#!/bin/sh\necho never-run; exit 1\n")
	got, err := engineFileVersion(exe)
	if err != nil || got != "2.1.18" {
		t.Fatalf("version = %q, %v", got, err)
	}
	// Status reads it live through fileVersionFn without running anything.
	m := New(Paths{Engine: exe, Bridge: filepath.Join(dir, "none"), Mode: ModeInteractive})
	if st := m.Status(context.Background()); st.EngineVersion != "2.1.18" || st.Running {
		t.Fatalf("status = %+v", st)
	}
	if _, err := engineFileVersion(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing engine reported a version")
	}
}

// Privacy mode blanks monitors through the Windows venue companion. A Mac
// has nothing to do that with, so a start that asks for it is refused before
// anything is launched, rather than streaming without the privacy it promised.
func TestDarwinLaunchBridgeRefusesPrivacyMode(t *testing.T) {
	m := newDarwinManager(t, t.TempDir(), true)
	for _, args := range []proto.SolarbeamStartArgs{{SessionID: "s", PrivacyMode: true}, {SessionID: "s", PrivacyInput: true}} {
		err := m.launchBridge(args)
		if err == nil || !strings.Contains(err.Error(), "privacy mode is not available on this Mac") {
			t.Fatalf("privacy start accepted: %v", err)
		}
		if m.interactiveBridgeProc != nil {
			t.Fatal("a bridge was launched for a refused start")
		}
	}
}

func TestDarwinEngineWatcherKeepsClassifyingAfterReady(t *testing.T) {
	w := newDarwinEngineWatcher(nil)
	done := make(chan struct{})
	go func() {
		_, _ = w.Write([]byte("Info: SolarBeam: enter"))
		_, _ = w.Write([]byte("ing bridge mode\nInfo: capturing\n"))
	}()
	if err := w.wait(2*time.Second, done, "engine.log"); err != nil {
		t.Fatalf("wait = %v", err)
	}
	if w.Reason() != "" {
		t.Fatalf("reason before any fault = %q", w.Reason())
	}
	// A later permission loss is remembered for the supervisor even though the
	// startup verdict is settled.
	_, _ = w.Write([]byte("Error: No screen capture permission!\n"))
	if w.Reason() != ReasonScreenRecording {
		t.Fatalf("late fault not recorded: %q", w.Reason())
	}
}

func TestDarwinEngineWatcherFatalBeforeReadyNamesTheGrant(t *testing.T) {
	w := newDarwinEngineWatcher(nil)
	_, _ = w.Write([]byte("Error: No screen capture permission!\nError: Please activate it in 'System Preferences' -> 'Privacy' -> 'Screen Recording'\n"))
	err := w.wait(time.Second, make(chan struct{}), "/x/engine.log")
	if err == nil || !strings.Contains(err.Error(), ReasonScreenRecording) || !strings.Contains(err.Error(), "/x/engine.log") {
		t.Fatalf("err = %v", err)
	}
	// Exit with no verdict is reported as such, not as a timeout.
	w = newDarwinEngineWatcher(nil)
	exited := make(chan struct{})
	close(exited)
	if err := w.wait(time.Second, exited, "l"); err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("err = %v", err)
	}
}

func TestEndDarwinSurvivorsTouchesNothingOutOfScope(t *testing.T) {
	calls := 0
	list := func() ([]psEntry, error) {
		calls++
		return []psEntry{
			{PID: os.Getpid(), UID: os.Getuid() + 1, Comm: "/a/engine"}, // another user: never
			{PID: os.Getpid(), UID: os.Getuid(), Comm: "/a/other"},      // other path: never
		}, nil
	}
	if err := endDarwinSurvivors(os.Getuid(), []string{"/a/engine", "/a/bridge"}, list); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("listed %d times; nothing to wait for", calls)
	}
}

// ---------------------------------------------------------------------------
// Dry run with a fake engine bundle and a fake bridge. Opt-in
// (READYFLEET_DARWIN_DRYRUN=1): it spawns real processes and binds the
// bridge's loopback port, which the unit suite must never do. It proves the
// launch/watch/stop path, the permission verdict and the bounded relaunch
// without a real engine, a real bridge or the C2.

const fakeEngineScript = `#!/bin/sh
# Fake SolarBeam engine for the ReadyFleet darwin manager dry run.
if [ "$1" = "--help" ]; then echo "fake engine --help"; exit 0; fi
echo "Info: fake engine start ws=$SOLARBEAM_BRIDGE_WS machine=$SOLARBEAM_MACHINE_ID pointer=${SOLARBEAM_INPUT_POINTER-unset}"
case "$FAKE_ENGINE_MODE" in
  noperm)
    echo "Error: No screen capture permission!"
    echo "Error: Please activate it in 'System Preferences' -> 'Privacy' -> 'Screen Recording'"
    echo "Error: Platform failed to initialize"
    sleep 1; exit 1 ;;
  crash)
    echo "Info: SolarBeam: entering bridge mode (SOLARBEAM_BRIDGE_WS=$SOLARBEAM_BRIDGE_WS)"
    sleep "${FAKE_ENGINE_CRASH_AFTER:-1}"; echo "Fatal: fake crash"; exit 2 ;;
  *)
    echo "Info: SolarBeam: entering bridge mode (SOLARBEAM_BRIDGE_WS=$SOLARBEAM_BRIDGE_WS)"
    trap 'echo "Info: fake engine got TERM"; exit 0' TERM
    while :; do sleep 1; done ;;
esac
`

const fakeBridgeScript = `#!/bin/sh
# Fake bridge: listen on the loopback WS port so waitForListener succeeds.
if [ "$1" = "--help" ]; then echo "fake bridge --help"; exit 0; fi
echo "fake bridge start session=$SESSION_ID readyapp=$READYAPP_URL"
exec /usr/bin/python3 -I -c '
import signal, socket, sys
s = socket.socket(); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
host, port = __import__("os").environ["BRIDGE_WS_ADDR"].rsplit(":", 1)
s.bind((host, int(port))); s.listen(1)
signal.signal(signal.SIGTERM, lambda *_: sys.exit(3))
signal.pause()
'
`

func dryRun(t *testing.T) string {
	t.Helper()
	if os.Getenv("READYFLEET_DARWIN_DRYRUN") != "1" {
		t.Skip("set READYFLEET_DARWIN_DRYRUN=1 to run the fake-engine dry run (spawns processes, binds 127.0.0.1:9092)")
	}
	// A hand-launched bridge may hold 9092 on this Mac (it did on 9 Nov 2026,
	// another builder's); the dry run takes a free loopback port instead and
	// tells the fake bridge and fake engine about it the way production does
	// (BRIDGE_WS_ADDR / SOLARBEAM_BRIDGE_WS).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	prevAddr, prevURL := bridgeWSAddr, bridgeWSURL
	bridgeWSAddr, bridgeWSURL = addr, "ws://"+addr+"/bridge"
	t.Cleanup(func() { bridgeWSAddr, bridgeWSURL = prevAddr, prevURL })
	dir, err := os.MkdirTemp("", "rfdry")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Logf("dry run bridge address %s", addr)
	return dir
}

func dryRunManager(t *testing.T, dir string) *Manager {
	_, exe := writeFakeBundle(t, dir, fakeEngineScript)
	bridge := writeFakeBridge(t, dir, fakeBridgeScript)
	m := New(Paths{StateDir: dir, Root: dir, Engine: exe, Bridge: bridge, Mode: ModeInteractive, DarwinHostEnabled: true})
	t.Cleanup(func() {
		m.mu.Lock()
		_ = m.stopSolarbeamInteractive()
		m.mu.Unlock()
	})
	return m
}

func TestDryRunLaunchWatchStop(t *testing.T) {
	dir := dryRun(t)
	t.Setenv("FAKE_ENGINE_MODE", "run")
	t.Setenv("SOLARBEAM_INPUT_POINTER", "1") // must NOT reach the children
	m := dryRunManager(t, dir)
	changes := make(chan struct{}, 8)
	m.SetSessionListener(func() { changes <- struct{}{} })
	ctx := context.Background()
	args := proto.SolarbeamStartArgs{SessionID: "dry-1", MachineID: "mac-1", ReadyappURL: "https://example.invalid", BundleToken: "t"}
	started := time.Now()
	if err := m.Start(ctx, args); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("start took %s", time.Since(started))
	<-changes
	st := m.Status(ctx)
	if !st.Running || st.CurrentSessionID != "dry-1" || st.EngineVersion != "2.1.18" {
		t.Fatalf("status after start = %+v", st)
	}
	if !m.bridgeProcessRunning() {
		t.Fatal("bridge not running after start")
	}
	if !m.HostAvailability().Available {
		t.Fatalf("availability = %+v", m.HostAvailability())
	}
	if err := m.Stop(ctx, proto.StopSolarbeamArgs{SessionID: "dry-1"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	<-changes
	if m.engineProcessRunning() || m.bridgeProcessRunning() || m.Status(ctx).Running {
		t.Fatal("children still running after stop")
	}
	engineLog, err := os.ReadFile(filepath.Join(DarwinLogDir(dir), "engine.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"entering bridge mode", "pointer=unset", "machine=mac-1", "got TERM"} {
		if !strings.Contains(string(engineLog), want) {
			t.Errorf("engine.log lacks %q:\n%s", want, engineLog)
		}
	}
	bridgeLog, err := os.ReadFile(filepath.Join(DarwinLogDir(dir), "bridge.log"))
	if err != nil || !strings.Contains(string(bridgeLog), "session=dry-1") {
		t.Errorf("bridge.log = %q, %v", bridgeLog, err)
	}
}

func TestDryRunMissingScreenRecordingIsReportedNotLooped(t *testing.T) {
	dir := dryRun(t)
	t.Setenv("FAKE_ENGINE_MODE", "noperm")
	m := dryRunManager(t, dir)
	ctx := context.Background()
	args := proto.SolarbeamStartArgs{SessionID: "dry-2", MachineID: "mac-1", BundleToken: "t"}
	err := m.Start(ctx, args)
	if err == nil || !strings.Contains(err.Error(), ReasonScreenRecording) {
		t.Fatalf("Start = %v, want the Screen Recording reason", err)
	}
	if m.engineProcessRunning() || m.bridgeProcessRunning() {
		t.Fatal("children left running after a failed start")
	}
	if a := m.HostAvailability(); a.Available || a.Reason != ReasonScreenRecording {
		t.Fatalf("availability = %+v", a)
	}
	// A retry inside the cooldown is refused by prepare: no bridge is launched.
	err = m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "dry-3", MachineID: "mac-1", BundleToken: "t"})
	if err == nil || !strings.Contains(err.Error(), "ago") || m.bridgeProcessRunning() {
		t.Fatalf("retry = %v (bridge running %v)", err, m.bridgeProcessRunning())
	}
}

func TestDryRunEngineCrashIsRelaunchedThenGivenUp(t *testing.T) {
	dir := dryRun(t)
	t.Setenv("FAKE_ENGINE_MODE", "crash")
	t.Setenv("FAKE_ENGINE_CRASH_AFTER", "1")
	m := dryRunManager(t, dir)
	m.darwin.policy = engineRestartPolicy{MaxRestarts: 2, Base: 100 * time.Millisecond, Cap: 200 * time.Millisecond}
	ended := make(chan struct{}, 8)
	m.SetSessionListener(func() { ended <- struct{}{} })
	ctx := context.Background()
	if err := m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "dry-4", MachineID: "mac-1", BundleToken: "t"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-ended // start
	// Expect: crash at ~1 s, relaunch 1 (ready), crash, relaunch 2, crash, give up.
	select {
	case <-ended:
	case <-time.After(15 * time.Second):
		t.Fatal("session was not ended by the supervisor")
	}
	m.mu.Lock()
	restarts, fault, sid, last := m.darwin.restarts, m.darwin.fault, m.currentSessionID, m.lastStoppedSessionID
	m.mu.Unlock()
	if restarts != 3 || sid != "" || last != "dry-4" || !strings.Contains(fault.Reason, "giving up") {
		t.Fatalf("restarts=%d fault=%+v current=%q last=%q", restarts, fault, sid, last)
	}
	if m.engineProcessRunning() || m.bridgeProcessRunning() {
		t.Fatal("children still running after the supervisor gave up")
	}
	// ReadyApp's later stop replays as success.
	if err := m.Stop(ctx, proto.StopSolarbeamArgs{SessionID: "dry-4"}); err != nil {
		t.Fatalf("stop after local end: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(DarwinLogDir(dir), "engine.log"))
	if n := strings.Count(string(log), "entering bridge mode"); n != 3 {
		t.Errorf("engine.log shows %d launches, want 3:\n%s", n, log)
	}
}
