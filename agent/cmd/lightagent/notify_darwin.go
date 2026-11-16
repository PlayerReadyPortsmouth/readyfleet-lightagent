//go:build darwin

package main

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strings"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// osascriptNotifier renders show_notification as a macOS notification via
// `display notification`. Without it the tray stub would report success for a
// notice nobody saw. The first notification may need approving once in System
// Settings > Notifications (Script Editor); until then osascript still exits 0.
type osascriptNotifier struct{ logger *slog.Logger }

func newLightNotifier(_ *trayicon.Tray, logger *slog.Logger) notifier {
	return osascriptNotifier{logger: logger}
}

func (n osascriptNotifier) Notify(title, body string, _ trayicon.NotifySeverity, _ func()) error {
	out, err := exec.Command("osascript", "-e", notificationScript(title, body)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript notification: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
