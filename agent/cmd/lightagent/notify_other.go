//go:build !linux && !darwin

package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// newLightNotifier uses the system tray everywhere except Linux and macOS. On
// Windows that is a real balloon with a working click handler; on any
// other platform it is trayicon's own no-op stub, unchanged — the light
// agent ships for Windows, Linux and macOS only.
func newLightNotifier(tray *trayicon.Tray, _ *slog.Logger) notifier { return tray }
