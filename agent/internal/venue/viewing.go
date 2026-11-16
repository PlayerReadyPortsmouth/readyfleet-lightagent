package venue

import (
	"sync"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Viewing is the companion's cue to draw the "Staff are viewing this screen"
// bar. A count and a surface, never who is watching.
type Viewing struct {
	Active       bool   `json:"active"`
	WatcherCount int    `json:"watcherCount,omitempty"`
	Kind         string `json:"kind,omitempty"`
}

// ViewingTracker holds the C2's last viewing_started/viewing_stopped. A start
// lapses after proto.ViewingRenewTTL without a renewal, so a lost stop (C2
// restart, dropped socket) cannot leave the bar up for ever.
type ViewingTracker struct {
	mu      sync.Mutex
	current Viewing
	until   time.Time
}

// Handle applies one viewing envelope; other types are ignored.
func (t *ViewingTracker) Handle(env proto.Envelope, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch env.Type {
	case proto.MsgViewingStarted:
		var d proto.ViewingData
		_ = env.DecodeData(&d)
		if d.Kind != proto.ViewingKindSolarBeam {
			d.Kind = proto.ViewingKindScreen
		}
		t.current = Viewing{Active: true, WatcherCount: max(d.WatcherCount, 1), Kind: d.Kind}
		t.until = now.Add(proto.ViewingRenewTTL)
	case proto.MsgViewingStopped:
		t.current = Viewing{}
	}
}

// Current is the state to publish to the companion at now.
func (t *ViewingTracker) Current(now time.Time) Viewing {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current.Active && now.After(t.until) {
		t.current = Viewing{}
	}
	return t.current
}
