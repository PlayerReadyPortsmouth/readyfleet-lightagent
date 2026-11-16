//go:build linux

package trayicon

// The Linux light agent belongs to the logged-in user. A private session-bus
// connection publishes a StatusNotifierItem and DBusMenu, understood by Plasma
// and other hosts implementing that protocol. Nothing is installed in the
// desktop and no bus is auto-started. The tray is deliberately independent of
// the optional companion window: its owner can always quit the BYOD agent.
import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/desktopnotify"
)

const (
	itemPath      dbus.ObjectPath = "/StatusNotifierItem"
	menuPath      dbus.ObjectPath = "/MenuBar"
	itemInterface                 = "org.kde.StatusNotifierItem"
	menuInterface                 = "com.canonical.dbusmenu"
	watcherName                   = "org.kde.StatusNotifierWatcher"
	watcherPath   dbus.ObjectPath = "/StatusNotifierWatcher"
)

type MenuItem struct {
	ID        int32
	Label     string
	Checked   bool
	Disabled  bool
	Separator bool
	OnClick   func()
}

type NotifySeverity uint32

const (
	NotifyInfo NotifySeverity = iota
	NotifyWarning
	NotifyError
)

type Tray struct {
	Tooltip    string
	IconPath   string
	OnActivate func()

	mu    sync.Mutex
	state *linuxTrayState
	quit  bool
	badge bool
	// Only tests override these seams. Production uses a private login bus.
	dial       func() (*dbus.Conn, error)
	notifyDial desktopnotify.Dialer
}

type linuxTrayState struct {
	mu       sync.RWMutex
	tooltip  string
	icon     []iconPixmap
	badge    bool
	items    []MenuItem
	revision uint32
	activate func()
	ctx      context.Context
	cancel   context.CancelFunc
	events   chan trayEvent
	changed  chan struct{}
}

type trayEvent struct {
	id       int32
	activate bool
}

func (t *Tray) Run(ctx context.Context, items []MenuItem) error {
	if ctx.Err() != nil {
		return nil
	}
	t.mu.Lock()
	if t.state != nil {
		t.mu.Unlock()
		return errors.New("trayicon: already running")
	}
	if t.quit {
		t.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	state := &linuxTrayState{tooltip: t.Tooltip, icon: loadTrayIcon(t.IconPath), badge: t.badge,
		items: copyMenu(items), revision: 1, activate: t.OnActivate, ctx: runCtx, cancel: cancel,
		events: make(chan trayEvent, 32), changed: make(chan struct{}, 1)}
	t.state = state
	dial := t.dial
	t.mu.Unlock()
	defer func() { cancel(); t.mu.Lock(); t.state = nil; t.mu.Unlock() }()
	if dial == nil {
		dial = dialTrayBus
	}
	conn, err := dial()
	if err != nil {
		return fmt.Errorf("trayicon: connect to login session bus: %w", err)
	}
	defer conn.Close()
	if err = exportTray(conn, state); err != nil {
		return err
	}
	signals := make(chan *dbus.Signal, 16)
	conn.Signal(signals)
	defer conn.RemoveSignal(signals)
	if err = conn.AddMatchSignal(dbus.WithMatchSender("org.freedesktop.DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"), dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, watcherName)); err != nil {
		return fmt.Errorf("trayicon: watch tray host: %w", err)
	}
	// A desktop can start its tray host after the user unit, or restart Plasma.
	// Register with the private unique bus name; no process-global name collision.
	register := func() bool {
		callCtx, done := context.WithTimeout(runCtx, 2*time.Second)
		defer done()
		return conn.Object(watcherName, watcherPath).CallWithContext(callCtx,
			watcherName+".RegisterStatusNotifierItem", dbus.FlagNoAutoStart, conn.Names()[0]).Err == nil
	}
	registered := register()
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	go state.dispatch()
	for {
		select {
		case <-runCtx.Done():
			return nil
		case <-conn.Context().Done():
			return errors.New("trayicon: login session bus disconnected")
		case <-retry.C:
			if !registered {
				registered = register()
			}
		case sig := <-signals:
			if watcherAppeared(sig) {
				registered = register()
			} else if watcherChanged(sig) {
				registered = false
			}
		case <-state.changed:
			state.mu.RLock()
			rev := state.revision
			state.mu.RUnlock()
			// Hosts cache menu properties; a new layout revision explicitly invalidates
			// their old labels and enabled flags. Item properties are fetched on signal.
			_ = conn.Emit(menuPath, menuInterface+".LayoutUpdated", rev, int32(0))
			_ = conn.Emit(itemPath, itemInterface+".NewToolTip")
			_ = conn.Emit(itemPath, itemInterface+".NewIcon")
		}
	}
}

func dialTrayBus() (*dbus.Conn, error) {
	conn, err := dbus.SessionBusPrivateNoAutoStartup()
	if err != nil {
		return nil, err
	}
	if err = conn.Auth(nil); err == nil {
		err = conn.Hello()
	}
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func watcherChanged(sig *dbus.Signal) bool {
	return sig != nil && sig.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(sig.Body) == 3 && sig.Body[0] == watcherName
}
func watcherAppeared(sig *dbus.Signal) bool {
	if !watcherChanged(sig) {
		return false
	}
	owner, ok := sig.Body[2].(string)
	return ok && owner != ""
}
func copyMenu(items []MenuItem) []MenuItem {
	out := make([]MenuItem, 0, len(items))
	seen := map[int32]bool{}
	// Menu IDs are stable caller-owned positive values. The root is always 0;
	// separators commonly arrive with ID 0, so give those private negative IDs.
	separator := int32(-1)
	for _, item := range items {
		if item.Separator {
			item.ID = separator
			separator--
			item.OnClick = nil
		} else if item.ID <= 0 || seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		out = append(out, item)
	}
	return out
}
func (s *linuxTrayState) changedNow() {
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
func (s *linuxTrayState) enqueue(event trayEvent) *dbus.Error {
	select {
	case <-s.ctx.Done():
		return dbus.MakeFailedError(errors.New("tray has closed"))
	default:
	}
	select {
	case s.events <- event:
		return nil
	default:
		return dbus.MakeFailedError(errors.New("tray is busy"))
	}
}
func (s *linuxTrayState) callback(event trayEvent) func() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if event.activate {
		return s.activate
	}
	for _, item := range s.items {
		if item.ID == event.id && !item.Disabled && !item.Separator {
			return item.OnClick
		}
	}
	return nil
}
func (s *linuxTrayState) dispatch() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case event := <-s.events:
			if s.ctx.Err() != nil {
				return
			}
			if callback := s.callback(event); callback != nil {
				callback()
			}
		}
	}
}
func (t *Tray) SetTooltip(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Tooltip = value
	if s := t.state; s != nil {
		s.mu.Lock()
		s.tooltip = value
		s.mu.Unlock()
		s.changedNow()
	}
}
func (t *Tray) SetIconPath(path string) error {
	icon := loadTrayIcon(path)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.IconPath = path
	if s := t.state; s != nil {
		s.mu.Lock()
		s.icon = icon
		s.mu.Unlock()
		s.changedNow()
	}
	return nil
}
func (t *Tray) SetMenuItems(items []MenuItem) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s := t.state; s != nil {
		s.mu.Lock()
		s.items = copyMenu(items)
		s.revision++
		s.mu.Unlock()
		s.changedNow()
	}
}
func (t *Tray) SetBadge(active bool) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.badge = active
	if s := t.state; s != nil {
		s.mu.Lock()
		s.badge = active
		s.mu.Unlock()
		s.changedNow()
	}
	return nil
}
func (t *Tray) Quit() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.quit = true
	if s := t.state; s != nil {
		s.cancel()
	}
}

// Notify uses the same login-session notification implementation as BYOD
// consent. Click actions are one-shot, bounded and cancelled on agent exit;
// notices without a callback don't keep an extra connection alive.
func (t *Tray) Notify(title, body string, severity NotifySeverity, onClick func()) error {
	t.mu.Lock()
	state, dial := t.state, t.notifyDial
	t.mu.Unlock()
	if state == nil {
		return errors.New("trayicon: tray is not running")
	}
	urgency := desktopnotify.UrgencyNormal
	if severity == NotifyError {
		urgency = desktopnotify.UrgencyCritical
	}
	if onClick == nil {
		return (desktopnotify.Notifier{Dial: dial, Timeout: 5 * time.Second}).Show(state.ctx, title, body, urgency)
	}
	if dial == nil {
		dial = desktopnotify.Dial
	}
	session, err := dial()
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(state.ctx, 5*time.Second)
	id, err := session.Notify(callCtx, desktopnotify.Notification{AppName: "ReadyFleet", Summary: title, Body: body,
		Urgency: urgency, ExpireMillis: 15000, Actions: []desktopnotify.Action{{Key: "default", Label: "Open"}}})
	cancel()
	if err != nil {
		_ = session.Shutdown()
		return err
	}
	go func() {
		defer session.Shutdown()
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-state.ctx.Done():
				return
			case <-timer.C:
				return
			case event, ok := <-session.Events():
				if !ok {
					return
				}
				if event.ID != id {
					continue
				}
				if event.Kind == desktopnotify.EventAction && event.ActionKey == "default" {
					if state.ctx.Err() == nil {
						onClick()
					}
					return
				}
				if event.Kind == desktopnotify.EventClosed {
					return
				}
			}
		}
	}()
	return nil
}

func exportTray(conn *dbus.Conn, s *linuxTrayState) error {
	for _, entry := range []struct {
		path   dbus.ObjectPath
		name   string
		object interface{}
	}{
		{itemPath, itemInterface, &notifierItem{s}}, {itemPath, "org.freedesktop.DBus.Properties", &trayProperties{s, false}},
		{menuPath, menuInterface, &dbusMenu{s}}, {menuPath, "org.freedesktop.DBus.Properties", &trayProperties{s, true}},
	} {
		if err := conn.Export(entry.object, entry.path, entry.name); err != nil {
			return fmt.Errorf("trayicon: export %s: %w", entry.name, err)
		}
	}
	for _, entry := range []struct {
		path       dbus.ObjectPath
		name       string
		object     interface{}
		properties []introspect.Property
	}{
		{itemPath, itemInterface, &notifierItem{s}, itemIntrospection()},
		{menuPath, menuInterface, &dbusMenu{s}, []introspect.Property{{Name: "Version", Type: "u", Access: "read"}, {Name: "TextDirection", Type: "s", Access: "read"}, {Name: "Status", Type: "s", Access: "read"}, {Name: "IconThemePath", Type: "as", Access: "read"}}},
	} {
		signals := []introspect.Signal{{Name: "NewIcon"}, {Name: "NewToolTip"}}
		if entry.path == menuPath {
			signals = []introspect.Signal{{Name: "LayoutUpdated", Args: []introspect.Arg{{Name: "revision", Type: "u"}, {Name: "parent", Type: "i"}}}}
		}
		node := &introspect.Node{Interfaces: []introspect.Interface{introspect.IntrospectData,
			{Name: entry.name, Methods: introspect.Methods(entry.object), Properties: entry.properties, Signals: signals},
			{Name: "org.freedesktop.DBus.Properties", Methods: introspect.Methods(&trayProperties{s, entry.path == menuPath})}}}
		if err := conn.Export(introspect.NewIntrospectable(node), entry.path, "org.freedesktop.DBus.Introspectable"); err != nil {
			return err
		}
	}
	return nil
}
