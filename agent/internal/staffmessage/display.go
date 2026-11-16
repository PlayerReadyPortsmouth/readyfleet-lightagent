package staffmessage

import (
	"sync"
	"time"
)

// BannerDuration is how long the companion keeps one message on screen
// unless it is dismissed first.
const BannerDuration = 15 * time.Second

// Item is a message waiting for, or on, the companion's screen. Ack is
// called exactly once with the companion's answer.
type Item struct {
	Message Message
	Ack     func(Ack)
}

// Queue is the companion's display order: one message on screen at a time,
// the rest waiting in arrival order. It holds no UI; the window drives it.
// Safe for concurrent use.
type Queue struct {
	Duration time.Duration

	mu      sync.Mutex
	waiting []Item
	current *Item
	shownAt time.Time
}

func (q *Queue) duration() time.Duration {
	if q.Duration > 0 {
		return q.Duration
	}
	return BannerDuration
}

// Room reports whether the companion should collect another message. The
// broker's MaxInFlight is the same bound, so a full queue simply stops
// polling rather than accepting something it will time out on.
func (q *Queue) Room() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.waiting)
	if q.current != nil {
		n++
	}
	return n < MaxInFlight
}

// Push adds a message behind any already waiting. False when full; the
// caller must then ack it as not shown.
func (q *Queue) Push(it Item) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.waiting)
	if q.current != nil {
		n++
	}
	if n >= MaxInFlight {
		return false
	}
	q.waiting = append(q.waiting, it)
	return true
}

// Expire ends the current message once it has been up for Duration.
// Reports whether it did.
func (q *Queue) Expire(now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current != nil && now.Sub(q.shownAt) >= q.duration() {
		q.current = nil
		return true
	}
	return false
}

// Dismiss ends the current message now (the person pressed Dismiss).
func (q *Queue) Dismiss() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.current = nil
}

// Promote makes the next waiting message current when nothing is on screen,
// returning it. The caller shows it (or acks it as not shown and calls
// Dismiss, then Promote again).
func (q *Queue) Promote(now time.Time) (Item, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current != nil || len(q.waiting) == 0 {
		return Item{}, false
	}
	it := q.waiting[0]
	q.waiting = q.waiting[1:]
	q.current = &it
	q.shownAt = now
	return it, true
}

// Current is the message on screen, how many wait behind it, and how long
// it has left.
func (q *Queue) Current(now time.Time) (Item, int, time.Duration, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current == nil {
		return Item{}, len(q.waiting), 0, false
	}
	return *q.current, len(q.waiting), max(0, q.duration()-now.Sub(q.shownAt)), true
}

// Drain removes everything (companion shutting down) and returns what was
// waiting, so the caller can ack those as not shown.
func (q *Queue) Drain() []Item {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.waiting
	q.waiting = nil
	q.current = nil
	return out
}

// ViaFor names the surfaces that showed a message, or "" when it does not
// count as displayed: the banner is required, the tray balloon is extra (see
// Ack.Via).
func ViaFor(banner, tray bool) string {
	switch {
	case banner && tray:
		return "banner+tray"
	case banner:
		return "banner"
	}
	return ""
}
