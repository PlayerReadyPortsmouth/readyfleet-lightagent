package menuhelper

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLauncher_RelaunchesWithBackoffAndStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var starts int
	var slept []time.Duration
	l := &Launcher{
		Exe:         "/Applications/ReadyFleet Menu.app/Contents/MacOS/ReadyFleet Menu",
		Args:        []string{"--socket", "/tmp/menu.sock"},
		MinBackoff:  time.Second,
		MaxBackoff:  4 * time.Second,
		StableAfter: time.Hour, // nothing in this test counts as a stable run
		Start: func(_ context.Context, exe string, args []string) (func() error, error) {
			starts++
			if exe == "" || len(args) != 2 {
				t.Errorf("start called with %q %v", exe, args)
			}
			if starts == 1 {
				return nil, errors.New("exec: not found")
			}
			return func() error { return errors.New("exit status 1") }, nil
		},
		sleep: func(_ context.Context, d time.Duration) bool {
			slept = append(slept, d)
			if len(slept) == 5 {
				cancel()
				return false
			}
			return true
		},
	}
	l.Run(ctx)
	if starts != 5 {
		t.Fatalf("starts = %d, want 5", starts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("slept %v", slept)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("sleep %d = %v, want %v", i, slept[i], want[i])
		}
	}
}

func TestLauncher_ReturnsWhenTheHelperExitsAfterContextEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Launcher{
		Exe: "helper",
		Start: func(ctx context.Context, _ string, _ []string) (func() error, error) {
			return func() error { <-ctx.Done(); return ctx.Err() }, nil
		},
		sleep: func(context.Context, time.Duration) bool { t.Fatal("slept after shutdown"); return false },
	}
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx was cancelled")
	}
}
