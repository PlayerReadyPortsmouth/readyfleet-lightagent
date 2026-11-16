//go:build windows

package sessionindicator

import (
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/signedin"
)

// ⚠ NOTHING IN THIS FILE HAS BEEN RUN, for the same reason as
// indicator_windows.go: it was written on a Linux host. It compiles
// (GOOS=windows go vet ./internal/sessionindicator/) and the decision
// logic it feeds is tested on every platform through the screenStateFn
// seam, but the WTSQuerySessionInformationW call itself has not been
// observed against a real Windows session.

// wtsCurrentSession is WTS_CURRENT_SESSION: query the caller's own
// session. Correct here and NOT the same choice internal/endofday makes:
// that package is the managed agent's Windows SERVICE, which lives in
// session 0 and has to go hunting for the interactive session. The light
// agent this indicator belongs to runs interactively, in the person's
// own session, so its own session IS the one with the lock screen on it.
const wtsCurrentSession = 0xFFFFFFFF

// readScreenState answers what it can on Windows.
//
// Lock state comes from WTSINFOEX.SessionFlags, which is the documented
// query for it and needs no window, no message pump and no privilege
// beyond querying one's own session.
//
// DISPLAY POWER IS DELIBERATELY LEFT UNKNOWN. Win32 has no synchronous
// query for console display power state at all: it is published only as
// a GUID_CONSOLE_DISPLAY_STATE WM_POWERBROADCAST notification, delivered
// to a window that registered for it with RegisterPowerSettingNotification
// and has been pumping messages since before the transition happened.
// This package has no such window when the bar is down — the only window
// it creates is the bar itself — so there is nothing here that could have
// observed the last transition. Reporting Unknown means Decide falls
// through to SHOW, which is the safe direction; claiming No would be a
// guess dressed as a reading, and claiming Yes would suppress the
// indicator on every Windows machine.
//
// In practice the gap is small: a Windows machine whose displays have
// powered down through idle has almost always locked as well, and the
// lock check above catches that.
func readScreenState() ScreenState {
	locked, ok := windowsSessionLocked()
	if !ok {
		return ScreenState{}
	}
	return ScreenState{Locked: triOf(locked), LockedSource: "WTSINFOEX SessionFlags"}
}

func windowsSessionLocked() (locked bool, ok bool) {
	return signedin.SessionLocked(wtsCurrentSession)
}
