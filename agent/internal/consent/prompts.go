package consent

import (
	"context"
	"sync"
)

// Prompts tracks the open desktop prompts by ReadyApp request id so the C2
// can close one when the owner decides in ReadyApp. Safe for concurrent use.
type Prompts struct {
	mu   sync.Mutex
	open map[string]*Prompt
}

// Prompt is one open prompt. Ctx is cancelled by Dismiss or End.
type Prompt struct {
	Ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	dismissed bool
}

// Dismissed reports whether the prompt was closed remotely. Check it after
// the prompt returns: a platform dialog that ignores ctx may still hand back
// "Accept" late, and a remote decision must win.
func (p *Prompt) Dismissed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dismissed
}

func (p *Prompt) dismiss() {
	p.mu.Lock()
	p.dismissed = true
	p.mu.Unlock()
	p.cancel()
}

// Begin registers a prompt; call End when it returns. An empty id is not
// registered, so it simply cannot be dismissed.
func (ps *Prompts) Begin(parent context.Context, id string) (*Prompt, func()) {
	ctx, cancel := context.WithCancel(parent)
	p := &Prompt{Ctx: ctx, cancel: cancel}
	if id == "" {
		return p, cancel
	}
	ps.mu.Lock()
	if ps.open == nil {
		ps.open = map[string]*Prompt{}
	}
	if old := ps.open[id]; old != nil { // a re-sent request replaces the old prompt
		old.dismiss()
	}
	ps.open[id] = p
	ps.mu.Unlock()
	return p, func() {
		ps.mu.Lock()
		if ps.open[id] == p {
			delete(ps.open, id)
		}
		ps.mu.Unlock()
		cancel()
	}
}

// Dismiss closes the open prompt for id and reports whether one was open.
func (ps *Prompts) Dismiss(id string) bool {
	ps.mu.Lock()
	p := ps.open[id]
	delete(ps.open, id)
	ps.mu.Unlock()
	if p == nil {
		return false
	}
	p.dismiss()
	return true
}
