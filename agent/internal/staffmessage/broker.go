package staffmessage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Broker timings. A companion re-polls as soon as a long-poll ends, so
// PresenceGrace only has to bridge the gap between two polls.
const (
	DefaultPollWait      = 20 * time.Second
	DefaultPresenceGrace = 5 * time.Second
	DefaultClaimTimeout  = 10 * time.Second
	// The companion shows messages one at a time for up to BannerDuration
	// each and acks when a message reaches the screen, so a message behind
	// MaxInFlight-1 others can legitimately wait that long.
	DefaultAckTimeout = 90 * time.Second
	MaxInFlight       = 5
)

// Delivery errors. Run maps them to report codes.
var (
	ErrNoCompanion = errors.New("no signed-in user session with the venue companion running")
	ErrNotClaimed  = errors.New("the venue companion did not collect the message")
	ErrNoAck       = errors.New("the venue companion did not confirm the message was shown")
	ErrBusy        = errors.New("too many staff messages are already waiting on this PC")
)

type delivery struct {
	msg     Message
	claimed chan struct{} // closed when a companion collects msg
	done    chan Ack      // buffered(1): the companion's answer
}

// Broker hands messages from command handlers to the companion's long-poll
// and routes its ack back. Safe for concurrent use; the zero value is not
// usable, call NewBroker.
type Broker struct {
	PollWait      time.Duration
	PresenceGrace time.Duration
	ClaimTimeout  time.Duration
	AckTimeout    time.Duration
	Now           func() time.Time

	mu       sync.Mutex
	pending  []*delivery
	inFlight map[string]*delivery // collected, awaiting ack
	waiters  int
	lastSeen time.Time
	wake     chan struct{} // closed and replaced when pending grows
}

// NewBroker returns a Broker with the default timings.
func NewBroker() *Broker {
	return &Broker{
		PollWait: DefaultPollWait, PresenceGrace: DefaultPresenceGrace,
		ClaimTimeout: DefaultClaimTimeout, AckTimeout: DefaultAckTimeout,
		Now: time.Now, inFlight: map[string]*delivery{}, wake: make(chan struct{}),
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("staffmessage: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// presentLocked: a companion is long-polling now, or finished a poll within
// PresenceGrace (it is between two polls).
func (b *Broker) presentLocked() bool {
	if b.waiters > 0 {
		return true
	}
	return !b.lastSeen.IsZero() && b.Now().Sub(b.lastSeen) <= b.PresenceGrace
}

// Present reports whether a companion is currently collecting messages.
func (b *Broker) Present() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.presentLocked()
}

// Deliver queues msg for the companion and waits until it is acknowledged
// or a timeout decides the outcome. A returned Ack is the companion's own
// answer (Displayed may be false); an error means no answer was obtained.
func (b *Broker) Deliver(ctx context.Context, msg Message) (Ack, error) {
	b.mu.Lock()
	if !b.presentLocked() {
		b.mu.Unlock()
		return Ack{}, ErrNoCompanion
	}
	if len(b.pending)+len(b.inFlight) >= MaxInFlight {
		b.mu.Unlock()
		return Ack{}, ErrBusy
	}
	msg.ID = newID()
	d := &delivery{msg: msg, claimed: make(chan struct{}), done: make(chan Ack, 1)}
	b.pending = append(b.pending, d)
	close(b.wake)
	b.wake = make(chan struct{})
	b.mu.Unlock()

	claim := time.NewTimer(b.ClaimTimeout)
	defer claim.Stop()
	select {
	case <-d.claimed:
	case <-claim.C:
		if b.withdraw(d) {
			return Ack{}, ErrNotClaimed
		}
		// Collected in the same instant the timer fired: carry on waiting.
	case <-ctx.Done():
		if b.withdraw(d) {
			return Ack{}, ctx.Err()
		}
	}

	ack := time.NewTimer(b.AckTimeout)
	defer ack.Stop()
	select {
	case a := <-d.done:
		return a, nil
	case <-ack.C:
	case <-ctx.Done():
	}
	b.mu.Lock()
	delete(b.inFlight, d.msg.ID)
	b.mu.Unlock()
	// An ack racing the timeout still wins.
	select {
	case a := <-d.done:
		return a, nil
	default:
	}
	if ctx.Err() != nil {
		return Ack{}, ctx.Err()
	}
	return Ack{}, ErrNoAck
}

// withdraw removes an uncollected delivery; false if it was already collected.
func (b *Broker) withdraw(d *delivery) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, p := range b.pending {
		if p == d {
			b.pending = append(b.pending[:i], b.pending[i+1:]...)
			return true
		}
	}
	return false
}

// Next is the companion's long-poll: it returns the oldest waiting message,
// or ok=false after PollWait (or when ctx ends). While it waits the
// companion counts as present.
func (b *Broker) Next(ctx context.Context) (Message, bool) {
	timer := time.NewTimer(b.PollWait)
	defer timer.Stop()
	b.mu.Lock()
	b.waiters++
	defer func() {
		b.waiters--
		b.lastSeen = b.Now()
		b.mu.Unlock()
	}()
	for {
		if len(b.pending) > 0 {
			d := b.pending[0]
			b.pending = b.pending[1:]
			b.inFlight[d.msg.ID] = d
			close(d.claimed)
			return d.msg, true
		}
		wake := b.wake
		b.mu.Unlock()
		select {
		case <-wake:
			b.mu.Lock()
		case <-timer.C:
			b.mu.Lock()
			return Message{}, false
		case <-ctx.Done():
			b.mu.Lock()
			return Message{}, false
		}
	}
}

// Acknowledge routes the companion's answer to the waiting handler. False
// when the id is unknown, was never collected, or already answered or timed
// out.
func (b *Broker) Acknowledge(a Ack) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	d, ok := b.inFlight[a.ID]
	if !ok {
		return false
	}
	delete(b.inFlight, a.ID)
	d.done <- a
	return true
}
