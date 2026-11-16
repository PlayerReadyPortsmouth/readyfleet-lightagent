package consent

import (
	"context"
	"testing"
)

// stubAsker records that it was asked and answers with a fixed outcome.
type stubAsker struct {
	asked int
	seen  Request
	out   Outcome
}

func (s *stubAsker) Ask(_ context.Context, req Request) Outcome {
	s.asked++
	s.seen = req
	return s.out
}

func TestIsOwner(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		requester string
		want      bool
	}{
		{"exact match", "abc-123", "abc-123", true},
		{"case-insensitive GUID", "A1B2C3D4-0000-0000-0000-00000000FFFF", "a1b2c3d4-0000-0000-0000-00000000ffff", true},
		{"surrounding whitespace on the owner", "  abc-123\n", "abc-123", true},
		{"surrounding whitespace on the requester", "abc-123", "\tabc-123 ", true},
		{"different people", "abc-123", "def-456", false},
		{"empty requester never matches", "abc-123", "", false},
		{"empty owner never matches", "", "abc-123", false},
		{"two blanks are not the same person", "", "", false},
		{"whitespace-only is blank", "   ", "  ", false},
		{"internal difference is not whitespace", "abc 123", "abc123", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOwner(tt.owner, tt.requester); got != tt.want {
				t.Errorf("IsOwner(%q, %q) = %v, want %v", tt.owner, tt.requester, got, tt.want)
			}
		})
	}
}

func TestGateDecide_OwnerIsNeverPrompted(t *testing.T) {
	asker := &stubAsker{out: OutcomeDeclined}
	g := Gate{OwnerID: "OWNER-OID", Asker: asker}

	got := g.Decide(context.Background(), Request{RequesterID: " owner-oid "})

	if got != OutcomeOwnerUnattended {
		t.Errorf("outcome = %q, want %q", got, OutcomeOwnerUnattended)
	}
	if !got.Allowed() {
		t.Error("owner-unattended must be allowed")
	}
	if asker.asked != 0 {
		t.Errorf("asker was called %d times; the owner must never be prompted for their own device", asker.asked)
	}
}

func TestGateDecide_NonOwnerIsPrompted(t *testing.T) {
	tests := []struct {
		name      string
		owner     string
		requester string
	}{
		{"a different person", "owner-oid", "someone-else-oid"},
		{"no requester identity at all", "owner-oid", ""},
		{"machine enrolled before owner identity existed", "", "someone-else-oid"},
		{"neither side known", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			asker := &stubAsker{out: OutcomeAccepted}
			g := Gate{OwnerID: tt.owner, Asker: asker}

			got := g.Decide(context.Background(), Request{RequesterID: tt.requester, Title: "t", Body: "b"})

			if got != OutcomeAccepted {
				t.Errorf("outcome = %q, want %q", got, OutcomeAccepted)
			}
			if asker.asked != 1 {
				t.Errorf("asker called %d times, want 1 — a non-owner must be put to the person at the device", asker.asked)
			}
			if asker.seen.Title != "t" || asker.seen.Body != "b" {
				t.Errorf("asker saw %+v, want the request's own title/body passed through verbatim", asker.seen)
			}
		})
	}
}

func TestGateDecide_NoAskerFailsClosed(t *testing.T) {
	g := Gate{OwnerID: "owner-oid"}

	got := g.Decide(context.Background(), Request{RequesterID: "someone-else"})

	if got != OutcomeNoPrompter {
		t.Errorf("outcome = %q, want %q", got, OutcomeNoPrompter)
	}
	if got.Allowed() {
		t.Error("a build with no prompter must DECLINE, not consent on the person's behalf")
	}
}

// TestGateDecide_PassesEveryAskerOutcomeThrough pins the gate as a pure
// relay for whatever the platform prompt reported: it must not reinterpret
// a decline as an accept, nor an accept as a decline.
func TestGateDecide_PassesEveryAskerOutcomeThrough(t *testing.T) {
	for _, out := range []Outcome{
		OutcomeAccepted, OutcomeDeclined, OutcomeDismissed, OutcomeTimedOut,
		OutcomeCancelled, OutcomeNoSessionBus, OutcomeNoNotificationDaemon,
	} {
		t.Run(string(out), func(t *testing.T) {
			g := Gate{OwnerID: "owner", Asker: &stubAsker{out: out}}
			if got := g.Decide(context.Background(), Request{RequesterID: "stranger"}); got != out {
				t.Errorf("outcome = %q, want %q", got, out)
			}
		})
	}
}

// TestOutcomeAllowed is the whole security posture in one table: exactly
// two outcomes let a connection proceed. Adding an Outcome without
// deciding which side of this line it falls on should break here.
func TestOutcomeAllowed(t *testing.T) {
	allowed := map[Outcome]bool{
		OutcomeOwnerUnattended:      true,
		OutcomeAccepted:             true,
		OutcomeDeclined:             false,
		OutcomeDismissed:            false,
		OutcomeTimedOut:             false,
		OutcomeCancelled:            false,
		OutcomeNoSessionBus:         false,
		OutcomeNoNotificationDaemon: false,
		OutcomeNoPrompter:           false,
	}
	for out, want := range allowed {
		if got := out.Allowed(); got != want {
			t.Errorf("Outcome(%q).Allowed() = %v, want %v", out, got, want)
		}
	}
}

func TestGateDecide_AssignedUserExemptForOwnDeviceOnly(t *testing.T) {
	for _, tt := range []struct {
		name, sessionType, id, email string
		prompted                     bool
	}{
		{"own device, assigned by id", "staff_remote_own_device", "oid-1", "", false},
		{"own device, assigned by email, any case", "STAFF_REMOTE_OWN_DEVICE", "", "Ann@Example.com", false},
		{"cyp_session is still prompted", "cyp_session", "oid-1", "ann@example.com", true},
		{"absent type defaults to cyp_session", "", "oid-1", "ann@example.com", true},
		{"unknown type is prompted", "whatever", "oid-1", "", true},
		{"own device, someone else", "staff_remote_own_device", "oid-2", "bob@example.com", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Assigned user recorded as the email in one case and the id in the other.
			assigned := "oid-1"
			if tt.email != "" && tt.id == "" {
				assigned = "ann@example.com"
			}
			a := &stubAsker{out: OutcomeDeclined}
			out := Gate{AssignedUser: assigned, Asker: a}.Decide(context.Background(),
				Request{RequesterID: tt.id, RequesterEmail: tt.email, SessionType: tt.sessionType})
			if tt.prompted && (a.asked != 1 || out != OutcomeDeclined) {
				t.Fatalf("want a prompt and a decline, got asked=%d out=%q", a.asked, out)
			}
			if !tt.prompted && (a.asked != 0 || out != OutcomeAssignedUnattended || !out.Allowed()) {
				t.Fatalf("want unattended, got asked=%d out=%q", a.asked, out)
			}
		})
	}
}

func TestOutcomeReported(t *testing.T) {
	for o, want := range map[Outcome]string{
		OutcomeAccepted: "accepted", OutcomeDeclined: "declined", OutcomeTimedOut: "timed_out",
		OutcomeDismissed: "no_answer", OutcomeCancelled: "no_answer", OutcomeNoSessionBus: "no_answer",
		OutcomeNoNotificationDaemon: "no_answer", OutcomeNoPrompter: "no_answer",
	} {
		if got, ok := o.Reported(); !ok || got != want {
			t.Errorf("%q reported %q,%v want %q", o, got, ok, want)
		}
	}
	for _, o := range []Outcome{OutcomeOwnerUnattended, OutcomeAssignedUnattended} {
		if _, ok := o.Reported(); ok {
			t.Errorf("%q must not be reported: nobody was asked", o)
		}
	}
}

func TestPromptsDismiss(t *testing.T) {
	var ps Prompts
	p, end := ps.Begin(context.Background(), "req-1")
	defer end()
	if p.Dismissed() || p.Ctx.Err() != nil {
		t.Fatal("fresh prompt looks dismissed")
	}
	if ps.Dismiss("other") {
		t.Fatal("dismissed a prompt that is not open")
	}
	if !ps.Dismiss("req-1") || !p.Dismissed() || p.Ctx.Err() == nil {
		t.Fatal("dismiss did not close the prompt")
	}
	if ps.Dismiss("req-1") {
		t.Fatal("second dismiss found an open prompt")
	}
}
