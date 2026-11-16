package venue

import "fmt"

type Mode string

const (
	APClubs Mode = "ap_clubs"
	VR      Mode = "vr"
)

type Preview struct {
	Mode     Mode     `json:"mode"`
	Steps    []string `json:"steps"`
	Blockers []string `json:"blockers"`
	CanApply bool     `json:"canApply"`
}

// Plan describes the agreed sequence without granting authority or executing it.
// An empty service list or stopped snapshot never proves complete startup-route
// ownership, account readiness, idle work or a Windows desktop restriction.
func Plan(mode Mode, v Snapshot) (Preview, error) {
	if mode != APClubs && mode != VR {
		return Preview{}, fmt.Errorf("unsupported venue mode")
	}
	p := Preview{Mode: mode, Steps: []string{
		"Verify staff authorisation and finish the active session before preparing a change.",
		"Hold the new desktop using a verified Windows restriction.",
		"Stop the reviewed mode-owned services and launchers; verify no respawn.",
	}}
	if mode == VR {
		p.Steps = append(p.Steps, "Start and verify the approved VR services and applications.")
	} else {
		p.Steps = append(p.Steps, "Keep VR services stopped; prepare AP / Clubs applications.")
	}
	p.Steps = append(p.Steps, "Release the desktop only after the selected mode is verified.")
	p.Blockers = []string{"Read-only preview: mode execution is not implemented.", "Staff PIN, account readiness, Windows restriction and startup manifest need the PC-1 trial."}
	if !v.Connected {
		p.Blockers = append(p.Blockers, "Controller connection has not been verified.")
	}
	if len(v.Services) == 0 {
		p.Blockers = append(p.Blockers, "No reviewed service names have been configured.")
	}
	for _, s := range v.Services {
		if s.State != "stopped" {
			p.Blockers = append(p.Blockers, s.Name+": "+s.State+"; stopped state not verified.")
		}
	}
	return p, nil
}
