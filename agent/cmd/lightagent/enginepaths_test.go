package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The BYOD light agent runs as the current user with no elevation, so the
// engine cannot live under Program Files: creating that directory fails
// outright. Observed live on 2026-08-18, machine BYOD-216299 —
//
//	solarbeam_engine_update failed:
//	create install dir: mkdir C:\Program Files\SolarBeam: Access is denied
//
// which is why no BYOD device has ever had a working engine. These pin the
// per-user location so the default can't drift back to a path that needs
// rights this binary deliberately never has.

func TestLightEngineDir_IsUnderLocalAppData(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)

	dir := lightEngineDir()

	want := filepath.Join(`C:\Users\test\AppData\Local`, "SolarBeam")
	if dir != want {
		t.Errorf("lightEngineDir() = %q, want %q", dir, want)
	}
}

func TestLightEngineDir_NeverProgramFiles(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)

	for _, p := range []string{lightEngineDir(), lightEnginePath(), lightBridgePath()} {
		if strings.Contains(strings.ToLower(p), "program files") {
			t.Errorf("%q resolves under Program Files — the light agent cannot write there", p)
		}
	}
}

func TestLightEnginePaths_SitBesideEachOtherInTheEngineDir(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)

	dir := lightEngineDir()
	if got := filepath.Dir(lightEnginePath()); got != dir {
		t.Errorf("engine dir = %q, want %q", got, dir)
	}
	if got := filepath.Dir(lightBridgePath()); got != dir {
		t.Errorf("bridge dir = %q, want %q", got, dir)
	}
	if base := filepath.Base(lightEnginePath()); base != "sunshine.exe" {
		t.Errorf("engine binary = %q, want sunshine.exe", base)
	}
	if base := filepath.Base(lightBridgePath()); base != "solarbeam-bridge.exe" {
		t.Errorf("bridge binary = %q, want solarbeam-bridge.exe", base)
	}
}

// The engine dir is a sibling of the agent's own install dir, not inside it:
// the agent replaces itself on self-update and must not take the engine with
// it, and the engine is extracted from its own release artifact.
func TestLightEngineDir_IsSiblingOfAgentDir(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)

	if lightEngineDir() == lightAppDataDir() {
		t.Fatal("engine dir must not be the agent's own install dir")
	}
	if filepath.Dir(lightEngineDir()) != filepath.Dir(lightAppDataDir()) {
		t.Errorf("engine dir %q and agent dir %q should be siblings",
			lightEngineDir(), lightAppDataDir())
	}
}

// The dev fallback mirrors lightAppDataDir's: no LocalAppData (non-Windows
// dev boxes, fakeagent) must still yield a relative path, never an absolute
// Windows one.
func TestLightEngineDir_DevFallback(t *testing.T) {
	t.Setenv("LocalAppData", "")

	dir := lightEngineDir()
	if filepath.IsAbs(dir) || strings.Contains(dir, `\`) {
		t.Errorf("dev fallback = %q, want a relative path", dir)
	}
}

// The existing env overrides stay authoritative — they are how a mentor with
// an engine already installed elsewhere, or a dev box, points at it.
func TestSolarbeamPath_EnvOverrideStillWins(t *testing.T) {
	t.Setenv("LocalAppData", `C:\Users\test\AppData\Local`)
	t.Setenv("SOLARBEAM_ENGINE_PATH", `D:\custom\sunshine.exe`)

	if got := solarbeamPath("SOLARBEAM_ENGINE_PATH", lightEnginePath()); got != `D:\custom\sunshine.exe` {
		t.Errorf("env override = %q, want D:\\custom\\sunshine.exe", got)
	}
}
