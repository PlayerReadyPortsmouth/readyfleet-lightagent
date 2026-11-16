package venuepolicy

import (
	"github.com/playerreadyportsmouth/readyfleet/proto"
	"testing"
)

func TestCoreTeamPolicy(t *testing.T) {
	var s State
	if s.ScreenAllowed() || s.StartAllowed(proto.SolarbeamStartArgs{}) {
		t.Fatal("unknown policy allowed access")
	}
	s.Set(&proto.VenuePolicy{RequireConnectionApproval: true, ScreenMonitorLockedOff: true})
	if s.ScreenAllowed() {
		t.Fatal("Core Team screen capture allowed")
	}
	for _, a := range []proto.SolarbeamStartArgs{{}, {ConnectionApproved: true}, {RequireConnectionApproval: true}} {
		if s.StartAllowed(a) {
			t.Fatal("unapproved connection allowed")
		}
	}
	if !s.StartAllowed(proto.SolarbeamStartArgs{RequireConnectionApproval: true, ConnectionApproved: true}) {
		t.Fatal("approved connection refused")
	}
	s.Set(&proto.VenuePolicy{})
	if !s.ScreenAllowed() || !s.StartAllowed(proto.SolarbeamStartArgs{}) {
		t.Fatal("ordinary venue behaviour changed")
	}
	s.Set(nil)
	if s.ScreenAllowed() {
		t.Fatal("policy reset did not fail closed")
	}
}

func TestCoreTeamAssignedUserOwnDeviceStart(t *testing.T) {
	var s State
	s.Set(&proto.VenuePolicy{RequireConnectionApproval: true, AssignedUser: "ann@example.com"})
	own := proto.SolarbeamStartArgs{SessionType: proto.SessionTypeOwnDevice, RequesterEmail: "Ann@Example.com"}
	if !s.StartAllowed(own) {
		t.Fatal("assigned user refused for own-device")
	}
	cyp := own
	cyp.SessionType = ""
	other := own
	other.RequesterEmail = "bob@example.com"
	for _, a := range []proto.SolarbeamStartArgs{cyp, other, {SessionType: proto.SessionTypeOwnDevice}} {
		if s.StartAllowed(a) {
			t.Fatalf("approval bypassed for %+v", a)
		}
	}
}

// Tier 3 BYOD in an approval-required venue (Aurora, 9 Nov 2026): the owner
// starts without a prompt only with the C2's requester_is_owner marker AND a
// requester matching the agent's own enrolled owner.
func TestOwnerSelfStart(t *testing.T) {
	var s State
	s.Set(&proto.VenuePolicy{RequireConnectionApproval: true})
	owner := "E12F7E1B-727F-435C-B827-9F9E45313E48"
	own := proto.SolarbeamStartArgs{RequireConnectionApproval: true, RequesterIsOwner: true,
		SessionType: proto.SessionTypeOwnDevice, RequesterID: "e12f7e1b-727f-435c-b827-9f9e45313e48"}
	if got := s.StartDecision(own, owner); got != StartOwnerSelf {
		t.Fatalf("owner self-start = %q, want %q", got, StartOwnerSelf)
	}
	noMarker := own
	noMarker.RequesterIsOwner = false
	other := own
	other.RequesterID = "someone-else"
	cyp := own
	cyp.SessionType = ""
	for name, a := range map[string]proto.SolarbeamStartArgs{"no C2 marker": noMarker, "other staff": other, "cyp session": cyp} {
		if got := s.StartDecision(a, owner); got != "" {
			t.Errorf("%s: allowed (%q), want prompt/refusal", name, got)
		}
	}
	if got := s.StartDecision(own, ""); got != "" {
		t.Errorf("no enrolled owner: allowed (%q)", got)
	}
	// Tier 1 venue: unchanged, no approval needed whatever the marker says.
	s.Set(&proto.VenuePolicy{})
	if got := s.StartDecision(proto.SolarbeamStartArgs{}, ""); got != StartNotRequired {
		t.Errorf("tier 1 = %q", got)
	}
	// Unknown policy still refuses the owner.
	s.Set(nil)
	if got := s.StartDecision(own, owner); got != "" {
		t.Errorf("unknown policy allowed owner (%q)", got)
	}
}
