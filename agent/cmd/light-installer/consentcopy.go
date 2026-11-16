package main

// consentText is what the welcome screen tells the person before anything is
// installed. It is the one place the installer describes what the agent will
// and will not do, so it has to be true for the platform it is running on.
type consentText struct {
	intro   string
	bullets []string
	done    string
}

// consentCopy picks the wording for goos. The Windows and Linux light agents
// host SolarBeam sessions; the macOS one is watch-only (docs/macos-light-agent.md)
// and must not promise streaming it cannot do, and a Mac has no "Windows
// account" to run under (PR #201 review, point 3).
func consentCopy(goos string) consentText {
	switch goos {
	case "darwin":
		return consentText{
			intro: "This installs a small background helper that connects this Mac to Player Ready's ReadyFleet, so it shows up as your device. " +
				"It runs only under your own macOS user account — nothing changes for any other user of this Mac.",
			bullets: []string{
				"Shows this Mac as online or offline in ReadyFleet, and a ReadyFleet item in your menu bar",
				"Runs automatically when you log in, under your account only",
				"Connects only to ReadyFleet's servers, over an encrypted connection",
				"Does not yet host SolarBeam sessions on a Mac; connection requests are declined unless you turn prompts on",
				"Never: requests admin rights, opens a remote shell, or runs arbitrary code from the server",
			},
			done: "The ReadyFleet light agent is installed, running, and connected. Look for the ReadyFleet item in your menu bar.",
		}
	case "linux":
		return consentText{
			intro: "This installs a small background helper so this computer can host SolarBeam streaming sessions for Player Ready. " +
				"It runs only under your own user account — nothing changes for any other user of this computer.",
			bullets: []string{
				"Host SolarBeam streaming sessions when you start one",
				"Runs automatically when you log in, under your account only",
				"Connects only to ReadyFleet's servers, over an encrypted connection",
				"Never: requests admin rights, opens a remote shell, or runs arbitrary code from the server",
			},
			done: "The ReadyFleet Light Agent is installed, running, and connected. You can start hosting SolarBeam sessions from this computer now.",
		}
	default:
		return consentText{
			intro: "This installs a small background helper so this PC can host SolarBeam streaming sessions for Player Ready. " +
				"It runs only under your own Windows account — nothing changes for any other user of this PC.",
			bullets: []string{
				"Host SolarBeam streaming sessions when you start one",
				"Runs automatically when you log in, under your account only",
				"Connects only to ReadyFleet's servers, over an encrypted connection",
				"Never: requests admin rights, opens a remote shell, or runs arbitrary code from the server",
			},
			done: "The ReadyFleet Light Agent is installed, running, and connected. You can start hosting SolarBeam sessions from this PC now.",
		}
	}
}
