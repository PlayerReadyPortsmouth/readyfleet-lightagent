// Package consent decides what happens when someone asks to connect to a
// person's own device.
//
// The BYOD light agent runs on a mentor's personal PC, not on company
// hardware, and the promise ReadyApp makes them is specific: only you can
// authorise a connection, and your device asks you, on its screen, every
// time. That promise has exactly two exceptions and one failure mode, and
// this package is where all three are decided so both platforms behave
// identically:
//
//   - The device's own enrolled OWNER connecting to their own machine is
//     not "someone asking" at all. Staff have unattended access to their
//     own device; prompting them for permission to use their own PC is
//     noise, not consent. Decided here, agent-side, against the owner
//     identity the enrollment carried (config.OwnerActorID, sourced from
//     the controller's machines.owner_actor_id) — not taken on trust from
//     whoever sent the command.
//   - ANYONE ELSE is asked, on the device's screen, and nothing proceeds
//     until they answer.
//   - Anything that stops the question being asked and answered — no
//     desktop session, no notification daemon, a dismissed prompt, a
//     prompt nobody answered — is a DECLINE. Fail closed: a promise that
//     "your device asks you" cannot be kept by a code path that quietly
//     answers on the person's behalf, and the safe direction to be wrong
//     in is "we did not connect".
//
// The decision is deliberately platform-neutral and lives above the
// prompting mechanism (a Windows TaskDialog, a Linux D-Bus notification
// with Accept/Decline actions). Asker is the seam; every fail-closed path
// arrives here as an Outcome, so there is one place that says what a
// decision means and one log line that says which path produced it.
package consent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Outcome is the result of a consent decision. Every value is either
// allowed (exactly two are) or a decline naming the path that produced it,
// so the agent log answers "why did that connection not go ahead?" without
// needing a second line of context.
type Outcome string

const (
	// OutcomeOwnerUnattended — the requester IS this device's enrolled
	// owner. Allowed without prompting; nobody was asked because there was
	// nobody to ask.
	OutcomeOwnerUnattended Outcome = "owner_unattended"

	// OutcomeAssignedUnattended — the requester is the laptop's assigned
	// user connecting for an own-device session (and only that type).
	OutcomeAssignedUnattended Outcome = "assigned_unattended"

	// OutcomeAccepted — a human at the device chose Accept.
	OutcomeAccepted Outcome = "accepted"

	// OutcomeDeclined — a human at the device chose Decline.
	OutcomeDeclined Outcome = "declined"

	// OutcomeDismissed — the prompt was closed without an answer (swiped
	// away, cleared with the rest of the notification centre).
	OutcomeDismissed Outcome = "dismissed"

	// OutcomeTimedOut — the prompt was displayed and nobody answered it
	// within the prompt's own window, or the notification daemon expired
	// it first.
	OutcomeTimedOut Outcome = "timed_out"

	// OutcomeCancelled — the agent itself gave up waiting (shutdown, the
	// command's context ended).
	OutcomeCancelled Outcome = "cancelled"

	// OutcomeNoSessionBus — there is no desktop session to draw a prompt
	// on. On Linux this is a headless or not-logged-in machine:
	// DBUS_SESSION_BUS_ADDRESS is absent or the bus refuses a connection.
	OutcomeNoSessionBus Outcome = "no_session_bus"

	// OutcomeNoNotificationDaemon — there is a session bus but nothing on
	// it implements org.freedesktop.Notifications, or it stopped answering
	// mid-prompt. The question could not be put on screen.
	OutcomeNoNotificationDaemon Outcome = "no_notification_daemon"

	// OutcomeNoPrompter — this build has no way to ask on this platform.
	// The historical bug this package exists to fix: the non-Windows
	// prompter returned "yes" unconditionally without displaying
	// anything, so a Linux mentor was never asked and always consented.
	OutcomeNoPrompter Outcome = "no_prompter"
)

// Allowed reports whether the outcome permits the connection to proceed.
// Exactly two outcomes do; everything else — including every error and
// every absent-desktop path — is a decline.
func (o Outcome) Allowed() bool {
	return o == OutcomeOwnerUnattended || o == OutcomeAssignedUnattended || o == OutcomeAccepted
}

// Reported maps an outcome to the value sent to the C2 (proto.Consent*).
// ok is false when nobody was asked (owner or assigned user), so there is
// nothing to report. Every non-answer, including each failure to put the
// question, is "no_answer": fail closed, ReadyApp reads it as declined.
func (o Outcome) Reported() (value string, ok bool) {
	switch o {
	case OutcomeOwnerUnattended, OutcomeAssignedUnattended:
		return "", false
	case OutcomeAccepted:
		return proto.ConsentAccepted, true
	case OutcomeDeclined:
		return proto.ConsentDeclined, true
	case OutcomeTimedOut:
		return proto.ConsentTimedOut, true
	}
	return proto.ConsentNoAnswer, true
}

// Request is one "someone wants to connect to your device" question.
type Request struct {
	// Title and Body are the notification's own text, already written by
	// ReadyApp to name WHO is asking ("Aurora Nicholas wants to connect to
	// your PC") and which device. Shown to the person verbatim: the whole
	// point is that they can see who they are answering.
	Title string
	Body  string

	// URL is the device page the request is recorded on. Opened only when
	// a human actually chose Accept — see Gate.Decide's doc comment for
	// why the authoritative approval still happens there.
	URL string

	// RequesterID is the identity of whoever asked, as attributed by the
	// controller from its own authenticated caller — never a value the
	// caller supplied in a body it controls. Empty means "unattributed",
	// which is treated as not-the-owner and therefore prompts.
	RequesterID string

	// RequesterEmail is the same attribution by email; an assigned user may
	// be recorded as either. Controller-filled, like RequesterID.
	RequesterEmail string

	// SessionType is the session type asked for; empty means cyp_session.
	// Only "staff_remote_own_device" can use the assigned-user exemption.
	SessionType string
}

// Asker puts the question on the device's screen and reports what came
// back. Implementations must never block indefinitely and must never
// return an allowed Outcome for a prompt that was not answered by a human:
// their entire contract is that Accepted means somebody pressed Accept.
type Asker interface {
	Ask(ctx context.Context, req Request) Outcome
}

// Gate is the decision. Construct one per agent with the owner identity
// this machine enrolled under and the platform's Asker.
type Gate struct {
	// OwnerID is the enrolled owner's actor id (the controller's
	// machines.owner_actor_id, delivered in the enrollment response and
	// persisted in the agent's config). Empty on a machine enrolled before
	// the field existed, which simply means every request is prompted —
	// the safe direction.
	OwnerID string

	// AssignedUser is the managed laptop's assigned_user from the C2 (venue
	// policy). It is exempt like the owner, for own-device sessions only.
	AssignedUser string

	// Asker is the platform's prompt. Nil means this build cannot ask,
	// which is a decline, not a pass.
	Asker Asker

	// Logger receives exactly one line per decision. Defaults to
	// slog.Default().
	Logger *slog.Logger
}

// Decide answers one connection request.
//
// The owner check comes first and short-circuits the prompt entirely: a
// mentor connecting to their own machine is unattended access to their own
// property, and is logged at info so it stays auditable locally even
// though nobody was asked.
//
// Everything else is put to the person at the keyboard. Note what Accepted
// does and does not mean: it means a human at this device said yes, which
// is what unblocks opening the request page. The authoritative approval is
// still recorded there, in their signed-in ReadyApp session — letting
// someone into your home PC should be attributable to a person who
// authenticated, not only to a button press on an unlocked desktop. The
// prompt is the gate; the page is the record.
func (g Gate) Decide(ctx context.Context, req Request) Outcome {
	log := g.Logger
	if log == nil {
		log = slog.Default()
	}

	if IsOwner(g.OwnerID, req.RequesterID) {
		log.Info("connection request allowed without prompting: requester is this device's enrolled owner",
			"outcome", string(OutcomeOwnerUnattended), "requester_id", req.RequesterID)
		return OutcomeOwnerUnattended
	}

	if AssignedOwnDevice(g.AssignedUser, req.SessionType, req.RequesterID, req.RequesterEmail) {
		log.Info("connection request allowed without prompting: requester is this laptop's assigned user, own-device session",
			"outcome", string(OutcomeAssignedUnattended), "requester_id", req.RequesterID)
		return OutcomeAssignedUnattended
	}

	if g.Asker == nil {
		log.Warn("connection request DECLINED: this build has no way to ask on this platform",
			"outcome", string(OutcomeNoPrompter), "requester_id", req.RequesterID)
		return OutcomeNoPrompter
	}

	out := g.Asker.Ask(ctx, req)
	if out.Allowed() {
		log.Info("connection request allowed: answered at the device",
			"outcome", string(out), "requester_id", req.RequesterID)
		return out
	}
	log.Warn("connection request DECLINED",
		"outcome", string(out), "requester_id", req.RequesterID)
	return out
}

// IsOwner reports whether requesterID identifies the same person as
// ownerID.
//
// Both sides are Entra object ids that have travelled through a database
// column, a JSON body and a config file, so they are compared after
// trimming surrounding whitespace and case-folding — a GUID differing only
// in hex casing is the same GUID, and a stray newline from a hand-edited
// config must not silently turn an owner into a stranger.
//
// An empty identity on EITHER side is never a match. That is the rule that
// makes an unattributed request, or a machine enrolled before owner
// identity was carried, prompt rather than pass: two blanks are not the
// same person.
func IsOwner(ownerID, requesterID string) bool {
	owner := normaliseIdentity(ownerID)
	requester := normaliseIdentity(requesterID)
	return owner != "" && requester != "" && owner == requester
}

// AssignedOwnDevice reports whether the requester is the assigned user AND
// the session is the own-device type. Any other type (or none: cyp_session)
// never qualifies, so Core Team approval still applies to it.
func AssignedOwnDevice(assigned, sessionType, requesterID, requesterEmail string) bool {
	return proto.NormalizeSessionType(sessionType) == proto.SessionTypeOwnDevice &&
		(IsOwner(assigned, requesterID) || IsOwner(assigned, requesterEmail))
}

func normaliseIdentity(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
