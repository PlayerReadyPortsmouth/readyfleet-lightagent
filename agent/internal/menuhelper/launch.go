package menuhelper

import (
	"context"
	"log/slog"
	"os/exec"
	"syscall"
	"time"
)

// Launcher keeps the helper app running for as long as the agent runs. The
// helper is a plain child process (its bundle executable run directly, so
// LaunchServices still reads the bundle's Info.plist and LSUIElement), waited
// on and relaunched with backoff when it exits. When ctx ends the child gets
// SIGTERM and a few seconds to leave.
//
// Start is the process seam: tests substitute it so nothing is spawned.
type Launcher struct {
	Exe    string
	Args   []string
	Logger *slog.Logger

	// Start runs the helper and returns a wait function that blocks until
	// it exits. Nil means execStart.
	Start func(ctx context.Context, exe string, args []string) (wait func() error, err error)

	// MinBackoff and MaxBackoff bound the relaunch delay (1s and 60s when
	// zero). A helper that stayed up for at least StableAfter resets the
	// delay; one that keeps crashing on start backs off exponentially.
	MinBackoff, MaxBackoff, StableAfter time.Duration

	// sleep is the clock seam for tests.
	sleep func(ctx context.Context, d time.Duration) bool
}

func (l *Launcher) defaults() {
	if l.Logger == nil {
		l.Logger = slog.Default()
	}
	if l.Start == nil {
		l.Start = execStart
	}
	if l.MinBackoff <= 0 {
		l.MinBackoff = time.Second
	}
	if l.MaxBackoff <= 0 {
		l.MaxBackoff = time.Minute
	}
	if l.StableAfter <= 0 {
		l.StableAfter = time.Minute
	}
	if l.sleep == nil {
		l.sleep = func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
				return true
			case <-ctx.Done():
				return false
			}
		}
	}
}

// Run blocks until ctx ends. It never returns an error: a helper that cannot
// start is logged and retried, and must never take the agent down.
func (l *Launcher) Run(ctx context.Context) {
	l.defaults()
	delay := l.MinBackoff
	for ctx.Err() == nil {
		started := time.Now()
		wait, err := l.Start(ctx, l.Exe, l.Args)
		if err != nil {
			l.Logger.Warn("menu helper: could not start", "exe", l.Exe, "err", err, "retry_in", delay.String())
		} else {
			l.Logger.Info("menu helper started", "exe", l.Exe)
			err = wait()
			if ctx.Err() != nil {
				return
			}
			if time.Since(started) >= l.StableAfter {
				delay = l.MinBackoff
			}
			l.Logger.Warn("menu helper exited; relaunching", "err", err, "ran_for", time.Since(started).Round(time.Second).String(), "retry_in", delay.String())
		}
		if !l.sleep(ctx, delay) {
			return
		}
		if delay *= 2; delay > l.MaxBackoff {
			delay = l.MaxBackoff
		}
	}
}

func execStart(ctx context.Context, exe string, args []string) (func() error, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}
