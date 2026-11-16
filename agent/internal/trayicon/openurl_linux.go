//go:build linux

package trayicon

import (
	"fmt"
	"net/url"
	"os/exec"
)

// openerCommand is the desktop's standard "open this in whatever handles
// it" launcher, present on every desktop environment the BYOD bundle
// supports. A var so tests can point it at something that isn't a browser.
var openerCommand = "xdg-open"

// OpenURL hands a URL to the person's browser.
//
// It exists because Accept on a Linux consent prompt has to land
// somewhere: the prompt is the gate, but the authoritative approval is
// recorded on the devices page in the mentor's signed-in session. Until
// this build, OpenURL on every non-Windows platform returned nil without
// doing anything, so pressing Accept would have opened nothing at all.
//
// Same https-only guard as the Windows build: the URL arrives over the
// wire, and handing an arbitrary scheme to xdg-open means handing it to
// whatever handler the desktop has registered for that scheme.
//
// Started, not waited on: xdg-open forks a browser that outlives this call
// by hours. The reap goroutine exists so the agent, which runs for weeks,
// does not accumulate a zombie per opened link.
func OpenURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("trayicon: OpenURL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("trayicon: OpenURL: only https URLs are allowed, got %q", u.Scheme)
	}
	cmd := exec.Command(openerCommand, rawURL)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("trayicon: OpenURL: start %s: %w", openerCommand, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
