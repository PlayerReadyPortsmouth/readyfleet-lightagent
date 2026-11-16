package companionadapter

import (
	"context"
	"errors"
	"sync"

	ui "github.com/playerreadyportsmouth/readyfleet/agent/internal/companionui"
)

// StartPanel isolates the native panel lifetime from Fleet and its tray. A dead
// panel never becomes an inert click or an unprotected browser fallback. Notify
// receives no error details; logFailure may record diagnostics in the local log.
func StartPanel(ctx context.Context, run func(context.Context, <-chan ui.OpenRequest) error, logFailure func(error), notify func()) func() {
	requests := make(chan ui.OpenRequest, 4)
	var mu sync.Mutex
	failed := false
	report := func() {
		if notify != nil && ctx.Err() == nil {
			notify()
		}
	}
	go func() {
		err := run(ctx, requests)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("native panel stopped unexpectedly")
		}
		mu.Lock()
		failed = true
		mu.Unlock()
		if logFailure != nil {
			logFailure(err)
		}
		report()
	}()
	return func() {
		if ctx.Err() != nil {
			return
		}
		mu.Lock()
		unavailable := failed
		mu.Unlock()
		if unavailable {
			go report()
			return
		}
		select {
		case requests <- ui.OpenPanel:
		default:
		}
	}
}
