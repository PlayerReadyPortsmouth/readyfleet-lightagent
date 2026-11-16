//go:build darwin

package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/config"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/menuhelper"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
)

func boolp(b bool) *bool { return &b }

// A Mac with no helper, or with menu_prompts off, asks with nothing — which
// consent.Gate turns into OutcomeNoPrompter, a decline. Only an installed
// helper AND the key on routes the question to the menu bar.
func TestMenuHelper_ConsentAskerIsOffByDefault(t *testing.T) {
	var none *menuHelper
	if none.consentAsker(nil) != nil {
		t.Fatal("no helper installed must mean no asker")
	}
	srv, err := menuhelper.Listen(filepath.Join(shortDir(t), "menu.sock"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	off := &menuHelper{server: srv, prompts: config.Config{}.MenuPromptsEnabled()}
	if off.consentAsker(nil) != nil {
		t.Fatal("menu_prompts absent must leave darwin decline-all")
	}
	on := &menuHelper{server: srv, prompts: config.Config{MenuPrompts: boolp(true)}.MenuPromptsEnabled()}
	asker := on.consentAsker(nil)
	if asker == nil {
		t.Fatal("menu_prompts on with a helper must ask through it")
	}
	// Even then, with no helper connected the question cannot be put, and
	// that is a decline, never a pass.
	g := newConsentGate("owner", nil, asker, nil)
	if out := g.Decide(context.Background(), consent.Request{RequesterID: "someone-else"}); out.Allowed() || out != consent.OutcomeNoPrompter {
		t.Fatalf("outcome %q", out)
	}
}

func TestStartMenuHelper_NilWithoutABundle(t *testing.T) {
	// lightAppDataDir() resolves to the real Application Support directory;
	// this test must not create anything there, so it only asserts the
	// not-installed path via the executable check on a scratch directory.
	dir := shortDir(t)
	if _, err := os.Stat(menuHelperExe(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir unexpectedly has a helper: %v", err)
	}
	if got := menuHelperExe(dir); got != filepath.Join(dir, "ReadyFleet Menu.app", "Contents", "MacOS", "ReadyFleet Menu") {
		t.Errorf("helper exe path = %q", got)
	}
}

type fixedStats struct{ connected bool }

func (f fixedStats) Stats() transport.Stats { return transport.Stats{Connected: f.connected} }

type fixedViewers struct {
	n       int
	err     error
	running bool
	// listener is what runMenuStatus registered; tests fire it to stand in
	// for the manager's session start/stop.
	listener func()
}

func (f *fixedViewers) RemoteViewers(context.Context) (int, error) { return f.n, f.err }
func (f *fixedViewers) Status(context.Context) solarbeam.Status {
	return solarbeam.Status{Running: f.running}
}
func (f *fixedViewers) SetSessionListener(fn func()) { f.listener = fn }

func TestRunMenuStatus_PublishesOnlineVenueAndSessionLive(t *testing.T) {
	srv, err := menuhelper.Listen(filepath.Join(shortDir(t), "menu.sock"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(ctx)
	defer cancel()
	m := &menuHelper{server: srv}
	cfg := config.Config{VenueID: "venue-uuid"}

	// A failed viewer probe keeps the previous flag (false here), the same
	// rule the session indicator applies.
	runCtx, stop := context.WithCancel(ctx)
	go runMenuStatus(runCtx, m, fixedStats{connected: true}, &fixedViewers{err: errors.New("bridge down")}, cfg)
	time.Sleep(50 * time.Millisecond)
	stop()
	st := lastPublished(srv)
	if st == nil || !st.Online || st.Venue != "venue-uuid" || st.SessionLive || st.DeviceName != "" {
		t.Fatalf("published %+v", st)
	}

	runCtx, stop = context.WithCancel(ctx)
	go runMenuStatus(runCtx, m, fixedStats{connected: false}, &fixedViewers{n: 1}, cfg)
	time.Sleep(50 * time.Millisecond)
	stop()
	st = lastPublished(srv)
	if st == nil || st.Online || !st.SessionLive {
		t.Fatalf("published %+v", st)
	}
	// A Mac with no helper: returns at once instead of ticking forever.
	runMenuStatus(context.Background(), nil, fixedStats{}, &fixedViewers{}, cfg)
}

func lastPublished(s *menuhelper.Server) *menuhelper.Status { return s.LastStatus() }

func shortDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "rfm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// The LIVE label follows the engine, not only the viewer count: the screen is
// captured from bridge mode on, before anyone joins. And the manager's session
// listener wakes the loop at once, so a start or stop is published without
// waiting for the 3 s poll.
func TestRunMenuStatus_SessionLiveFollowsTheEngineAndTheListener(t *testing.T) {
	srv, err := menuhelper.Listen(filepath.Join(shortDir(t), "menu.sock"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx)
	m := &menuHelper{server: srv}
	host := &fixedViewers{n: 0, running: false}
	go runMenuStatus(ctx, m, fixedStats{connected: true}, host, config.Config{VenueID: "v"})
	waitFor := func(want bool) {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if st := lastPublished(srv); st != nil && st.SessionLive == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("session_live did not become %v; last %+v", want, lastPublished(srv))
	}
	waitFor(false)
	if host.listener == nil {
		t.Fatal("runMenuStatus did not register a session listener")
	}
	// Engine running, nobody watching yet: live. Published on the kick, long
	// before the next tick.
	host.running = true
	host.listener()
	waitFor(true)
	host.running = false
	host.listener()
	waitFor(false)
}
