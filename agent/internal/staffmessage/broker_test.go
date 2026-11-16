package staffmessage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func fastBroker() *Broker {
	b := NewBroker()
	b.PollWait = 2 * time.Second
	b.PresenceGrace = 200 * time.Millisecond
	b.ClaimTimeout = 300 * time.Millisecond
	b.AckTimeout = 300 * time.Millisecond
	return b
}

var sample = Message{Kind: "staff_message", Title: "Hello", Body: "Time to pack up", Severity: SeverityInfo}

// startPoller runs one companion long-poll and returns what it collected.
func startPoller(ctx context.Context, b *Broker) <-chan Message {
	out := make(chan Message, 1)
	go func() {
		if m, ok := b.Next(ctx); ok {
			out <- m
		}
		close(out)
	}()
	return out
}

func waitPresent(t *testing.T, b *Broker) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !b.Present() {
		if time.Now().After(deadline) {
			t.Fatal("poller never became present")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeliverWithoutCompanionFailsImmediately(t *testing.T) {
	b := fastBroker()
	start := time.Now()
	_, err := b.Deliver(context.Background(), sample)
	if !errors.Is(err, ErrNoCompanion) {
		t.Fatalf("err = %v, want ErrNoCompanion", err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("no-companion failure waited instead of failing fast")
	}
}

func TestDeliverDisplayedAck(t *testing.T) {
	b := fastBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := startPoller(ctx, b)
	waitPresent(t, b)

	type res struct {
		ack Ack
		err error
	}
	done := make(chan res, 1)
	go func() { a, err := b.Deliver(ctx, sample); done <- res{a, err} }()

	m := <-got
	if m.Title != sample.Title || !ValidID(m.ID) {
		t.Fatalf("collected %+v", m)
	}
	if !b.Acknowledge(Ack{ID: m.ID, Displayed: true, Via: "banner+tray"}) {
		t.Fatal("ack for an in-flight message was refused")
	}
	r := <-done
	if r.err != nil || !r.ack.Displayed || r.ack.Via != "banner+tray" {
		t.Fatalf("deliver = %+v, %v", r.ack, r.err)
	}
	if b.Acknowledge(Ack{ID: m.ID, Displayed: true, Via: "banner"}) {
		t.Fatal("a second ack for the same message was accepted")
	}
}

func TestDeliverNotCollectedIsNotClaimed(t *testing.T) {
	b := fastBroker()
	// A poll that just ended keeps the companion "present" for the grace
	// period, but nobody collects.
	ctx, cancel := context.WithCancel(context.Background())
	got := startPoller(ctx, b)
	waitPresent(t, b)
	cancel()
	<-got
	if !b.Present() {
		t.Fatal("companion should still be within its presence grace")
	}
	_, err := b.Deliver(context.Background(), sample)
	if !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("err = %v, want ErrNotClaimed", err)
	}
	// Withdrawn: a later poll must not receive the stale message.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if m, ok := b.Next(ctx2); ok {
		t.Fatalf("stale message %+v delivered after timeout", m)
	}
	time.Sleep(250 * time.Millisecond)
	if _, err := b.Deliver(context.Background(), sample); !errors.Is(err, ErrNoCompanion) {
		t.Fatalf("after grace err = %v, want ErrNoCompanion", err)
	}
}

func TestDeliverCollectedWithoutAckTimesOut(t *testing.T) {
	b := fastBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := startPoller(ctx, b)
	waitPresent(t, b)
	errc := make(chan error, 1)
	go func() { _, err := b.Deliver(ctx, sample); errc <- err }()
	m := <-got
	if err := <-errc; !errors.Is(err, ErrNoAck) {
		t.Fatalf("err = %v, want ErrNoAck", err)
	}
	if b.Acknowledge(Ack{ID: m.ID, Displayed: true, Via: "banner"}) {
		t.Fatal("a late ack after the timeout was accepted")
	}
}

func TestDeliverRelaysUndisplayedAck(t *testing.T) {
	b := fastBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := startPoller(ctx, b)
	waitPresent(t, b)
	type res struct {
		ack Ack
		err error
	}
	done := make(chan res, 1)
	go func() { a, err := b.Deliver(ctx, sample); done <- res{a, err} }()
	m := <-got
	b.Acknowledge(Ack{ID: m.ID, Reason: ReasonLocked})
	r := <-done
	if r.err != nil || r.ack.Displayed || r.ack.Reason != ReasonLocked {
		t.Fatalf("deliver = %+v, %v", r.ack, r.err)
	}
}

func TestUnknownAckRefused(t *testing.T) {
	b := fastBroker()
	if b.Acknowledge(Ack{ID: newID(), Displayed: true, Via: "banner"}) {
		t.Fatal("ack for a message never issued was accepted")
	}
}

func TestBusyAtMaxInFlight(t *testing.T) {
	b := fastBroker()
	b.ClaimTimeout = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Keep the companion present but never collecting.
	b.mu.Lock()
	b.waiters = 1
	b.mu.Unlock()
	var wg sync.WaitGroup
	for i := 0; i < MaxInFlight; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = b.Deliver(ctx, sample) }()
	}
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		n := len(b.pending)
		b.mu.Unlock()
		if n == MaxInFlight {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d pending", n)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := b.Deliver(ctx, sample); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	cancel()
	wg.Wait()
}

func TestNextReturnsQueuedInOrder(t *testing.T) {
	b := fastBroker()
	b.AckTimeout = time.Second
	b.mu.Lock()
	b.waiters = 1 // present
	b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := sample, sample
	first.Title, second.Title = "first", "second"
	go func() { _, _ = b.Deliver(ctx, first) }()
	time.Sleep(20 * time.Millisecond)
	go func() { _, _ = b.Deliver(ctx, second) }()
	time.Sleep(20 * time.Millisecond)
	a, ok := b.Next(ctx)
	c, ok2 := b.Next(ctx)
	if !ok || !ok2 || a.Title != "first" || c.Title != "second" {
		t.Fatalf("order = %q, %q", a.Title, c.Title)
	}
}
