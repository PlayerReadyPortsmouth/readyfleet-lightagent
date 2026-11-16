// Package companionui provides the shared user-session native companion panel.
// It displays backend facts and builds exact, reviewed requests; it is never an
// authority source. Backend implementations must revalidate every mutation.
package companionui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type Kind uint8

const (
	NoAction Kind = iota
	SetHostingAvailable
	DisconnectHosting
	OpenReadyApp
	CheckUpdates
	AdmitGuest
	TakeExclusive
	ReleaseExclusive
	FocusViewer
	LeaveViewer
	SignIn
	SignOut
	SelectSession
)

type Capability struct {
	Allowed bool
	Reason  string
}
type Capabilities [SelectSession + 1]Capability
type Device struct{ ID, Name, Profile, Connection, AgentVersion, EngineVersion, UpdateStatus string }
type Hosting struct {
	Known, Available, Active bool
	SessionID, Revision      string
	ViewerNames              []string
}
type PendingGuest struct{ Identity, OccurrenceID, Label, OccurrenceLabel string }
type Viewing struct {
	RegistrationID, SessionID, ParticipantSID, HostName, Revision string
	Connected, Exclusive, Ambiguous                               bool
	PendingGuest                                                  PendingGuest
}
type Authority struct {
	Locked, HostingAuthorized, ViewerAuthorized bool
	Label, StaffName                            string
}
type SessionChoice struct{ RegistrationID, Label string }
type Snapshot struct {
	Sessions               []SessionChoice
	ObservedAt, ValidUntil time.Time
	Device                 Device
	Hosting                Hosting
	Viewing                Viewing
	Authority              Authority
	Capabilities           Capabilities
	Details                string
	Notice                 string
}

func (s Snapshot) Clone() Snapshot {
	s.Hosting.ViewerNames = append([]string(nil), s.Hosting.ViewerNames...)
	s.Sessions = append([]SessionChoice(nil), s.Sessions...)
	return s
}
func (s Snapshot) Fresh(now time.Time) bool {
	return !s.ObservedAt.IsZero() && !now.Before(s.ObservedAt) && now.Sub(s.ObservedAt) <= 15*time.Second && now.Before(s.ValidUntil)
}

type Action struct {
	Kind                                                                                                          Kind
	RequestID, DeviceID, ExpectedRevision, RegistrationID, SessionID, ParticipantSID, GuestIdentity, OccurrenceID string
	Enabled                                                                                                       bool
}
type ActionResult struct{ Message string }
type Backend interface {
	Snapshot(context.Context) (Snapshot, error)
	Action(context.Context, Action) (ActionResult, error)
}
type Review struct {
	Action                           Action
	Title, Description, ConfirmLabel string
}

func (s Snapshot) Allow(kind Kind, now time.Time) error {
	if kind <= NoAction || kind > SelectSession {
		return errors.New("Unknown action")
	}
	if !s.Fresh(now) {
		return errors.New("Status is unavailable or out of date. Refresh to continue.")
	}
	if s.Authority.Locked {
		return errors.New("Unlock this Windows session to continue.")
	}
	if !s.Capabilities[kind].Allowed {
		if s.Capabilities[kind].Reason != "" {
			return errors.New(s.Capabilities[kind].Reason)
		}
		return errors.New("This action is unavailable.")
	}
	switch kind {
	case SetHostingAvailable, DisconnectHosting:
		if !s.Authority.HostingAuthorized || !s.Hosting.Known || s.Device.ID == "" || s.Hosting.Revision == "" {
			return errors.New("Staff or owner authorisation and current hosting status are required.")
		}
		if kind == DisconnectHosting && (!s.Hosting.Active || s.Hosting.SessionID == "") {
			return errors.New("No confirmed incoming session.")
		}
	case CheckUpdates:
		if s.Hosting.Active || !s.Hosting.Known {
			return errors.New("Updates wait until no session is active.")
		}
	case AdmitGuest, TakeExclusive, ReleaseExclusive, FocusViewer, LeaveViewer:
		v := s.Viewing
		if !s.Authority.ViewerAuthorized || !v.Connected || v.Ambiguous || v.RegistrationID == "" || v.SessionID == "" || v.ParticipantSID == "" || v.Revision == "" {
			return errors.New("Choose and authorise one current browser session.")
		}
		if kind == AdmitGuest && (v.PendingGuest.Identity == "" || v.PendingGuest.OccurrenceID == "" || v.PendingGuest.Label == "" || v.PendingGuest.OccurrenceLabel == "") {
			return errors.New("No exact waiting person and occurrence to review.")
		}
		if kind == ReleaseExclusive && !v.Exclusive {
			return errors.New("You do not hold exclusive control.")
		}
	}
	return nil
}
func (s Snapshot) Prepare(kind Kind, now time.Time) (Review, error) {
	if err := s.Allow(kind, now); err != nil {
		return Review{}, err
	}
	a := Action{Kind: kind, RequestID: uuid.NewString(), DeviceID: s.Device.ID}
	r := Review{Action: a, ConfirmLabel: "Confirm"}
	switch kind {
	case SetHostingAvailable:
		r.Action.SessionID = s.Hosting.SessionID
		r.Action.ExpectedRevision = s.Hosting.Revision
		r.Action.Enabled = !s.Hosting.Available
		r.Title = "Turn SolarBeam on?"
		r.Description = "Allow new SolarBeam connections to this PC. Fleet management stays connected."
		r.ConfirmLabel = "Turn on"
		if s.Hosting.Available {
			r.Title = "Turn SolarBeam off?"
			r.Description = "Refuse new connections until you turn it back on. Fleet management stays connected."
			r.ConfirmLabel = "Turn off"
			if s.Hosting.Active {
				r.Description = "This disconnects the active viewer and refuses new connections until you turn it back on. Fleet management stays connected."
			}
		}
	case DisconnectHosting:
		r.Action.SessionID = s.Hosting.SessionID
		r.Action.ExpectedRevision = s.Hosting.Revision
		r.Title = "Disconnect incoming session?"
		r.Description = "Everyone viewing this PC will disconnect. This does not turn off Fleet management."
		r.ConfirmLabel = "Disconnect"
	case SignIn:
		r.Title = "Sign in to staff controls"
		r.ConfirmLabel = "Sign in"
	case SignOut:
		r.Title = "Sign out of staff controls?"
		r.Description = "Remove this companion’s staff authority. Fleet management remains connected."
		r.ConfirmLabel = "Sign out"
	case OpenReadyApp:
		r.Title = "Open ReadyApp in browser"
		r.ConfirmLabel = "Open"
	case CheckUpdates:
		r.Title = "Check update status"
		r.Description = "The backend checks availability. Installed versions change only after confirmed installation."
		r.ConfirmLabel = "Check"
	default:
		v := s.Viewing
		r.Action.RegistrationID = v.RegistrationID
		r.Action.SessionID = v.SessionID
		r.Action.ParticipantSID = v.ParticipantSID
		r.Action.ExpectedRevision = v.Revision
		switch kind {
		case AdmitGuest:
			r.Action.GuestIdentity = v.PendingGuest.Identity
			r.Action.OccurrenceID = v.PendingGuest.OccurrenceID
			r.Title = "Review admission"
			r.Description = fmt.Sprintf("Admit %s to %s on %s?", v.PendingGuest.Label, v.PendingGuest.OccurrenceLabel, v.HostName)
			r.ConfirmLabel = "Admit this person"
		case TakeExclusive:
			r.Title = "Take exclusive control?"
			r.Description = "Only you will drive the selected session on " + v.HostName + ". Other viewers stay connected."
			r.ConfirmLabel = "Take control"
		case ReleaseExclusive:
			r.Title = "Restore shared input?"
			r.Description = "Release exclusive control on " + v.HostName + ". Your input remains available alongside other permitted viewers."
			r.ConfirmLabel = "Restore shared input"
		case FocusViewer:
			r.Title = "Focus browser"
			r.ConfirmLabel = "Focus"
		case LeaveViewer:
			r.Title = "Leave my connection?"
			r.Description = "Leave your selected browser connection to " + v.HostName + ". Everyone else's session remains open."
			r.ConfirmLabel = "Leave my connection"
		}
	}
	return r, nil
}

// Matches prevents a delayed confirmation from targeting a new session/guest.
func (r Review) Matches(s Snapshot, now time.Time) bool {
	var next Review
	var err error
	if r.Action.Kind == SelectSession {
		next, err = s.PrepareSelection(r.Action.RegistrationID, now)
	} else {
		next, err = s.Prepare(r.Action.Kind, now)
	}
	if err != nil {
		return false
	}
	a, b := r.Action, next.Action
	a.RequestID = ""
	b.RequestID = ""
	return a == b
}
func HostingText(s Snapshot, now time.Time) string {
	if !s.Fresh(now) || !s.Hosting.Known {
		return "SolarBeam status unavailable"
	}
	if !s.Hosting.Available {
		return "SolarBeam off · new connections refused"
	}
	if !s.Hosting.Active {
		return "SolarBeam on · nobody connected"
	}
	names := s.Hosting.ViewerNames
	if len(names) == 0 {
		return "Connected · Remote viewer"
	}
	return "Connected · " + strings.Join(names, ", ")
}

func (s Snapshot) PrepareSelection(registrationID string, now time.Time) (Review, error) {
	if err := s.Allow(SelectSession, now); err != nil {
		return Review{}, err
	}
	for _, choice := range s.Sessions {
		if choice.RegistrationID == registrationID {
			return Review{Action: Action{Kind: SelectSession, RequestID: uuid.NewString(), DeviceID: s.Device.ID, RegistrationID: registrationID}, Title: "Select browser connection", Description: choice.Label, ConfirmLabel: "Use this connection"}, nil
		}
	}
	return Review{}, errors.New("Browser connection no longer available")
}
