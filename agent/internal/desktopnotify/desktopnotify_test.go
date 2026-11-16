package desktopnotify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
)

// fakeSession is a scripted Session: it records what was posted and lets a
// test push the signals a notification daemon would have sent. This is the
// seam that makes every fail-closed path testable on any platform, with no
// bus, no daemon and no desktop.
type fakeSession struct {
	mu sync.Mutex

	notifyID  uint32
	notifyErr error
	posted    []Notification

	events chan Event

	closed     []uint32
	shutdowns  int
	closeCalls int
}

func newFakeSession() *fakeSession {
	return &fakeSession{notifyID: 7, events: make(chan Event, 4)}
}

func (f *fakeSession) Notify(_ context.Context, n Notification) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notifyErr != nil {
		return 0, f.notifyErr
	}
	f.posted = append(f.posted, n)
	return f.notifyID, nil
}

func (f *fakeSession) Close(_ context.Context, id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeSession) Events() <-chan Event { return f.events }

func (f *fakeSession) Shutdown() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdowns++
	return nil
}

func (f *fakeSession) lastPosted(t *testing.T) Notification {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posted) == 0 {
		t.Fatal("nothing was posted to the notification daemon")
	}
	return f.posted[len(f.posted)-1]
}

func (f *fakeSession) withdrawn() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.closed...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPrompter(sess Session) Prompter {
	return Prompter{
		Dial:    func() (Session, error) { return sess, nil },
		Logger:  quietLogger(),
		Timeout: 2 * time.Second,
	}
}

var testRequest = consent.Request{
	Title:       "Aurora Nicholas wants to connect to your PC",
	Body:        "Cheerful-Otter-12 — open your SolarBeam devices to approve or decline.",
	RequesterID: "requester-oid",
}

// TestAsk_AcceptActionAllows is the only path in this package that may
// return an allowed outcome.
func TestAsk_AcceptActionAllows(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: ActionAccept}

	got := testPrompter(sess).Ask(context.Background(), testRequest)

	if got != consent.OutcomeAccepted {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeAccepted)
	}
	if !got.Allowed() {
		t.Error("Accept must allow the connection")
	}
	if w := sess.withdrawn(); len(w) != 1 || w[0] != 7 {
		t.Errorf("withdrawn = %v, want the answered notification (7) taken off screen", w)
	}
}

func TestAsk_DeclineActionDeclines(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: ActionDecline}

	got := testPrompter(sess).Ask(context.Background(), testRequest)

	if got != consent.OutcomeDeclined {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeDeclined)
	}
	if got.Allowed() {
		t.Error("Decline must not allow the connection")
	}
}

// TestAsk_UnknownActionDeclines: an action key this build never registered
// is not a yes. Reading anything other than the Accept key as consent is
// exactly the class of bug this package exists to remove.
func TestAsk_UnknownActionDeclines(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: "default"}

	if got := testPrompter(sess).Ask(context.Background(), testRequest); got != consent.OutcomeDeclined {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeDeclined)
	}
}

func TestAsk_DismissedDeclines(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventClosed, ID: 7, Reason: closeReasonDismissed}

	got := testPrompter(sess).Ask(context.Background(), testRequest)

	if got != consent.OutcomeDismissed {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeDismissed)
	}
	if got.Allowed() {
		t.Error("a dismissed prompt must not allow the connection")
	}
}

// TestAsk_DaemonExpiryDeclines separates "the desktop expired it" from
// "the agent's own window ran out": both decline, and the log should be
// able to say which.
func TestAsk_DaemonExpiryDeclines(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventClosed, ID: 7, Reason: closeReasonExpired}

	if got := testPrompter(sess).Ask(context.Background(), testRequest); got != consent.OutcomeTimedOut {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeTimedOut)
	}
}

func TestAsk_TimeoutDeclines(t *testing.T) {
	sess := newFakeSession() // no events at all: nobody touches the prompt
	p := testPrompter(sess)
	p.Timeout = 30 * time.Millisecond

	start := time.Now()
	got := p.Ask(context.Background(), testRequest)

	if got != consent.OutcomeTimedOut {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeTimedOut)
	}
	if got.Allowed() {
		t.Error("an unanswered prompt must not allow the connection")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Ask took %s; the timeout should have ended it", elapsed)
	}
	if w := sess.withdrawn(); len(w) != 1 || w[0] != 7 {
		t.Errorf("withdrawn = %v, want the abandoned prompt taken off screen so nobody answers a dead question", w)
	}
}

func TestAsk_NoSessionBusDeclines(t *testing.T) {
	p := Prompter{
		Dial:   func() (Session, error) { return nil, ErrNoSessionBus },
		Logger: quietLogger(),
	}

	got := p.Ask(context.Background(), testRequest)

	if got != consent.OutcomeNoSessionBus {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeNoSessionBus)
	}
	if got.Allowed() {
		t.Error("a headless machine must DECLINE, not consent silently")
	}
}

func TestAsk_NoNotificationDaemonDeclines(t *testing.T) {
	p := Prompter{
		Dial:   func() (Session, error) { return nil, ErrNoNotificationDaemon },
		Logger: quietLogger(),
	}

	if got := p.Ask(context.Background(), testRequest); got != consent.OutcomeNoNotificationDaemon {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeNoNotificationDaemon)
	}
}

func TestAsk_UnsupportedPlatformDeclines(t *testing.T) {
	p := Prompter{
		Dial:   func() (Session, error) { return nil, ErrUnsupported },
		Logger: quietLogger(),
	}

	if got := p.Ask(context.Background(), testRequest); got != consent.OutcomeNoPrompter {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeNoPrompter)
	}
}

// TestAsk_DaemonRefusesNotifyDeclines covers a bus that exists and a daemon
// that answered NameHasOwner but then failed the Notify call.
func TestAsk_DaemonRefusesNotifyDeclines(t *testing.T) {
	sess := newFakeSession()
	sess.notifyErr = errors.New("org.freedesktop.DBus.Error.ServiceUnknown")

	if got := testPrompter(sess).Ask(context.Background(), testRequest); got != consent.OutcomeNoNotificationDaemon {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeNoNotificationDaemon)
	}
}

// TestAsk_BusDropsMidPromptDeclines: the connection going away while the
// question is on screen means nobody can answer it any more.
func TestAsk_BusDropsMidPromptDeclines(t *testing.T) {
	sess := newFakeSession()
	close(sess.events)

	if got := testPrompter(sess).Ask(context.Background(), testRequest); got != consent.OutcomeNoNotificationDaemon {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeNoNotificationDaemon)
	}
}

func TestAsk_ContextCancelDeclines(t *testing.T) {
	sess := newFakeSession()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := testPrompter(sess).Ask(ctx, testRequest)

	if got != consent.OutcomeCancelled {
		t.Fatalf("outcome = %q, want %q", got, consent.OutcomeCancelled)
	}
	if w := sess.withdrawn(); len(w) != 1 {
		t.Errorf("withdrawn = %v, want the prompt withdrawn even though ctx was already cancelled", w)
	}
}

// TestAsk_IgnoresOtherNotifications: signals about somebody else's
// notification must not be read as an answer to ours.
func TestAsk_IgnoresOtherNotifications(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 99, ActionKey: ActionAccept}
	sess.events <- Event{Kind: EventClosed, ID: 42, Reason: closeReasonDismissed}
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: ActionDecline}

	if got := testPrompter(sess).Ask(context.Background(), testRequest); got != consent.OutcomeDeclined {
		t.Fatalf("outcome = %q, want %q — signals for other notification ids must be ignored", got, consent.OutcomeDeclined)
	}
}

// TestAsk_PostsWhoIsAskingWithTwoCriticalActions pins the shape of the
// question itself: the person must be able to see who is asking, and must
// have both answers available.
func TestAsk_PostsWhoIsAskingWithTwoCriticalActions(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: ActionAccept}

	testPrompter(sess).Ask(context.Background(), testRequest)

	n := sess.lastPosted(t)
	if n.Summary != testRequest.Title {
		t.Errorf("summary = %q, want the request title naming who is asking (%q)", n.Summary, testRequest.Title)
	}
	if n.Body != testRequest.Body {
		t.Errorf("body = %q, want %q", n.Body, testRequest.Body)
	}
	if n.Urgency != UrgencyCritical {
		t.Errorf("urgency = %d, want critical (%d)", n.Urgency, UrgencyCritical)
	}
	if n.ExpireMillis != expireNever {
		t.Errorf("expire = %d, want %d — the agent's own timeout is the authority on how long the question stands", n.ExpireMillis, expireNever)
	}
	want := []Action{{Key: ActionAccept, Label: AcceptLabel}, {Key: ActionDecline, Label: DeclineLabel}}
	if len(n.Actions) != 2 || n.Actions[0] != want[0] || n.Actions[1] != want[1] {
		t.Errorf("actions = %+v, want %+v", n.Actions, want)
	}
}

func TestAsk_AlwaysHangsUp(t *testing.T) {
	sess := newFakeSession()
	sess.events <- Event{Kind: EventAction, ID: 7, ActionKey: ActionAccept}

	testPrompter(sess).Ask(context.Background(), testRequest)

	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.shutdowns != 1 {
		t.Errorf("Shutdown called %d times, want 1 — the agent runs for weeks, a connection per prompt is a leak per prompt", sess.shutdowns)
	}
}

// TestShow_PostsAOneWayNoticeWithNoActions is the show_notification path:
// a real notification on Linux where the build used to do nothing at all
// and report success.
func TestShow_PostsAOneWayNoticeWithNoActions(t *testing.T) {
	sess := newFakeSession()
	n := Notifier{Dial: func() (Session, error) { return sess, nil }, Logger: quietLogger()}

	if err := n.Show(context.Background(), "ReadyFleet updated", "Now on 0.8.2", UrgencyNormal); err != nil {
		t.Fatalf("Show: %v", err)
	}

	posted := sess.lastPosted(t)
	if posted.Summary != "ReadyFleet updated" || posted.Body != "Now on 0.8.2" {
		t.Errorf("posted %+v, want the title and body through verbatim", posted)
	}
	if len(posted.Actions) != 0 {
		t.Errorf("actions = %+v, want none — a notice is not a question", posted.Actions)
	}
	if posted.ExpireMillis != expireDaemonDefault {
		t.Errorf("expire = %d, want the daemon's default (%d)", posted.ExpireMillis, expireDaemonDefault)
	}
	if posted.AppName != AppName {
		t.Errorf("app name = %q, want %q", posted.AppName, AppName)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.shutdowns != 1 {
		t.Errorf("Shutdown called %d times, want 1", sess.shutdowns)
	}
}

// TestShow_ReportsFailureInsteadOfClaimingSuccess: the old Linux no-op
// reported exit 0 for a notification nobody could have seen. A notice that
// did not reach a screen must surface as an error the handler can report.
func TestShow_ReportsFailureInsteadOfClaimingSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		dial Dialer
	}{
		{"no session bus", func() (Session, error) { return nil, ErrNoSessionBus }},
		{"no notification daemon", func() (Session, error) { return nil, ErrNoNotificationDaemon }},
		{"unsupported platform", func() (Session, error) { return nil, ErrUnsupported }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := Notifier{Dial: tc.dial, Logger: quietLogger()}
			if err := n.Show(context.Background(), "t", "b", UrgencyNormal); err == nil {
				t.Fatal("Show returned nil; a notice that never reached a screen must not report success")
			}
		})
	}
}

func TestShow_DaemonRefusalIsAnError(t *testing.T) {
	sess := newFakeSession()
	sess.notifyErr = errors.New("daemon said no")
	n := Notifier{Dial: func() (Session, error) { return sess, nil }, Logger: quietLogger()}

	if err := n.Show(context.Background(), "t", "b", UrgencyNormal); err == nil {
		t.Fatal("Show returned nil despite the daemon refusing the notification")
	}
}

func TestUrgencyFromSeverity(t *testing.T) {
	tests := map[string]Urgency{
		"":            UrgencyNormal,
		"info":        UrgencyNormal,
		"warning":     UrgencyNormal,
		"error":       UrgencyCritical,
		"not-a-thing": UrgencyNormal,
	}
	for severity, want := range tests {
		if got := UrgencyFromSeverity(severity); got != want {
			t.Errorf("UrgencyFromSeverity(%q) = %d, want %d", severity, got, want)
		}
	}
}

// TestPrompter_ImplementsAsker keeps the seam honest: the decision layer
// takes a consent.Asker, and this is what plugs into it on Linux.
func TestPrompter_ImplementsAsker(t *testing.T) {
	var _ consent.Asker = Prompter{}
}
