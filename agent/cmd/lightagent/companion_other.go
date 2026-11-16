//go:build !windows

package main

import (
	"context"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

func runLightCompanion(context.Context, string, *transport.Client, *solarbeam.Manager, func() error, *trayicon.Tray) (func(), bool) {
	return func() {}, false
}
