//go:build linux

package main

import (
	"context"
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/desktopnotify"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// desktopNotifier renders show_notification as a real Linux desktop
// notification.
//
// The build this replaces routed every notice into trayicon's non-Windows
// stub, whose Notify returns nil without drawing anything — so the agent
// reported exit 0 for a notification that had never existed. Every
// "session ending", "agent updated" and "engine installed" notice sent to
// a Linux BYOD machine since the platform shipped was silently discarded
// and reported as delivered.
type desktopNotifier struct {
	logger *slog.Logger
	// dial is nil in production, where desktopnotify picks its own real
	// session bus; tests supply a scripted one.
	dial desktopnotify.Dialer
}

// newLightNotifier ignores the tray: there is no system tray on Linux
// (trayicon.Tray's non-Windows build is a no-op by design), so the tray
// argument exists only to keep one wiring call across platforms.
func newLightNotifier(_ *trayicon.Tray, logger *slog.Logger) notifier {
	return desktopNotifier{logger: logger}
}

// Notify posts a one-way notice and returns as soon as the daemon has
// taken it. onClick is dropped: a notice carries no actions, so there is
// no click for a desktop to report back. The connection request — the one
// notification that IS a question — never reaches here; runShowNotification
// routes it to the consent gate first.
//
// A returned error surfaces as a failed command upstream. That is the
// point: reporting success for a notice nobody could have seen is what
// hid this gap for a month.
func (d desktopNotifier) Notify(title, body string, severity trayicon.NotifySeverity, _ func()) error {
	n := desktopnotify.Notifier{Logger: d.logger, Dial: d.dial}
	return n.Show(context.Background(), title, body, urgencyFor(severity))
}

// urgencyFor re-maps the tray severity the shared handler speaks back onto
// a desktop urgency. The handler converts the wire's severity string to a
// trayicon.NotifySeverity before it gets here, so this is the second half
// of that round trip rather than a second, competing mapping.
func urgencyFor(severity trayicon.NotifySeverity) desktopnotify.Urgency {
	switch severity {
	case trayicon.NotifyError:
		return desktopnotify.UrgencyCritical
	default:
		return desktopnotify.UrgencyNormal
	}
}
