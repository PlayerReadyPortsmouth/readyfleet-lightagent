package main

import (
	"strings"
	"testing"
)

func TestConsentCopy_NamesThePlatformItRunsOn(t *testing.T) {
	all := func(c consentText) string { return c.intro + " " + strings.Join(c.bullets, " ") + " " + c.done }

	mac := all(consentCopy("darwin"))
	for _, wrong := range []string{"Windows account", "this PC", "host SolarBeam streaming sessions"} {
		if strings.Contains(mac, wrong) {
			t.Errorf("darwin copy still says %q", wrong)
		}
	}
	for _, want := range []string{"macOS user account", "this Mac", "menu bar", "declined"} {
		if !strings.Contains(mac, want) {
			t.Errorf("darwin copy lacks %q", want)
		}
	}

	win := all(consentCopy("windows"))
	if !strings.Contains(win, "Windows account") || strings.Contains(win, "Mac") {
		t.Errorf("windows copy wrong: %s", win)
	}
	lin := all(consentCopy("linux"))
	if strings.Contains(lin, "Windows") || strings.Contains(lin, "Mac") {
		t.Errorf("linux copy names another platform: %s", lin)
	}
	// Every platform keeps the same hard promises.
	for _, c := range []string{mac, win, lin} {
		if !strings.Contains(c, "Never: requests admin rights") || !strings.Contains(c, "encrypted connection") {
			t.Errorf("missing a shared promise: %s", c)
		}
	}
}
