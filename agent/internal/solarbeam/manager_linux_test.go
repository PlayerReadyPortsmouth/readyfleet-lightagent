//go:build linux

package solarbeam

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// stubInputReady pins the /dev/uinput check for the duration of a test. Without
// it these tests depend on the host: logind grants a developer's active seat a
// uaccess ACL on /dev/uinput, so the real check passes on a desktop and fails in
// CI, where there is no seat.
func stubInputReady(t *testing.T, err error) {
	t.Helper()
	previous := inputReadyFn
	inputReadyFn = func() error { return err }
	t.Cleanup(func() { inputReadyFn = previous })
}

func TestPrepareEngineInteractiveRequiresExecutableEngineAndBridge(t *testing.T) {
	stubInputReady(t, nil)
	dir := t.TempDir()
	engine := filepath.Join(dir, "sunshine")
	bridge := filepath.Join(dir, "solarbeam-bridge")
	for _, path := range []string{engine, bridge} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Paths{Engine: engine, Bridge: bridge, Mode: ModeInteractive})
	if err := m.prepareEngineInteractive(); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if err := os.Chmod(bridge, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareEngineInteractive(); err == nil {
		t.Fatal("non-executable bridge accepted")
	}
}

// A host whose privileged integration step never ran must be refused BEFORE the
// bridge touches LiveKit — otherwise the session starts, the young person sees a
// picture, and the mentor discovers remotely that the pointer will not move.
func TestPrepareEngineInteractiveRefusesAHostThatCannotInject(t *testing.T) {
	stubInputReady(t, errors.New("no permission to open /dev/uinput"))
	dir := t.TempDir()
	engine := filepath.Join(dir, "sunshine")
	bridge := filepath.Join(dir, "solarbeam-bridge")
	for _, path := range []string{engine, bridge} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m := New(Paths{Engine: engine, Bridge: bridge, Mode: ModeInteractive})
	err := m.prepareEngineInteractive()
	if err == nil {
		t.Fatal("a host that cannot open /dev/uinput was accepted")
	}
	// The message has to tell a mentor what to DO. This one reaches them through
	// a failed session-start, with no other explanation available.
	if !strings.Contains(err.Error(), "/dev/uinput") {
		t.Errorf("error does not name the device: %v", err)
	}
}

// The real check, run against the host. Skipped rather than failed where the
// device is not reachable, because that is a property of the machine running
// the tests, not of the code — but when it IS reachable, opening it must be
// side-effect free and must not leave a virtual device behind.
func TestOpenUinputIsSideEffectFree(t *testing.T) {
	if err := openUinput(); err != nil {
		t.Skipf("/dev/uinput not usable here: %v", err)
	}
	if err := openUinput(); err != nil {
		t.Fatalf("second open failed, so the first had a side effect: %v", err)
	}
}

func TestBuildEngineCmdCarriesLinuxBridgeEnvironment(t *testing.T) {
	cmd := buildEngineCmd("/opt/solarbeam/sunshine", proto.SolarbeamStartArgs{MachineID: "machine-1"})
	if cmd.Dir != "/opt/solarbeam" {
		t.Fatalf("Dir = %q", cmd.Dir)
	}
	want := map[string]bool{"SOLARBEAM_BRIDGE_WS=" + bridgeWSURL: false, "SOLARBEAM_MACHINE_ID=machine-1": false}
	for _, value := range cmd.Env {
		if _, ok := want[value]; ok {
			want[value] = true
		}
	}
	for value, found := range want {
		if !found {
			t.Errorf("missing environment %q", value)
		}
	}
}

func TestEngineFileVersionReadsBundleVersion(t *testing.T) {
	dir := t.TempDir()
	engine := filepath.Join(dir, "sunshine")
	if err := os.WriteFile(engine, []byte("not executed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("2.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := engineFileVersion(engine)
	if err != nil || got != "2.1.0" {
		t.Fatalf("version = %q, err=%v", got, err)
	}
}
