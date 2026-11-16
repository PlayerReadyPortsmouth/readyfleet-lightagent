package companionui

import "testing"

func TestEverySessionChangeRevokesStaffAuthority(t *testing.T) {
	cases := []struct {
		code   uintptr
		reason RevokeReason
		hide   bool
	}{
		{0x1, RevokeSessionChanged, true},      // console connect (user switch back)
		{0x2, RevokeSessionDisconnected, true}, // console disconnect (fast user switch)
		{0x3, RevokeSessionChanged, true},      // remote connect
		{0x4, RevokeSessionDisconnected, true}, // remote disconnect
		{0x5, RevokeSessionChanged, false},     // logon
		{0x6, RevokeSessionChanged, true},      // logoff
		{0x7, RevokeSessionLocked, true},       // lock
		{0x8, RevokeSessionChanged, false},     // unlock
		{0x9, RevokeSessionChanged, false},     // remote control
		{0xff, RevokeSessionChanged, false},    // unrecognised
	}
	for _, c := range cases {
		reason, hide := SessionChangeRevocation(c.code)
		if reason != c.reason || hide != c.hide {
			t.Fatalf("code %#x = (%v, %v), want (%v, %v)", c.code, reason, hide, c.reason, c.hide)
		}
		if reason == NotRevoked || reason.String() == "" {
			t.Fatalf("code %#x does not revoke with a shown reason", c.code)
		}
	}
}
