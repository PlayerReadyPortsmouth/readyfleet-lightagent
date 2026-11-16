//go:build linux

package connectionprompt

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/desktopnotify"
)

// NewAsker asks on the Linux desktop, over
// org.freedesktop.Notifications, with Accept and Decline buttons and
// urgency critical — see agent/internal/desktopnotify.
//
// This replaces the build's original non-Windows prompter, which returned
// "yes" unconditionally and displayed nothing: a mentor on Linux was never
// asked and always consented. Everything that stops the question reaching
// a screen now declines instead, and says which path it took.
//
// The light agent runs as the person's own systemd user unit, so
// DBUS_SESSION_BUS_ADDRESS is inherited from their login session and the
// notification lands on the desktop they are actually sitting at. On a
// machine with no session — headless, or nobody logged in — there is
// nothing to inherit, and the request is declined rather than answered on
// their behalf.
func NewAsker(logger *slog.Logger) consent.Asker {
	return desktopnotify.Prompter{Logger: logger}
}
