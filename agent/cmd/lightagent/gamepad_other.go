//go:build !windows

package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// gamepadSupport is Windows-only: Linux BYOD machines get gamepads through
// uinput/uhid, which packaging/linux/install-system-integration.sh sets up
// with the one sudo step. There is no driver to install from the tray.
type gamepadSupport struct{}

func newGamepadSupport(*solarbeam.Manager, *slog.Logger) *gamepadSupport { return nil }

func (*gamepadSupport) tooltipLine() string            { return "" }
func (*gamepadSupport) menuItems() []trayicon.MenuItem { return nil }
