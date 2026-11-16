package lightinstall

import (
	"strings"
	"testing"
)

func TestDarwinPathsStayUnderHome(t *testing.T) {
	p := DarwinPaths("/Users/aurora")
	root := "/Users/aurora/Library/Application Support/ReadyFleet"
	want := Paths{
		Root:       root,
		AgentExe:   root + "/readyfleet-light",
		ConfigPath: root + "/config.json",
		KeyPath:    root + "/client.key",
		LogPath:    root + "/logs/lightagent.log",
	}
	if p != want {
		t.Fatalf("paths = %+v, want %+v", p, want)
	}
	if got := LaunchAgentPlistPath("/Users/aurora"); got != "/Users/aurora/Library/LaunchAgents/uk.co.playerready.readyfleet-light.plist" {
		t.Fatalf("plist path = %s", got)
	}
	if got := LaunchdLogPath("/Users/aurora"); got != "/Users/aurora/Library/Logs/ReadyFleet/readyfleet-light.launchd.log" {
		t.Fatalf("log path = %s", got)
	}
}

func TestLaunchAgentPlistRendersJobAndEscapesPaths(t *testing.T) {
	plist := LaunchAgentPlist("/Users/a&b/Library/Application Support/ReadyFleet/readyfleet-light", "/Users/a&b/Library/Logs/x.log")
	for _, want := range []string{
		"<string>uk.co.playerready.readyfleet-light</string>",
		"<array>\n\t\t<string>/Users/a&amp;b/Library/Application Support/ReadyFleet/readyfleet-light</string>\n\t</array>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>StandardOutPath</key>\n\t<string>/Users/a&amp;b/Library/Logs/x.log</string>",
		"<key>StandardErrorPath</key>\n\t<string>/Users/a&amp;b/Library/Logs/x.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	// No root job, no shell, no listener.
	for _, banned := range []string{"UserName", "Sockets", "/bin/sh", "/bin/bash"} {
		if strings.Contains(plist, banned) {
			t.Errorf("plist must not contain %q", banned)
		}
	}
}
