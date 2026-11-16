package companionui

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Executor gives buttons and shortcuts one path with fresh identity/revision
// checks and no duplicate local dispatch. The backend remains the final atomic
// authority check and must honour idempotency across process restarts.
type Executor struct {
	backend Backend
	mu      sync.Mutex
	seen    map[string]struct{}
	order   []string
}

func NewExecutor(backend Backend) *Executor {
	return &Executor{backend: backend, seen: map[string]struct{}{}}
}
func (e *Executor) Execute(ctx context.Context, review Review) (ActionResult, error) {
	if !e.mu.TryLock() {
		return ActionResult{}, errors.New("A request is already running.")
	}
	defer e.mu.Unlock()
	if e.backend == nil || review.Action.RequestID == "" {
		return ActionResult{}, errors.New("Invalid action request")
	}
	if _, seen := e.seen[review.Action.RequestID]; seen {
		return ActionResult{}, errors.New("This request has already been submitted. Review again before retrying.")
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	s, err := e.backend.Snapshot(c)
	cancel()
	if err != nil {
		return ActionResult{}, err
	}
	if !review.Matches(s, time.Now()) {
		return ActionResult{}, errors.New("Status changed. Review the current session before trying again.")
	}
	if err := ctx.Err(); err != nil {
		return ActionResult{}, err
	}
	e.seen[review.Action.RequestID] = struct{}{}
	e.order = append(e.order, review.Action.RequestID)
	if len(e.order) > 128 {
		delete(e.seen, e.order[0])
		e.order = e.order[1:]
	}
	c, cancel = context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e.backend.Action(c, review.Action)
}
