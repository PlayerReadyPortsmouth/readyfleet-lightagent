package main

import (
	"context"
	"sort"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/runtime"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/update"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// fakeNotifier satisfies notifier without a real tray/window.
type fakeNotifier struct{}

func (fakeNotifier) Notify(title, body string, severity trayicon.NotifySeverity, onClick func()) error {
	return nil
}

// fakeGate satisfies gate without a desktop to draw a prompt on.
type fakeGate struct {
	called  bool
	outcome consent.Outcome
}

func (f *fakeGate) Decide(context.Context, consent.Request) consent.Outcome {
	f.called = true
	return f.outcome
}

// TestLightHandlers_ExactCommandSet is Task 4's explicit Step 1 acceptance
// bar: the handler set must equal exactly these seven kinds — no more
// (that would put a capability on the wire the byod_solarbeam allowlist
// doesn't grant anyway, but the binary itself should never even try), no
// fewer (that would silently break a permitted capability).
func TestLightHandlers_ExactCommandSet(t *testing.T) {
	manager := solarbeam.New(solarbeam.Paths{})
	updater := update.New(update.Config{})

	handlers := LightHandlers(manager, updater, fakeNotifier{}, &fakeGate{}, nil, nil, "")

	want := []proto.CommandKind{
		proto.CmdInventoryRefresh,
		proto.CmdStartSolarbeam,
		proto.CmdStopSolarbeam,
		proto.CmdSetSolarbeamAvailability,
		proto.CmdSolarbeamEngineUpdate,
		proto.CmdUpdate,
		proto.CmdShowNotification,
	}
	if len(handlers) != len(want) {
		t.Fatalf("handler count = %d, want %d (got kinds: %v)", len(handlers), len(want), handlerKinds(handlers))
	}
	for _, kind := range want {
		if _, ok := handlers[kind]; !ok {
			t.Errorf("missing handler for %q", kind)
		}
	}
}

// A representative sample of forbidden kinds — the managed-only surface —
// must never appear, reinforcing the exact-set test above with named
// negatives an accidental future addition is more likely to trip.
func TestLightHandlers_NeverIncludesManagedOnlyKinds(t *testing.T) {
	manager := solarbeam.New(solarbeam.Paths{})
	updater := update.New(update.Config{})
	handlers := LightHandlers(manager, updater, fakeNotifier{}, &fakeGate{}, nil, nil, "")

	// Wire values, not the proto.Cmd* constants. What must never be handled
	// is the value that arrives on the wire: a constant renamed or removed
	// while still serialising to "shell" is still a remote shell, and this
	// test should fail on that. It also lets the public staff-facing mirror
	// carry this test while pruning the managed command surface it asserts
	// the absence of — the constants need not exist for the guarantee to hold.
	forbidden := []proto.CommandKind{
		"shell",
		"shutdown",
		"reboot",
		"install",
		"launch",
		"set_default_boot",
		"configure_unattended",
		"scan_lan",
		"wake",
		"push_install",
		"install_signing_trust",
		"list_dir",
		"download_file",
		"upload_file",
	}
	for _, kind := range forbidden {
		if _, ok := handlers[kind]; ok {
			t.Errorf("light handlers must NOT include managed-only kind %q", kind)
		}
	}
}

func handlerKinds(handlers map[proto.CommandKind]runtime.Handler) []string {
	out := make([]string, 0, len(handlers))
	for k := range handlers {
		out = append(out, string(k))
	}
	sort.Strings(out)
	return out
}
