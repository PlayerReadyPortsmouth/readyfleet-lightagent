package main

import "testing"

func TestNotificationScriptEscapesQuotesBackslashesAndNewlines(t *testing.T) {
	got := notificationScript(`Say "hi"`, "a\\b\nc")
	want := `display notification "a\\b\nc" with title "Say \"hi\""`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}
