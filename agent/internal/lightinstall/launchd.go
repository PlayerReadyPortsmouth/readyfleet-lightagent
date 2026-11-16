package lightinstall

import (
	"bytes"
	"encoding/xml"
	"path/filepath"
)

// The macOS layout and launchd job. Deliberately free of build tags and of any
// launchctl call so the rendering and the paths are unit-tested on every host;
// install_darwin.go is the only file that touches launchd itself.

// LaunchdLabel names the user LaunchAgent. It is also the plist's file stem.
const LaunchdLabel = "uk.co.playerready.readyfleet-light"

// DarwinPaths is the light install's on-disk layout for the user whose home
// directory is home: current-user, no root, nothing outside the home.
// The agent writes its own JSON log under Root/logs (polled by the installer
// to confirm a real connection); launchd's captured stdout/stderr go to
// ~/Library/Logs, where Console.app finds them.
func DarwinPaths(home string) Paths {
	root := filepath.Join(home, "Library", "Application Support", "ReadyFleet")
	return Paths{
		Root:       root,
		AgentExe:   filepath.Join(root, "readyfleet-light"),
		ConfigPath: filepath.Join(root, "config.json"),
		KeyPath:    filepath.Join(root, "client.key"),
		LogPath:    filepath.Join(root, "logs", "lightagent.log"),
	}
}

// LaunchAgentPlistPath is where the job definition lives for a user session.
func LaunchAgentPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

// LaunchdLogPath receives the agent's stdout and stderr (panics, early
// startup failures) before its own JSON logger is running.
func LaunchdLogPath(home string) string {
	return filepath.Join(home, "Library", "Logs", "ReadyFleet", "readyfleet-light.launchd.log")
}

// LaunchAgentPlist renders the job. It runs in the user's GUI session
// (RunAtLoad), and KeepAlive restarts it only after a failure: a clean exit,
// which is what `unlink` and a deliberate stop produce, must stay stopped.
// There is no network listener, no shell and no root: the agent only dials out.
func LaunchAgentPlist(agentPath, logPath string) string {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + LaunchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(agentPath) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + xmlText(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(logPath) + `</string>
</dict>
</plist>
`)
	return b.String()
}

func xmlText(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
