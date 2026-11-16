//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/gamepaddriver"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/installerui"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

const (
	trayMenuIDGamepadInstall = 110
	trayMenuIDGamepadRemove  = 111
	trayMenuIDGamepadStatus  = 112
)

// gamepadSupport is the tray's "Install gamepad support…" flow. Nothing in it
// runs unless the owner picks the menu item and then presses Continue on the
// consent screen; the Windows administrator prompt that follows is the
// owner's second, separate decision. See internal/gamepaddriver's package doc
// for the trust chain.
type gamepadSupport struct {
	mgr    *gamepaddriver.Manager
	logger *slog.Logger

	mu       sync.Mutex
	cached   gamepaddriver.Status
	cachedAt time.Time
	running  bool
}

const gamepadStatusTTL = 30 * time.Second

func newGamepadSupport(solarbeamMgr *solarbeam.Manager, logger *slog.Logger) *gamepadSupport {
	sessionActive := func() bool {
		return solarbeamMgr.Status(context.Background()).Running
	}
	return &gamepadSupport{
		mgr: &gamepaddriver.Manager{
			Spec:     gamepaddriver.ViGEmBus,
			Deps:     gamepaddriver.DefaultDeps(sessionActive),
			StateDir: filepath.Join(lightAppDataDir(), "gamepad-driver"),
		},
		logger: logger,
	}
}

func (g *gamepadSupport) status() gamepaddriver.Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.cachedAt) < gamepadStatusTTL {
		return g.cached
	}
	st, err := g.mgr.Status()
	if err != nil {
		g.logger.Warn("gamepad driver status", "err", err)
	}
	g.cached, g.cachedAt = st, time.Now()
	return st
}

func (g *gamepadSupport) invalidate() {
	g.mu.Lock()
	g.cachedAt = time.Time{}
	g.mu.Unlock()
}

// tooltipLine is the tray tooltip's gamepad line.
func (g *gamepadSupport) tooltipLine() string {
	st := g.status()
	switch {
	case st.Ready():
		return "Gamepads: ready"
	case st.Installed:
		return "Gamepads: driver too old"
	default:
		return "Gamepads: not installed"
	}
}

// menuItems returns the gamepad entries for the tray menu.
func (g *gamepadSupport) menuItems() []trayicon.MenuItem {
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	st := g.status()
	switch {
	case running:
		return []trayicon.MenuItem{{ID: trayMenuIDGamepadStatus, Label: "Changing gamepad support…", Disabled: true}}
	case st.Ready() && st.InstalledByUs:
		return []trayicon.MenuItem{{ID: trayMenuIDGamepadRemove, Label: "Remove gamepad support…", OnClick: g.startUninstall}}
	case st.Ready():
		// Installed by other software (DS4Windows, a Steam tool…). Not ours
		// to remove; say it works and leave it alone.
		return []trayicon.MenuItem{{ID: trayMenuIDGamepadStatus, Label: "Gamepad support: ready", Disabled: true}}
	case st.Installed:
		return []trayicon.MenuItem{{ID: trayMenuIDGamepadStatus, Label: "Gamepad driver is too old (see Windows Settings > Apps)", Disabled: true}}
	default:
		return []trayicon.MenuItem{{ID: trayMenuIDGamepadInstall, Label: "Install gamepad support…", OnClick: g.startInstall}}
	}
}

func (g *gamepadSupport) begin() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		return false
	}
	g.running = true
	return true
}

func (g *gamepadSupport) finish() {
	g.mu.Lock()
	g.running = false
	g.cachedAt = time.Time{}
	g.mu.Unlock()
}

const gamepadWizardTitle = "ReadyFleet: gamepad support"

// gamepadConsentText is the owner-facing explanation. Plain language, and it
// tells them what the Windows prompt should say so they can spot one that
// does not match.
func gamepadConsentText(spec gamepaddriver.Spec) string {
	return fmt.Sprintf(
		"For a game controller to work in a SolarBeam session on this PC, Windows needs a virtual-gamepad driver called %s. "+
			"It is free third-party software, digitally signed by its maker, %s, and widely used by game-streaming tools.\n\n"+
			"• Windows will ask you for administrator permission. The prompt should name %s as the verified publisher. If it names anyone else, choose No.\n"+
			"• It adds one driver and nothing else: no background program, no account, no change to your files.\n"+
			"• The driver does nothing until a SolarBeam session asks for a controller.\n"+
			"• You can remove it at any time from this menu (Remove gamepad support), or from Windows Settings > Apps.\n\n"+
			"Choose Continue to download it (about %.0f MB) and check it is the genuine, signed release.",
		spec.Name, spec.Publisher, spec.Publisher, float64(spec.SizeBytes)/1e6)
}

func (g *gamepadSupport) startInstall() {
	if !g.begin() {
		return
	}
	go func() {
		defer g.finish()
		w := installerui.Wizard{Title: gamepadWizardTitle}
		if !w.ShowConfirm("Install gamepad support on this PC?", gamepadConsentText(g.mgr.Spec)) {
			g.logger.Info("gamepad driver install: owner chose not to continue")
			return
		}
		var outcome gamepaddriver.Outcome
		err := w.RunWithProgress(4, func(report func(string)) error {
			var err error
			outcome, err = g.mgr.Install(context.Background(), report)
			return err
		})
		g.invalidate()
		g.logger.Info("gamepad driver install finished", "outcome", int(outcome), "err", errString(err))
		switch {
		case errors.Is(err, gamepaddriver.ErrSessionActive):
			w.ShowResult(false, "Not now: a SolarBeam session is running", "Try again after the session has ended. Nothing was changed.")
		case err != nil:
			w.ShowResult(false, "Gamepad support was not installed", err.Error()+"\n\nNothing else on this PC was changed. Questions: readyapp@player-ready.co.uk")
		case outcome == gamepaddriver.OutcomeDeclined:
			w.ShowResult(true, "Nothing was installed", "Administrator permission was not given, so nothing on this PC changed. You can try again from the ReadyFleet menu at any time.")
		case outcome == gamepaddriver.OutcomeRebootRequired:
			w.ShowResult(true, "Restart this PC to finish", "The gamepad driver is installed, but Windows needs a restart before controllers will work.")
		case outcome == gamepaddriver.OutcomeAlreadyInstalled:
			w.ShowResult(true, "Gamepad support is already installed", "Controllers will work in SolarBeam sessions on this PC.")
		default:
			w.ShowResult(true, "Gamepad support installed", "Controllers will work in the next SolarBeam session on this PC. You can remove it at any time from the ReadyFleet menu.")
		}
	}()
}

func (g *gamepadSupport) startUninstall() {
	if !g.begin() {
		return
	}
	go func() {
		defer g.finish()
		w := installerui.Wizard{Title: gamepadWizardTitle}
		if !w.ShowConfirm("Remove gamepad support from this PC?",
			"This removes the "+g.mgr.Spec.Name+" driver. Windows will ask for administrator permission.\n\n"+
				"Controllers will stop working in SolarBeam sessions on this PC until you install it again. "+
				"Other software that uses the same driver (for example DS4Windows) will stop working too.") {
			return
		}
		var outcome gamepaddriver.Outcome
		err := w.RunWithProgress(2, func(report func(string)) error {
			var err error
			outcome, err = g.mgr.Uninstall(context.Background(), report)
			return err
		})
		g.invalidate()
		g.logger.Info("gamepad driver uninstall finished", "outcome", int(outcome), "err", errString(err))
		switch {
		case errors.Is(err, gamepaddriver.ErrSessionActive):
			w.ShowResult(false, "Not now: a SolarBeam session is running", "Try again after the session has ended. Nothing was changed.")
		case err != nil:
			w.ShowResult(false, "Gamepad support was not removed", err.Error())
		case outcome == gamepaddriver.OutcomeDeclined:
			w.ShowResult(true, "Nothing was removed", "Administrator permission was not given, so the driver is still installed.")
		case outcome == gamepaddriver.OutcomeRebootRequired:
			w.ShowResult(true, "Restart this PC to finish", "The gamepad driver will be gone after a restart.")
		default:
			w.ShowResult(true, "Gamepad support removed", "The driver has been removed from this PC.")
		}
	}()
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
