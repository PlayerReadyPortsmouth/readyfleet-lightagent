package solarbeam

// Host watchdog: restarts a wedged SolarBeam engine through the same preload
// path the agent uses at start (RestartEngine), so link auth is restored,
// which a bare SunshineService restart does not do (PC-1, 28 Oct 2026).
//
// Triggers (each needs 60 s of continuous evidence):
//   - stalled: a viewer is connected (bridge viewers > 0, LiveKit up) and the
//     bridge's monotonic video frame counter has not advanced.
//   - helper_pipe_lost: the engine log reported "IPC pipe connection to helper
//     process was lost" and no frame has flowed since.
//
// Deliberately NOT triggers: "link authentication configuration unavailable"
// and bridge-connect failures. On a healthy idle PC they repeat thousands of
// times a day; the link authenticates when a session is admitted.
//
// Guard rails: off unless config `solarbeam_watchdog` is true; at most
// watchdogMaxPerHour restarts per rolling hour; exponential backoff between
// restarts; no restart while a driver/MSI install looks in progress; an
// unreadable bridge status never triggers a restart (a restart cuts viewers).

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	watchdogStallAfter   = 60 * time.Second
	watchdogTick         = 10 * time.Second
	watchdogMaxPerHour   = 3
	watchdogBaseBackoff  = 5 * time.Minute
	watchdogMaxBackoff   = 40 * time.Minute
	watchdogHealthyReset = time.Hour
	markerPipeLost       = "IPC pipe connection to helper process was lost"
	markerNoCapture      = "not applied (no active capture)"
)

// Audit event ids (machine_events, channel "ReadyFleet", provider "solarbeam-watchdog").
const (
	WatchdogEventRestart    = 4100
	WatchdogEventFailed     = 4101
	WatchdogEventSuppressed = 4102
)

// WatchdogSample is one reading of the bridge. Frames is nil when the bridge
// predates video_frames_total, which disables the stall trigger.
type WatchdogSample struct {
	Viewers int
	Live    bool
	Frames  *uint64
	Running bool // bridge process exists
}

// WatchdogEvent is what the watchdog reports for the audit trail.
type WatchdogEvent struct {
	ID     int
	Reason string
	Detail string
	At     time.Time
}

// WatchdogDeps are the watchdog's seams; tests replace all of them.
type WatchdogDeps struct {
	Now         func() time.Time
	Sample      func(context.Context) (WatchdogSample, error)
	LogMarkers  func() (pipeLost, noCapture bool) // new log lines since the last call
	InstallBusy func() bool
	Restart     func(ctx context.Context, reason string) error
	Report      func(context.Context, WatchdogEvent)
}

// Watchdog is the trigger state machine. Not safe for concurrent Tick.
type Watchdog struct {
	d WatchdogDeps

	lastFrames    *uint64
	stalledSince  time.Time
	pipeLostAt    time.Time
	noCaptureAt   time.Time
	restarts      []time.Time
	backoffUntil  time.Time
	backoff       time.Duration
	suppressedAt  time.Time
	warnedNoField bool
}

func NewWatchdog(d WatchdogDeps) *Watchdog { return &Watchdog{d: d} }

// Run ticks until ctx ends.
func (w *Watchdog) Run(ctx context.Context) {
	t := time.NewTicker(watchdogTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick takes one reading and restarts the engine if a trigger has held for 60 s.
func (w *Watchdog) Tick(ctx context.Context) {
	now := w.d.Now()
	pipeLost, noCapture := w.d.LogMarkers()
	if pipeLost && w.pipeLostAt.IsZero() {
		w.pipeLostAt = now
	}
	if noCapture {
		w.noCaptureAt = now
	}
	s, err := w.d.Sample(ctx)
	if err != nil {
		// Unknown is never a reason to cut viewers.
		w.stalledSince = time.Time{}
		return
	}
	if !s.Running {
		s.Viewers = 0
	}

	advanced := s.Frames != nil && w.lastFrames != nil && *s.Frames > *w.lastFrames
	if s.Frames != nil {
		w.lastFrames = s.Frames
	} else if !w.warnedNoField && s.Viewers > 0 {
		w.warnedNoField = true
		slog.Warn("solarbeam watchdog: bridge reports no video_frames_total; stall trigger disabled",
			"phase", "watchdog_no_frame_counter")
	}
	if advanced {
		w.pipeLostAt = time.Time{} // capture recovered by itself
	}

	viewing := s.Viewers > 0 && s.Live
	switch {
	case !viewing || s.Frames == nil || advanced:
		w.stalledSince = time.Time{}
	case w.stalledSince.IsZero():
		w.stalledSince = now
	}

	reason := ""
	switch {
	case !w.stalledSince.IsZero() && now.Sub(w.stalledSince) >= watchdogStallAfter:
		reason = "stalled"
	case !w.pipeLostAt.IsZero() && now.Sub(w.pipeLostAt) >= watchdogStallAfter:
		reason = "helper_pipe_lost"
	}
	if reason == "" {
		return
	}
	w.attempt(ctx, now, reason, s)
}

func (w *Watchdog) attempt(ctx context.Context, now time.Time, reason string, s WatchdogSample) {
	if w.d.InstallBusy() {
		return // driver/MSI install in progress: keep waiting, evidence stays
	}
	if now.Before(w.backoffUntil) {
		return
	}
	cut := now.Add(-time.Hour)
	kept := w.restarts[:0]
	for _, t := range w.restarts {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	w.restarts = kept
	if len(w.restarts) >= watchdogMaxPerHour {
		if now.Sub(w.suppressedAt) >= time.Hour {
			w.suppressedAt = now
			w.d.Report(ctx, WatchdogEvent{ID: WatchdogEventSuppressed, Reason: reason, At: now,
				Detail: fmt.Sprintf("restart limit of %d per hour reached; needs a person", watchdogMaxPerHour)})
		}
		return
	}
	// Backoff doubles while restarts keep being needed; resets after a healthy hour.
	if len(w.restarts) == 0 || now.Sub(w.restarts[len(w.restarts)-1]) > watchdogHealthyReset {
		w.backoff = watchdogBaseBackoff
	} else {
		w.backoff = min(w.backoff*2, watchdogMaxBackoff)
	}
	w.restarts = append(w.restarts, now)
	w.backoffUntil = now.Add(w.backoff)

	detail := fmt.Sprintf("viewers=%d live=%t", s.Viewers, s.Live)
	if !w.noCaptureAt.IsZero() && now.Sub(w.noCaptureAt) < 2*time.Minute {
		detail += "; engine reports no active capture"
	}
	id := WatchdogEventRestart
	if err := w.d.Restart(ctx, reason); err != nil {
		id = WatchdogEventFailed
		detail += "; restart failed: " + err.Error()
	}
	slog.Warn("solarbeam watchdog: engine restart", "phase", "watchdog_restart", "reason", reason, "detail", detail)
	w.d.Report(ctx, WatchdogEvent{ID: id, Reason: reason, Detail: detail, At: now})
	w.stalledSince = time.Time{}
	w.pipeLostAt = time.Time{}
	w.lastFrames = nil
}

// RestartEngine restarts the engine through the preload path with the
// "already healthy" shortcut disabled. Managed warm-engine profile only.
func (m *Manager) RestartEngine(ctx context.Context, machineID, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.hostingOff.Load() {
		return ErrHostingDisabled
	}
	if !m.warmEngineEnabled() {
		return fmt.Errorf("restart engine: managed warm-engine profile only")
	}
	if _, err := m.linkAuthMode(); err != nil {
		return err
	}
	if machineID == "" {
		return fmt.Errorf("restart engine: machine id required")
	}
	if err := m.prepareFn(); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	orig := m.healthyFn
	m.healthyFn = func() bool { return false }
	defer func() { m.healthyFn = orig }()
	if err := m.preloadFn(machineID); err != nil {
		return fmt.Errorf("engine restart: %w", err)
	}
	slog.Info("solarbeam: engine restarted", "phase", "engine_restarted", "reason", reason)
	return nil
}

// WatchdogSample reads the bridge for the watchdog. Unlike SessionActive an
// unreadable bridge is an error, never "active".
func (m *Manager) WatchdogSample(ctx context.Context) (WatchdogSample, error) {
	if !m.bridgeRunningFn() {
		return WatchdogSample{}, nil
	}
	st, err := m.readBridgeStatus(ctx)
	if err != nil {
		return WatchdogSample{}, err
	}
	return WatchdogSample{Viewers: *st.Viewers, Live: st.LiveKitConnected, Frames: st.VideoFramesTotal, Running: true}, nil
}

// logTail yields whether new engine-log lines since the previous call hold the
// markers. It starts at the current end of file, so history never triggers.
type logTail struct {
	path   string
	offset int64
	primed bool
}

func (l *logTail) markers() (pipeLost, noCapture bool) {
	data, size, ok := readFrom(l.path, l.offset, l.primed)
	l.offset, l.primed = size, ok || l.primed
	text := string(data)
	return strings.Contains(text, markerPipeLost), strings.Contains(text, markerNoCapture)
}

// NewEngineWatchdog wires a Watchdog to this Manager. report receives audit events.
func (m *Manager) NewEngineWatchdog(machineID string, report func(context.Context, WatchdogEvent)) *Watchdog {
	tail := &logTail{path: engineLogPath(m.paths.Engine)}
	return NewWatchdog(WatchdogDeps{
		Now:         time.Now,
		Sample:      m.WatchdogSample,
		LogMarkers:  tail.markers,
		InstallBusy: installInProgress,
		Restart: func(ctx context.Context, reason string) error {
			return m.RestartEngine(ctx, machineID, reason)
		},
		Report: report,
	})
}
