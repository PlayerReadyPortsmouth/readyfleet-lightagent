//go:build linux

package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"os"
	"path/filepath"
)

func linuxUserDir(env string, fallback ...string) string {
	if value := os.Getenv(env); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(fallback...)
	}
	parts := append([]string{home}, fallback...)
	return filepath.Join(parts...)
}

func platformLightAppDataDir() string {
	// LocalAppData support keeps cross-platform tests and Wine/dev overrides
	// deterministic; a native Linux install uses XDG locations.
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "ReadyFleet Light")
	}
	return filepath.Join(linuxUserDir("XDG_CONFIG_HOME", ".config"), "readyfleet-light")
}
func platformLightEngineDir() string {
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "SolarBeam")
	}
	return filepath.Join(linuxUserDir("XDG_DATA_HOME", ".local", "share"), "solarbeam")
}
func platformEngineBinary() string { return "sunshine" }
func platformBridgeBinary() string { return "solarbeam-bridge" }

// platformSolarbeamPaths applies platform-specific config overrides to the
// SolarBeam paths. Nothing to apply here: the solarbeam_host keys are macOS
// only (see platform_paths_darwin.go).
func platformSolarbeamPaths(_ config.Config, base solarbeam.Paths) solarbeam.Paths { return base }

// platformLogHostAvailability is a no-op off macOS.
func platformLogHostAvailability(*slog.Logger, *solarbeam.Manager) {}
