//go:build darwin

package trayicon

import "testing"

func TestOpenURL_Darwin_RefusesNonHTTPS(t *testing.T) {
	old := openerCommand
	openerCommand = "/usr/bin/false" // never reached; guards the test if the scheme check regresses
	defer func() { openerCommand = old }()
	for _, raw := range []string{"http://readyapp.player-ready.co.uk", "file:///etc/passwd", "javascript:alert(1)", "://bad"} {
		if err := OpenURL(raw); err == nil {
			t.Errorf("%q: opened", raw)
		}
	}
}
