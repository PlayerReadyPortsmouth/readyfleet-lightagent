// Package venuepolicy holds the authenticated C2 policy for this connection.
package venuepolicy

import (
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/proto"
	"sync/atomic"
)

type State struct {
	policy atomic.Pointer[proto.VenuePolicy]
}

func (s *State) Set(p *proto.VenuePolicy) {
	if p == nil {
		s.policy.Store(nil)
		return
	}
	copy := *p
	s.policy.Store(&copy)
}

// Unknown policy blocks screen capture and SolarBeam, while allowing management.
func (s *State) ScreenAllowed() bool {
	p := s.policy.Load()
	return p != nil && !p.ScreenMonitorLockedOff
}
func (s *State) RequiresApproval() bool {
	p := s.policy.Load()
	return p == nil || p.RequireConnectionApproval
}
func (s *State) StartAllowed(a proto.SolarbeamStartArgs) bool {
	return s.StartDecision(a, "") != ""
}

// Start decisions ("" = refused).
const (
	StartNotRequired = "not_required"
	StartApproved    = "approved"
	StartAssigned    = "assigned_own_device"
	StartOwnerSelf   = "owner_self"
)

// StartDecision says why a start_solarbeam may proceed, or "" to refuse.
// ownerActorID is this agent's enrolled owner (light agent config; "" on
// the managed agent). args arrive only in a C2 command over the agent's
// authenticated connection, and the C2 overwrites RequesterIsOwner, so the
// marker cannot come from local input; it is still only honoured when the
// requester also matches an owner identity this agent holds itself.
func (s *State) StartDecision(a proto.SolarbeamStartArgs, ownerActorID string) string {
	p := s.policy.Load()
	if p == nil {
		return ""
	}
	if !p.RequireConnectionApproval {
		return StartNotRequired
	}
	if a.RequireConnectionApproval && a.ConnectionApproved {
		return StartApproved
	}
	// Core Team: the assigned user starting an own-device session needs no
	// approval; every other person and session type still does.
	if consent.AssignedOwnDevice(p.AssignedUser, a.SessionType, a.RequesterID, a.RequesterEmail) {
		return StartAssigned
	}
	// Tier 3 (BYOD in an approval-required venue): the C2-marked owner,
	// re-checked against this agent's enrolled owner.
	if a.RequesterIsOwner && proto.NormalizeSessionType(a.SessionType) == proto.SessionTypeOwnDevice &&
		consent.IsOwner(ownerActorID, a.RequesterID) {
		return StartOwnerSelf
	}
	return ""
}

// AssignedUser is the C2's assigned_user for this machine ("" when unknown).
func (s *State) AssignedUser() string {
	if p := s.policy.Load(); p != nil {
		return p.AssignedUser
	}
	return ""
}
