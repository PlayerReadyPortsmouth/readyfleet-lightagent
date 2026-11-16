package solarbeam

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The mode gate is the whole ruling: warm engine is a managed-venue-PC
// behaviour, and a mentor's personal BYOD machine must never get an
// always-running capture engine as a default.
func TestWarmEngineEnabled_ManagedOnlyAndRevertible(t *testing.T) {
	tests := []struct {
		name  string
		paths Paths
		want  bool
	}{
		{"managed default (zero value)", Paths{}, true},
		{"managed explicitly", Paths{Mode: ModeManaged}, true},
		{"managed reverted by the kill switch", Paths{DisableWarmEngine: true}, false},
		{"BYOD interactive", Paths{Mode: ModeInteractive}, false},
		{"BYOD interactive with the switch off too", Paths{Mode: ModeInteractive, DisableWarmEngine: true}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := New(tc.paths).warmEngineEnabled(); got != tc.want {
				t.Fatalf("warmEngineEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreload_ManagedPreparesThenPreloads(t *testing.T) {
	m := New(Paths{})
	var order []string
	var gotMachineID string
	m.prepareFn = func() error { order = append(order, "prepare"); return nil }
	m.preloadFn = func(machineID string) error {
		order = append(order, "preload")
		gotMachineID = machineID
		return nil
	}

	if err := m.Preload(context.Background(), "uuid-1"); err != nil {
		t.Fatalf("Preload: %v", err)
	}
	if got := strings.Join(order, ","); got != "prepare,preload" {
		t.Fatalf("step order = %q, want %q", got, "prepare,preload")
	}
	if gotMachineID != "uuid-1" {
		t.Fatalf("preloadFn machine id = %q, want %q", gotMachineID, "uuid-1")
	}
}

// BYOD (§5.1): no preload at all — the engine only ever runs while a
// session is live on someone's personal machine.
func TestPreload_InteractiveIsNoOp(t *testing.T) {
	m := New(Paths{Mode: ModeInteractive})
	m.prepareFn = func() error { t.Fatal("ModeInteractive must not prepare the engine at agent start"); return nil }
	m.preloadFn = func(string) error { t.Fatal("ModeInteractive must not preload the engine"); return nil }

	if err := m.Preload(context.Background(), "uuid-1"); err != nil {
		t.Fatalf("Preload on ModeInteractive must be a silent no-op, got %v", err)
	}
}

func TestPreload_WarmDisabledIsNoOp(t *testing.T) {
	m := New(Paths{DisableWarmEngine: true})
	m.prepareFn = func() error { t.Fatal("a reverted machine must not preload"); return nil }
	m.preloadFn = func(string) error { t.Fatal("a reverted machine must not preload"); return nil }

	if err := m.Preload(context.Background(), "uuid-1"); err != nil {
		t.Fatalf("Preload with warm-engine disabled must be a no-op, got %v", err)
	}
}

// An empty machine id would write SOLARBEAM_MACHINE_ID= into bridge.env,
// and the engine validates session tokens against that audience — a warm
// engine holding a blank one would reject every session.
func TestPreload_RequiresMachineID(t *testing.T) {
	m := New(Paths{})
	m.prepareFn = func() error { t.Fatal("must reject before preparing"); return nil }
	m.preloadFn = func(string) error { t.Fatal("must reject before preloading"); return nil }

	err := m.Preload(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "machine id required") {
		t.Fatalf("err = %v, want a machine-id-required error", err)
	}
}

func TestPreload_PrepareFailureSkipsPreload(t *testing.T) {
	m := New(Paths{})
	preloaded := false
	m.prepareFn = func() error { return errors.New("engine service host not installed") }
	m.preloadFn = func(string) error { preloaded = true; return nil }

	err := m.Preload(context.Background(), "uuid-1")
	if preloaded {
		t.Fatal("preload must not run when prepare fails")
	}
	if err == nil || !strings.HasPrefix(err.Error(), "engine: ") {
		t.Fatalf("err = %v, want prefixed with 'engine: '", err)
	}
}

func TestPreload_FailureIsReportedNotSwallowed(t *testing.T) {
	m := New(Paths{})
	m.prepareFn = func() error { return nil }
	m.preloadFn = func(string) error { return errors.New("SunshineService did not reach running") }

	err := m.Preload(context.Background(), "uuid-1")
	if err == nil || !strings.HasPrefix(err.Error(), "engine preload: ") {
		t.Fatalf("err = %v, want prefixed with 'engine preload: '", err)
	}
}

// ModeInteractive's session lifecycle is untouched by the warm-engine
// work: Start still launches the engine as a child process and Stop still
// kills it, per session, with no service and no preload involved.
func TestNew_ModeInteractive_KeepsPerSessionEngineLifecycle(t *testing.T) {
	m := New(Paths{Mode: ModeInteractive})
	if reflect.ValueOf(m.engineFn).Pointer() != reflect.ValueOf(m.startEngineInteractive).Pointer() {
		t.Error("ModeInteractive must still start the engine per session")
	}
	if reflect.ValueOf(m.stopFn).Pointer() != reflect.ValueOf(m.stopSolarbeamInteractive).Pointer() {
		t.Error("ModeInteractive must still stop the engine at session end")
	}
}

func TestNew_ModeManaged_WiresWarmEngineSeams(t *testing.T) {
	m := New(Paths{})
	if reflect.ValueOf(m.engineFn).Pointer() != reflect.ValueOf(m.startEngine).Pointer() {
		t.Error("ModeManaged must wire the service-backed startEngine")
	}
	if reflect.ValueOf(m.preloadFn).Pointer() != reflect.ValueOf(m.preloadEngine).Pointer() {
		t.Error("ModeManaged must wire preloadEngine")
	}
	if reflect.ValueOf(m.healthyFn).Pointer() != reflect.ValueOf(m.engineHealthy).Pointer() {
		t.Error("ModeManaged must wire engineHealthy")
	}
	if reflect.ValueOf(m.bridgeRunningFn).Pointer() != reflect.ValueOf(m.bridgeProcessRunning).Pointer() {
		t.Error("ModeManaged must wire bridgeProcessRunning")
	}
	if m.bridgeStatusURL != defaultBridgeStatusURL {
		t.Errorf("bridgeStatusURL = %q, want %q", m.bridgeStatusURL, defaultBridgeStatusURL)
	}
}
