package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/sessionindicator"
)

// sessionStartedKind is a show_notification kind meaning "somebody has
// just joined this machine's SolarBeam session".
//
// It needs no proto change: proto.ShowNotificationArgs.Kind is an open
// string (its own doc comment already anticipates further kinds), and the
// C2's notify endpoint passes it through untouched. The one convention
// this adds is on an existing field — for THIS kind, Body is the viewer's
// DISPLAY NAME and nothing else — so the agent has a name to put on the
// bar. The bridge's /status tells the agent how many remote participants
// are watching but never who, so a name can only come from the C2.
//
// Everything about it fails soft: an absent notification, an empty Body,
// or a Body that is not a plausible name simply leaves the bar reading
// "Your screen is being viewed remotely", which is the whole warning. The
// name is an improvement on the message, never a precondition for it.
const sessionStartedKind = "session_started"

// indicatorPollInterval matches the tray's own 3s poll. The bar is a
// safety signal, so it wants to be up promptly, but the thing it polls is
// a loopback HTTP endpoint on the same machine and there is nothing to be
// gained from hammering it.
const indicatorPollInterval = 3 * time.Second

// viewerProbe is the subset of *solarbeam.Manager the driver needs.
type viewerProbe interface {
	RemoteViewers(ctx context.Context) (int, error)
}

// sessionIndicatorDriver keeps the on-screen bar in step with who is
// actually watching.
//
// It polls the LiveKit bridge rather than tracking start/stop commands: a
// session command says a stream was set up, the viewer count says a human
// is looking at it, and only the second is what the bar claims. That also
// means the bar is correct after an agent restart mid-session, with no
// state to persist.
type sessionIndicatorDriver struct {
	ind      sessionindicator.Indicator
	probe    viewerProbe
	logger   *slog.Logger
	required bool

	mu     sync.Mutex
	viewer string
}

func newSessionIndicatorDriver(ind sessionindicator.Indicator, probe viewerProbe, required bool, logger *slog.Logger) *sessionIndicatorDriver {
	if logger == nil {
		logger = slog.Default()
	}
	return &sessionIndicatorDriver{ind: ind, probe: probe, logger: logger, required: required}
}

// SetViewer records the name the C2 announced for the current session.
func (d *sessionIndicatorDriver) SetViewer(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.viewer = strings.TrimSpace(name)
}

func (d *sessionIndicatorDriver) currentViewer() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.viewer
}

// Preflight puts the bar up and takes it straight down, to find out
// whether the indicator works on this machine BEFORE a session starts.
//
// This is the whole of the require_session_indicator policy: it is called
// from the start_solarbeam path, and its error is only fatal when the
// operator has set that key. Doing it as a real show rather than a
// capability guess is deliberate — every failure mode that matters
// (missing layer-shell, an X11 session, SetWindowDisplayAffinity refusing
// on an old Windows build) only shows up when something is actually
// drawn.
func (d *sessionIndicatorDriver) Preflight() error {
	if d == nil || d.ind == nil {
		return nil
	}
	if err := d.ind.Show(sessionindicator.Notice{}); err != nil {
		return err
	}
	return d.ind.Hide()
}

// GateSessionStart applies the require_session_indicator policy to a
// session about to start. It returns an error ONLY when the operator has
// asked for that; otherwise a broken indicator is logged as loudly as
// this codebase logs anything and the session proceeds.
//
// A SKIP IS NOT A FAILURE, and this is the one place that distinction
// has teeth. When the machine is locked or its displays are asleep the
// indicator is deliberately not attempted (sessionindicator.Decide), and
// that must not refuse the session even under require_session_indicator:
// the policy exists to stop a session running unannounced in front of
// somebody, and there is nobody in front of a locked screen. Aurora's
// ruling of 7 Sep 2026 — on BYOD a connection from anyone but the
// device's owner already carries an explicit consent prompt, so the
// person has been told by a route that does not depend on the bar.
// Treating a skip as an inability would turn every locked machine into a
// machine that refuses support, which is exactly backwards.
func (d *sessionIndicatorDriver) GateSessionStart() error {
	if d == nil || d.ind == nil {
		return nil
	}
	err := d.Preflight()
	if err == nil {
		return nil
	}
	var skipped *sessionindicator.Skipped
	if errors.As(err, &skipped) {
		d.logger.Info("session indicator: skipped for this session; nobody is at the screen to see it",
			"phase", "session_indicator_skipped", "reason", skipped.Reason,
			"required", d.required)
		return nil
	}
	if d.required {
		d.logger.Error("session indicator: cannot be shown; refusing to host a session nobody at the machine would be told about",
			"phase", "session_indicator_required", "err", err)
		return fmt.Errorf("session indicator unavailable and require_session_indicator is set: %w", err)
	}
	d.logger.Error("session indicator: cannot be shown; this session will run with NO on-screen warning at the machine",
		"phase", "session_indicator_unavailable", "err", err,
		"remedy", "set require_session_indicator to refuse sessions instead")
	return nil
}

// Run drives the bar until ctx is cancelled. Never returns an error: a
// broken indicator must not take the agent's connection down with it, the
// same rule the tray runs under.
func (d *sessionIndicatorDriver) Run(ctx context.Context) {
	ticker := time.NewTicker(indicatorPollInterval)
	defer ticker.Stop()
	defer func() {
		if err := d.ind.Close(); err != nil {
			d.logger.Warn("session indicator: close", "err", err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.tick(ctx)
		}
	}
}

// tick is one poll-and-decide step, split out so it can be tested without
// a clock.
func (d *sessionIndicatorDriver) tick(ctx context.Context) {
	viewers, err := d.probe.RemoteViewers(ctx)
	if err != nil {
		// Ambiguity leaves the bar exactly as it is. Raising it on a
		// failed probe would cry wolf; lowering it would hide a live
		// session. Neither is a safe default, so nothing changes and the
		// next tick, three seconds later, decides.
		d.logger.Warn("session indicator: could not read the bridge's viewer count; leaving the bar as it is",
			"phase", "session_indicator_probe", "err", err)
		return
	}
	if viewers > 0 {
		err := d.ind.Show(sessionindicator.Notice{Viewer: d.currentViewer()})
		if err == nil {
			return
		}
		var skipped *sessionindicator.Skipped
		if errors.As(err, &skipped) {
			// Not an error condition. The bar is re-decided on every
			// tick, so it goes up within one poll interval of the person
			// coming back to the machine.
			d.logger.Info("session indicator: skipped this tick; nobody is at the screen to see the warning",
				"phase", "session_indicator_skipped", "viewers", viewers, "reason", skipped.Reason)
			return
		}
		d.logger.Error("session indicator: a remote viewer is connected and the on-screen warning could not be shown",
			"phase", "session_indicator_show_failed", "viewers", viewers, "err", err)
		return
	}
	// Nobody is watching: drop the announced name too, so the next
	// session cannot inherit the previous viewer's.
	d.SetViewer("")
	if err := d.ind.Hide(); err != nil {
		d.logger.Warn("session indicator: hide", "err", err)
	}
}

// startSessionIndicator builds the driver if the machine has the feature
// turned on, and returns nil if it does not.
//
// A New() failure is NOT fatal here and does not gate anything: it means
// this platform has no implementation at all, which is a deployment fact
// the operator can see in the log, not a per-session condition. The
// per-session policy lives in GateSessionStart.
func startSessionIndicator(enabled, required bool, probe viewerProbe, logger *slog.Logger) *sessionIndicatorDriver {
	if !enabled {
		return nil
	}
	ind, err := sessionindicator.New()
	if err != nil {
		if errors.Is(err, sessionindicator.ErrUnsupported) {
			logger.Error("session indicator is enabled but this platform has no implementation; sessions will run with no on-screen warning",
				"phase", "session_indicator_unsupported", "err", err)
		} else {
			logger.Error("session indicator is enabled but could not be created; sessions will run with no on-screen warning",
				"phase", "session_indicator_create_failed", "err", err)
		}
		return nil
	}
	return newSessionIndicatorDriver(ind, probe, required, logger)
}
