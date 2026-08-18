package main

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/installerui"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/trayicon"
)

// connectionRequestKind is the one notification that must not be a tray
// balloon. A balloon fades after a few seconds whether or not anyone saw it,
// which is fine for "your agent updated" and wrong for "a colleague is asking
// to connect to your PC": miss it and the request expires unanswered 30
// minutes later, with the mentor never knowing they were asked.
//
// So this kind gets a dialog that stays on screen until the mentor does
// something with it, and its action opens the request on the devices page
// where they accept or decline. The decision deliberately happens there, in
// their authenticated session, rather than in this dialog — letting someone
// into your home PC should be recorded against a person who signed in, not
// against a process running on the machine.
const connectionRequestKind = "connection_request"

// prompter shows a dialog that persists until answered, returning true when
// the mentor chose to open the request. An interface so tests can assert the
// routing without a window, mirroring `notifier` above it.
type prompter interface {
	Prompt(title, body string) bool
}

// wizardPrompter draws the prompt with the same TaskDialog the installer
// wizard uses. Requires the ComCtl32 v6 manifest — see agent.manifest and
// manifest_test.go; without it Windows silently falls back to v5, which has
// no TaskDialogIndirect, and the mentor is never asked at all.
type wizardPrompter struct{}

func (wizardPrompter) Prompt(title, body string) bool {
	w := installerui.Wizard{Title: "ReadyFleet"}
	return w.ShowWelcome(
		title,
		body,
		[]string{
			"You choose whether to allow it — nothing connects until you do",
			"Opens your devices page, where you can accept or decline",
			"If you ignore this, the request expires on its own",
		},
		"Questions or concerns? Contact readyapp@player-ready.co.uk.",
	)
}

// showConnectionRequest puts the prompt up without blocking the command
// handler. The dialog is modal and waits on a human, which can be minutes;
// holding the handler that long would stall the agent's command loop and
// outlive the command's own TTL. The result reported upstream therefore means
// "the mentor was asked", not "the mentor answered" — answering happens on the
// web page, and the request row is what records it.
func showConnectionRequest(p prompter, title, body, url string) {
	go func() {
		if !p.Prompt(title, body) {
			return
		}
		if url == "" {
			return
		}
		if err := trayicon.OpenURL(url); err != nil {
			slog.Default().Warn("connection request prompt: open url", "err", err)
		}
	}()
}
