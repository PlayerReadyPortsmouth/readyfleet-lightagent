package companionui

// Staff authority on a shared PC.
//
// The panel runs under the young person's Windows account. After a member of
// staff approves this device in their ReadyApp browser, the panel can drive
// staff-only actions (disconnect, availability Off, leave, admit…). That
// authority belongs to the person who approved it, not to whoever is sitting at
// the keyboard next, so it must not outlive the staff member's presence: the
// panel reports every signal that they may have gone, and a backend that holds a
// staff lease implements StaffAuthority to drop it at once.
//
// Hiding the panel is not revocation. The backend enforces the revocation where
// actions execute, so a request that races a lock, or arrives from a stale
// review, is refused there rather than merely made invisible.

// RevokeReason says why staff authority was dropped; it is shown to the user.
type RevokeReason uint8

const (
	NotRevoked RevokeReason = iota
	RevokeSessionLocked
	RevokeSessionDisconnected
	RevokeSessionChanged
	RevokePanelClosed
	RevokeIdle
	RevokeSessionUnmonitored
)

func (r RevokeReason) String() string {
	switch r {
	case RevokeSessionLocked:
		return "Windows was locked."
	case RevokeSessionDisconnected:
		return "The Windows session was disconnected."
	case RevokeSessionChanged:
		return "The Windows session changed user or connection."
	case RevokePanelClosed:
		return "The controls panel was closed."
	case RevokeIdle:
		return "Nobody used the controls panel for a while."
	case RevokeSessionUnmonitored:
		return "This PC cannot report Windows lock and user-switch events."
	}
	return ""
}

// StaffAuthority is implemented by backends whose staff authority is a lease
// granted from a browser. Every method must return promptly without I/O: the
// panel calls them from its native message loop.
type StaffAuthority interface {
	// StaffSessionMonitored reports whether lock/disconnect/user-switch
	// notifications are being received. Until true, a backend must grant no
	// staff authority: a lock it cannot see is a lock it cannot act on.
	StaffSessionMonitored(bool)
	// StaffInteraction records local input on the panel, resetting the idle
	// timeout. Background status refreshes are not interaction.
	StaffInteraction()
	// RevokeStaff drops staff authority and any pending staff sign-in now.
	RevokeStaff(RevokeReason)
}

// Windows WM_WTSSESSION_CHANGE codes (wtsapi32.h).
const (
	wtsConsoleConnect    = 0x1
	wtsConsoleDisconnect = 0x2
	wtsRemoteConnect     = 0x3
	wtsRemoteDisconnect  = 0x4
	wtsSessionLogoff     = 0x6
	wtsSessionLock       = 0x7
)

// SessionChangeRevocation maps a WM_WTSSESSION_CHANGE code to the revocation it
// causes. Every notification for this session revokes: fast user switching
// surfaces as a console disconnect/connect pair, an RDP takeover as a remote
// connect, and an unlock is only ever seen after the lock that already revoked.
// Treating an unrecognised code as a change fails closed. The bool reports
// whether the panel should also hide and drop its shortcuts.
func SessionChangeRevocation(code uintptr) (RevokeReason, bool) {
	switch code {
	case wtsSessionLock:
		return RevokeSessionLocked, true
	case wtsConsoleDisconnect, wtsRemoteDisconnect:
		return RevokeSessionDisconnected, true
	case wtsConsoleConnect, wtsRemoteConnect, wtsSessionLogoff:
		return RevokeSessionChanged, true
	}
	return RevokeSessionChanged, false
}
