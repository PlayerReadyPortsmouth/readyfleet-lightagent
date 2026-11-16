package solarbeam

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type wdRig struct {
	now        time.Time
	sample     WatchdogSample
	sampleErr  error
	pipeLost   bool
	busy       bool
	restartErr error
	restarts   []string
	events     []WatchdogEvent
	w          *Watchdog
}

func newRig() *wdRig {
	r := &wdRig{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	r.w = NewWatchdog(WatchdogDeps{
		Now:    func() time.Time { return r.now },
		Sample: func(context.Context) (WatchdogSample, error) { return r.sample, r.sampleErr },
		LogMarkers: func() (bool, bool) {
			p := r.pipeLost
			r.pipeLost = false
			return p, false
		},
		InstallBusy: func() bool { return r.busy },
		Restart: func(_ context.Context, reason string) error {
			r.restarts = append(r.restarts, reason)
			return r.restartErr
		},
		Report: func(_ context.Context, e WatchdogEvent) { r.events = append(r.events, e) },
	})
	return r
}

func (r *wdRig) tickFor(d time.Duration) {
	for end := r.now.Add(d); r.now.Before(end); {
		r.now = r.now.Add(10 * time.Second)
		r.w.Tick(context.Background())
	}
}

func u(n uint64) *uint64 { return &n }

func viewing(frames uint64) WatchdogSample {
	return WatchdogSample{Viewers: 1, Live: true, Frames: u(frames), Running: true}
}

func TestWatchdog_StallWithViewerRestartsAfter60s(t *testing.T) {
	r := newRig()
	r.sample = viewing(5)
	r.tickFor(50 * time.Second)
	if len(r.restarts) != 0 {
		t.Fatalf("restarted before 60s: %v", r.restarts)
	}
	r.tickFor(30 * time.Second)
	if len(r.restarts) != 1 || r.restarts[0] != "stalled" {
		t.Fatalf("restarts = %v, want [stalled]", r.restarts)
	}
	if len(r.events) != 1 || r.events[0].ID != WatchdogEventRestart {
		t.Fatalf("events = %+v", r.events)
	}
}

func TestWatchdog_NoRestartWhenFramesFlowOrNoViewerOrUnknown(t *testing.T) {
	r := newRig()
	n := uint64(1)
	for i := 0; i < 20; i++ { // frames advancing
		n += 30
		r.sample = viewing(n)
		r.tickFor(10 * time.Second)
	}
	r.sample = WatchdogSample{Viewers: 0, Live: true, Frames: u(n), Running: true}
	r.tickFor(5 * time.Minute)
	r.sample = WatchdogSample{Viewers: 1, Live: true, Running: true} // old bridge: no counter
	r.tickFor(5 * time.Minute)
	r.sample, r.sampleErr = viewing(n), errors.New("status unreadable")
	r.tickFor(5 * time.Minute)
	if len(r.restarts) != 0 {
		t.Fatalf("unexpected restarts: %v", r.restarts)
	}
}

func TestWatchdog_PipeLostThatNeverRecoversRestarts(t *testing.T) {
	r := newRig()
	r.sample = WatchdogSample{} // no session
	r.pipeLost = true
	r.tickFor(90 * time.Second)
	if len(r.restarts) != 1 || r.restarts[0] != "helper_pipe_lost" {
		t.Fatalf("restarts = %v", r.restarts)
	}
}

func TestWatchdog_PipeLostThatRecoversDoesNotRestart(t *testing.T) {
	r := newRig()
	r.sample = viewing(1)
	r.tickFor(10 * time.Second)
	r.pipeLost = true
	for i := uint64(1); i <= 30; i++ { // frames flow again
		r.sample = viewing(500 * i)
		r.tickFor(10 * time.Second)
	}
	if len(r.restarts) != 0 {
		t.Fatalf("restarted despite recovery: %v", r.restarts)
	}
}

func TestWatchdog_WaitsOutDriverInstall(t *testing.T) {
	r := newRig()
	r.busy = true
	r.sample = viewing(1)
	r.tickFor(10 * time.Minute)
	if len(r.restarts) != 0 {
		t.Fatalf("restarted during install: %v", r.restarts)
	}
	r.busy = false
	r.tickFor(10 * time.Second)
	if len(r.restarts) != 1 {
		t.Fatalf("want restart once install ends, got %v", r.restarts)
	}
}

func TestWatchdog_BackoffAndHourlyCap(t *testing.T) {
	r := newRig()
	r.sample = viewing(1)
	r.tickFor(3 * time.Minute) // first restart at ~60s
	r.tickFor(2 * time.Minute) // still inside the 5 min backoff
	if len(r.restarts) != 1 {
		t.Fatalf("backoff not honoured: %d restarts", len(r.restarts))
	}
	r.tickFor(50 * time.Minute)
	if len(r.restarts) != watchdogMaxPerHour {
		t.Fatalf("restarts = %d, want cap %d", len(r.restarts), watchdogMaxPerHour)
	}
	suppressed := 0
	for _, e := range r.events {
		if e.ID == WatchdogEventSuppressed {
			suppressed++
		}
	}
	if suppressed != 1 {
		t.Fatalf("suppressed events = %d, want 1", suppressed)
	}
}

func TestWatchdog_FailedRestartIsAudited(t *testing.T) {
	r := newRig()
	r.restartErr = errors.New("boom")
	r.sample = viewing(1)
	r.tickFor(90 * time.Second)
	if len(r.events) != 1 || r.events[0].ID != WatchdogEventFailed {
		t.Fatalf("events = %+v", r.events)
	}
}

func TestRestartEngine_ForcesRestartPastHealthyShortcut(t *testing.T) {
	m := New(Paths{})
	m.prepareFn = func() error { return nil }
	m.healthyFn = func() bool { return true }
	var healthyDuring = true
	called := false
	m.preloadFn = func(id string) error {
		called = id == "m1"
		healthyDuring = m.healthyFn()
		return nil
	}
	if err := m.RestartEngine(context.Background(), "m1", "stalled"); err != nil {
		t.Fatal(err)
	}
	if !called || healthyDuring {
		t.Fatalf("called=%v healthyDuring=%v; want forced restart", called, healthyDuring)
	}
	if !m.healthyFn() {
		t.Fatal("healthyFn not restored")
	}
	if err := New(Paths{Mode: ModeInteractive}).RestartEngine(context.Background(), "m1", "x"); err == nil {
		t.Fatal("BYOD must refuse")
	}
}

func TestLogTail_OnlyNewLinesAndRotation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sunshine.log")
	os.WriteFile(p, []byte("old "+markerPipeLost+"\n"), 0o600)
	l := &logTail{path: p}
	if a, _ := l.markers(); a {
		t.Fatal("history must not trigger")
	}
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("x " + markerPipeLost + "\n")
	f.Close()
	if a, _ := l.markers(); !a {
		t.Fatal("new pipe-lost line missed")
	}
	if a, _ := l.markers(); a {
		t.Fatal("marker reported twice")
	}
	os.WriteFile(p, []byte(markerNoCapture+"\n"), 0o600) // rotated, smaller
	if _, b := l.markers(); !b {
		t.Fatal("rotated file not re-read")
	}
}
