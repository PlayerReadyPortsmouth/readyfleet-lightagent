//go:build darwin

package main

import (
	"path/filepath"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
)

func TestDarwinSolarbeamPaths_DefaultsAreOffAndUnderApplicationSupport(t *testing.T) {
	home := "/Users/aurora"
	noEnv := func(string) string { return "" }
	noBundle := func(string) string { return "" }
	got := darwinSolarbeamPaths(config.Config{}, solarbeam.Paths{Mode: solarbeam.ModeInteractive, StateDir: "/s"}, home, noEnv, noBundle)
	dir := filepath.Join(home, "Library", "Application Support", "ReadyFleet", "solarbeam")
	if got.DarwinHostEnabled {
		t.Fatal("solarbeam_host absent must leave the Mac watch-only")
	}
	if got.Root != dir {
		t.Errorf("Root = %q", got.Root)
	}
	if want := filepath.Join(dir, "Vibeshine.app", "Contents", "MacOS", "Vibeshine"); got.Engine != want {
		t.Errorf("Engine = %q, want %q", got.Engine, want)
	}
	if want := filepath.Join(dir, "solarbeam-bridge"); got.Bridge != want {
		t.Errorf("Bridge = %q, want %q", got.Bridge, want)
	}
	if got.Mode != solarbeam.ModeInteractive || got.StateDir != "/s" {
		t.Error("base fields must be preserved")
	}
}

func TestDarwinSolarbeamPaths_ConfigAndBundleOverrides(t *testing.T) {
	on := true
	cfg := config.Config{SolarbeamHost: &on, SolarbeamDir: "/Volumes/SB", SolarbeamEngineApp: "/Apps/Engine.app", SolarbeamBridge: "/Volumes/SB/bin/bridge"}
	bundle := func(app string) string {
		if app == "/Apps/Engine.app" {
			return "Engine-2.1.18"
		}
		return ""
	}
	got := darwinSolarbeamPaths(cfg, solarbeam.Paths{}, "/Users/aurora", func(string) string { return "" }, bundle)
	if !got.DarwinHostEnabled || got.Root != "/Volumes/SB" || got.Bridge != "/Volumes/SB/bin/bridge" {
		t.Fatalf("paths = %+v", got)
	}
	if want := "/Apps/Engine.app/Contents/MacOS/Engine-2.1.18"; got.Engine != want {
		t.Errorf("Engine = %q, want %q (from CFBundleExecutable)", got.Engine, want)
	}
	// Developer environment overrides still win.
	env := map[string]string{"SOLARBEAM_ENGINE_PATH": "/dev/engine", "SOLARBEAM_BRIDGE_PATH": "/dev/bridge"}
	got = darwinSolarbeamPaths(cfg, solarbeam.Paths{Engine: "/dev/engine", Bridge: "/dev/bridge"}, "/Users/aurora", func(k string) string { return env[k] }, bundle)
	if got.Engine != "/dev/engine" || got.Bridge != "/dev/bridge" {
		t.Errorf("env overrides lost: %+v", got)
	}
}
