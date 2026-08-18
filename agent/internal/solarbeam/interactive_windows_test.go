//go:build windows

package solarbeam

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// TestInteractiveMode_StartTrackStop exercises the real Win32 pieces
// ModeInteractive added — startEngineInteractive, engineProcessRunning
// (via processAlive's OpenProcess/GetExitCodeProcess), and
// stopSolarbeamInteractive — against a real running process rather than
// a mocked seam. cmd.exe with no /c command just sits idle waiting for
// input, which is exactly the "stays alive until killed" shape needed
// here; it needs no window/console interaction to spawn or terminate.
func TestInteractiveMode_StartTrackStop(t *testing.T) {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	cmdExe := filepath.Join(sysRoot, "System32", "cmd.exe")
	if _, err := os.Stat(cmdExe); err != nil {
		t.Skipf("cmd.exe not found at %s: %v", cmdExe, err)
	}

	m := New(Paths{Engine: cmdExe, Mode: ModeInteractive})

	if m.engineProcessRunning() {
		t.Fatal("engineProcessRunning() = true before anything started")
	}

	if err := m.startEngineInteractive(proto.SolarbeamStartArgs{MachineID: "test-machine"}); err != nil {
		t.Fatalf("startEngineInteractive: %v", err)
	}
	t.Cleanup(func() { _ = m.stopSolarbeamInteractive() })

	if !m.engineProcessRunning() {
		t.Fatal("engineProcessRunning() = false right after a real process was started")
	}
	firstPID := m.interactiveEngineProc.Pid

	// Starting again must kill the first instance and track a new one —
	// matches the service path's restart-for-a-fresh-session semantics.
	if err := m.startEngineInteractive(proto.SolarbeamStartArgs{MachineID: "test-machine-2"}); err != nil {
		t.Fatalf("second startEngineInteractive: %v", err)
	}
	if m.interactiveEngineProc.Pid == firstPID {
		t.Fatal("second start reused the first PID; expected a fresh process")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(firstPID) {
		time.Sleep(50 * time.Millisecond)
	}
	if processAlive(firstPID) {
		t.Error("first instance still alive after the second start; expected it to be killed")
	}

	if err := m.stopSolarbeamInteractive(); err != nil {
		t.Fatalf("stopSolarbeamInteractive: %v", err)
	}
	if m.engineProcessRunning() {
		t.Error("engineProcessRunning() = true after stopSolarbeamInteractive")
	}
}

// TestProcessAlive_ReflectsRealProcessLifecycle checks processAlive
// directly against a short-lived real process, independent of Manager —
// isolates the OpenProcess/GetExitCodeProcess logic itself.
func TestProcessAlive_ReflectsRealProcessLifecycle(t *testing.T) {
	sysRoot := os.Getenv("SystemRoot")
	if sysRoot == "" {
		sysRoot = `C:\Windows`
	}
	cmdExe := filepath.Join(sysRoot, "System32", "cmd.exe")
	if _, err := os.Stat(cmdExe); err != nil {
		t.Skipf("cmd.exe not found at %s: %v", cmdExe, err)
	}

	cmd := exec.Command(cmdExe, "/c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := cmd.Process.Pid

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && processAlive(pid) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatal("processAlive still true well after the process should have exited")
	}
	_ = cmd.Wait()
}
