// Package companionadapter joins local observations with a scoped broker lease.
// Incoming viewer names never become authority; a venue mutation always travels
// through the approving staff browser. BYOD owner operations are injected apart.
//
// The staff lease is presence-bound. The panel runs in the young person's own
// Windows session, so a staff approval lasts only while the staff member is
// plausibly still at the PC: it is revoked on session lock, disconnect or user
// switch, on panel close, and after StaffIdleTimeout without panel input
// (companionui.StaffAuthority). Revocation erases the native credential and any
// pending pairing, and Action refuses every staff-routed request afterwards —
// the check lives here, where requests execute, not in the panel's rendering.
// Authority is withheld whenever the panel cannot vouch for those signals.
package companionadapter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/companionclient"
	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
)

type LocalSnapshot func(context.Context) (ui.Snapshot, error)
type Options struct {
	Local         LocalSnapshot
	Client        *companionclient.Client
	ClientFactory func(ui.Snapshot) (*companionclient.Client, error)
	OpenApproval  func(string) error
	OpenReadyApp  func() error
	// OwnerHosting receives the exact reviewed local state and must atomically
	// check its revision/session before changing the device policy.
	OwnerHosting    func(context.Context, ui.Action) error
	OwnerDisconnect func(context.Context, ui.Action) error
	// StaffIdleTimeout ends staff authority after this long without panel
	// input. Zero means DefaultStaffIdleTimeout; other values are clamped to
	// [MinStaffIdleTimeout, MaxStaffIdleTimeout].
	StaffIdleTimeout time.Duration
	// Now is a test seam for the idle clock; nil means time.Now.
	Now func() time.Time
}

const (
	DefaultStaffIdleTimeout = 5 * time.Minute
	MinStaffIdleTimeout     = time.Minute
	MaxStaffIdleTimeout     = 15 * time.Minute
)

// ErrStaffSignInExpired refuses a staff-routed action once authority is gone.
var ErrStaffSignInExpired = errors.New("Staff sign-in expired. Sign in again to continue.")

// ClampStaffIdleTimeout bounds a configured idle timeout; zero selects the default.
func ClampStaffIdleTimeout(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return DefaultStaffIdleTimeout
	case d < MinStaffIdleTimeout:
		return MinStaffIdleTimeout
	case d > MaxStaffIdleTimeout:
		return MaxStaffIdleTimeout
	}
	return d
}

type Adapter struct {
	mu                       sync.Mutex
	opts                     Options
	selected                 string
	state                    companionclient.State
	last                     ui.Snapshot
	commandID, commandStatus string
	idle                     time.Duration
	// grantedAt is when the current staff lease was first observed; zero means
	// no staff authority. expired is the sticky reason shown after revocation.
	grantedAt       time.Time
	expired         ui.RevokeReason
	seenRevocations uint64

	// Signals from the panel's message loop. signalMu is never held across
	// I/O, so the UI thread cannot stall behind a slow broker call holding mu.
	signalMu    sync.Mutex
	monitored   bool
	revocations uint64
	reason      ui.RevokeReason
	interaction time.Time
}

func New(opts Options) *Adapter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Adapter{opts: opts, idle: ClampStaffIdleTimeout(opts.StaffIdleTimeout)}
}

var _ ui.StaffAuthority = (*Adapter)(nil)

func (a *Adapter) StaffSessionMonitored(ok bool) {
	a.signalMu.Lock()
	a.monitored = ok
	a.signalMu.Unlock()
	if !ok {
		a.RevokeStaff(ui.RevokeSessionUnmonitored)
	}
}
func (a *Adapter) StaffInteraction() {
	now := a.opts.Now()
	a.signalMu.Lock()
	a.interaction = now
	a.signalMu.Unlock()
}

// RevokeStaff records the revocation without waiting for mu; the next Snapshot
// or Action (including one already queued behind mu) applies it before doing
// anything else.
func (a *Adapter) RevokeStaff(reason ui.RevokeReason) {
	if reason == ui.NotRevoked {
		return
	}
	a.signalMu.Lock()
	a.revocations++
	a.reason = reason
	a.signalMu.Unlock()
}

// enforceStaffLocked applies pending revocations, missing session monitoring
// and the idle timeout. It reports whether staff authority remains current.
func (a *Adapter) enforceStaffLocked() bool {
	a.signalMu.Lock()
	monitored, revocations, reason, interaction := a.monitored, a.revocations, a.reason, a.interaction
	a.signalMu.Unlock()
	// Read the clock after the signals, so an interaction recorded concurrently
	// can never appear to be later than now.
	now := a.opts.Now()
	if revocations != a.seenRevocations {
		a.seenRevocations = revocations
		a.revokeLocked(reason)
		return false
	}
	if !monitored {
		a.revokeLocked(ui.RevokeSessionUnmonitored)
		return false
	}
	if a.grantedAt.IsZero() {
		return false
	}
	last := a.grantedAt
	if interaction.After(last) {
		last = interaction
	}
	// A clock reading before the last activity is not trustworthy evidence of
	// presence; fail closed rather than extend the lease.
	if now.Before(last) || now.Sub(last) > a.idle {
		a.revokeLocked(ui.RevokeIdle)
		return false
	}
	return a.opts.Client != nil
}

// revokeLocked drops local staff state and the native credential. The sticky
// "expired" state is shown only if there was something to expire.
func (a *Adapter) revokeLocked(reason ui.RevokeReason) {
	held := !a.grantedAt.IsZero()
	if a.opts.Client != nil && a.opts.Client.Revoke() {
		held = true
	}
	if held {
		a.expired = reason
	}
	a.grantedAt = time.Time{}
	a.selected = ""
	a.state = companionclient.State{}
	a.commandID, a.commandStatus = "", ""
}

func (a *Adapter) showExpiredLocked(s *ui.Snapshot) {
	if a.expired == ui.NotRevoked {
		return
	}
	s.Authority.Label = "Staff sign-in expired"
	s.Notice = "Staff sign-in expired. Sign in again to use staff controls."
	s.Details += "\nStaff sign-in expired: " + a.expired.String()
}
func (a *Adapter) Snapshot(ctx context.Context) (ui.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.opts.Local == nil {
		return ui.Snapshot{}, errors.New("local status unavailable")
	}
	s, err := a.opts.Local(ctx)
	if err != nil {
		return s, err
	}
	s = s.Clone()
	if a.opts.Client != nil && a.opts.Client.DeviceID() != s.Device.ID {
		_ = a.opts.Client.SignOut(ctx)
		a.opts.Client = nil
		a.grantedAt = time.Time{}
		a.selected = ""
		a.state = companionclient.State{}
	}
	if a.opts.Client == nil && a.opts.ClientFactory != nil {
		if client, err := a.opts.ClientFactory(s); err == nil {
			a.opts.Client = client
		}
	}
	a.enforceStaffLocked()
	a.signalMu.Lock()
	monitored := a.monitored
	a.signalMu.Unlock()
	s.Capabilities[ui.OpenReadyApp] = ui.Capability{Allowed: a.opts.OpenReadyApp != nil}
	if s.Hosting.Active {
		s.Details += "\nReadyApp opens in your normal browser. Browser content is visible in the stream."
	}
	s.Capabilities[ui.SignIn] = ui.Capability{Allowed: a.opts.Client != nil && a.opts.OpenApproval != nil && monitored}
	if !monitored && a.opts.Client != nil {
		s.Capabilities[ui.SignIn].Reason = ui.RevokeSessionUnmonitored.String()
	}
	if a.opts.OwnerHosting != nil && s.Authority.HostingAuthorized {
		s.Capabilities[ui.SetHostingAvailable] = ui.Capability{Allowed: s.Hosting.Known}
	}
	if a.opts.OwnerDisconnect != nil && s.Authority.HostingAuthorized {
		s.Capabilities[ui.DisconnectHosting] = ui.Capability{Allowed: s.Hosting.Known && s.Hosting.Active && s.Hosting.SessionID != ""}
	}
	if a.opts.Client == nil {
		a.showExpiredLocked(&s)
		a.last = s.Clone()
		return s, nil
	}
	state, err := a.opts.Client.State(ctx)
	// The broker call can take seconds. Re-enforce after it, so a lock that
	// arrived meanwhile wins over whatever authority the response describes.
	if err == nil {
		if a.grantedAt.IsZero() {
			a.grantedAt = a.opts.Now()
		}
		if !a.enforceStaffLocked() {
			state, err = companionclient.State{}, companionclient.ErrSignedOut
		}
	}
	if err != nil {
		// A retained token is not current authority. Keep only the user's exact
		// selection and display name for recovery; local BYOD ownership is separate.
		if a.opts.Client.HasBinding() {
			s.Authority.StaffName = a.state.StaffName
			s.Authority.Label = "Browser unavailable"
			s.Capabilities[ui.SignIn] = ui.Capability{}
			s.Capabilities[ui.SignOut] = ui.Capability{Allowed: true}
			s.Notice = "Browser unavailable. Open ReadyApp in your approving browser to reconnect."
			s.Details += "\nStaff controls are paused until the approving ReadyApp browser reconnects."
		} else {
			a.selected = ""
			a.state = companionclient.State{}
			a.grantedAt = time.Time{}
			if p := a.opts.Client.Pairing(); p != nil {
				s.Capabilities[ui.SignIn] = ui.Capability{}
				s.Capabilities[ui.SignOut] = ui.Capability{Allowed: true}
				s.Details += "\nStaff approval pending. Match code " + p.UserCode + " in the browser. To restart, sign out then sign in."
				s.Notice = "Approve code " + p.UserCode + " in your browser."
				if errors.Is(err, companionclient.ErrPairingUncertain) {
					s.Notice = "Approval response not confirmed. Sign out, then sign in to restart."
				}
			} else if !errors.Is(err, companionclient.ErrSignedOut) {
				s.Notice = "Staff sign-in unavailable. Try again when ReadyApp reconnects."
			}
			a.showExpiredLocked(&s)
		}
		a.last = s.Clone()
		return s, nil
	}
	if state.DeviceID != s.Device.ID {
		return ui.Snapshot{}, errors.New("Broker binding does not match this device; sign out and pair again")
	}
	a.state = state
	a.expired = ui.NotRevoked
	s.Authority.StaffName = state.StaffName
	if s.Authority.StaffName == "" {
		s.Authority.StaffName = "Staff signed in"
	}
	s.Authority.Label = s.Authority.StaffName
	s.Capabilities[ui.SignIn] = ui.Capability{}
	s.Capabilities[ui.SignOut] = ui.Capability{Allowed: true}
	s.ValidUntil = minTime(s.ValidUntil, state.ExpiresAt)
	for _, kind := range state.HostAllowedActions {
		if a.opts.OwnerHosting != nil && kind == "set_hosting" {
			continue
		}
		switch kind {
		case "set_hosting":
			s.Authority.HostingAuthorized = true
			s.Capabilities[ui.SetHostingAvailable] = ui.Capability{Allowed: true}
		case "disconnect_hosting":
			s.Authority.HostingAuthorized = true
			s.Capabilities[ui.DisconnectHosting] = ui.Capability{Allowed: true}
		case "check_updates":
			s.Capabilities[ui.CheckUpdates] = ui.Capability{Allowed: true}
		}
	}
	if state.HostMutationPending {
		s.Capabilities[ui.SetHostingAvailable] = ui.Capability{Reason: "A hosting change is pending confirmation."}
		s.Capabilities[ui.DisconnectHosting] = ui.Capability{Reason: "A hosting change is pending confirmation."}
		s.Notice = "Hosting change pending; waiting for device confirmation."
	}
	sort.Slice(state.Sessions, func(i, j int) bool { return state.Sessions[i].RegistrationID < state.Sessions[j].RegistrationID })
	for i, session := range state.Sessions {
		label := session.HostName
		if session.ActivityLabel != "" {
			label += " · " + session.ActivityLabel
		} else {
			label += fmt.Sprintf(" · browser connection %d", i+1)
		}
		s.Sessions = append(s.Sessions, ui.SessionChoice{RegistrationID: session.RegistrationID, Label: label})
		if session.RegistrationID != a.selected {
			continue
		}
		s.Viewing = ui.Viewing{RegistrationID: session.RegistrationID, SessionID: session.SessionID, ParticipantSID: session.ParticipantSID, HostName: session.HostName, Revision: session.Revision, Connected: true, Exclusive: session.ExclusiveHeldBySelf}
		s.Authority.ViewerAuthorized = true
		occurrenceLabel := session.ActivityLabel
		if !session.ActivityStartsAt.IsZero() {
			occurrenceLabel += " · " + session.ActivityStartsAt.Local().Format("Mon 2 Jan 15:04")
		}
		s.Viewing.PendingGuest = ui.PendingGuest{Identity: session.CYPContactID, OccurrenceID: session.ActivityID, Label: session.CYPName, OccurrenceLabel: occurrenceLabel}
		for _, op := range session.AllowedActions {
			if kind := uiKind(op); kind != ui.NoAction {
				s.Capabilities[kind] = ui.Capability{Allowed: true}
			}
		}
	}
	if s.Viewing.RegistrationID == "" {
		a.selected = ""
		s.Viewing.Ambiguous = len(s.Sessions) > 1
	}
	s.Capabilities[ui.SelectSession] = ui.Capability{Allowed: len(s.Sessions) > 0}
	if a.commandID != "" {
		s.Details += "\nLast requested action: " + a.commandStatus
		for _, cmd := range state.Commands {
			if cmd.CommandID == a.commandID {
				a.commandStatus = commandText(cmd)
				s.Notice = a.commandStatus
				s.Details += "\n" + a.commandStatus
				break
			}
		}
	}
	a.last = s.Clone()
	return s, nil
}
func minTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}
func uiKind(action string) ui.Kind {
	switch action {
	case "take":
		return ui.TakeExclusive
	case "release":
		return ui.ReleaseExclusive
	case "admit":
		return ui.AdmitGuest
	case "focus":
		return ui.FocusViewer
	case "leave":
		return ui.LeaveViewer
	}
	return ui.NoAction
}
func commandText(c companionclient.Command) string {
	switch c.Status {
	case "queued", "executing":
		return "Request " + c.Status + "; waiting for the approving browser."
	case "succeeded":
		return "Browser action completed. Device changes still require confirmed status."
	case "failed":
		return "Request failed; refresh and review before retrying."
	default:
		return "Request outcome is not confirmed."
	}
}
func (a *Adapter) Action(ctx context.Context, action ui.Action) (ui.ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := ui.ActionResult{}
	// Enforced before the reviewed snapshot is consulted: a.last may still
	// describe authority that a lock or the idle timeout has since revoked.
	staffCurrent := a.enforceStaffLocked()
	if a.staffRouted(action.Kind) && !staffCurrent {
		if a.expired != ui.NotRevoked {
			return result, ErrStaffSignInExpired
		}
		return result, companionclient.ErrSignedOut
	}
	if action.Kind != ui.SignIn && action.Kind != ui.SignOut {
		var expected ui.Review
		var err error
		if action.Kind == ui.SelectSession {
			expected, err = a.last.PrepareSelection(action.RegistrationID, time.Now())
		} else {
			expected, err = a.last.Prepare(action.Kind, time.Now())
		}
		if err != nil {
			return result, err
		}
		left, right := action, expected.Action
		left.RequestID = ""
		right.RequestID = ""
		if left != right {
			return result, errors.New("Reviewed device or session binding changed")
		}
	}
	switch action.Kind {
	case ui.SignIn:
		if a.opts.Client == nil || a.opts.OpenApproval == nil {
			return result, errors.New("staff sign-in unavailable")
		}
		a.signalMu.Lock()
		monitored := a.monitored
		a.signalMu.Unlock()
		if !monitored {
			return result, errors.New("Staff sign-in unavailable: " + ui.RevokeSessionUnmonitored.String())
		}
		a.expired = ui.NotRevoked
		p, err := a.opts.Client.BeginPairing(ctx)
		if err != nil {
			return result, err
		}
		if err = a.opts.OpenApproval(p.VerificationURL); err != nil {
			return result, err
		}
		return ui.ActionResult{Message: "In the browser, approve this device with code " + p.UserCode + "."}, nil
	case ui.SignOut:
		if a.opts.Client == nil {
			return result, nil
		}
		err := a.opts.Client.SignOut(ctx)
		a.selected = ""
		a.state = companionclient.State{}
		a.grantedAt = time.Time{}
		a.expired = ui.NotRevoked
		return ui.ActionResult{Message: "Staff credentials cleared from this companion."}, err
	case ui.SelectSession:
		if !a.last.Fresh(time.Now()) {
			return result, errors.New("Session list is out of date")
		}
		for _, session := range a.state.Sessions {
			if session.RegistrationID == action.RegistrationID {
				a.selected = session.RegistrationID
				return ui.ActionResult{Message: "Browser connection selected."}, nil
			}
		}
		return result, errors.New("Selected browser connection no longer exists")
	case ui.OpenReadyApp:
		if a.opts.OpenReadyApp == nil {
			return result, errors.New("ReadyApp window unavailable")
		}
		return result, a.opts.OpenReadyApp()
	case ui.DisconnectHosting:
		if a.opts.OwnerDisconnect != nil {
			if err := a.opts.OwnerDisconnect(ctx, action); err != nil {
				return result, err
			}
			return ui.ActionResult{Message: "Local disconnect requested; refreshing confirmed status."}, nil
		}
	case ui.SetHostingAvailable:
		if a.opts.OwnerHosting != nil {
			if err := a.opts.OwnerHosting(ctx, action); err != nil {
				return result, err
			}
			return ui.ActionResult{Message: "Local hosting preference applied; refreshing confirmed status."}, nil
		}
	}
	if a.opts.Client == nil {
		return result, errors.New("Staff controls are signed out")
	}
	req := companionclient.Action{IdempotencyKey: action.RequestID}
	switch action.Kind {
	case ui.SetHostingAvailable:
		req.Action = "set_hosting"
		req.Enabled = &action.Enabled
		req.ExpectedSessionID = action.SessionID
	case ui.DisconnectHosting:
		req.Action = "disconnect_hosting"
		req.ExpectedSessionID = action.SessionID
	case ui.CheckUpdates:
		req.Action = "check_updates"
	default:
		if action.RegistrationID == "" || action.RegistrationID != a.selected {
			return result, errors.New("Browser selection changed")
		}
		req.RegistrationID = action.RegistrationID
		req.ExpectedRevision = action.ExpectedRevision
		switch action.Kind {
		case ui.TakeExclusive:
			req.Action = "take"
		case ui.ReleaseExclusive:
			req.Action = "release"
		case ui.AdmitGuest:
			req.Action = "admit"
			req.Admission = &companionclient.Admission{ActivityID: action.OccurrenceID, CYPContactID: action.GuestIdentity}
		case ui.FocusViewer:
			req.Action = "focus"
		case ui.LeaveViewer:
			req.Action = "leave"
		default:
			return result, errors.New("Unsupported companion action")
		}
	}
	command, err := a.opts.Client.Action(ctx, req)
	if err != nil {
		return result, err
	}
	a.commandID = command.CommandID
	a.commandStatus = commandText(command)
	return ui.ActionResult{Message: a.commandStatus}, nil
}

// staffRouted reports whether an action exercises the browser-granted staff
// lease. Sign-in/out, opening ReadyApp and BYOD owner operations do not.
func (a *Adapter) staffRouted(kind ui.Kind) bool {
	switch kind {
	case ui.SignIn, ui.SignOut, ui.OpenReadyApp:
		return false
	case ui.SetHostingAvailable:
		return a.opts.OwnerHosting == nil
	case ui.DisconnectHosting:
		return a.opts.OwnerDisconnect == nil
	}
	return true
}
func Names(names []string) string { return strings.Join(names, ", ") }
