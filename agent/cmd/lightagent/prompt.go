package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/connectionprompt"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/transport"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venuepolicy"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// connectionRequestKind is the one notification that is a question rather
// than a notice. A balloon or a toast fades after a few seconds whether or
// not anyone saw it, which is fine for "your agent updated" and wrong for
// "a colleague is asking to connect to your PC": miss it and the request
// expires unanswered 30 minutes later, with the mentor never knowing they
// were asked.
//
// So this kind gets a prompt that stays until it is answered, and the
// answer is a real one — agent/internal/consent decides what happens, and
// its rules are the same on both platforms. Accept opens the request on
// the devices page, where the decision is recorded against a person who
// signed in rather than against a button press on an unlocked desktop.
const connectionRequestKind = "connection_request"

// consentPromptBudget bounds the whole detached decision, prompt included.
// Comfortably longer than the prompt's own timeout so the prompt is what
// normally ends the wait and the log names the real reason; this is the
// backstop for a prompt implementation that somehow never returns.
const consentPromptBudget = 3 * time.Minute

// gate is the seam runShowNotification calls — consent.Gate satisfies it,
// and tests substitute a fake to assert routing without a desktop.
type gate interface {
	Decide(ctx context.Context, req consent.Request) consent.Outcome
}

// openURLFn is the browser hand-off, behind a var so tests can assert
// which outcomes steer a mentor's browser without opening one.
var openURLFn = trayicon.OpenURL

// newConsentGate builds the decision for this machine: the owner identity
// its enrollment carried, plus whatever this platform can ask with (asker;
// nil means it cannot, which declines), under the C2's venue policy. The
// asker comes from menuHelper.consentAsker: the platform prompt on Windows
// and Linux, the menu-bar helper or nothing on macOS.
func newConsentGate(ownerActorID string, policy *venuepolicy.State, asker consent.Asker, logger *slog.Logger) venueConsentGate {
	return venueConsentGate{
		Gate: consent.Gate{
			OwnerID: ownerActorID,
			Asker:   asker,
			Logger:  logger,
		},
		policy: policy,
	}
}

// venueConsentGate drops the enrolled-owner exemption on a venue that
// requires connection approval (Core Team), and while the policy is still
// unknown: every connection there is put to the person at the laptop,
// including one from whoever minted its enrolment token. Same rule as the
// managed agent, whose connection requests always reach the companion prompt.
type venueConsentGate struct {
	consent.Gate
	policy *venuepolicy.State
}

func (g venueConsentGate) Decide(ctx context.Context, req consent.Request) consent.Outcome {
	inner := g.Gate
	if g.policy == nil || g.policy.RequiresApproval() {
		inner.OwnerID = ""
	}
	if g.policy != nil {
		inner.AssignedUser = g.policy.AssignedUser()
	}
	return inner.Decide(ctx, req)
}

// showConnectionRequest puts the question without blocking the command
// handler. The prompt waits on a human, which can be a minute; holding the
// handler that long would stall the agent's command loop and outlive the
// command's own TTL. The result reported upstream therefore means "the
// device was asked", not "the person answered" — the answer lands on the
// request row when they open the page.
func showConnectionRequest(g gate, args proto.ShowNotificationArgs, send transport.SendFunc) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), consentPromptBudget)
		defer cancel()
		p, end := consentPrompts.Begin(ctx, args.RequestID)
		defer end()

		connectionprompt.Show(p.Ctx, reportingGate{g: g, p: p, requestID: args.RequestID, send: send}, args, openURLFn)
	}()
}

// consentPrompts holds the open prompts so the C2 can close one.
var consentPrompts = &consent.Prompts{}

// reportingGate tells the C2 how the prompt ended (when the request has an
// id). A prompt the owner already decided in ReadyApp reports nothing and
// never counts as accepted, whatever a stuck platform dialog later returns.
type reportingGate struct {
	g         gate
	p         *consent.Prompt
	requestID string
	send      transport.SendFunc
}

func (r reportingGate) Decide(ctx context.Context, req consent.Request) consent.Outcome {
	out := r.g.Decide(ctx, req)
	if r.p.Dismissed() {
		return consent.OutcomeCancelled
	}
	if v, ok := out.Reported(); ok && r.requestID != "" && r.send != nil {
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if env, err := proto.NewEnvelope(proto.MsgConsentDecision, uuid.Nil, nil, proto.ConsentDecisionData{RequestID: r.requestID, Outcome: v}); err == nil {
			if err := r.send(rctx, env); err != nil {
				slog.Default().Warn("report consent decision", "err", err)
			}
		}
	}
	return out
}

// handleConsentDismiss closes the open prompt for a request the owner has
// decided in ReadyApp.
func handleConsentDismiss(_ context.Context, env proto.Envelope) {
	var d proto.ConsentDismissData
	if env.DecodeData(&d) != nil || d.RequestID == "" {
		return
	}
	consentPrompts.Dismiss(d.RequestID)
}
