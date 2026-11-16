//go:build linux

package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/desktopnotify"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// recordingSession is a desktopnotify.Session that records what the agent
// posted, so this test proves a notification was actually built and handed
// to a daemon rather than swallowed.
type recordingSession struct {
	mu     sync.Mutex
	posted []desktopnotify.Notification
	err    error
}

func (r *recordingSession) Notify(_ context.Context, n desktopnotify.Notification) (uint32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return 0, r.err
	}
	r.posted = append(r.posted, n)
	return 1, nil
}

func (r *recordingSession) Close(context.Context, uint32) error { return nil }
func (r *recordingSession) Events() <-chan desktopnotify.Event  { return make(chan desktopnotify.Event) }
func (r *recordingSession) Shutdown() error                     { return nil }

func (r *recordingSession) all() []desktopnotify.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]desktopnotify.Notification(nil), r.posted...)
}

// TestNewLightNotifier_IsNotTheNoOpTray is the regression this whole file
// exists for. On Linux, show_notification used to route into trayicon's
// non-Windows stub, whose Notify returns nil having drawn nothing — so the
// agent reported exit 0 for a notification that never existed. The wiring
// must not hand back the tray on Linux.
func TestNewLightNotifier_IsNotTheNoOpTray(t *testing.T) {
	tray := &trayicon.Tray{}

	n := newLightNotifier(tray, nil)

	if _, isTray := n.(*trayicon.Tray); isTray {
		t.Fatal("newLightNotifier returned the tray on Linux; its Notify is a no-op that reports success")
	}
	if _, ok := n.(desktopNotifier); !ok {
		t.Fatalf("newLightNotifier returned %T, want a desktopNotifier", n)
	}
}

func TestDesktopNotifier_PostsARealOneWayNotification(t *testing.T) {
	sess := &recordingSession{}
	d := desktopNotifier{dial: func() (desktopnotify.Session, error) { return sess, nil }}

	if err := d.Notify("SolarBeam session ending", "5 minutes left", trayicon.NotifyInfo, nil); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	posted := sess.all()
	if len(posted) != 1 {
		t.Fatalf("posted %d notifications, want 1", len(posted))
	}
	if posted[0].Summary != "SolarBeam session ending" || posted[0].Body != "5 minutes left" {
		t.Errorf("posted %+v, want the title and body verbatim", posted[0])
	}
	if len(posted[0].Actions) != 0 {
		t.Errorf("actions = %+v, want none — a notice is not a question", posted[0].Actions)
	}
}

// TestDesktopNotifier_ReportsFailureInsteadOfSilence: a notice that could
// not reach a screen must surface as a command failure, not the exit 0 the
// no-op used to report.
func TestDesktopNotifier_ReportsFailureInsteadOfSilence(t *testing.T) {
	d := desktopNotifier{dial: func() (desktopnotify.Session, error) { return nil, desktopnotify.ErrNoSessionBus }}

	if err := d.Notify("t", "b", trayicon.NotifyInfo, nil); err == nil {
		t.Fatal("Notify returned nil with no session bus; that is the silent no-op this replaces")
	}
}

func TestDesktopNotifier_DaemonRefusalIsAnError(t *testing.T) {
	sess := &recordingSession{err: errors.New("daemon said no")}
	d := desktopNotifier{dial: func() (desktopnotify.Session, error) { return sess, nil }}

	if err := d.Notify("t", "b", trayicon.NotifyInfo, nil); err == nil {
		t.Fatal("Notify returned nil despite the daemon refusing it")
	}
}

func TestUrgencyFor(t *testing.T) {
	cases := map[trayicon.NotifySeverity]desktopnotify.Urgency{
		trayicon.NotifyInfo:    desktopnotify.UrgencyNormal,
		trayicon.NotifyWarning: desktopnotify.UrgencyNormal,
		trayicon.NotifyError:   desktopnotify.UrgencyCritical,
	}
	for in, want := range cases {
		if got := urgencyFor(in); got != want {
			t.Errorf("urgencyFor(%v) = %d, want %d", in, got, want)
		}
	}
}
