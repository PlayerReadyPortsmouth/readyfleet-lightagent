// Package desktopnotify puts notifications on a Linux desktop over
// org.freedesktop.Notifications, including the one kind that is a question
// rather than a notice.
//
// Two jobs, one mechanism:
//
//   - Notifier.Show is a one-way notice ("your agent updated"). No
//     actions, no waiting.
//   - Prompter.Ask is a consent question ("someone wants to connect to
//     your PC"). Two actions, urgency critical, and it waits for
//     ActionInvoked or NotificationClosed naming the notification it
//     posted.
//
// Why D-Bus directly and not notify-send: notify-send posts a notification
// and exits. It cannot tell the agent which button the person pressed,
// which is the entire content of a consent answer. A consent prompt that
// cannot receive an answer is a consent prompt that has to invent one, and
// inventing one is the bug this package was written to remove.
//
// Everything above the wire is behind the Session seam, so the decision
// logic — which paths mean yes, and which of the many ways this can fail
// mean no — is exercised by ordinary table tests on every platform, not
// only on a developer's desktop with a notification daemon running.
//
// Fail closed, every time. Ask returns an allowed Outcome only when a
// human pressed Accept. No session bus (headless, no login), no
// notification daemon on the bus, a daemon that stops answering, a prompt
// dismissed unanswered, a prompt nobody touched inside the window, or a
// cancelled agent — all decline, each under its own Outcome so the log
// says which.
package desktopnotify

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
)

// DefaultTimeout bounds how long a consent prompt waits for an answer.
//
// Short on purpose. This is not the lifetime of the request — the request
// row on the server lives for half an hour and the mentor can approve it
// from the web page long after this prompt is gone. This is how long the
// agent is willing to hold a goroutine open waiting for a desktop that may
// have been locked, suspended or logged out the moment after the
// notification appeared. Expiring it declines nothing that a person can
// still go and approve deliberately.
const DefaultTimeout = 45 * time.Second

// Action keys sent on the wire and matched in ActionInvoked. The label is
// what the person reads; the key is what we compare, so a translated or
// re-worded label can never change which button means yes.
const (
	ActionAccept  = "readyfleet-accept"
	ActionDecline = "readyfleet-decline"
)

// expireNever asks the daemon not to time the notification out itself. The
// agent's own DefaultTimeout is the single authority on how long the
// question stands, so a daemon with a two-second default cannot silently
// shorten a consent window (and a daemon with none cannot leave a dead
// prompt on screen after the agent has already declined).
const expireNever int32 = 0

// expireDaemonDefault lets the daemon decide, which is right for a
// one-way notice nobody has to answer.
const expireDaemonDefault int32 = -1

// Urgency is the org.freedesktop.Notifications "urgency" hint.
type Urgency byte

const (
	UrgencyLow      Urgency = 0
	UrgencyNormal   Urgency = 1
	UrgencyCritical Urgency = 2
)

// Errors the Dialer and Session report. They are what separates "there is
// no desktop here" from "there is a desktop but nothing is listening",
// which are different operational problems with different fixes.
var (
	// ErrNoSessionBus — no session bus to talk to at all.
	ErrNoSessionBus = errors.New("desktopnotify: no D-Bus session bus")

	// ErrNoNotificationDaemon — a session bus, but no
	// org.freedesktop.Notifications on it.
	ErrNoNotificationDaemon = errors.New("desktopnotify: no notification daemon on the session bus")

	// ErrUnsupported — this platform has no desktop notifications at all.
	ErrUnsupported = errors.New("desktopnotify: unsupported on this platform")
)

// Notification is one org.freedesktop.Notifications Notify call.
type Notification struct {
	AppName string
	Summary string
	Body    string
	// Actions are the buttons, in display order. Empty for a one-way
	// notice — a notice with buttons is a question, and this package
	// deliberately does not let one be posted by accident.
	Actions      []Action
	Urgency      Urgency
	ExpireMillis int32
}

// Action is one button: Key is matched against ActionInvoked, Label is
// what the person reads.
type Action struct {
	Key   string
	Label string
}

// EventKind distinguishes the two signals a consent prompt can end on.
type EventKind int

const (
	// EventAction is ActionInvoked: a button was pressed.
	EventAction EventKind = iota
	// EventClosed is NotificationClosed: the notification went away
	// without (or after) an answer.
	EventClosed
)

// Close reasons from the org.freedesktop.Notifications specification.
const (
	closeReasonExpired   uint32 = 1
	closeReasonDismissed uint32 = 2
)

// Event is one signal about one notification.
type Event struct {
	Kind      EventKind
	ID        uint32
	ActionKey string
	Reason    uint32
}

// Session is the narrow slice of a D-Bus connection this package needs.
// It is the seam: the Linux build returns a real godbus-backed session,
// tests return a scripted one, and the decision logic below never knows
// the difference.
//
// Events must be subscribed before Notify returns an id, otherwise a
// person fast enough to answer immediately answers into a channel nobody
// is listening on yet.
type Session interface {
	// Notify posts a notification and returns the daemon's id for it.
	Notify(ctx context.Context, n Notification) (uint32, error)
	// Close asks the daemon to withdraw a notification, so a prompt the
	// agent has already given up on does not sit on screen with buttons
	// that no longer do anything.
	Close(ctx context.Context, id uint32) error
	// Events yields ActionInvoked / NotificationClosed for the whole
	// connection; callers filter by id. Closed when the connection drops.
	Events() <-chan Event
	// Shutdown releases the connection.
	Shutdown() error
}

// Dialer opens a session. Swapped in tests; defaults to Dial, which is
// real on Linux and ErrUnsupported everywhere else.
type Dialer func() (Session, error)

// AppName is what the desktop shows as the sender.
const AppName = "ReadyFleet"

// Notifier shows one-way notices. Zero value is usable on Linux.
type Notifier struct {
	Dial    Dialer
	Logger  *slog.Logger
	AppName string
	// Timeout bounds the D-Bus round trip, not how long the notice is on
	// screen — a wedged daemon must not hold an agent command handler
	// open. Defaults to DefaultTimeout.
	Timeout time.Duration
}

// Show posts a notice and returns as soon as the daemon has taken it. It
// registers no actions and waits for no answer: nothing about a notice is
// a question, so there is nothing to fail closed about. A returned error
// means the notice genuinely did not reach a screen, which the caller
// reports upstream rather than claiming a success nobody saw.
func (n Notifier) Show(ctx context.Context, title, body string, urgency Urgency) error {
	dial := n.Dial
	if dial == nil {
		dial = Dial
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	appName := n.AppName
	if appName == "" {
		appName = AppName
	}

	sess, err := dial()
	if err != nil {
		return err
	}
	defer func() { _ = sess.Shutdown() }()

	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := sess.Notify(callCtx, Notification{
		AppName:      appName,
		Summary:      title,
		Body:         body,
		Urgency:      urgency,
		ExpireMillis: expireDaemonDefault,
	}); err != nil {
		return err
	}
	return nil
}

// Prompter asks a consent question and waits for the answer. It
// implements consent.Asker.
type Prompter struct {
	Dial    Dialer
	Logger  *slog.Logger
	AppName string
	// Timeout is how long the question stands before it is withdrawn and
	// declined. Defaults to DefaultTimeout.
	Timeout time.Duration
}

// AcceptLabel and DeclineLabel are the button texts. Plain words, because
// the person reading them is being asked to let a colleague into their own
// computer and should not have to interpret anything.
const (
	AcceptLabel  = "Accept"
	DeclineLabel = "Decline"
)

// Ask puts the question on the person's screen and reports what they did.
//
// The one guarantee: it returns consent.OutcomeAccepted only after
// ActionInvoked names this notification and this Accept button. Every
// other route out — including every route where nothing was ever
// displayed — is a decline carrying the reason.
func (p Prompter) Ask(ctx context.Context, req consent.Request) consent.Outcome {
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	dial := p.Dial
	if dial == nil {
		dial = Dial
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	appName := p.AppName
	if appName == "" {
		appName = AppName
	}

	sess, err := dial()
	if err != nil {
		// No desktop to ask on. Which of the three is worth distinguishing
		// in the log: headless machine, broken user session, or a build
		// that has no desktop notifications at all.
		switch {
		case errors.Is(err, ErrNoNotificationDaemon):
			log.Warn("consent prompt not shown: session bus has no notification daemon", "err", err)
			return consent.OutcomeNoNotificationDaemon
		case errors.Is(err, ErrUnsupported):
			log.Warn("consent prompt not shown: desktop notifications are unsupported on this platform", "err", err)
			return consent.OutcomeNoPrompter
		default:
			log.Warn("consent prompt not shown: no D-Bus session bus (headless, or nobody logged in)", "err", err)
			return consent.OutcomeNoSessionBus
		}
	}
	defer func() { _ = sess.Shutdown() }()

	// Subscribe before posting: a person who answers the instant the
	// notification appears must not answer into a channel that does not
	// exist yet.
	events := sess.Events()

	callCtx, cancelCall := context.WithTimeout(ctx, timeout)
	id, err := sess.Notify(callCtx, Notification{
		AppName: appName,
		Summary: req.Title,
		Body:    req.Body,
		Actions: []Action{
			{Key: ActionAccept, Label: AcceptLabel},
			{Key: ActionDecline, Label: DeclineLabel},
		},
		Urgency:      UrgencyCritical,
		ExpireMillis: expireNever,
	})
	cancelCall()
	if err != nil {
		log.Warn("consent prompt not shown: notification daemon refused it", "err", err)
		return consent.OutcomeNoNotificationDaemon
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				// The connection went away mid-question. Nobody can
				// answer now, and nobody did.
				p.withdraw(ctx, sess, id)
				log.Warn("consent prompt abandoned: session bus dropped while waiting for an answer")
				return consent.OutcomeNoNotificationDaemon
			}
			if ev.ID != id {
				continue // somebody else's notification
			}
			switch ev.Kind {
			case EventAction:
				if ev.ActionKey == ActionAccept {
					p.withdraw(ctx, sess, id)
					return consent.OutcomeAccepted
				}
				// Decline, or any action key this build did not register
				// — an answer we cannot read as yes is a no.
				p.withdraw(ctx, sess, id)
				log.Warn("consent prompt declined at the device", "action", ev.ActionKey)
				return consent.OutcomeDeclined
			case EventClosed:
				// Already gone; nothing to withdraw.
				if ev.Reason == closeReasonExpired {
					log.Warn("consent prompt expired on the desktop with no answer")
					return consent.OutcomeTimedOut
				}
				log.Warn("consent prompt dismissed without an answer", "reason", ev.Reason)
				return consent.OutcomeDismissed
			}
		case <-timer.C:
			p.withdraw(ctx, sess, id)
			log.Warn("consent prompt timed out with no answer", "after", timeout.String())
			return consent.OutcomeTimedOut
		case <-ctx.Done():
			p.withdraw(ctx, sess, id)
			log.Warn("consent prompt abandoned: the agent stopped waiting", "err", ctx.Err())
			return consent.OutcomeCancelled
		}
	}
}

// withdraw takes a prompt off the screen once the agent has stopped
// listening to it, so a mentor never presses Accept on a question that was
// answered for them minutes ago. Best effort by design: the decision is
// already made and a failure to tidy up must not change it.
//
// Uses a detached context — the common reason to withdraw is that ctx is
// already cancelled, and a cleanup call that cannot run in exactly that
// case is not cleanup.
func (p Prompter) withdraw(_ context.Context, sess Session, id uint32) {
	callCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = sess.Close(callCtx, id)
}

// UrgencyFromSeverity maps the wire's severity string
// (proto.ShowNotificationArgs.Severity) to a desktop urgency, defaulting
// unknown and empty values to normal — matching the Windows tray's
// fail-open treatment of the same field, so a server can introduce a
// severity an older agent has never heard of without breaking it.
//
// UrgencyLow is deliberately never produced. freedesktop has only three
// levels and low is the one desktops are entitled to swallow under Do Not
// Disturb; a notice the controller went to the trouble of dispatching to
// one specific machine should not be silently dropped for being routine.
// That collapses "info" and "warning" onto normal, which is the same
// distinction the Windows tray draws with its balloon icon and nothing
// more.
func UrgencyFromSeverity(severity string) Urgency {
	if severity == "error" {
		return UrgencyCritical
	}
	return UrgencyNormal
}
