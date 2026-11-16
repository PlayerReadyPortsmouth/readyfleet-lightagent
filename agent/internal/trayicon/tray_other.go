//go:build !windows && !linux

package trayicon

import "context"

// Tray is a no-op on platforms without an implemented native tray.
type Tray struct {
	Tooltip  string
	IconPath string
	// OnActivate optionally replaces the left-click menu. Set before Run.
	OnActivate func()
}

// MenuItem mirrors the Windows build's type so callers compile
// cross-platform without a build-tagged import split of their own.
type MenuItem struct {
	ID        int32
	Label     string
	Checked   bool
	Disabled  bool
	Separator bool
	OnClick   func()
}

// Run blocks until ctx is cancelled, doing nothing — there is no tray to
// drive on a non-Windows host.
func (t *Tray) Run(ctx context.Context, items []MenuItem) error {
	<-ctx.Done()
	return nil
}

func (t *Tray) SetTooltip(s string)           {}
func (t *Tray) SetIconPath(path string) error { return nil }
func (t *Tray) SetMenuItems(items []MenuItem) {}
func (t *Tray) Quit()                         {}

// NotifySeverity mirrors the Windows build's type so callers compile
// cross-platform without a build-tagged import split of their own.
type NotifySeverity uint32

const (
	NotifyInfo    NotifySeverity = 0
	NotifyWarning NotifySeverity = 1
	NotifyError   NotifySeverity = 2
)

func (t *Tray) Notify(title, body string, severity NotifySeverity, onClick func()) error { return nil }

func (t *Tray) SetBadge(active bool) error { return nil }
