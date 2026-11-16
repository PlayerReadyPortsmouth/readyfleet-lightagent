//go:build darwin

package trayicon

import (
	"fmt"
	"net/url"
	"os/exec"
)

// openerCommand is LaunchServices' command-line front: it hands the URL to
// the person's default browser. A var so tests can point it elsewhere.
var openerCommand = "open"

// OpenURL hands a URL to the person's browser.
//
// Until the menu-bar helper, OpenURL on darwin was the `_other` no-op: the
// agent never prompted on a Mac, so nothing ever called it with a URL a
// person was waiting on. A menu-bar Approve changes that — the prompt is
// the gate, but the approval is recorded on the devices page in the
// person's signed-in session, so Approve has to land there.
//
// Same https-only guard as the Windows and Linux builds: the URL arrives
// over the wire, and `open` would hand any other scheme to whatever app
// claims it.
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
