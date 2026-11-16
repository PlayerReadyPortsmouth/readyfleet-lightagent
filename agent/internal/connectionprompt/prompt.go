// Package connectionprompt shares the BYOD prompt/approval-page flow.
package connectionprompt

import (
	"context"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// AnswerTimeout is how long a remote-venue connection question waits for an
// answer before it counts as a decline.
const AnswerTimeout = 2 * time.Minute

type Gate interface {
	Decide(context.Context, consent.Request) consent.Outcome
}

func Show(ctx context.Context, g Gate, args proto.ShowNotificationArgs, open func(string) error) bool {
	out := g.Decide(ctx, consent.Request{Title: args.Title, Body: args.Body, URL: args.URL, RequesterID: args.RequesterID, RequesterEmail: args.RequesterEmail, SessionType: args.SessionType})
	return out == consent.OutcomeAccepted && args.URL != "" && open(args.URL) == nil
}

// ShowWithin is Show bounded by timeout: no answer in time is a DECLINE, and
// the caller is released so later staff messages are not held up. Platform
// prompts (the Windows TaskDialog) do not honour ctx, so the dialog may stay
// on screen; a late Accept on it is ignored and opens nothing.
func ShowWithin(ctx context.Context, timeout time.Duration, g Gate, args proto.ShowNotificationArgs, open func(string) error) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	guarded := func(u string) error {
		if err := ctx.Err(); err != nil {
			return err // answered after the deadline: already declined
		}
		return open(u)
	}
	done := make(chan bool, 1)
	go func() { done <- Show(ctx, g, args, guarded) }()
	select {
	case ok := <-done:
		return ok
	case <-ctx.Done():
		return false
	}
}
