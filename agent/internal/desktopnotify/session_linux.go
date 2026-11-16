//go:build linux

package desktopnotify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
)

// The org.freedesktop.Notifications addresses. Fixed by the specification;
// there is no discovery step.
const (
	notifyBusName    = "org.freedesktop.Notifications"
	notifyObjectPath = dbus.ObjectPath("/org/freedesktop/Notifications")
	notifyInterface  = "org.freedesktop.Notifications"

	methodNotify = notifyInterface + ".Notify"
	methodClose  = notifyInterface + ".CloseNotification"

	signalActionInvoked = notifyInterface + ".ActionInvoked"
	signalClosed        = notifyInterface + ".NotificationClosed"
)

// signalBuffer sizes the channel godbus delivers signals on. Generous
// relative to the one or two signals a single prompt produces, because
// godbus DROPS signals rather than blocking when the buffer is full: a
// desktop noisy with other applications' notifications must not be able to
// starve this connection of the one signal that carries a person's answer.
// A private connection with a match rule scoped to this interface keeps
// the traffic small in the first place.
const signalBuffer = 32

// dbusSession is the real Session: one PRIVATE session-bus connection per
// prompt.
//
// Private rather than the process-wide dbus.SessionBus() for two reasons.
// A shared connection's signal channels and match rules are shared too, so
// closing one would disturb every other user of it; and a prompt that ends
// must be able to hang up, because the light agent runs for weeks and a
// leaked connection per notification is a leak per notification.
type dbusSession struct {
	conn   *dbus.Conn
	obj    dbus.BusObject
	raw    chan *dbus.Signal
	events chan Event
	done   chan struct{}
}

// Dial opens a session bus and confirms something is actually listening
// for notifications on it.
//
// The ownership check is not ceremony: a Notify call to an unowned name
// returns an error that reads like a transport fault, and the difference
// between "this machine has no desktop" and "this desktop has no
// notification service" is the difference between an expected headless
// decline and a broken user session someone should fix. Checking up front
// lets the caller log the right one.
//
// Never auto-starts a bus. SessionBusPrivateNoAutoStartup is deliberate:
// the agent runs as the person's own systemd user unit, where
// DBUS_SESSION_BUS_ADDRESS is inherited from a real login session. If it
// is missing, the honest answer is that there is no desktop here — not to
// fork a fresh dbus-daemon that nobody's screen is attached to and post
// consent prompts into it.
func Dial() (Session, error) {
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		// Report the environment godbus looked at, because "there is no
		// session bus" and "the unit did not inherit the address" look
		// identical from the log otherwise, and only one of them is a
		// packaging bug. godbus also falls back to /run/user/<uid>/bus on
		// its own, so an empty address variable is not conclusive — which
		// is exactly why this asks godbus rather than guessing from the
		// environment first.
		return nil, fmt.Errorf("%w (DBUS_SESSION_BUS_ADDRESS=%q XDG_RUNTIME_DIR=%q): %v",
			ErrNoSessionBus, os.Getenv("DBUS_SESSION_BUS_ADDRESS"), os.Getenv("XDG_RUNTIME_DIR"), err)
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: auth: %v", ErrNoSessionBus, err)
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: hello: %v", ErrNoSessionBus, err)
	}

	var hasOwner bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, notifyBusName).Store(&hasOwner); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: %v", ErrNoNotificationDaemon, err)
	}
	if !hasOwner {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: nothing owns %s", ErrNoNotificationDaemon, notifyBusName)
	}

	// Match only this interface's signals on this object, so the buffer
	// carries answers and not the whole session's chatter.
	if err := conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notifyObjectPath),
		dbus.WithMatchInterface(notifyInterface),
	); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("desktopnotify: subscribe to notification signals: %w", err)
	}

	s := &dbusSession{
		conn:   conn,
		obj:    conn.Object(notifyBusName, notifyObjectPath),
		raw:    make(chan *dbus.Signal, signalBuffer),
		events: make(chan Event, signalBuffer),
		done:   make(chan struct{}),
	}
	conn.Signal(s.raw)
	go s.pump()
	return s, nil
}

// pump translates godbus signals into the platform-neutral Event the
// decision logic waits on, and closes the events channel when the
// connection goes away — which is what tells a waiting prompt that nobody
// can answer it any more.
func (s *dbusSession) pump() {
	defer close(s.events)
	for {
		select {
		case <-s.done:
			return
		case sig, ok := <-s.raw:
			if !ok {
				return
			}
			ev, ok := eventFromSignal(sig)
			if !ok {
				continue
			}
			select {
			case s.events <- ev:
			case <-s.done:
				return
			}
		}
	}
}

// eventFromSignal decodes ActionInvoked (uint32 id, string action_key) and
// NotificationClosed (uint32 id, uint32 reason). A signal whose body does
// not match its documented shape is dropped rather than guessed at: a
// malformed answer is not an answer, and the caller's timeout is a safer
// fallback than a decoded-by-accident one.
func eventFromSignal(sig *dbus.Signal) (Event, bool) {
	if sig == nil {
		return Event{}, false
	}
	switch sig.Name {
	case signalActionInvoked:
		if len(sig.Body) < 2 {
			return Event{}, false
		}
		id, ok := sig.Body[0].(uint32)
		if !ok {
			return Event{}, false
		}
		key, ok := sig.Body[1].(string)
		if !ok {
			return Event{}, false
		}
		return Event{Kind: EventAction, ID: id, ActionKey: key}, true
	case signalClosed:
		if len(sig.Body) < 2 {
			return Event{}, false
		}
		id, ok := sig.Body[0].(uint32)
		if !ok {
			return Event{}, false
		}
		reason, ok := sig.Body[1].(uint32)
		if !ok {
			return Event{}, false
		}
		return Event{Kind: EventClosed, ID: id, Reason: reason}, true
	default:
		return Event{}, false
	}
}

func (s *dbusSession) Notify(ctx context.Context, n Notification) (uint32, error) {
	actions := make([]string, 0, len(n.Actions)*2)
	for _, a := range n.Actions {
		actions = append(actions, a.Key, a.Label)
	}
	hints := map[string]dbus.Variant{
		"urgency": dbus.MakeVariant(byte(n.Urgency)),
		// Ask the daemon to keep a critical prompt where the person will
		// see it rather than folding it straight into a history list.
		"resident": dbus.MakeVariant(n.Urgency == UrgencyCritical),
	}

	var id uint32
	call := s.obj.CallWithContext(ctx, methodNotify, 0,
		n.AppName,
		uint32(0), // replaces_id: never replace, each prompt is its own question
		"",        // app_icon: the desktop's own default for an unknown app
		n.Summary,
		n.Body,
		actions,
		hints,
		n.ExpireMillis,
	)
	if call.Err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNoNotificationDaemon, call.Err)
	}
	if err := call.Store(&id); err != nil {
		return 0, fmt.Errorf("desktopnotify: decode Notify reply: %w", err)
	}
	return id, nil
}

func (s *dbusSession) Close(ctx context.Context, id uint32) error {
	call := s.obj.CallWithContext(ctx, methodClose, 0, id)
	if call.Err != nil {
		return fmt.Errorf("desktopnotify: CloseNotification: %w", call.Err)
	}
	return nil
}

func (s *dbusSession) Events() <-chan Event { return s.events }

func (s *dbusSession) Shutdown() error {
	select {
	case <-s.done:
		return nil // already shut down; Shutdown is called from a defer
	default:
	}
	close(s.done)
	s.conn.RemoveSignal(s.raw)
	if err := s.conn.Close(); err != nil && !errors.Is(err, dbus.ErrClosed) {
		return fmt.Errorf("desktopnotify: close session bus: %w", err)
	}
	return nil
}
