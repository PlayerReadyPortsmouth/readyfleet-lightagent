package solarbeam

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

var errBridgeLaunch = errors.New("bridge launch failed")

// The engine launch must run AFTER the bridge is up: the engine's WS client
// does not retry, so the bridge has to be listening on :9092 first. The
// engine also receives the per-session machine id for input-injection auth.
func TestStart_StartsEngineAfterBridge(t *testing.T) {
	want := proto.SolarbeamStartArgs{
		SessionID: "sess-1", MachineID: "uuid-1", BundleToken: "btok",
	}
	m := New(Paths{})
	var order []string
	var engineArgs proto.SolarbeamStartArgs
	m.prepareFn = func() error { order = append(order, "prepare"); return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { order = append(order, "bridge"); return nil }
	m.engineFn = func(a proto.SolarbeamStartArgs) error {
		order = append(order, "engine")
		engineArgs = a
		return nil
	}

	if err := m.Start(context.Background(), want); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, wantOrder := strings.Join(order, ","), "prepare,bridge,engine"; got != wantOrder {
		t.Fatalf("step order = %q, want %q", got, wantOrder)
	}
	if engineArgs != want {
		t.Fatalf("engineFn args = %+v, want %+v", engineArgs, want)
	}
}

func TestStart_LaunchFailureReportedAndSkipsEngine(t *testing.T) {
	m := New(Paths{})
	engineStarted := false
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { return errBridgeLaunch }
	m.engineFn = func(proto.SolarbeamStartArgs) error { engineStarted = true; return nil }

	err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "s", MachineID: "m", BundleToken: "b"})
	if engineStarted {
		t.Fatal("engine must NOT launch when the bridge launch fails")
	}
	if err == nil || !strings.HasPrefix(err.Error(), "bridge launch: ") {
		t.Fatalf("err = %v, want prefixed with 'bridge launch: '", err)
	}
}

func TestStart_EngineFailureReportedWithEnginePrefix(t *testing.T) {
	m := New(Paths{})
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return errors.New("CreateProcessAsUser failed") }

	err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "s", MachineID: "m", BundleToken: "b"})
	if err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("err = %v, want prefixed with 'engine: '", err)
	}
}

func TestStart_PrepareFailureSkipsLaunch(t *testing.T) {
	m := New(Paths{})
	launched := false
	m.prepareFn = func() error { return errors.New("no interactive session") }
	m.launchFn = func(proto.SolarbeamStartArgs) error { launched = true; return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return nil }

	err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "s", MachineID: "m", BundleToken: "b"})
	if launched {
		t.Fatal("bridge must NOT launch when the engine precondition fails")
	}
	if err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("err = %v, want prefixed with 'engine: '", err)
	}
}

func TestStop_StopsAndReportsSuccess(t *testing.T) {
	m := New(Paths{})
	stopped := false
	m.stopFn = func() error { stopped = true; return nil }

	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "sess-1"}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !stopped {
		t.Fatal("stopFn must run for Stop")
	}
}

func TestStop_FailureReportedWithStopPrefix(t *testing.T) {
	m := New(Paths{})
	m.stopFn = func() error { return errors.New("SunshineService stop failed") }

	err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "sess-1"})
	if err == nil || !strings.HasPrefix(err.Error(), "stop: ") {
		t.Fatalf("err = %v, want prefixed with 'stop: '", err)
	}
}

func TestStatus_ReflectsStatusFn(t *testing.T) {
	m := New(Paths{})
	m.statusFn = func() bool { return true }
	if got := m.Status(context.Background()); !got.Running {
		t.Fatalf("Status.Running = %v, want true", got.Running)
	}
	m.statusFn = func() bool { return false }
	if got := m.Status(context.Background()); got.Running {
		t.Fatalf("Status.Running = %v, want false", got.Running)
	}
}

func TestStatus_PrefersLiveFileVersionOverLastUpdated(t *testing.T) {
	m := New(Paths{})
	m.statusFn = func() bool { return false }
	m.lastUpdatedVersion = "1.0.0-from-update"
	m.fileVersionFn = func(string) (string, error) { return "2.0.5-from-disk", nil }

	got := m.Status(context.Background())
	if got.EngineVersion != "2.0.5-from-disk" {
		t.Fatalf("EngineVersion = %q, want the live file version to win over lastUpdatedVersion", got.EngineVersion)
	}
}

func TestStatus_FallsBackToLastUpdatedWhenFileVersionUnreadable(t *testing.T) {
	m := New(Paths{})
	m.statusFn = func() bool { return false }
	m.lastUpdatedVersion = "1.0.0-from-update"
	m.fileVersionFn = func(string) (string, error) { return "", errors.New("engine not installed") }

	got := m.Status(context.Background())
	if got.EngineVersion != "1.0.0-from-update" {
		t.Fatalf("EngineVersion = %q, want fallback to lastUpdatedVersion when the file can't be read", got.EngineVersion)
	}
}

// New's Mode selection is the actual fix for a real bug this session: the
// light agent shared Manager.Start with the managed agent and it
// unconditionally tried to create a Windows service, which fails outright
// without admin rights. These compare method-value pointers (a legitimate
// way to assert "which concrete implementation got wired", short of
// exercising real Windows APIs) rather than duplicating that live bug.
func TestNew_ModeManaged_WiresServiceBackedSeams(t *testing.T) {
	m := New(Paths{})
	if reflect.ValueOf(m.prepareFn).Pointer() != reflect.ValueOf(m.prepareEngine).Pointer() {
		t.Error("ModeManaged (zero value) must wire prepareEngine, not the interactive variant")
	}
	if reflect.ValueOf(m.statusFn).Pointer() != reflect.ValueOf(m.engineServiceRunning).Pointer() {
		t.Error("ModeManaged (zero value) must wire engineServiceRunning, not the interactive variant")
	}
}

func TestNew_ModeInteractive_WiresNoServiceSeams(t *testing.T) {
	m := New(Paths{Mode: ModeInteractive})
	if reflect.ValueOf(m.prepareFn).Pointer() != reflect.ValueOf(m.prepareEngineInteractive).Pointer() {
		t.Error("ModeInteractive must wire prepareEngineInteractive, which never touches the SCM")
	}
	if reflect.ValueOf(m.statusFn).Pointer() != reflect.ValueOf(m.engineProcessRunning).Pointer() {
		t.Error("ModeInteractive must wire engineProcessRunning, which never touches the SCM")
	}
}
