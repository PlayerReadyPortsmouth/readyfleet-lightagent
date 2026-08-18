package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/hiddenbrowser"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

//go:embed tray-connected.ico
var trayIconConnectedData []byte

//go:embed tray-offline.ico
var trayIconOfflineData []byte

const (
	trayMenuIDToggleConn    = 100
	trayMenuIDQuit          = 101
	trayMenuIDHiddenBrowser = 102
)

// newLightTray stages the tray's icon assets and constructs the
// (not-yet-running) Tray. Split out from runTrayIcon so main.go can build
// the tray before the command handler map — CmdShowNotification's handler
// needs a live *trayicon.Tray to call Notify on, and the handler map is
// built before runTrayIcon would otherwise construct one.
func newLightTray() (tray *trayicon.Tray, connectedIconPath, offlineIconPath string) {
	connectedIconPath, offlineIconPath = writeTrayIcons()
	return &trayicon.Tray{IconPath: connectedIconPath}, connectedIconPath, offlineIconPath
}

// runTrayIcon drives the light agent's system tray icon for its whole
// life: live connection stats, SolarBeam engine status, and a
// disconnect/reconnect toggle (client.SetPaused — see
// internal/transport/client.go). Runs until ctx is cancelled. Errors are
// logged, not fatal — a tray icon failing to render must never take the
// real agent connection down with it, so this never returns an error to
// its caller. tray must be one built by newLightTray, not yet run.
func runTrayIcon(ctx context.Context, tray *trayicon.Tray, connectedIconPath, offlineIconPath string, client *transport.Client, solarbeamMgr *solarbeam.Manager) {
	var paused atomic.Bool
	launcher := hiddenbrowser.NewLauncher(tray, filepath.Join(lightAppDataDir(), "hidden-readyapp-profile"))

	menuItems := func() []trayicon.MenuItem {
		toggleLabel := "Disconnect"
		if paused.Load() {
			toggleLabel = "Reconnect"
		}
		return []trayicon.MenuItem{
			{ID: trayMenuIDToggleConn, Label: toggleLabel, OnClick: func() {
				next := !paused.Load()
				paused.Store(next)
				client.SetPaused(next)
			}},
			{ID: trayMenuIDHiddenBrowser, Label: "Open hidden ReadyApp", OnClick: func() {
				_ = launcher.Open()
			}},
			{Separator: true},
			{ID: trayMenuIDQuit, Label: "Quit ReadyFleet Light Agent", OnClick: func() {
				tray.Quit()
			}},
		}
	}

	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		lastConnected := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats := client.Stats()
				tray.SetTooltip(trayTooltip(stats, solarbeamMgr.Status(ctx)))
				tray.SetMenuItems(menuItems())
				if stats.Connected != lastConnected && connectedIconPath != "" && offlineIconPath != "" {
					lastConnected = stats.Connected
					path := offlineIconPath
					if stats.Connected {
						path = connectedIconPath
					}
					_ = tray.SetIconPath(path)
				}
			}
		}
	}()

	_ = tray.Run(ctx, menuItems())
}

func trayTooltip(stats transport.Stats, sb solarbeam.Status) string {
	connLine := "Not connected"
	if stats.Connected {
		connLine = "Connected"
	}
	sbLine := "SolarBeam: not running"
	if sb.Running {
		sbLine = "SolarBeam: running"
		if sb.EngineVersion != "" {
			sbLine += " (v" + sb.EngineVersion + ")"
		}
	}
	return fmt.Sprintf("ReadyFleet Light Agent v%s\n%s\n%s", agentVersion, connLine, sbLine)
}

// writeTrayIcons stages the embedded tray .ico assets to disk once —
// Shell_NotifyIcon's LoadImageW-based loader needs a real file path, not
// in-memory bytes. Written into the agent's own install directory
// (already writable per-user, no elevation) so they survive between
// runs instead of a temp dir that could get cleaned mid-session. Returns
// ("", "") on any write failure so callers fall back to the stock icon
// rather than fail the whole tray.
func writeTrayIcons() (connectedPath, offlinePath string) {
	dir := lightAppDataDir()
	connected := filepath.Join(dir, "tray-connected.ico")
	offline := filepath.Join(dir, "tray-offline.ico")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", ""
	}
	if err := os.WriteFile(connected, trayIconConnectedData, 0o644); err != nil {
		return "", ""
	}
	if err := os.WriteFile(offline, trayIconOfflineData, 0o644); err != nil {
		return "", ""
	}
	return connected, offline
}
