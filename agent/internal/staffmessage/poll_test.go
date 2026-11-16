package staffmessage

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClient struct {
	mu   sync.Mutex
	msgs chan Message
	acks []Ack
}

func (c *fakeClient) Next(ctx context.Context) (Message, bool, error) {
	select {
	case m := <-c.msgs:
		return m, true, nil
	case <-ctx.Done():
		return Message{}, false, ctx.Err()
	case <-time.After(10 * time.Millisecond):
		return Message{}, false, nil
	}
}

func (c *fakeClient) Ack(_ context.Context, a Ack) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.acks = append(c.acks, a)
	return nil
}

func (c *fakeClient) snapshot() []Ack {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Ack(nil), c.acks...)
}

func id(n byte) string { return strings.Repeat(string('a'+n), 32) }

func TestPollQueuesValidAndAcksInvalid(t *testing.T) {
	c := &fakeClient{msgs: make(chan Message, 4)}
	q := &Queue{}
	woke := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Poll(ctx, c, q, func() { woke <- struct{}{} }, PollOptions{RetryDelay: time.Millisecond})
	}()

	c.msgs <- Message{ID: id(0), Kind: "k", Title: "bad", Body: "", Severity: "info"}
	c.msgs <- Message{ID: id(1), Kind: "k", Title: "ok", Body: "fine", Severity: "info"}
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("valid message never queued")
	}
	it, ok := q.Promote(time.Now())
	if !ok || it.Message.ID != id(1) {
		t.Fatalf("queued %+v", it)
	}
	acks := c.snapshot()
	if len(acks) != 1 || acks[0].ID != id(0) || acks[0].Reason != ReasonInvalid {
		t.Fatalf("acks = %+v", acks)
	}
	it.Ack(Ack{Displayed: true, Via: "banner"})
	it.Ack(Ack{Reason: ReasonDisplayFailed}) // second call is ignored
	cancel()
	<-done
	acks = c.snapshot()
	if len(acks) != 2 || acks[1].ID != id(1) || !acks[1].Displayed {
		t.Fatalf("acks = %+v", acks)
	}
}

func TestPollAcksWaitingMessagesOnExit(t *testing.T) {
	c := &fakeClient{msgs: make(chan Message, 2)}
	q := &Queue{}
	woke := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Poll(ctx, c, q, func() { woke <- struct{}{} }, PollOptions{}) }()
	c.msgs <- Message{ID: id(2), Kind: "k", Title: "t", Body: "b"}
	<-woke
	cancel()
	<-done
	acks := c.snapshot()
	if len(acks) != 1 || acks[0].ID != id(2) || acks[0].Displayed || acks[0].Reason != ReasonDisplayFailed {
		t.Fatalf("acks = %+v", acks)
	}
}

func TestQueueOneAtATime(t *testing.T) {
	q := &Queue{Duration: time.Second}
	now := time.Unix(1000, 0)
	for i := 0; i < MaxInFlight; i++ {
		if !q.Push(Item{Message: Message{ID: id(byte(i))}}) {
			t.Fatalf("push %d refused", i)
		}
	}
	if q.Room() || q.Push(Item{}) {
		t.Fatal("queue accepted more than MaxInFlight")
	}
	first, ok := q.Promote(now)
	if !ok || first.Message.ID != id(0) {
		t.Fatal("first not promoted")
	}
	if _, ok := q.Promote(now); ok {
		t.Fatal("two messages on screen at once")
	}
	if _, waiting, left, ok := q.Current(now.Add(400 * time.Millisecond)); !ok || waiting != MaxInFlight-1 || left != 600*time.Millisecond {
		t.Fatalf("current waiting=%d left=%s ok=%v", waiting, left, ok)
	}
	if q.Expire(now.Add(999 * time.Millisecond)) {
		t.Fatal("expired early")
	}
	if !q.Expire(now.Add(time.Second)) {
		t.Fatal("did not expire after Duration")
	}
	second, _ := q.Promote(now)
	q.Dismiss()
	third, _ := q.Promote(now)
	if second.Message.ID != id(1) || third.Message.ID != id(2) {
		t.Fatal("not in arrival order")
	}
	if !q.Room() {
		t.Fatal("no room after messages left")
	}
	if len(q.Drain()) != 2 {
		t.Fatal("drain lost waiting messages")
	}
}

func TestViaNeedsBanner(t *testing.T) {
	if ViaFor(false, true) != "" || ViaFor(true, false) != "banner" || ViaFor(true, true) != "banner+tray" {
		t.Fatal("tray balloon alone counted as displayed")
	}
}

func TestCoreConnectionRequestUsesInteractivePrompt(t *testing.T) {
	c := &fakeClient{msgs: make(chan Message, 1)}
	q := &Queue{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan Message, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		Poll(ctx, c, q, nil, PollOptions{ConnectionRequest: func(_ context.Context, m Message) bool { called <- m; return true }})
	}()
	c.msgs <- Message{ID: id(4), Kind: ConnectionRequestKind, Title: "Connection request", Body: "Support wants to connect", ApprovalURL: "https://readyapp.player-ready.co.uk/devices", RequesterID: "support"}
	select {
	case m := <-called:
		if m.RequesterID != "support" {
			t.Fatal("requester lost")
		}
	case <-time.After(time.Second):
		t.Fatal("question never reached interactive prompt")
	}
	cancel()
	<-done
	acks := c.snapshot()
	if len(acks) != 1 || !acks[0].Displayed || acks[0].Validate() != nil {
		t.Fatalf("prompt acknowledgement: %+v", acks)
	}
	if _, ok := q.Promote(time.Now()); ok {
		t.Fatal("consent question fell into ordinary banner")
	}
}
