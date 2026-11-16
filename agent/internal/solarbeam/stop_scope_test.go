package solarbeam

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func startBoundSession(t *testing.T, m *Manager, sid string) {
	t.Helper()
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return nil }
	if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: sid}); err != nil {
		t.Fatal(err)
	}
}

func TestStopRejectsReplacedAndUnknownSession(t *testing.T) {
	for _, mode := range []Mode{ModeManaged, ModeInteractive} {
		m := New(Paths{Mode: mode})
		stopped := 0
		m.stopFn = func() error { stopped++; return nil }
		if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "old"}); err == nil {
			t.Fatal("unknown restart identity accepted")
		}
		startBoundSession(t, m, "old")
		startBoundSession(t, m, "replacement")
		if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "old"}); err == nil {
			t.Fatal("stale stop accepted")
		}
		if stopped != 0 {
			t.Fatal("stale/unknown stop reached platform cleanup")
		}
		if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "replacement"}); err != nil {
			t.Fatal(err)
		}
		if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "replacement"}); err != nil {
			t.Fatalf("successful stop replay: %v", err)
		}
		if stopped != 1 {
			t.Fatalf("cleanup calls=%d", stopped)
		}
	}
}

func TestStartFailureDoesNotAuthorizeWrongCleanup(t *testing.T) {
	for _, phase := range []string{"prepare", "bridge", "engine"} {
		t.Run(phase, func(t *testing.T) {
			m := New(Paths{})
			startBoundSession(t, m, "old")
			stopped := 0
			m.stopFn = func() error { stopped++; return nil }
			fail := func() error { return errors.New("start failure") }
			switch phase {
			case "prepare":
				m.prepareFn = fail
			case "bridge":
				m.launchFn = func(proto.SolarbeamStartArgs) error { return fail() }
			case "engine":
				m.engineFn = func(proto.SolarbeamStartArgs) error { return fail() }
			}
			if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "replacement"}); err == nil {
				t.Fatal("expected failure")
			}
			if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "replacement"}); err == nil {
				t.Fatal("failed replacement acquired stop authority")
			}
			err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "old"})
			if phase == "prepare" {
				if err != nil || stopped != 1 {
					t.Fatal("preparation failure lost untouched session binding")
				}
			} else if err == nil || stopped != 0 {
				t.Fatal("partial replacement retained old stop authority")
			}
		})
	}
}

func TestFailedStopRetainsAuthorityForRetry(t *testing.T) {
	m := New(Paths{})
	startBoundSession(t, m, "current")
	m.stopFn = func() error { return errors.New("temporary stop failure") }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "current"}); err == nil {
		t.Fatal("expected failure")
	}
	m.stopFn = func() error { return nil }
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "current"}); err != nil {
		t.Fatal(err)
	}
}

func TestStaleStopWaitsForReplacementBinding(t *testing.T) {
	m := New(Paths{})
	startBoundSession(t, m, "old")
	launching := make(chan struct{})
	release := make(chan struct{})
	startDone := make(chan error, 1)
	stopDone := make(chan error, 1)
	m.launchFn = func(proto.SolarbeamStartArgs) error { close(launching); <-release; return nil }
	m.stopFn = func() error { t.Error("stale stop reached cleanup"); return nil }
	go func() { startDone <- m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "replacement"}) }()
	<-launching
	go func() { stopDone <- m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "old"}) }()
	select {
	case <-stopDone:
		t.Fatal("stop bypassed launch mutex")
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err == nil {
		t.Fatal("stale stop accepted after replacement completed")
	}
}

func TestSameSessionRetryFailureClearsAuthority(t *testing.T) {
	m := New(Paths{})
	startBoundSession(t, m, "same")
	m.launchFn = func(proto.SolarbeamStartArgs) error { return errors.New("replacement failed") }
	m.stopFn = func() error { t.Error("failed retry reached cleanup"); return nil }
	if err := m.Start(context.Background(), proto.SolarbeamStartArgs{SessionID: "same"}); err == nil {
		t.Fatal("expected failure")
	}
	if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: "same"}); err == nil {
		t.Fatal("failed retry retained stop authority")
	}
}
