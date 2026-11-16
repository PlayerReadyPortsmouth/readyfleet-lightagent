//go:build darwin

// Startup verdict for the macOS engine child.
//
// Same idea as enginewatch_linux.go, with two differences the Mac needs. The
// verdict carries a REASON (ReasonScreenRecording, ReasonAccessibility, …) so
// the manager can refuse further starts for a while with a sentence that
// tells the person what to grant, instead of relaunching an engine that will
// fail the same way. And classification keeps going after the ready line:
// macOS can withdraw a Screen Recording grant while the engine runs (Sequoia
// re-asks periodically for some capture routes), and the supervisor wants to
// know that the engine's exit was a permission loss rather than a crash.
package solarbeam

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// darwinEngineReadyTimeout bounds how long a launch waits for the engine's
// bridge-mode line. Same bound as Linux and for the same reason: the LiveKit
// room gives up on a video track that never publishes after ~31 s, so the
// agent must report a failure before that. A VideoToolbox encoder probe on a
// cold start is expected to be seconds, not tens of seconds (unverified until
// the real engine runs; A3 of the plan measures it).
const darwinEngineReadyTimeout = 20 * time.Second

type darwinEngineWatcher struct {
	sink io.Writer

	mu      sync.Mutex
	partial []byte
	verdict chan struct{}
	decided bool
	fatal   bool
	cause   string
	// reason is the latest fatal reason seen at ANY time, before or after
	// the ready line. lastLine is the line that set it.
	reason   string
	lastLine string
}

func newDarwinEngineWatcher(sink io.Writer) *darwinEngineWatcher {
	return &darwinEngineWatcher{sink: sink, verdict: make(chan struct{})}
}

func (w *darwinEngineWatcher) Write(p []byte) (int, error) {
	if w.sink != nil {
		// A failing log must never break the engine's stdout (short write
		// semantics would look like a broken pipe to the child).
		_, _ = w.sink.Write(p)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	for {
		idx := strings.IndexByte(string(w.partial), '\n')
		if idx < 0 {
			if len(w.partial) > 64*1024 {
				w.partial = w.partial[len(w.partial)-4096:]
			}
			break
		}
		line := string(w.partial[:idx])
		w.partial = w.partial[idx+1:]
		class, reason := classifyDarwinEngineLine(line)
		switch class {
		case darwinLineReady:
			w.decide(false, line)
		case darwinLineFatal:
			w.reason, w.lastLine = reason, strings.TrimSpace(line)
			w.decide(true, line)
		}
	}
	return len(p), nil
}

// decide records the startup outcome once. Caller holds w.mu.
func (w *darwinEngineWatcher) decide(fatal bool, line string) {
	if w.decided {
		return
	}
	w.decided = true
	w.fatal = fatal
	w.cause = strings.TrimSpace(line)
	close(w.verdict)
}

// Reason is the latest fatal reason seen, "" when none.
func (w *darwinEngineWatcher) Reason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reason
}

func (w *darwinEngineWatcher) fatalError(reason, cause, logPath string) error {
	if isPermissionReason(reason) {
		return fmt.Errorf("%s: grant it to the engine app in System Settings > Privacy & Security, then start again (engine said: %s; see %s)", reason, cause, logPath)
	}
	return fmt.Errorf("engine could not start capture: %s (%s; see %s)", reason, cause, logPath)
}

// wait blocks until the engine is in bridge mode, has said it cannot host,
// has exited, or timeout elapses. exited is the manager's per-child done
// channel, so an engine that dies during startup fails the launch at once.
func (w *darwinEngineWatcher) wait(timeout time.Duration, exited <-chan struct{}, logPath string) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-w.verdict:
		w.mu.Lock()
		fatal, cause, reason := w.fatal, w.cause, w.reason
		w.mu.Unlock()
		if fatal {
			return w.fatalError(reason, cause, logPath)
		}
		return nil
	case <-exited:
		w.mu.Lock()
		decided, fatal, cause, reason := w.decided, w.fatal, w.cause, w.reason
		w.mu.Unlock()
		if decided && !fatal {
			return nil
		}
		if decided {
			return w.fatalError(reason, cause, logPath)
		}
		return errors.New("engine exited during startup without entering bridge mode (see " + logPath + ")")
	case <-timer.C:
		return fmt.Errorf("engine did not enter bridge mode within %s (see %s)", timeout, logPath)
	}
}
