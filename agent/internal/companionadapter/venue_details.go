package companionadapter

import (
	"fmt"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/venue"
	"strings"
)

// Explicit display allowlist: opaque connection keys, session identities,
// authority revisions and raw error/diagnostic payloads are never rendered.
func VenueDetails(v venue.Snapshot) string {
	connection := "Disconnected"
	if v.Connected {
		connection = "Connected"
	}
	viewers := "Unknown"
	if v.Remote.Known {
		names := make([]string, 0, len(v.Remote.Viewers))
		for _, viewer := range v.Remote.Viewers {
			names = append(names, viewer.Name)
		}
		viewers = fmt.Sprintf("%d", v.Remote.Count)
		if len(names) > 0 {
			viewers += " · " + strings.Join(names, ", ")
		}
	}
	return fmt.Sprintf("Fleet connection: %s\nAgent: %s\nSolarBeam: %s\nSolarBeam update: %s\nConnected viewers: %s\nMode: %s", connection, v.Agent.Installed, v.SolarBeam.Installed, v.SolarBeam.UpdateState, viewers, v.ModeState)
}
