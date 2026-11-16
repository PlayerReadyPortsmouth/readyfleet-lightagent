//go:build darwin

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
)

func darwinHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

// platformLightAppDataDir is ~/Library/Application Support/ReadyFleet, the same
// directory the installer writes config.json and the key into.
func platformLightAppDataDir() string { return lightinstall.DarwinPaths(darwinHome()).Root }

// platformLightEngineDir is the SolarBeam host directory,
// ~/Library/Application Support/ReadyFleet/solarbeam (solarbeam.DarwinSolarbeamDir):
// engine app, bridge and child logs together, under the agent's own data
// directory so the rollback is one directory and one config key.
func platformLightEngineDir() string { return solarbeam.DarwinSolarbeamDir(darwinHome()) }

// platformEngineBinary is the default engine executable relative to the
// SolarBeam directory: the Mach-O inside the default bundle. The real name is
// read from the bundle's Info.plist by platformSolarbeamPaths when the bundle
// is present; this is only the path shown when it is not.
func platformEngineBinary() string {
	return filepath.Join(solarbeam.DarwinEngineAppName, "Contents", "MacOS", strings.TrimSuffix(solarbeam.DarwinEngineAppName, ".app"))
}
func platformBridgeBinary() string { return solarbeam.DarwinBridgeName }

// platformSolarbeamPaths applies config.json's macOS keys: solarbeam_host,
// solarbeam_dir, solarbeam_engine_app and solarbeam_bridge. The
// SOLARBEAM_ENGINE_PATH / SOLARBEAM_BRIDGE_PATH environment overrides, when
// set, still win (they are the developer's override on every platform).
func platformSolarbeamPaths(cfg config.Config, base solarbeam.Paths) solarbeam.Paths {
	return darwinSolarbeamPaths(cfg, base, darwinHome(), os.Getenv, readBundleExecutable)
}

// readBundleExecutable returns the bundle's CFBundleExecutable, or "" when the
// Info.plist cannot be read (the bundle is not installed yet, or is not XML).
func readBundleExecutable(appPath string) string {
	f, err := os.Open(solarbeam.DarwinInfoPlistPath(appPath))
	if err != nil {
		return ""
	}
	defer f.Close()
	exe, err := solarbeam.DarwinBundleExecutable(f)
	if err != nil {
		return ""
	}
	return exe
}

// darwinSolarbeamPaths is the pure form of platformSolarbeamPaths.
func darwinSolarbeamPaths(cfg config.Config, base solarbeam.Paths, home string, getenv func(string) string, bundleExe func(string) string) solarbeam.Paths {
	dir := cfg.SolarbeamDir
	if dir == "" {
		dir = solarbeam.DarwinSolarbeamDir(home)
	}
	app := cfg.SolarbeamEngineApp
	if app == "" {
		app = filepath.Join(dir, solarbeam.DarwinEngineAppName)
	}
	bridge := cfg.SolarbeamBridge
	if bridge == "" {
		bridge = filepath.Join(dir, solarbeam.DarwinBridgeName)
	}
	out := base
	out.Root = dir
	out.DarwinHostEnabled = cfg.SolarbeamHostEnabled()
	if getenv("SOLARBEAM_ENGINE_PATH") == "" {
		out.Engine = solarbeam.DarwinEngineExecutable(app, bundleExe(app))
	}
	if getenv("SOLARBEAM_BRIDGE_PATH") == "" {
		out.Bridge = bridge
	}
	return out
}

// platformLogHostAvailability writes one line at start saying whether this
// Mac can host and what it cannot do, so nobody has to send a start to find
// out. Privacy mode is named explicitly because ReadyApp can ask for it.
func platformLogHostAvailability(logger *slog.Logger, mgr *solarbeam.Manager) {
	a := mgr.HostAvailability()
	if !a.Enabled {
		logger.Info("solarbeam host: off on this Mac (watch-only); set solarbeam_host to true in config.json to host", "unsupported", a.Capabilities.Unsupported())
		return
	}
	if !a.Available {
		logger.Warn("solarbeam host: enabled but not ready", "reason", a.Reason, "unsupported", a.Capabilities.Unsupported())
		return
	}
	logger.Info("solarbeam host: ready; privacy mode is not available on a Mac and is refused",
		"unsupported", a.Capabilities.Unsupported())
}
