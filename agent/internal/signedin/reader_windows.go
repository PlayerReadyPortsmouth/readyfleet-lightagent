//go:build windows

package signedin

import (
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wtsSessionInfo = 24 // WTS_INFO_CLASS WTSSessionInfo -> WTSINFOW
	wtsActive      = 0
)

var procWTSQuery = windows.NewLazySystemDLL("wtsapi32.dll").NewProc("WTSQuerySessionInformationW")

// wtsInfo mirrors WTSINFOW (wtsapi32.h); see agent/internal/endofday for the
// layout notes.
type wtsInfo struct {
	State                   int32
	SessionID               uint32
	IncomingBytes           uint32
	OutgoingBytes           uint32
	IncomingFrames          uint32
	OutgoingFrames          uint32
	IncomingCompressedBytes uint32
	OutgoingCompressedBytes uint32
	WinStationName          [32]uint16
	Domain                  [17]uint16
	UserName                [21]uint16
	ConnectTime             int64
	DisconnectTime          int64
	LastInputTime           int64
	LogonTime               int64
	CurrentTime             int64
}

func query(session uint32) (*wtsInfo, func()) {
	var buf *uint16
	var n uint32
	r1, _, _ := procWTSQuery.Call(0, uintptr(session), wtsSessionInfo,
		uintptr(unsafe.Pointer(&buf)), uintptr(unsafe.Pointer(&n)))
	if r1 == 0 || buf == nil {
		return nil, func() {}
	}
	free := func() { windows.WTSFreeMemory(uintptr(unsafe.Pointer(buf))) }
	if uintptr(n) < unsafe.Sizeof(wtsInfo{}) {
		free()
		return nil, func() {}
	}
	return (*wtsInfo)(unsafe.Pointer(buf)), free
}

// Read returns the account on the console session (or, failing that, the
// first Active session with a user) and its logon time. The sign-in screen
// has no user name, so it reads as nobody. A locked session still has its
// user, and is reported.
func Read() (string, time.Time) {
	candidates := []uint32{}
	if id := windows.WTSGetActiveConsoleSessionId(); id != 0xFFFFFFFF && id != 0 {
		candidates = append(candidates, id)
	}
	var infos *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &infos, &count); err == nil && infos != nil {
		for _, s := range unsafe.Slice(infos, int(count)) {
			if s.State == wtsActive && s.SessionID != 0 {
				candidates = append(candidates, s.SessionID)
			}
		}
		windows.WTSFreeMemory(uintptr(unsafe.Pointer(infos)))
	}
	for _, id := range candidates {
		info, free := query(id)
		if info == nil {
			continue
		}
		user := windows.UTF16ToString(info.UserName[:])
		logon := info.LogonTime
		free()
		if user == "" {
			continue
		}
		var since time.Time
		if logon > 0 {
			// FILETIME: 100ns ticks since 1601-01-01.
			since = time.Unix(0, (logon-116444736000000000)*100).UTC()
		}
		return user, since
	}
	return "", time.Time{}
}
