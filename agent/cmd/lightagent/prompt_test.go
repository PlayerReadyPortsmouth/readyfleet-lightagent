package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venuepolicy"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// stubGate answers with a fixed outcome and records what it was asked.
type stubGate struct {
	mu   sync.Mutex
	out  consent.Outcome
	seen []consent.Request
}

func (s *stubGate) Decide(_ context.Context, req consent.Request) consent.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	return s.out
}

func (s *stubGate) requests() []consent.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]consent.Request(nil), s.seen...)
}

// captureOpenURL swaps the browser hand-off for a recorder, restoring it
// when the test ends.
func captureOpenURL(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var opened []string
	prev := openURLFn
	openURLFn = func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, u)
		return nil
	}
	t.Cleanup(func() { openURLFn = prev })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), opened...)
	}
}

// settle gives the detached decision goroutine time to run. showConnectionRequest
// deliberately does not block the command handler, so every assertion here
// is about what happened after it returned.
func settle(t *testing.T, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !done() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

const requestURL = "https://readyapp.player-ready.co.uk/settings/solarbeam-devices?request=req-1"

func connectionRequestArgs() proto.ShowNotificationArgs {
	return proto.ShowNotificationArgs{
		Kind:        connectionRequestKind,
		Title:       "Alex Staff wants to connect to your PC",
		Body:        "Cheerful-Otter-12 — open your SolarBeam devices to approve or decline.",
		URL:         requestURL,
		RequesterID: "alex-staff-oid",
	}
}

// TestShowConnectionRequest_PassesTheRequesterToTheGate: the whole
// owner-vs-everyone-else decision hinges on this value surviving the trip
// from the wire to the gate.
func TestShowConnectionRequest_PassesTheRequesterToTheGate(t *testing.T) {
	captureOpenURL(t)
	g := &stubGate{out: consent.OutcomeDeclined}

	showConnectionRequest(g, connectionRequestArgs(), nil)

	settle(t, func() bool { return len(g.requests()) > 0 })
	got := g.requests()
	if len(got) != 1 {
		t.Fatalf("gate consulted %d times, want 1", len(got))
	}
	if got[0].RequesterID != "alex-staff-oid" {
		t.Errorf("requester = %q, want the id the controller attributed", got[0].RequesterID)
	}
	if got[0].Title != "Alex Staff wants to connect to your PC" {
		t.Errorf("title = %q, want the text naming who is asking", got[0].Title)
	}
	if got[0].URL != requestURL {
		t.Errorf("url = %q, want %q", got[0].URL, requestURL)
	}
}

func TestShowConnectionRequest_AcceptOpensTheRequestPage(t *testing.T) {
	opened := captureOpenURL(t)
	g := &stubGate{out: consent.OutcomeAccepted}

	showConnectionRequest(g, connectionRequestArgs(), nil)

	settle(t, func() bool { return len(opened()) > 0 })
	if got := opened(); len(got) != 1 || got[0] != requestURL {
		t.Fatalf("opened = %v, want exactly %q", got, requestURL)
	}
}

// TestShowConnectionRequest_NothingButAcceptOpensAnything is the
// fail-closed rule at the handler's own altitude: only a human pressing
// Accept steers the mentor's browser. An owner connecting to their own
// device was never asked and has no page waiting; everything else is a
// decline.
func TestShowConnectionRequest_NothingButAcceptOpensAnything(t *testing.T) {
	for _, out := range []consent.Outcome{
		consent.OutcomeOwnerUnattended,
		consent.OutcomeDeclined,
		consent.OutcomeDismissed,
		consent.OutcomeTimedOut,
		consent.OutcomeCancelled,
		consent.OutcomeNoSessionBus,
		consent.OutcomeNoNotificationDaemon,
		consent.OutcomeNoPrompter,
	} {
		t.Run(string(out), func(t *testing.T) {
			opened := captureOpenURL(t)
			g := &stubGate{out: out}

			showConnectionRequest(g, connectionRequestArgs(), nil)

			settle(t, func() bool { return len(g.requests()) > 0 })
			// Give the goroutine a beat past the decision to do the wrong
			// thing, so this is not passing on timing alone.
			time.Sleep(20 * time.Millisecond)
			if got := opened(); len(got) != 0 {
				t.Fatalf("outcome %q opened %v; only Accept may open the request page", out, got)
			}
		})
	}
}

func TestShowConnectionRequest_AcceptWithNoURLOpensNothing(t *testing.T) {
	opened := captureOpenURL(t)
	args := connectionRequestArgs()
	args.URL = ""
	g := &stubGate{out: consent.OutcomeAccepted}

	showConnectionRequest(g, args, nil)

	settle(t, func() bool { return len(g.requests()) > 0 })
	time.Sleep(20 * time.Millisecond)
	if got := opened(); len(got) != 0 {
		t.Fatalf("opened %v with no URL in the args", got)
	}
}

// TestNewConsentGate_CarriesTheEnrolledOwner: the owner identity the
// enrollment persisted is what the gate compares against.
//
// Only the owner-match path is exercised here, deliberately. It
// short-circuits before the platform Asker, so the assertion holds on
// every platform without this test ever reaching a real TaskDialog or
// posting a real notification to the developer's own desktop. What
// happens once a request DOES reach the Asker is the consent and
// desktopnotify packages' own tests, against their seams.
func TestNewConsentGate_CarriesTheEnrolledOwner(t *testing.T) {
	var byod venuepolicy.State
	byod.Set(&proto.VenuePolicy{})
	g := newConsentGate("owner-oid", &byod, nil, nil)
	if g.OwnerID != "owner-oid" {
		t.Errorf("OwnerID = %q, want the config's owner_actor_id", g.OwnerID)
	}
	if g.Decide(context.Background(), consent.Request{RequesterID: "OWNER-OID"}) != consent.OutcomeOwnerUnattended {
		t.Error("the enrolled owner must not be prompted for their own device")
	}

	if unowned := newConsentGate("", &byod, nil, nil); unowned.OwnerID != "" {
		t.Errorf("OwnerID = %q, want empty — an agent that never learned an owner must match nobody", unowned.OwnerID)
	}
}

type countingAsker struct{ asked int }

func (a *countingAsker) Ask(context.Context, consent.Request) consent.Outcome {
	a.asked++
	return consent.OutcomeDeclined
}

// TestConsentGate_CoreTeamAsksTheOwnerToo: on a venue that requires
// connection approval (Core Team), and while the policy is unknown, the
// enrolled owner gets the same prompt as everyone else, as on Windows.
func TestConsentGate_CoreTeamAsksTheOwnerToo(t *testing.T) {
	var core venuepolicy.State
	core.Set(&proto.VenuePolicy{RequireConnectionApproval: true, ScreenMonitorLockedOff: true})
	var unknown venuepolicy.State
	for name, policy := range map[string]*venuepolicy.State{"core team": &core, "unknown": &unknown, "nil": nil} {
		asker := &countingAsker{}
		g := venueConsentGate{Gate: consent.Gate{OwnerID: "owner-oid", Asker: asker}, policy: policy}
		if out := g.Decide(context.Background(), consent.Request{RequesterID: "owner-oid"}); out.Allowed() || asker.asked != 1 {
			t.Errorf("%s: outcome %q after %d prompts, want the owner asked once and declined", name, out, asker.asked)
		}
	}
}

// reportedOutcomes captures MsgConsentDecision envelopes.
func reportedOutcomes() (transport.SendFunc, func() []proto.ConsentDecisionData) {
	var mu sync.Mutex
	var got []proto.ConsentDecisionData
	send := func(_ context.Context, env proto.Envelope) error {
		var d proto.ConsentDecisionData
		if env.Type == proto.MsgConsentDecision && env.DecodeData(&d) == nil {
			mu.Lock()
			got = append(got, d)
			mu.Unlock()
		}
		return nil
	}
	return send, func() []proto.ConsentDecisionData {
		mu.Lock()
		defer mu.Unlock()
		return append([]proto.ConsentDecisionData(nil), got...)
	}
}

func TestShowConnectionRequest_ReportsOutcomeToC2(t *testing.T) {
	captureOpenURL(t)
	for out, want := range map[consent.Outcome]string{
		consent.OutcomeAccepted: "accepted", consent.OutcomeDeclined: "declined",
		consent.OutcomeTimedOut: "timed_out", consent.OutcomeNoSessionBus: "no_answer",
	} {
		send, reports := reportedOutcomes()
		args := connectionRequestArgs()
		args.RequestID = "req-" + want
		showConnectionRequest(&stubGate{out: out}, args, send)
		settle(t, func() bool { return len(reports()) > 0 })
		if got := reports(); len(got) != 1 || got[0].Outcome != want || got[0].RequestID != args.RequestID {
			t.Fatalf("outcome %q reported %+v, want %q", out, got, want)
		}
	}
}

func TestShowConnectionRequest_UnattendedOrUnidentifiedReportsNothing(t *testing.T) {
	captureOpenURL(t)
	send, reports := reportedOutcomes()
	args := connectionRequestArgs()
	args.RequestID = "req-owner"
	g := &stubGate{out: consent.OutcomeOwnerUnattended}
	showConnectionRequest(g, args, send)
	noID := connectionRequestArgs()
	g2 := &stubGate{out: consent.OutcomeDeclined}
	showConnectionRequest(g2, noID, send)
	settle(t, func() bool { return len(g.requests()) > 0 && len(g2.requests()) > 0 })
	time.Sleep(20 * time.Millisecond)
	if got := reports(); len(got) != 0 {
		t.Fatalf("reported %+v", got)
	}
}

// blockingGate waits for its context, like a prompt nobody has answered.
type blockingGate struct{ started chan struct{} }

func (b blockingGate) Decide(ctx context.Context, _ consent.Request) consent.Outcome {
	close(b.started)
	<-ctx.Done()
	return consent.OutcomeAccepted // a stuck platform dialog answering late
}

func TestConsentDismiss_ClosesPromptWithoutAcceptOrReport(t *testing.T) {
	opened := captureOpenURL(t)
	send, reports := reportedOutcomes()
	args := connectionRequestArgs()
	args.RequestID = "req-dismiss"
	g := blockingGate{started: make(chan struct{})}
	showConnectionRequest(g, args, send)
	<-g.started

	env, err := proto.NewEnvelope(proto.MsgConsentDismiss, uuid.Nil, nil, proto.ConsentDismissData{RequestID: "req-dismiss", Decision: "approved"})
	if err != nil {
		t.Fatal(err)
	}
	handleConsentDismiss(context.Background(), env)

	settle(t, func() bool { return !consentPrompts.Dismiss("req-dismiss") })
	time.Sleep(50 * time.Millisecond)
	if got := opened(); len(got) != 0 {
		t.Fatalf("a remote decision let a late Accept open %v", got)
	}
	if got := reports(); len(got) != 0 {
		t.Fatalf("dismissed prompt reported %+v", got)
	}
}
