//go:build linux

package sessionindicator

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"
)

// The org.freedesktop.ScreenSaver addresses. Fixed by the (de-facto)
// specification every desktop screen locker implements; there is no
// discovery step. Both object paths are checked because the name is
// conventionally exported at each and desktops disagree about which —
// KDE's kwin_wayland answers on both, GNOME historically only on
// /org/freedesktop/ScreenSaver.
const (
	screenSaverBusName   = "org.freedesktop.ScreenSaver"
	screenSaverInterface = "org.freedesktop.ScreenSaver"
	methodGetActive      = screenSaverInterface + ".GetActive"
)

var screenSaverPaths = []dbus.ObjectPath{
	"/org/freedesktop/ScreenSaver",
	"/ScreenSaver",
}

// The systemd-logind addresses, used only as the fallback below.
const (
	logindBusName        = "org.freedesktop.login1"
	logindManagerPath    = dbus.ObjectPath("/org/freedesktop/login1")
	logindManagerIface   = "org.freedesktop.login1.Manager"
	logindUserIface      = "org.freedesktop.login1.User"
	logindSessionIface   = "org.freedesktop.login1.Session"
	methodGetUser        = logindManagerIface + ".GetUser"
	methodGetSession     = logindManagerIface + ".GetSession"
	propUserDisplay      = logindUserIface + ".Display"
	propSessionLockedHnt = logindSessionIface + ".LockedHint"
)

// drmRoot is where the kernel exposes one directory per DRM connector.
// A variable so the scanner can be pointed at a fixture tree in tests.
var drmRoot = "/sys/class/drm"

// readScreenState answers both questions on Linux.
//
// The two halves are completely independent mechanisms and either can
// come back Unknown on its own; a desktop with no screen locker on the
// bus still has readable DRM connectors, and a container with no
// /sys/class/drm still has a session bus.
func readScreenState() ScreenState {
	locked, lockedSrc := linuxLocked()
	off, offSrc := displaysOffUnder(drmRoot)
	return ScreenState{
		Locked:            locked,
		LockedSource:      lockedSrc,
		DisplaysOff:       off,
		DisplaysOffSource: offSrc,
	}
}

// linuxLocked asks the screen locker first and systemd-logind second.
//
// Order matters and is not arbitrary. org.freedesktop.ScreenSaver is
// served BY the thing that draws the lock screen (kwin_wayland on KDE,
// gnome-shell on GNOME), so its answer is the locker's own view of
// itself. logind's LockedHint is second-hand: it is only true if the
// locker remembered to call SetLockedHint, which not every desktop does,
// so a false there can mean "not locked" or "nobody told me" and the two
// are indistinguishable. Asking the authority first and the bookkeeping
// second means the common case is answered by the component that
// actually knows.
//
// Whichever answers first wins outright, rather than OR-ing the two.
// Combining them would let the weaker source override the stronger one
// in the direction that suppresses the indicator, and a suppressed
// indicator is the failure this package exists to prevent.
func linuxLocked() (Tri, string) {
	if v, ok := screenSaverActive(); ok {
		return triOf(v), "org.freedesktop.ScreenSaver"
	}
	if v, ok := logindLockedHint(); ok {
		return triOf(v), "logind LockedHint"
	}
	return Unknown, ""
}

// screenSaverActive calls GetActive on the session bus.
//
// SessionBusPrivateNoAutoStartup, like internal/desktopnotify: if there
// is no session bus address to inherit, the honest answer is "there is
// no desktop here", not to fork a dbus-daemon nobody's screen is
// attached to and interrogate it.
func screenSaverActive() (active bool, ok bool) {
	conn, err := dialPrivate(dbus.SessionBusPrivateNoAutoStartup)
	if err != nil {
		return false, false
	}
	defer func() { _ = conn.Close() }()

	var hasOwner bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, screenSaverBusName).Store(&hasOwner); err != nil || !hasOwner {
		return false, false
	}

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	for _, path := range screenSaverPaths {
		var v bool
		call := conn.Object(screenSaverBusName, path).CallWithContext(ctx, methodGetActive, 0)
		if call.Err != nil {
			continue
		}
		if err := call.Store(&v); err != nil {
			continue
		}
		return v, true
	}
	return false, false
}

// logindLockedHint reads LockedHint off the user's GRAPHICAL session.
//
// Not the calling process's session: the light agent runs as a systemd
// user unit, whose own logind session is the class=manager one, not the
// seat0 graphical login that has a lock screen. Going
// user -> Display -> LockedHint is what `loginctl show-user -p Display`
// does, and is the only route that lands on the session a person is
// actually sitting at. XDG_SESSION_ID is the fallback for the case where
// the user object has no Display session (a bare TTY login).
func logindLockedHint() (locked bool, ok bool) {
	conn, err := dialPrivate(dbus.SystemBusPrivate)
	if err != nil {
		return false, false
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	manager := conn.Object(logindBusName, logindManagerPath)

	sessionPath, ok := logindGraphicalSession(ctx, conn, manager)
	if !ok {
		return false, false
	}
	v, err := conn.Object(logindBusName, sessionPath).GetProperty(propSessionLockedHnt)
	if err != nil {
		return false, false
	}
	b, ok := v.Value().(bool)
	return b, ok
}

func logindGraphicalSession(ctx context.Context, conn *dbus.Conn, manager dbus.BusObject) (dbus.ObjectPath, bool) {
	var userPath dbus.ObjectPath
	if err := manager.CallWithContext(ctx, methodGetUser, 0, uint32(os.Getuid())).Store(&userPath); err == nil {
		v, err := conn.Object(logindBusName, userPath).GetProperty(propUserDisplay)
		if err == nil {
			// Display is (session-id, object-path); an empty id means the
			// user has no graphical session and the struct is a zero value.
			if pair, ok := v.Value().([]any); ok && len(pair) == 2 {
				id, _ := pair[0].(string)
				path, pathOK := pair[1].(dbus.ObjectPath)
				if id != "" && pathOK {
					return path, true
				}
			}
		}
	}
	if id := os.Getenv("XDG_SESSION_ID"); id != "" {
		var path dbus.ObjectPath
		if err := manager.CallWithContext(ctx, methodGetSession, 0, id).Store(&path); err == nil {
			return path, true
		}
	}
	return "", false
}

// dialPrivate opens and completes the handshake on a private bus
// connection, closing it again if any step fails so a half-open
// connection is never leaked. Private rather than the process-wide
// SessionBus()/SystemBus() for the same reason internal/desktopnotify
// uses one: this agent runs for weeks, and a shared connection cannot be
// hung up after a probe.
func dialPrivate(open func(...dbus.ConnOption) (*dbus.Conn, error)) (*dbus.Conn, error) {
	conn, err := open()
	if err != nil {
		return nil, err
	}
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// displaysOffUnder reports whether every display that is actually
// driving a screen is powered down.
//
// The kernel exposes one directory per DRM connector under
// /sys/class/drm, each with world-readable `status`, `enabled` and
// `dpms` files. Only connectors that are BOTH connected and enabled are
// considered: a machine with four unused DisplayPort connectors on the
// integrated GPU reads `dpms = Off` on all four permanently, and
// counting those would declare every such machine asleep forever. (That
// is not hypothetical — it is exactly what the pilot CachyOS box looks
// like: card0 has four disconnected connectors all reading Off while
// card1's two connected ones read On.)
//
// Yes only when there is at least one connector to judge and every one
// of them is off. No connectors at all is Unknown, not No: a container
// or a kernel without the sysfs nodes has told us nothing.
func displaysOffUnder(root string) (Tri, string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return Unknown, ""
	}
	considered, off := 0, 0
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		if readTrimmed(filepath.Join(dir, "status")) != "connected" {
			continue
		}
		if readTrimmed(filepath.Join(dir, "enabled")) != "enabled" {
			continue
		}
		dpms := readTrimmed(filepath.Join(dir, "dpms"))
		if dpms == "" {
			continue // no dpms attribute: this connector cannot answer
		}
		considered++
		if !strings.EqualFold(dpms, "On") {
			off++
		}
	}
	if considered == 0 {
		return Unknown, ""
	}
	if off == considered {
		return Yes, "DRM connector dpms"
	}
	return No, "DRM connector dpms"
}

func readTrimmed(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
