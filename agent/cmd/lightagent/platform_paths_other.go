//go:build !windows && !linux && !darwin

package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
)

func platformLightAppDataDir() string { return "lightagent-data" }
func platformLightEngineDir() string  { return "solarbeam-engine" }
func platformEngineBinary() string    { return "sunshine" }
func platformBridgeBinary() string    { return "solarbeam-bridge" }

// platformSolarbeamPaths applies platform-specific config overrides to the
// SolarBeam paths. Nothing to apply here: the solarbeam_host keys are macOS
// only (see platform_paths_darwin.go).
func platformSolarbeamPaths(_ config.Config, base solarbeam.Paths) solarbeam.Paths { return base }

// platformLogHostAvailability is a no-op off macOS.
func platformLogHostAvailability(*slog.Logger, *solarbeam.Manager) {}
