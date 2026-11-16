//go:build !linux

package desktopnotify

// Dial's non-Linux build has nothing to dial. org.freedesktop.Notifications
// is a Linux desktop service; Windows shows a tray balloon
// (internal/trayicon) and a TaskDialog instead.
//
// It returns an error rather than a session that pretends to work, so a
// consent prompt on a platform with no desktop notifications DECLINES and
// says why — the same fail-closed rule every other path in this package
// obeys. Nothing here ever answers on a person's behalf.
func Dial() (Session, error) { return nil, ErrUnsupported }
