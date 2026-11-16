package companionadapter

import (
	"context"
	"errors"
	"testing"
	"time"

	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
)

func TestPanelFailureIsLoggedAndEveryLaterClickHasGenericFeedback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logged := make(chan error, 1)
	notifications := make(chan struct{}, 4)
	privateError := errors.New("private diagnostic")
	open := StartPanel(ctx, func(context.Context, <-chan ui.OpenRequest) error { return privateError }, func(err error) { logged <- err }, func() { notifications <- struct{}{} })
	select {
	case err := <-logged:
		if err != privateError {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("failure not logged")
	}
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("initial failure hidden")
	}
	open()
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("failed panel leaves inert click")
	}
	select {
	case <-logged:
		t.Fatal("click logged raw diagnostics again")
	default:
	}
}
func TestPanelCancellationIsQuietAndOpenReachesLivePanel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	received := make(chan ui.OpenRequest, 1)
	stopped := make(chan struct{})
	open := StartPanel(ctx, func(ctx context.Context, requests <-chan ui.OpenRequest) error {
		defer close(stopped)
		select {
		case request := <-requests:
			received <- request
		case <-ctx.Done():
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	}, func(error) { t.Error("normal shutdown logged as failure") }, func() { t.Error("normal shutdown notified") })
	open()
	select {
	case request := <-received:
		if request != ui.OpenPanel {
			t.Fatal(request)
		}
	case <-time.After(time.Second):
		t.Fatal("live open lost")
	}
	cancel()
	<-stopped
	open()
}
