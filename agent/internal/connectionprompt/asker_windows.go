//go:build windows

package connectionprompt

import (
	"context"
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/installerui"
)

// wizardAsker draws the consent prompt with the same TaskDialog the
// installer wizard uses. Requires the ComCtl32 v6 manifest — see
// agent.manifest and manifest_test.go; without it Windows silently falls
// back to v5, which has no TaskDialogIndirect, runTaskDialog errors, and
// this returns Declined. That is the right failure: a dialog that could
// not be drawn is a question that was never asked.
type wizardAsker struct{}

func NewAsker(*slog.Logger) consent.Asker { return wizardAsker{} }

// Ask blocks on the dialog until the mentor answers it or closes it.
// ShowWelcome returns true only for its Continue button, so every other
// route — Cancel, the window's close box, a dialog that failed to
// initialise — arrives here as false and declines.
func (wizardAsker) Ask(_ context.Context, req consent.Request) consent.Outcome {
	w := installerui.Wizard{Title: "ReadyFleet"}
	ok := w.ShowWelcome(
		req.Title,
		req.Body,
		[]string{
			"You choose whether to allow it — nothing connects until you do",
			"Opens your devices page, where you can accept or decline",
			"If you ignore this, the request expires on its own",
		},
		"Questions or concerns? Contact readyapp@player-ready.co.uk.",
	)
	if ok {
		return consent.OutcomeAccepted
	}
	return consent.OutcomeDeclined
}
