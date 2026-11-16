//go:build linux

package signedin

import (
	"os/exec"
	"strings"
	"time"
)

// Read returns the active graphical (x11/wayland) local session's user, or
// "" when there is none.
func Read() (string, time.Time) {
	out, err := exec.Command("loginctl", "list-sessions", "--no-legend").Output()
	if err != nil {
		return "", time.Time{}
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		show, err := exec.Command("loginctl", "show-session", f[0],
			"-p", "Name", "-p", "Active", "-p", "Type", "-p", "Remote", "-p", "Timestamp").Output()
		if err != nil {
			continue
		}
		if u, at := parseSession(string(show)); u != "" {
			return u, at
		}
	}
	return "", time.Time{}
}
