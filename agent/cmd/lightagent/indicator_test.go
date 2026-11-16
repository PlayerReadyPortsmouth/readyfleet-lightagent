package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/sessionindicator"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// fakeIndicator records what the driver asked for, without a compositor.
type fakeIndicator struct {
	mu       sync.Mutex
	shown    []sessionindicator.Notice
	hides    int
	closes   int
	showErr  error
	visible  bool
	hideErr  error
	closeErr error
}

func (f *fakeIndicator) Show(n sessionindicator.Notice) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.showErr != nil {
		return f.showErr
	}
	f.shown = append(f.shown, n)
	f.visible = true
	return nil
}

func (f *fakeIndicator) Hide() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hides++
	f.visible = false
	return f.hideErr
}

func (f *fakeIndicator) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return f.closeErr
}

func (f *fakeIndicator) state() (shown []sessionindicator.Notice, hides int, visible bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sessionindicator.Notice(nil), f.shown...), f.hides, f.visible
}

type fakeProbe struct {
	viewers int
	err     error
}

func (f fakeProbe) RemoteViewers(context.Context) (int, error) { return f.viewers, f.err }

func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func TestIndicatorTickShowsTheBarWhileSomeoneIsWatching(t *testing.T) {
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{viewers: 2}, false, silentLogger())
	d.tick(context.Background())

	shown, _, visible := ind.state()
	if len(shown) != 1 || !visible {
		t.Fatalf("shown=%d visible=%v, want the bar up", len(shown), visible)
	}
}

func TestIndicatorTickHidesTheBarWhenNobodyIsWatching(t *testing.T) {
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{viewers: 0}, false, silentLogger())
	d.tick(context.Background())

	shown, hides, visible := ind.state()
	if len(shown) != 0 || hides != 1 || visible {
		t.Fatalf("shown=%d hides=%d visible=%v, want the bar down", len(shown), hides, visible)
	}
}

func TestIndicatorTickCarriesTheAnnouncedViewerName(t *testing.T) {
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{viewers: 1}, false, silentLogger())
	d.SetViewer("  Aurora Nicholas  ")
	d.tick(context.Background())

	shown, _, _ := ind.state()
	if len(shown) != 1 || shown[0].Viewer != "Aurora Nicholas" {
		t.Fatalf("shown=%+v, want the trimmed announced name", shown)
	}
}

func TestIndicatorForgetsTheViewerNameWhenTheSessionEnds(t *testing.T) {
	// Otherwise the next session's bar would name the previous session's
	// viewer, which is worse than naming nobody.
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{viewers: 0}, false, silentLogger())
	d.SetViewer("Aurora Nicholas")
	d.tick(context.Background())

	if got := d.currentViewer(); got != "" {
		t.Fatalf("viewer = %q after the session ended, want it cleared", got)
	}
}

func TestIndicatorTickLeavesTheBarAloneWhenTheProbeFails(t *testing.T) {
	// An unreadable bridge is not evidence either way. Raising the bar
	// would cry wolf; lowering it would hide a live session.
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{err: errors.New("bridge wedged")}, false, silentLogger())
	d.tick(context.Background())

	shown, hides, _ := ind.state()
	if len(shown) != 0 || hides != 0 {
		t.Fatalf("shown=%d hides=%d, want neither on an unreadable probe", len(shown), hides)
	}
}

func TestGateSessionStartAllowsWhenNotRequired(t *testing.T) {
	// The shipped default: a broken indicator is logged, not fatal.
	ind := &fakeIndicator{showErr: errors.New("no zwlr_layer_shell_v1")}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, false, silentLogger())
	if err := d.GateSessionStart(); err != nil {
		t.Fatalf("GateSessionStart = %v, want nil when require_session_indicator is unset", err)
	}
}

func TestGateSessionStartRefusesWhenRequired(t *testing.T) {
	sentinel := errors.New("no zwlr_layer_shell_v1")
	ind := &fakeIndicator{showErr: sentinel}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, true, silentLogger())
	err := d.GateSessionStart()
	if !errors.Is(err, sentinel) {
		t.Fatalf("GateSessionStart = %v, want it to wrap %v", err, sentinel)
	}
}

func TestGateSessionStartAllowsWhenTheIndicatorWorks(t *testing.T) {
	ind := &fakeIndicator{}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, true, silentLogger())
	if err := d.GateSessionStart(); err != nil {
		t.Fatalf("GateSessionStart = %v, want nil", err)
	}
	// The preflight must not leave the bar up: nobody is watching yet.
	_, _, visible := ind.state()
	if visible {
		t.Error("preflight left the bar on screen before anyone joined")
	}
}

func TestNilDriverIsInert(t *testing.T) {
	// nil is the shipped default (session_indicator absent). Every call
	// site has to tolerate it.
	var d *sessionIndicatorDriver
	if err := d.GateSessionStart(); err != nil {
		t.Fatalf("GateSessionStart on a nil driver = %v, want nil", err)
	}
	if err := d.Preflight(); err != nil {
		t.Fatalf("Preflight on a nil driver = %v, want nil", err)
	}
}

func TestStartSessionIndicatorReturnsNilWhenDisabled(t *testing.T) {
	if got := startSessionIndicator(false, false, fakeProbe{}, silentLogger()); got != nil {
		t.Fatalf("startSessionIndicator(enabled=false) = %v, want nil", got)
	}
}

func TestSessionStartedNotificationNamesTheViewerAndStillNotifies(t *testing.T) {
	// The contract on the open Kind field: for session_started, Body IS
	// the viewer's display name. It must ALSO remain a normal notice, so
	// a machine with no indicator loses nothing.
	env := makeShowNotificationEnv(t, proto.ShowNotificationArgs{
		Kind: sessionStartedKind, Title: "Someone has joined your SolarBeam session",
		Body: "Aurora Nicholas", Severity: "info",
	})
	notify := &recordingNotifier{}
	d := newSessionIndicatorDriver(&fakeIndicator{}, fakeProbe{}, false, silentLogger())

	res := collectResult(t, func(send func(context.Context, proto.Envelope) error) {
		runShowNotification(context.Background(), env, send, notify, &recordingGate{}, d)
	})
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d err=%s", res.ExitCode, res.Error)
	}
	if got := d.currentViewer(); got != "Aurora Nicholas" {
		t.Errorf("viewer = %q, want the announced name", got)
	}
	notify.mu.Lock()
	defer notify.mu.Unlock()
	if len(notify.calls) != 1 {
		t.Fatalf("Notify called %d times, want the notice still shown", len(notify.calls))
	}
}

// skipReason is what a Show returns when the machine is locked or asleep.
func skipReason(reason string) error {
	return &sessionindicator.Skipped{
		Reason: reason,
		State:  sessionindicator.ScreenState{Locked: sessionindicator.Yes},
	}
}

func TestGateSessionStartAllowsASkipEvenWhenRequired(t *testing.T) {
	// The load-bearing test for Aurora's ruling. require_session_indicator
	// exists to stop a session running unannounced in front of somebody;
	// there is nobody in front of a locked screen, so a skip must not
	// refuse the session even under the strict policy. If this ever goes
	// red, every locked BYOD machine has become a machine that refuses
	// support.
	ind := &fakeIndicator{showErr: skipReason("the session is locked")}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, true, silentLogger())

	if err := d.GateSessionStart(); err != nil {
		t.Fatalf("GateSessionStart = %v, want nil: a skip is not an inability to show", err)
	}
}

func TestGateSessionStartAllowsASkipWhenNotRequired(t *testing.T) {
	ind := &fakeIndicator{showErr: skipReason("the displays are asleep")}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, false, silentLogger())

	if err := d.GateSessionStart(); err != nil {
		t.Fatalf("GateSessionStart = %v, want nil", err)
	}
}

func TestGateSessionStartStillRefusesARealFailureWhenRequired(t *testing.T) {
	// The other side of the same coin: adding the skip path must not have
	// blunted the policy for the case it was written for.
	sentinel := errors.New("no zwlr_layer_shell_v1")
	ind := &fakeIndicator{showErr: sentinel}
	d := newSessionIndicatorDriver(ind, fakeProbe{}, true, silentLogger())

	err := d.GateSessionStart()
	if !errors.Is(err, sentinel) {
		t.Fatalf("GateSessionStart = %v, want it to wrap %v", err, sentinel)
	}
}

func TestIndicatorTickToleratesASkip(t *testing.T) {
	// A skipped tick is not a failed tick: the driver carries on polling
	// and puts the bar up as soon as the person is back.
	ind := &fakeIndicator{showErr: skipReason("the session is locked")}
	d := newSessionIndicatorDriver(ind, fakeProbe{viewers: 1}, false, silentLogger())

	d.tick(context.Background())

	shown, hides, visible := ind.state()
	if len(shown) != 0 || visible {
		t.Errorf("a skipped tick put a bar up: shown=%v visible=%v", shown, visible)
	}
	if hides != 0 {
		t.Errorf("a skipped tick hid the bar %d times; it must leave state alone", hides)
	}
}
