//go:build windows

package main

import (
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/connectionprompt"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"log/slog"
)

func newConsentAsker(logger *slog.Logger) consent.Asker { return connectionprompt.NewAsker(logger) }
