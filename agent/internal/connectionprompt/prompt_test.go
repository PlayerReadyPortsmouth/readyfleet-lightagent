package connectionprompt

import (
	"context"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
	"github.com/playerreadyportsmouth/readyfleet/proto"
	"testing"
	"time"
)

type answer consent.Outcome

func (a answer) Ask(context.Context, consent.Request) consent.Outcome { return consent.Outcome(a) }
func TestEveryCoreConnectionPromptsIncludingOwner(t *testing.T) {
	calls := 0
	open := func(string) error { calls++; return nil }
	args := proto.ShowNotificationArgs{URL: "https://readyapp.player-ready.co.uk/devices", RequesterID: "owner"}
	for _, out := range []consent.Outcome{consent.OutcomeAccepted, consent.OutcomeDeclined, consent.OutcomeTimedOut} {
		gate := consent.Gate{Asker: answer(out)} // no owner exemption for Core Team
		if Show(context.Background(), gate, args, open) != (out == consent.OutcomeAccepted) {
			t.Fatal("wrong prompt decision")
		}
	}
	if calls != 1 {
		t.Fatal("non-acceptance opened approval page")
	}
	if Show(context.Background(), consent.Gate{}, args, open) {
		t.Fatal("missing prompter admitted")
	}
}

type blockingAsker struct{ release chan struct{} }

func (b blockingAsker) Ask(context.Context, consent.Request) consent.Outcome {
	<-b.release
	return consent.OutcomeAccepted
}

func TestUnansweredPromptTimesOutAsDecline(t *testing.T) {
	ask := blockingAsker{release: make(chan struct{})}
	opened := make(chan string, 1)
	open := func(u string) error { opened <- u; return nil }
	args := proto.ShowNotificationArgs{URL: "https://readyapp.player-ready.co.uk/devices"}
	if ShowWithin(context.Background(), 20*time.Millisecond, consent.Gate{Asker: ask}, args, open) {
		t.Fatal("unanswered prompt counted as consent")
	}
	close(ask.release) // the person clicks Accept after the deadline
	select {
	case <-opened:
		t.Fatal("late accept opened the approval page")
	case <-time.After(100 * time.Millisecond):
	}
}
