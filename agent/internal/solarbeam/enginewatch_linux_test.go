//go:build linux

package solarbeam

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyEngineLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want engineVerdict
	}{
		{
			name: "the engine's own bridge-mode line",
			line: "[2026-09-07 11:31:21.761]: Info: SolarBeam: entering bridge mode (SOLARBEAM_BRIDGE_WS=ws://127.0.0.1:9092/bridge)",
			want: engineVerdictReady,
		},
		{
			name: "no display and no encoder",
			line: "[2026-09-07 09:58:07.001]: Fatal: Unable to find display or encoder during startup",
			want: engineVerdictFatal,
		},
		{
			name: "the missing WAYLAND_DISPLAY that caused all of this",
			line: "Error: [wayland] Environment variable WAYLAND_DISPLAY has not been defined",
			want: engineVerdictFatal,
		},
		{
			name: "a routine startup line decides nothing",
			line: "[2026-09-07 11:31:21.717]: Info: [wayland] Found monitor: AORUS FO27Q2",
			want: engineVerdictNone,
		},
		{
			name: "a bridge reconnect warning is not a startup failure",
			line: "Warning: solarbeam bridge: ws connect to ws://127.0.0.1:9092/bridge failed; retrying in 2s",
			want: engineVerdictNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEngineLine(tc.line); got != tc.want {
				t.Fatalf("classifyEngineLine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEngineWatcherReportsReadyOnBridgeMode(t *testing.T) {
	var sink bytes.Buffer
	w := newEngineWatcher(&sink)
	go func() {
		_, _ = w.Write([]byte("Info: [wayland] Found display [wayland-0]\n"))
		_, _ = w.Write([]byte("Info: SolarBeam: entering bridge mode (SOLARBEAM_BRIDGE_WS=ws://127.0.0.1:9092/bridge)\n"))
	}()
	if err := w.wait(2*time.Second, make(chan struct{})); err != nil {
		t.Fatalf("wait = %v, want nil", err)
	}
	if !strings.Contains(sink.String(), "Found display") {
		t.Fatalf("watcher swallowed output instead of teeing it: %q", sink.String())
	}
}

// A line arriving in two writes must still be classified — the engine's stdout
// is a pipe and nothing guarantees write boundaries land on newlines.
func TestEngineWatcherReassemblesSplitLines(t *testing.T) {
	w := newEngineWatcher(nil)
	go func() {
		_, _ = w.Write([]byte("Info: SolarBeam: enter"))
		_, _ = w.Write([]byte("ing bridge mode\n"))
	}()
	if err := w.wait(2*time.Second, make(chan struct{})); err != nil {
		t.Fatalf("wait = %v, want nil", err)
	}
}

func TestEngineWatcherFailsFastOnAFatalLine(t *testing.T) {
	w := newEngineWatcher(nil)
	go func() {
		_, _ = w.Write([]byte("Fatal: Unable to find display or encoder during startup\n"))
	}()
	err := w.wait(2*time.Second, make(chan struct{}))
	if err == nil {
		t.Fatal("wait = nil, want an error")
	}
	if !strings.Contains(err.Error(), "Unable to find display or encoder") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

// The exact regression: the engine is alive but wedged, so it emits normal
// startup chatter and never reaches bridge mode. This must fail, not succeed.
func TestEngineWatcherFailsWhenTheEngineHangsBeforeBridgeMode(t *testing.T) {
	w := newEngineWatcher(nil)
	_, _ = w.Write([]byte("Info: [portalgrab] Loaded portal restore token from disk\n"))
	err := w.wait(150*time.Millisecond, make(chan struct{}))
	if err == nil {
		t.Fatal("wait = nil for a hung engine, want an error")
	}
	if !strings.Contains(err.Error(), "portal") {
		t.Fatalf("timeout error should point at the portal: %v", err)
	}
}

func TestEngineWatcherFailsWhenTheEngineExitsDuringStartup(t *testing.T) {
	w := newEngineWatcher(nil)
	exited := make(chan struct{})
	close(exited)
	err := w.wait(5*time.Second, exited)
	if err == nil {
		t.Fatal("wait = nil for an engine that exited, want an error")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Ready-then-exit in the same instant must resolve as ready: the verdict, not
// the liveness of the process, is what the launch was waiting for.
func TestEngineWatcherPrefersTheVerdictOverAnExitRace(t *testing.T) {
	w := newEngineWatcher(nil)
	_, _ = w.Write([]byte("Info: SolarBeam: entering bridge mode\n"))
	exited := make(chan struct{})
	close(exited)
	if err := w.wait(2*time.Second, exited); err != nil {
		t.Fatalf("wait = %v, want nil", err)
	}
}

func TestEngineWatcherKeepsTeeingAfterTheVerdict(t *testing.T) {
	var sink bytes.Buffer
	w := newEngineWatcher(&sink)
	_, _ = w.Write([]byte("Info: SolarBeam: entering bridge mode\n"))
	_, _ = w.Write([]byte("Info: a later line\n"))
	if !strings.Contains(sink.String(), "a later line") {
		t.Fatalf("log file stopped receiving output after the verdict: %q", sink.String())
	}
}

// openLinuxChildLog creating its directory is the difference between a
// diagnosable failure and an invisible one; it silently returned nil for a day
// because ~/.local/state was read-only under the old unit.
func TestOpenLinuxChildLogCreatesItsDirectory(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", state)
	f := openLinuxChildLog("engine.log")
	if f == nil {
		t.Fatal("openLinuxChildLog returned nil")
	}
	defer f.Close()
	if _, err := os.Stat(filepath.Join(state, "readyfleet-light", "logs", "engine.log")); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}
