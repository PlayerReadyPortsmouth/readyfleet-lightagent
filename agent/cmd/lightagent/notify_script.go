package main

import "strings"

// Untagged so the quoting is tested on every host; only notify_darwin.go calls it.

// notificationScript builds the AppleScript. Both strings are quoted with
// backslash escapes, so a notice body cannot break out of its literal.
func notificationScript(title, body string) string {
	return "display notification " + appleScriptString(body) + " with title " + appleScriptString(title)
}

func appleScriptString(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\n`).Replace(s)
	return `"` + s + `"`
}
