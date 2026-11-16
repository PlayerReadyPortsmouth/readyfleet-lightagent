//go:build windows

package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"os"
	"path/filepath"
)

func platformLightAppDataDir() string {
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "ReadyFleet Light")
	}
	return "lightagent-data"
}
func platformLightEngineDir() string {
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "SolarBeam")
	}
	return "solarbeam-engine"
}
func platformEngineBinary() string { return "sunshine.exe" }
func platformBridgeBinary() string { return "solarbeam-bridge.exe" }

// platformSolarbeamPaths applies platform-specific config overrides to the
// SolarBeam paths. Nothing to apply here: the solarbeam_host keys are macOS
// only (see platform_paths_darwin.go).
func platformSolarbeamPaths(_ config.Config, base solarbeam.Paths) solarbeam.Paths { return base }

// platformLogHostAvailability is a no-op off macOS.
func platformLogHostAvailability(*slog.Logger, *solarbeam.Manager) {}
