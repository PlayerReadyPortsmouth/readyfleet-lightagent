//go:build darwin

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/menuhelper"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
)

// menuAppName is the helper bundle the installer places next to the agent in
// ~/Library/Application Support/ReadyFleet. Source: agent/darwin/menu.
const menuAppName = "ReadyFleet Menu.app"

// menuSocketName is the loopback socket the helper connects to, in the same
// directory (0600, owner only).
const menuSocketName = "menu.sock"

// menuStatusInterval matches the tray's and the session indicator's 3 s poll.
const menuStatusInterval = 3 * time.Second

// menuHelper is the agent's handle on the macOS menu-bar item: the socket
// server and whether prompts may go to it. nil means "no helper installed on
// this Mac", and every call site treats nil as the pre-helper behaviour.
type menuHelper struct {
	server  *menuhelper.Server
	prompts bool
}

// menuHelperExe is the bundle's executable, run directly (not via `open`) so
// the agent can wait on it and relaunch it.
func menuHelperExe(dir string) string {
	return filepath.Join(dir, menuAppName, "Contents", "MacOS", "ReadyFleet Menu")
}

// startMenuHelper looks for the helper bundle beside the agent and, when it
// is there, opens the socket and keeps the helper running until ctx ends.
// Returns nil when there is no bundle or the socket cannot be created: the
// agent carries on exactly as a Mac without a menu-bar item.
func startMenuHelper(ctx context.Context, cfg config.Config, logger *slog.Logger) *menuHelper {
	dir := lightAppDataDir()
	exe := menuHelperExe(dir)
	if info, err := os.Stat(exe); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		logger.Info("menu bar helper not installed; running without a menu-bar item", "expected", filepath.Join(dir, menuAppName))
		return nil
	}
	sock := filepath.Join(dir, menuSocketName)
	srv, err := menuhelper.Listen(sock, logger)
	if err != nil {
		logger.Warn("menu bar helper: could not open its socket; running without a menu-bar item", "err", err)
		return nil
	}
	go srv.Serve(ctx)
	go (&menuhelper.Launcher{
		Exe:    exe,
		Args:   []string{"--socket", sock, "--managed"},
		Logger: logger,
	}).Run(ctx)
	if cfg.MenuPromptsEnabled() {
		logger.Info("connection requests will be put to the person at this Mac through the menu bar (menu_prompts is on)")
	} else {
		logger.Info("connection requests are declined on this Mac (menu_prompts is off); the menu bar shows status only")
	}
	return &menuHelper{server: srv, prompts: cfg.MenuPromptsEnabled()}
}

// consentAsker is what the consent gate asks with on this Mac. The menu-bar
// prompt only when the operator turned menu_prompts on AND a helper is
// installed; otherwise nil, which consent.Gate treats as OutcomeNoPrompter —
// a decline that says why. The pre-helper darwin build was exactly this nil.
func (m *menuHelper) consentAsker(_ *slog.Logger) consent.Asker {
	if m == nil || !m.prompts {
		return nil
	}
	return m.server
}

// statusSource is the subset of the connection client the status line needs.
type statusSource interface {
	Stats() transport.Stats
}

// sessionSource is the subset of *solarbeam.Manager the session-live flag
// needs: who is watching, whether the engine is capturing, and a way to be
// told the moment a session starts or ends.
type sessionSource interface {
	RemoteViewers(ctx context.Context) (int, error)
	Status(ctx context.Context) solarbeam.Status
	SetSessionListener(func())
}

// runMenuStatus publishes the agent's state to the helper whenever it
// changes. The venue is the enrolled venue id and the device name is empty
// until the C2 hands the agent a display name: the helper shows what it is
// given and invents nothing. Runs until ctx ends; never returns an error.
//
// session_live is raised while the engine is RUNNING for a session or a
// remote viewer is present, whichever says so first. Running alone counts:
// the screen is being captured from the moment the engine enters bridge
// mode, before anyone has joined, and the red LIVE label is the only session
// indicator this Mac has (there is no capture-excluded bar and no privacy
// mode). The manager's session listener wakes the loop on start/stop so the
// label follows within milliseconds rather than at the next 3 s poll.
func runMenuStatus(ctx context.Context, m *menuHelper, client statusSource, host sessionSource, cfg config.Config) {
	if m == nil {
		return
	}
	kick := make(chan struct{}, 1)
	host.SetSessionListener(func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	})
	defer host.SetSessionListener(nil)
	ticker := time.NewTicker(menuStatusInterval)
	defer ticker.Stop()
	var last *menuhelper.Status
	viewerLive := false
	for {
		stats := client.Stats()
		if n, err := host.RemoteViewers(ctx); err == nil {
			// An error leaves the flag as it was, the same rule the
			// session indicator applies: neither raising nor lowering a
			// safeguarding signal on a failed probe is a safe default.
			viewerLive = n > 0
		}
		running := host.Status(ctx).Running
		next := menuhelper.Status{
			Online:       stats.Connected,
			Venue:        cfg.VenueID,
			DeviceName:   "",
			SessionLive:  running || viewerLive,
			AgentVersion: agentVersion,
		}
		if last == nil || *last != next {
			m.server.Publish(next)
			last = &next
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-kick:
		}
	}
}

var _ sessionSource = (*solarbeam.Manager)(nil)
