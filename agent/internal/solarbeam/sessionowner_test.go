package solarbeam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// ownedManager is a Manager with every platform seam faked and a real
// StateDir, so the session record behaves as it does on a venue PC.
func ownedManager(t *testing.T, dir string, paths Paths) (*Manager, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	paths.StateDir = dir
	m := New(paths)
	m.recoveredLinkCleanupFn = func() error { return nil }
	bridge := &atomic.Bool{}
	stops := &atomic.Int32{}
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { bridge.Store(true); return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.stopFn = func() error { stops.Add(1); bridge.Store(false); return nil }
	m.statusFn = func() bool { return false }
	m.bridgeRunningFn = bridge.Load
	return m, bridge, stops
}

func recordExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, sessionRecordName))
	return err == nil
}

// The Ports-Admin incident: the agent was restarted while start_solarbeam was
// in flight, the bridge it had launched kept running, and ReadyApp's stop was
// refused as "current session identity unknown". The restarted agent must
// now honour the stop for exactly that session.
func TestRestartMidStartStillHonoursTheMatchingStop(t *testing.T) {
	dir := t.TempDir()
	first, bridge, _ := ownedManager(t, dir, Paths{})
	// Model abrupt process death after the durable record and bridge spawn.
	// Returning an error from Start now runs orderly failure cleanup, which a
	// killed agent cannot run.
	first.saveSessionRecordLocked("7862196b")
	bridge.Store(true)
	if !bridge.Load() || !recordExists(dir) {
		t.Fatal("setup: bridge should be running with ownership recorded")
	}

	restarted, _, stops := ownedManager(t, dir, Paths{})
	restarted.bridgeRunningFn = bridge.Load
	restarted.stopFn = func() error { stops.Add(1); bridge.Store(false); return nil }

	if err := restarted.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "another"}); err == nil {
		t.Fatal("a stop for a different session was accepted")
	}
	if stops.Load() != 0 {
		t.Fatal("mismatched stop reached cleanup")
	}
	if err := restarted.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "7862196b"}); err != nil {
		t.Fatalf("matching stop after restart: %v", err)
	}
	if stops.Load() != 1 || bridge.Load() {
		t.Fatal("stop did not clean up the orphaned bridge")
	}
	if recordExists(dir) {
		t.Fatal("record survived a successful stop")
	}
	if err := restarted.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "7862196b"}); err != nil {
		t.Fatalf("stop replay: %v", err)
	}
}

// The reaper's first pass adopts the orphan at agent start, so Status and the
// tray show it and a later stop is judged against it.
func TestReaperAdoptsOrphanedSessionAtStartup(t *testing.T) {
	dir := t.TempDir()
	first, bridge, _ := ownedManager(t, dir, Paths{})
	startBoundSessionKeepingSeams(t, first, "live")

	restarted, _, _ := ownedManager(t, dir, Paths{})
	restarted.bridgeRunningFn = bridge.Load
	restarted.ReapSession(context.Background())
	if got := restarted.Status(context.Background()).CurrentSessionID; got != "live" {
		t.Fatalf("adopted session = %q", got)
	}
}

// A record whose bridge is gone is stale: it is cleared, and ReadyApp's stop
// for that session succeeds without touching anything.
func TestStaleRecordWithoutBridgeIsClearedAndStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, bridge, _ := ownedManager(t, dir, Paths{})
	startBoundSessionKeepingSeams(t, first, "ended")
	bridge.Store(false) // bridge exited while the agent was down

	restarted, _, stops := ownedManager(t, dir, Paths{})
	restarted.bridgeRunningFn = bridge.Load
	restarted.ReapSession(context.Background())
	if recordExists(dir) {
		t.Fatal("stale record kept")
	}
	if err := restarted.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "ended"}); err != nil {
		t.Fatalf("stop for the ended session: %v", err)
	}
	if err := restarted.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "other"}); err == nil {
		t.Fatal("unknown session stop accepted")
	}
	if stops.Load() != 0 {
		t.Fatal("stale record caused platform cleanup")
	}
}

// When the bridge exits by itself (session ended), the warm-engine agent ends
// the session locally after two consecutive observations, without running
// the by-name bridge kill, and SessionActive stops blocking installs.
func TestReaperEndsWarmSessionWhenBridgeExits(t *testing.T) {
	dir := t.TempDir()
	m, bridge, stops := ownedManager(t, dir, Paths{})
	startBoundSessionKeepingSeams(t, m, "s1")
	bridge.Store(false)

	m.ReapSession(context.Background())
	if m.Status(context.Background()).CurrentSessionID != "s1" {
		t.Fatal("reaped on a single observation")
	}
	m.ReapSession(context.Background())
	if m.Status(context.Background()).CurrentSessionID != "" {
		t.Fatal("session not reaped after confirmation")
	}
	if stops.Load() != 0 {
		t.Fatal("warm reap ran platform cleanup")
	}
	if recordExists(dir) {
		t.Fatal("record kept after reap")
	}
	if m.SessionActive(context.Background()) {
		t.Fatal("session still reported active after the bridge exited")
	}
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "s1"}); err != nil {
		t.Fatalf("ReadyApp's later stop must succeed idempotently: %v", err)
	}
}

// One bad process snapshot must not end a live session.
func TestReaperIgnoresASingleMissedObservation(t *testing.T) {
	m, bridge, _ := ownedManager(t, t.TempDir(), Paths{})
	startBoundSessionKeepingSeams(t, m, "s1")
	for i := 0; i < 5; i++ {
		bridge.Store(false)
		m.ReapSession(context.Background())
		bridge.Store(true)
		m.ReapSession(context.Background())
	}
	if m.Status(context.Background()).CurrentSessionID != "s1" {
		t.Fatal("alternating snapshots ended a live session")
	}
}

// With a per-session engine (warm disabled, or BYOD) the reaper stops it as a
// stop_solarbeam would, and retries if that fails.
func TestReaperStopsPerSessionEngine(t *testing.T) {
	for name, paths := range map[string]Paths{
		"warm-disabled": {DisableWarmEngine: true},
		"interactive":   {Mode: ModeInteractive},
	} {
		t.Run(name, func(t *testing.T) {
			m, bridge, stops := ownedManager(t, t.TempDir(), paths)
			startBoundSessionKeepingSeams(t, m, "s1")
			bridge.Store(false)
			fail := true
			m.stopFn = func() error {
				stops.Add(1)
				if fail {
					return errors.New("service stop timed out")
				}
				return nil
			}
			m.ReapSession(context.Background())
			m.ReapSession(context.Background())
			if stops.Load() != 1 || m.Status(context.Background()).CurrentSessionID != "s1" {
				t.Fatal("failed cleanup must keep the session for a retry")
			}
			fail = false
			m.ReapSession(context.Background())
			m.ReapSession(context.Background())
			if stops.Load() != 2 || m.Status(context.Background()).CurrentSessionID != "" {
				t.Fatalf("stops=%d session=%q", stops.Load(), m.Status(context.Background()).CurrentSessionID)
			}
		})
	}
}

// A new session start replaces the record, so a restart adopts the newest.
func TestRecordFollowsTheNewestStart(t *testing.T) {
	dir := t.TempDir()
	m, _, _ := ownedManager(t, dir, Paths{})
	startBoundSessionKeepingSeams(t, m, "first")
	startBoundSessionKeepingSeams(t, m, "second")
	if got := m.loadSessionRecord(); got != "second" {
		t.Fatalf("record = %q", got)
	}
}

func startBoundSessionKeepingSeams(t *testing.T, m *Manager, sid string) {
	t.Helper()
	if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: sid}); err != nil {
		t.Fatal(err)
	}
}

func TestBridgeExitMeaningNamesTheBridgeCodes(t *testing.T) {
	for code, want := range map[int]string{
		3: "session ended (room deleted/closed, removed, or authority ended)",
		4: "LiveKit connection lost",
		1: "failed or was terminated",
	} {
		if got := BridgeExitMeaning(code); got != want {
			t.Fatalf("code %d = %q", code, got)
		}
	}
}
