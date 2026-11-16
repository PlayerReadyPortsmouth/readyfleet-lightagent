//go:build !darwin

package main

import (
	"context"
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
)

// menuHelper exists only on macOS (menu_darwin.go). Elsewhere the tray is the
// platform's own (trayicon) and the consent prompt is the platform's own
// (connectionprompt), so this is nil and consentAsker defers to them.
type menuHelper struct{}

func startMenuHelper(context.Context, config.Config, *slog.Logger) *menuHelper { return nil }

func (*menuHelper) consentAsker(logger *slog.Logger) consent.Asker { return newConsentAsker(logger) }

func runMenuStatus(context.Context, *menuHelper, *transport.Client, *solarbeam.Manager, config.Config) {
}
