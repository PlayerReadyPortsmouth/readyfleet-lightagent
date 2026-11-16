package staffmessage

import (
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestValidate(t *testing.T) {
	ok := proto.ShowNotificationArgs{Kind: "staff_message", Title: "Session ends soon", Body: "Please save your game.", Severity: "warning"}
	cases := []struct {
		name     string
		mutate   func(*proto.ShowNotificationArgs)
		wantCode string
	}{
		{"valid", func(*proto.ShowNotificationArgs) {}, ""},
		{"connection request refused", func(a *proto.ShowNotificationArgs) { a.Kind = "connection_request" }, proto.NotifyRefused},
		{"connection request refused even with bad text", func(a *proto.ShowNotificationArgs) { a.Kind = "connection_request"; a.Title = "" }, proto.NotifyRefused},
		{"any url refused", func(a *proto.ShowNotificationArgs) { a.URL = "https://readyapp.player-ready.co.uk/x" }, proto.NotifyRefused},
		{"non-http url refused", func(a *proto.ShowNotificationArgs) { a.URL = "file:///C:/Windows" }, proto.NotifyRefused},
		{"missing kind", func(a *proto.ShowNotificationArgs) { a.Kind = "" }, proto.NotifyInvalid},
		{"odd kind", func(a *proto.ShowNotificationArgs) { a.Kind = "staff message" }, proto.NotifyInvalid},
		{"missing title", func(a *proto.ShowNotificationArgs) { a.Title = "  " }, proto.NotifyInvalid},
		{"title only controls", func(a *proto.ShowNotificationArgs) { a.Title = "\x00\x07\u202e" }, proto.NotifyInvalid},
		{"missing body", func(a *proto.ShowNotificationArgs) { a.Body = "" }, proto.NotifyInvalid},
		{"title 80 runes ok", func(a *proto.ShowNotificationArgs) { a.Title = strings.Repeat("é", 80) }, ""},
		{"title 81 runes", func(a *proto.ShowNotificationArgs) { a.Title = strings.Repeat("é", 81) }, proto.NotifyInvalid},
		{"body 300 runes ok", func(a *proto.ShowNotificationArgs) { a.Body = strings.Repeat("a", 300) }, ""},
		{"body 301 runes", func(a *proto.ShowNotificationArgs) { a.Body = strings.Repeat("a", 301) }, proto.NotifyInvalid},
		{"huge raw body", func(a *proto.ShowNotificationArgs) { a.Body = strings.Repeat("\x00", 5000) }, proto.NotifyInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := ok
			tc.mutate(&args)
			_, f := Validate(args)
			got := ""
			if f != nil {
				got = f.Code
			}
			if got != tc.wantCode {
				t.Fatalf("code = %q (%v), want %q", got, f, tc.wantCode)
			}
		})
	}
}

func TestValidateSanitises(t *testing.T) {
	m, f := Validate(proto.ShowNotificationArgs{
		Kind:     "staff_message",
		Title:    "  Tea\tis\u202e ready\x1b[31m  ",
		Body:     "Line one\r\n\r\n\r\n\u200bLine\x00 two\u2028 \n\n",
		Severity: "catastrophic",
	})
	if f != nil {
		t.Fatal(f)
	}
	if m.Title != "Tea is ready[31m" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Body != "Line one\n\nLine two" {
		t.Errorf("body = %q", m.Body)
	}
	if m.Severity != SeverityInfo {
		t.Errorf("unknown severity = %q, want info", m.Severity)
	}
}

func TestRevalidateNeedsBrokerID(t *testing.T) {
	m := Message{ID: strings.Repeat("a", 32), Kind: "k", Title: "t", Body: "b"}
	if _, f := Revalidate(m); f != nil {
		t.Fatal(f)
	}
	for _, id := range []string{"", "xyz", strings.Repeat("A", 32), strings.Repeat("a", 33)} {
		m.ID = id
		if _, f := Revalidate(m); f == nil {
			t.Errorf("accepted id %q", id)
		}
	}
	m.ID = strings.Repeat("a", 32)
	m.Kind = ConnectionRequestKind
	if _, f := Revalidate(m); f == nil || f.Code != proto.NotifyRefused {
		t.Errorf("companion accepted a connection request: %v", f)
	}
}

func TestAckValidate(t *testing.T) {
	id := strings.Repeat("0", 32)
	good := []Ack{
		{ID: id, Displayed: true, Via: "banner"},
		{ID: id, Displayed: true, Via: "banner+tray"},
		{ID: id, Reason: ReasonLocked},
		{ID: id, Reason: ReasonDisplayFailed},
	}
	for _, a := range good {
		if err := a.Validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := []Ack{
		{ID: "nope", Displayed: true, Via: "banner"},
		{ID: id, Displayed: true, Via: "tray"}, // a balloon alone is not display
		{ID: id, Displayed: true},
		{ID: id, Displayed: true, Via: "banner", Reason: ReasonLocked},
		{ID: id},
		{ID: id, Reason: "whatever"},
		{ID: id, Via: "banner", Reason: ReasonLocked},
	}
	for _, a := range bad {
		if a.Validate() == nil {
			t.Errorf("accepted %+v", a)
		}
	}
}
