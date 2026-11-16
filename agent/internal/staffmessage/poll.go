package staffmessage

import (
	"context"
	"sync"
	"time"
)

// Client is the companion's view of the notify pipe.
type Client interface {
	// Next long-polls; ok=false with a nil error means nothing arrived.
	Next(ctx context.Context) (msg Message, ok bool, err error)
	Ack(ctx context.Context, a Ack) error
}

// PollOptions tune Poll; zero values take the defaults.
type PollOptions struct {
	ConnectionRequest func(context.Context, Message) bool
	// RetryDelay follows a pipe error (service stopped, pipe absent, this
	// account not authorised) before polling again.
	RetryDelay time.Duration
	// FullDelay is the wait between Room checks while the queue is full.
	FullDelay time.Duration
	// AckTimeout bounds one ack call.
	AckTimeout time.Duration
	Logf       func(format string, args ...any)
}

// Poll runs in the companion for its lifetime: collect a message, revalidate
// it, queue it for the banner and call wake so the window picks it up. Every
// collected message is acked exactly once — by the window when shown or not,
// or here when it is invalid, the queue is full, or the companion exits.
func Poll(ctx context.Context, client Client, q *Queue, wake func(), opts PollOptions) {
	if opts.RetryDelay <= 0 {
		opts.RetryDelay = 5 * time.Second
	}
	if opts.FullDelay <= 0 {
		opts.FullDelay = 500 * time.Millisecond
	}
	if opts.AckTimeout <= 0 {
		opts.AckTimeout = 5 * time.Second
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	send := func(a Ack) {
		// Acks outlive a cancelled companion context on purpose: a shutdown
		// ack is what lets the service report promptly instead of timing out.
		c, cancel := context.WithTimeout(context.Background(), opts.AckTimeout)
		defer cancel()
		if err := client.Ack(c, a); err != nil {
			logf("staff message ack: %v", err)
		}
	}
	ackOnce := func(id string) func(Ack) {
		var once sync.Once
		return func(a Ack) {
			once.Do(func() {
				a.ID = id
				send(a)
			})
		}
	}
	defer func() {
		for _, it := range q.Drain() {
			it.Ack(Ack{Reason: ReasonDisplayFailed})
		}
	}()
	sleep := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	lastErr := ""
	for ctx.Err() == nil {
		if !q.Room() {
			if !sleep(opts.FullDelay) {
				return
			}
			continue
		}
		raw, ok, err := client.Next(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// An older service, a stopped service or another Windows account
			// fails identically every few seconds: log changes, not repeats.
			if msg := err.Error(); msg != lastErr {
				logf("staff message poll: %v", err)
				lastErr = msg
			}
			if !sleep(opts.RetryDelay) {
				return
			}
			continue
		}
		if lastErr != "" {
			logf("staff message poll: service reachable again")
			lastErr = ""
		}
		if !ok {
			continue
		}
		msg, f := Revalidate(raw)
		if f != nil {
			logf("staff message rejected: %v", f)
			if ValidID(raw.ID) {
				send(Ack{ID: raw.ID, Reason: ReasonInvalid})
			}
			continue
		}
		if msg.Kind == ConnectionRequestKind {
			accepted := opts.ConnectionRequest != nil && opts.ConnectionRequest(ctx, msg)
			reason := ReasonDisplayFailed
			if accepted {
				reason = ""
			}
			via := ""
			if accepted {
				via = "banner"
			}
			send(Ack{ID: msg.ID, Displayed: accepted, Reason: reason, Via: via})
			continue
		}
		it := Item{Message: msg, Ack: ackOnce(msg.ID)}
		if !q.Push(it) {
			it.Ack(Ack{Reason: ReasonDisplayFailed})
			continue
		}
		if wake != nil {
			wake()
		}
	}
}
