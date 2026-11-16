//go:build windows

package signedin

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modwtsapi32       = windows.NewLazySystemDLL("wtsapi32.dll")
	procWTSQueryEx    = modwtsapi32.NewProc("WTSQuerySessionInformationW")
	procWTSFreeMemory = modwtsapi32.NewProc("WTSFreeMemory")
)

// wtsSessionInfoEx is the WTS_INFO_CLASS value returning a WTSINFOEX.
const wtsSessionInfoEx = 25

// WTS_SESSIONSTATE_* from wtsapi32.h. Note the inversion: LOCK is 0.
//
// These values were famously reversed on Windows 7 and corrected in
// Windows 8; only the corrected meaning is honoured here, because
// SolarBeam's floor is Windows 10 2004 anyway (the indicator's own
// WDA_EXCLUDEFROMCAPTURE needs build 19041). Any other value is treated
// as "did not answer" rather than guessed at.
const (
	wtsSessionStateLock   = 0
	wtsSessionStateUnlock = 1
)

// wtsInfoExLevel1 mirrors WTSINFOEX_LEVEL1_W (wtsapi32.h). Only
// SessionFlags is read, but the whole struct must be laid out correctly
// for the field to be at the right offset: WINSTATIONNAME_LENGTH=32,
// USERNAME_LENGTH+1=21, DOMAIN_LENGTH+1=18, then the LARGE_INTEGERs,
// which Go aligns to 8 bytes exactly as MSVC does.
type wtsInfoExLevel1 struct {
	SessionID               uint32
	SessionState            int32
	SessionFlags            int32
	WinStationName          [32]uint16
	UserName                [21]uint16
	DomainName              [18]uint16
	LogonTime               int64
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	CurrentTime             int64
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
}

// wtsInfoEx mirrors WTSINFOEXW: a level discriminator followed by a union
// whose only defined member is level 1. The union is aligned to 8, so the
// payload does not start immediately after the uint32.
type wtsInfoEx struct {
	Level uint32
	_     uint32
	Data  wtsInfoExLevel1
}

// SessionLocked reads WTSINFOEX.SessionFlags for session (WTS_CURRENT_SESSION
// = 0xFFFFFFFF for the caller's own). ok is false when Windows did not answer.
// Lifted from internal/sessionindicator (light agent, own session) so the
// managed service can ask about the console session by id.
//
// ⚠ Not yet observed against a real Windows session (written on Linux,
// GOOS=windows go vet only); verify on a canary PC.
func SessionLocked(session uint32) (locked bool, ok bool) {
	var buf *uint16
	var size uint32
	r1, _, _ := procWTSQueryEx.Call(
		0, // WTS_CURRENT_SERVER_HANDLE
		uintptr(session),
		uintptr(wtsSessionInfoEx),
		uintptr(unsafe.Pointer(&buf)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 || buf == nil {
		return false, false
	}
	defer procWTSFreeMemory.Call(uintptr(unsafe.Pointer(buf)))

	if size < uint32(unsafe.Sizeof(wtsInfoEx{})) {
		return false, false
	}
	info := (*wtsInfoEx)(unsafe.Pointer(buf))
	if info.Level != 1 {
		return false, false
	}
	switch info.Data.SessionFlags {
	case wtsSessionStateLock:
		return true, true
	case wtsSessionStateUnlock:
		return false, true
	default:
		// WTS_SESSIONSTATE_UNKNOWN, or a value this build does not know.
		return false, false
	}
}

// ConsoleLocked reports the lock flag of the active console session.
func ConsoleLocked() (locked bool, ok bool) {
	id := windows.WTSGetActiveConsoleSessionId()
	if id == 0xFFFFFFFF || id == 0 {
		return false, false
	}
	return SessionLocked(id)
}
