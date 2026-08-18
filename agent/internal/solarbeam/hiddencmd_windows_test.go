//go:build windows

package solarbeam

import (
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// The engine and bridge are console-subsystem binaries, and once the agent
// itself went GUI-subsystem it stopped having a console for either child to
// inherit — so without CREATE_NO_WINDOW, Windows allocates each one a brand
// new, visible console window on every session start. Observed live on
// 2026-08-18: two console windows appearing on a real BYOD test session, one
// closing shortly after (the engine, once it exited without ever connecting
// to the bridge). This was invisible until that day only because no BYOD
// device had ever successfully installed an engine to launch in the first
// place — launchBridge and startEngineInteractive are otherwise unchanged
// from when the agent itself still had a console to share.
//
// Both real launch sites (launchBridge, startEngineInteractive) build their
// *exec.Cmd through this same constructor, so a regression in either can't
// slip past this test independently of the other.
func TestNewHiddenCmd_SuppressesConsoleWindow(t *testing.T) {
	for _, path := range []string{
		`C:\Windows\System32\cmd.exe`, // stand-in for the engine
		`C:\Windows\System32\cmd.exe`, // stand-in for the bridge
	} {
		cmd := newHiddenCmd(path)

		if cmd.SysProcAttr == nil {
			t.Fatal("SysProcAttr is nil")
		}
		if !cmd.SysProcAttr.HideWindow {
			t.Error("HideWindow = false; the child would get its own visible console window")
		}
	}
}

// The engine resolves relative asset paths against its working directory,
// not against its own binary location. Reproduced live: a real BYOD engine
// with assets/apps.json genuinely sitting beside it exited during config
// setup with "cannot copy file: No such file or directory [assets/apps.json]",
// because startEngineInteractive never set a working directory and the
// process inherited the AGENT's instead.
func TestBuildEngineCmd_RunsFromItsOwnDirectory(t *testing.T) {
	enginePath := `C:\Users\test\AppData\Local\SolarBeam\sunshine.exe`

	cmd := buildEngineCmd(enginePath, proto.SolarbeamStartArgs{MachineID: "test-machine"})

	want := `C:\Users\test\AppData\Local\SolarBeam`
	if cmd.Dir != want {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, want)
	}
}
