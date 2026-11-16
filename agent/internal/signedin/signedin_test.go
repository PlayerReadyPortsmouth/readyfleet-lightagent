package signedin

import (
	"testing"
	"time"
)

func TestReport(t *testing.T) {
	at := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	if r := Report(func() (string, time.Time) { return "AP-student", at }); r.User != "AP-student" || r.Since == nil || !r.Since.Equal(at) {
		t.Fatalf("signed in: %+v", r)
	}
	if r := Report(func() (string, time.Time) { return "", at }); r == nil || r.User != "" || r.Since != nil {
		t.Fatalf("nobody signed in must report an empty user and no time: %+v", r)
	}
	if Report(nil) != nil {
		t.Fatal("no reader must report nothing")
	}
}

func TestParseLoginctl(t *testing.T) {
	out := "Name=ara\nActive=yes\nType=wayland\nRemote=no\nTimestamp=Fri 2026-10-23 08:15:00 UTC\n"
	u, at := parseSession(out)
	if u != "ara" || at.IsZero() {
		t.Fatalf("got %q %v", u, at)
	}
	if u, _ := parseSession("Name=ara\nActive=yes\nType=tty\nRemote=no\n"); u != "" {
		t.Fatalf("a text-mode session is not a graphical one: %q", u)
	}
	if u, _ := parseSession("Name=ara\nActive=no\nType=x11\nRemote=no\n"); u != "" {
		t.Fatalf("inactive session: %q", u)
	}
}
