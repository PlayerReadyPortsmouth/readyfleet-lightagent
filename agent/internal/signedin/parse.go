package signedin

import (
	"strings"
	"time"
)

// parseSession reads `loginctl show-session` key=value output. It returns a
// user only for an active, local, graphical session.
func parseSession(out string) (string, time.Time) {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
			kv[k] = v
		}
	}
	if kv["Active"] != "yes" || kv["Remote"] == "yes" || (kv["Type"] != "x11" && kv["Type"] != "wayland") {
		return "", time.Time{}
	}
	at, _ := time.Parse("Mon 2006-01-02 15:04:05 MST", kv["Timestamp"])
	return kv["Name"], at
}
