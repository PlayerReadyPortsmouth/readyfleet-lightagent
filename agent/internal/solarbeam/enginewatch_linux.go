//go:build linux

// Startup verdict for the Linux engine child.
//
// The BYOD manager used to treat exec.Cmd.Start() returning nil as "SolarBeam
// started", and reported exit_code=0 to the C2 on that basis. That is a
// statement about fork+exec, not about capture: on 7 Sep 2026 it reported
// success for an engine that had no WAYLAND_DISPLAY and no usable
// capabilities, blocked forever inside an XDG screen-share portal call, and
// published audio with no video for the whole session. Nothing recorded a
// failure anywhere, because the only thing anyone had asked was whether a
// process existed.
//
// So the agent now reads the engine's own log stream and waits for the line
// where the engine says it has finished startup and is talking to the bridge
// ("SolarBeam: entering bridge mode"), or for a line where it says it has
// given up, or for the process to die, or for a timeout. Same bytes still go
// to ~/.local/state/readyfleet-light/logs/engine.log — the watcher tees rather
// than intercepts, so nothing is lost from the file a human reads afterwards.
package solarbeam

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// engineReadyTimeout bounds how long a launch waits for the engine to say it
// is in bridge mode.
//
// Calibrated against the failure it exists to catch: the LiveKit room gives up
// on a video track that never publishes after about 31 seconds. Reporting the
// launch failed AFTER the room has already timed out the track would be
// useless, so this has to fire first. A healthy cold start on the reference
// CachyOS box reaches bridge mode in well under a second (the engine's
// persisted encoder-probe cache is what makes that true), so 20s is roughly
// twenty times the observed need and still comfortably inside the room's
// patience.
const engineReadyTimeout = 20 * time.Second

// engineBridgeModeMarker is the engine's own startup-complete line. It is
// printed by the SolarBeam bridge-mode entry point in the engine fork
// (solarbeam-engine-v2), immediately after display and encoder setup have
// succeeded, and immediately before it dials the local bridge WS.
const engineBridgeModeMarker = "SolarBeam: entering bridge mode"

// engineFatalMarkers are lines on which the engine has already decided it
// cannot host. Matching one of these turns a 20-second wait into an immediate,
// accurate failure — and, more importantly, into an error message that names
// the cause instead of saying "timed out".
var engineFatalMarkers = []string{
	"Unable to find display or encoder during startup",
	"has not been defined",
}

type engineVerdict int

const (
	engineVerdictNone engineVerdict = iota
	engineVerdictReady
	engineVerdictFatal
)

// classifyEngineLine is the whole decision, isolated as a pure function so the
// marker set can be tested without spawning anything.
func classifyEngineLine(line string) engineVerdict {
	if strings.Contains(line, engineBridgeModeMarker) {
		return engineVerdictReady
	}
	for _, marker := range engineFatalMarkers {
		if strings.Contains(line, marker) {
			return engineVerdictFatal
		}
	}
	return engineVerdictNone
}

// engineWatcher is an io.Writer that forwards the engine's output to its log
// file while classifying it line by line. It keeps forwarding for the life of
// the process; only the verdict is one-shot.
type engineWatcher struct {
	sink io.Writer

	mu      sync.Mutex
	partial []byte
	// verdict is closed by the first line that decides the outcome. cause
	// carries the deciding line for the error message.
	verdict chan struct{}
	cause   string
	fatal   bool
	decided bool
}

func newEngineWatcher(sink io.Writer) *engineWatcher {
	return &engineWatcher{sink: sink, verdict: make(chan struct{})}
}

func (w *engineWatcher) Write(p []byte) (int, error) {
	if w.sink != nil {
		// A failing log file must never break the engine's stdout: the child
		// would get EPIPE-ish behaviour from a short write and could die for
		// no reason other than a full disk.
		_, _ = w.sink.Write(p)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.decided {
		return len(p), nil
	}
	w.partial = append(w.partial, p...)
	for {
		idx := indexByte(w.partial, '\n')
		if idx < 0 {
			// Cap the buffer so an engine that somehow emits no newline can
			// never grow it without bound.
			if len(w.partial) > 64*1024 {
				w.partial = w.partial[len(w.partial)-4096:]
			}
			break
		}
		line := string(w.partial[:idx])
		w.partial = w.partial[idx+1:]
		switch classifyEngineLine(line) {
		case engineVerdictReady:
			w.decide(false, line)
			return len(p), nil
		case engineVerdictFatal:
			w.decide(true, line)
			return len(p), nil
		}
	}
	return len(p), nil
}

// decide records the outcome once. Caller holds w.mu.
func (w *engineWatcher) decide(fatal bool, line string) {
	if w.decided {
		return
	}
	w.decided = true
	w.fatal = fatal
	w.cause = strings.TrimSpace(line)
	close(w.verdict)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// wait blocks until the engine says it is in bridge mode, says it cannot
// start, exits, or timeout elapses.
//
// exited is the manager's per-child done channel. Watching it matters: an
// engine that dies during startup would otherwise hold the launch for the full
// timeout before failing, and the C2 would learn nothing for 20 seconds about
// a machine that already knew.
func (w *engineWatcher) wait(timeout time.Duration, exited <-chan struct{}) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.verdict:
		w.mu.Lock()
		fatal, cause := w.fatal, w.cause
		w.mu.Unlock()
		if fatal {
			return fmt.Errorf("engine could not start capture: %s (see ~/.local/state/readyfleet-light/logs/engine.log)", cause)
		}
		return nil
	case <-exited:
		// It may have printed the ready line and exited in the same instant;
		// the verdict channel is authoritative, so re-check before failing.
		w.mu.Lock()
		decided, fatal, cause := w.decided, w.fatal, w.cause
		w.mu.Unlock()
		if decided && !fatal {
			return nil
		}
		if decided {
			return fmt.Errorf("engine could not start capture: %s (see ~/.local/state/readyfleet-light/logs/engine.log)", cause)
		}
		return errors.New("engine exited during startup without entering bridge mode (see ~/.local/state/readyfleet-light/logs/engine.log)")
	case <-timer.C:
		return fmt.Errorf("engine did not enter bridge mode within %s — it is most likely blocked in the XDG screen-share portal (see ~/.local/state/readyfleet-light/logs/engine.log)", timeout)
	}
}
