package solarbeam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func availabilityManager(t *testing.T, mode Mode) *Manager {
	t.Helper()
	m := New(Paths{StateDir: t.TempDir(), Mode: mode})
	m.disableHostFn = func() error { return nil }
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.statusFn = func() bool { return false }
	m.fileVersionFn = func(string) (string, error) { return "", nil }
	return m
}

func TestHostingAvailabilityPersistsWithoutChangingConfig(t *testing.T) {
	for _, mode := range []Mode{ModeManaged, ModeInteractive} {
		t.Run(string(rune('0'+mode)), func(t *testing.T) {
			ctx := context.Background()
			m := availabilityManager(t, mode)
			configPath := filepath.Join(m.paths.StateDir, "config.json")
			const config = `{"machine_id":"original","warm_engine":true}`
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if err := m.SetHostingEnabled(ctx, false); err != nil {
				t.Fatal(err)
			}
			restarted := New(m.paths)
			restarted.prepareFn = func() error { t.Fatal("disabled restart prepared engine"); return nil }
			if err := restarted.Start(ctx, proto.SolarbeamStartArgs{SessionID: "later"}); !errors.Is(err, ErrHostingDisabled) {
				t.Fatalf("start after restart: %v", err)
			}
			if err := restarted.SetRemoteHostingEnabled(ctx, true); mode == ModeInteractive && err == nil {
				t.Fatal("remote enable undid owner refusal")
			} else if mode == ModeManaged && err != nil {
				t.Fatal(err)
			}
			if err := restarted.SetHostingEnabled(ctx, true); err != nil {
				t.Fatal(err)
			}
			if New(m.paths).hostingOff.Load() {
				t.Fatal("local enable not persisted")
			}
			got, err := os.ReadFile(configPath)
			if err != nil || string(got) != config {
				t.Fatalf("config was changed: %s %v", got, err)
			}
		})
	}
}

func TestHostingAvailabilityInvalidStateFailsClosed(t *testing.T) {
	for _, contents := range []string{`{}`, `{"enabled":null}`, `{"enabled":"false"}`, `broken`} {
		t.Run(contents, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, hostingStateName), []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			m := New(Paths{StateDir: dir})
			m.statusFn = func() bool { return false }
			m.fileVersionFn = func(string) (string, error) { return "", nil }
			st := m.Status(context.Background())
			if st.HostingEnabled || st.HostingError == "" {
				t.Fatalf("bad state was trusted: %+v", st)
			}
		})
	}
}

func TestHostingAvailabilityFailureDoesNotClaimOffSuccess(t *testing.T) {
	ctx := context.Background()
	m := availabilityManager(t, ModeManaged)
	m.disableHostFn = func() error { return errors.New("service still running") }
	if err := m.SetHostingEnabled(ctx, false); err == nil {
		t.Fatal("cleanup failure hidden")
	}
	if st := m.Status(ctx); st.HostingEnabled || !strings.Contains(st.HostingError, "service still running") {
		t.Fatalf("dishonest status %+v", st)
	}
	if !New(m.paths).hostingOff.Load() {
		t.Fatal("cleanup failure lost persisted refusal")
	}
	m = availabilityManager(t, ModeManaged)
	m.paths.StateDir = filepath.Join(m.paths.StateDir, "missing")
	cleaned := false
	m.disableHostFn = func() error { cleaned = true; return nil }
	if err := m.SetHostingEnabled(ctx, false); err == nil || !cleaned || !m.hostingOff.Load() {
		t.Fatalf("save failure: err=%v cleaned=%v off=%v", err, cleaned, m.hostingOff.Load())
	}
	if err := m.SetHostingEnabled(ctx, true); err == nil || !m.hostingOff.Load() {
		t.Fatal("failed enable reopened admission")
	}
}

func TestHostingOffInterruptsStartAtEveryStage(t *testing.T) {
	for _, stage := range []string{"prepare", "bridge", "engine"} {
		t.Run(stage, func(t *testing.T) {
			m := availabilityManager(t, ModeManaged)
			entered := make(chan struct{})
			release := make(chan struct{})
			startDone := make(chan error, 1)
			offDone := make(chan error, 1)
			block := func() error { close(entered); <-release; return nil }
			var engineCalls atomic.Int32
			switch stage {
			case "prepare":
				m.prepareFn = block
			case "bridge":
				m.launchFn = func(proto.SolarbeamStartArgs) error { return block() }
			case "engine":
				m.engineFn = func(proto.SolarbeamStartArgs) error { engineCalls.Add(1); return block() }
			}
			if stage != "engine" {
				m.engineFn = func(proto.SolarbeamStartArgs) error { engineCalls.Add(1); return nil }
			}
			cleaned := false
			m.disableHostFn = func() error { cleaned = true; return nil }
			go func() { startDone <- m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "racing"}) }()
			<-entered
			go func() { offDone <- m.SetHostingEnabled(context.Background(), false) }()
			deadline := time.Now().Add(time.Second)
			for !m.hostingOff.Load() && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !m.hostingOff.Load() {
				close(release)
				<-startDone
				<-offDone
				t.Fatal("off did not immediately close admission")
			}
			close(release)
			if err := <-startDone; !errors.Is(err, ErrHostingDisabled) {
				t.Fatalf("inflight start: %v", err)
			}
			if err := <-offDone; err != nil {
				t.Fatal(err)
			}
			if !cleaned || m.currentSessionID != "" {
				t.Fatal("host generation survived off")
			}
			if stage != "engine" && engineCalls.Load() != 0 {
				t.Fatal("engine launched after off request")
			}
		})
	}
}

func TestHostingOffSurvivesEngineUpdateAndPostInstallPreload(t *testing.T) {
	ctx := context.Background()
	m := availabilityManager(t, ModeManaged)
	running := false
	cleanups := 0
	m.disableHostFn = func() error { cleanups++; running = false; return nil }
	m.statusFn = func() bool { return running }
	m.preloadFn = func(string) error { t.Fatal("disabled host was warmed"); return nil }
	m.TrustedSigners = map[string]struct{}{testTrustedSigner: {}}
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error { running = true; return nil }
	if err := m.SetHostingEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(ctx, validEngineUpdateArgs()); err != nil {
		t.Fatal(err)
	}
	if running {
		t.Fatal("engine update restarted disabled host")
	}
	running = true // an external managed installer may restart SunshineService
	if err := m.Preload(ctx, "machine"); err != nil {
		t.Fatal(err)
	}
	if running || cleanups != 3 || !New(m.paths).hostingOff.Load() {
		t.Fatalf("post install lost off: running=%v cleanups=%d", running, cleanups)
	}
}

func TestHostingReconcileStopsRestartSurvivor(t *testing.T) {
	m := availabilityManager(t, ModeInteractive)
	ctx := context.Background()
	if err := m.SetHostingEnabled(ctx, false); err != nil {
		t.Fatal(err)
	}
	restarted := New(m.paths)
	called := false
	restarted.disableHostFn = func() error { called = true; return nil }
	if err := restarted.ReconcileHosting(ctx); err != nil || !called {
		t.Fatalf("reconcile: %v called=%v", err, called)
	}
}

func TestReviewedHostingChangeRejectsReplacementAndABA(t *testing.T) {
	m := availabilityManager(t, ModeInteractive)
	ctx := context.Background()
	initial := m.Status(ctx).HostingRevision
	if initial == "" {
		t.Fatal("missing revision")
	}
	if err := m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "old"}); err != nil {
		t.Fatal(err)
	}
	old := m.Status(ctx)
	if old.CurrentSessionID != "old" || old.HostingRevision == initial {
		t.Fatalf("missing owned session: %+v", old)
	}
	if err := m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "replacement"}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetHostingEnabledIfCurrent(ctx, false, old.HostingRevision); err == nil || m.hostingOff.Load() {
		t.Fatal("stale review stopped replacement")
	}
	current := m.Status(ctx).HostingRevision
	if err := m.SetHostingEnabledIfCurrent(ctx, false, current); err != nil {
		t.Fatal(err)
	}
	if err := m.SetHostingEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := m.SetHostingEnabledIfCurrent(ctx, false, initial); err == nil {
		t.Fatal("ABA empty/enabled state accepted old confirmation")
	}
	if New(m.paths).hostingRevisionLocked() == initial {
		t.Fatal("restart reused revision epoch")
	}
}

func TestReviewedDisconnectRejectsSameSessionRelaunch(t *testing.T) {
	m := availabilityManager(t, ModeInteractive)
	ctx := context.Background()
	calls := 0
	m.stopFn = func() error { calls++; return nil }
	args := proto.SolarbeamStartArgs{SessionID: "same-session"}
	if err := m.Start(ctx, args); err != nil {
		t.Fatal(err)
	}
	old := m.Status(ctx).HostingRevision
	if err := m.Start(ctx, args); err != nil {
		t.Fatal(err)
	}
	if err := m.StopIfCurrent(ctx, proto.StopSolarbeamArgs{SessionID: args.SessionID}, old); err == nil || calls != 0 {
		t.Fatal("old review disconnected new launch")
	}
	current := m.Status(ctx).HostingRevision
	if err := m.StopIfCurrent(ctx, proto.StopSolarbeamArgs{SessionID: args.SessionID}, current); err != nil || calls != 1 {
		t.Fatalf("current review: %v calls=%d", err, calls)
	}
	if err := m.StopIfCurrent(ctx, proto.StopSolarbeamArgs{SessionID: args.SessionID}, current); err == nil || calls != 1 {
		t.Fatal("reviewed action replayed after mutation")
	}
}
