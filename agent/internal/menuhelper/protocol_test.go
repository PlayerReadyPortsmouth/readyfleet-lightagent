package menuhelper

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestEncodeDecode_RoundTripsEveryType(t *testing.T) {
	at := time.Date(2026, 11, 8, 21, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		in   any
	}{
		{"status", Status{Online: true, Venue: "COR", DeviceName: "Aurora's Mac", SessionLive: true, AgentVersion: "0.1.0"}},
		{"prompt", Prompt{ID: "p1", Requester: "Jo Bloggs wants to connect to your device", Detail: "BYOD-693971", ExpiresAt: at}},
		{"cancel", Cancel{ID: "p1"}},
		{"hello", Hello{Version: "0.1.0"}},
		{"decision", Decision{ID: "p1", Approve: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := Encode(tc.in)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
				t.Fatalf("not exactly one line: %q", line)
			}
			out, err := Decode(line)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			// The type tag is stamped by Encode; compare with it filled in.
			switch want := tc.in.(type) {
			case Status:
				want.Type = TypeStatus
				if out != want {
					t.Errorf("got %+v want %+v", out, want)
				}
			case Prompt:
				want.Type = TypePrompt
				got, ok := out.(Prompt)
				if !ok || got.ID != want.ID || got.Requester != want.Requester || got.Detail != want.Detail || !got.ExpiresAt.Equal(want.ExpiresAt) || got.Type != TypePrompt {
					t.Errorf("got %+v want %+v", out, want)
				}
			case Cancel:
				want.Type = TypeCancel
				if out != want {
					t.Errorf("got %+v want %+v", out, want)
				}
			case Hello:
				want.Type = TypeHello
				if out != want {
					t.Errorf("got %+v want %+v", out, want)
				}
			case Decision:
				want.Type = TypeDecision
				if out != want {
					t.Errorf("got %+v want %+v", out, want)
				}
			}
		})
	}
}

func TestEncode_StampsTheTypeAndRefusesStrangers(t *testing.T) {
	line, err := Encode(Status{Type: "wrong"})
	if err != nil || !bytes.Contains(line, []byte(`"type":"status"`)) {
		t.Fatalf("type not stamped: %q %v", line, err)
	}
	if _, err := Encode(struct{ X int }{1}); err == nil {
		t.Fatal("encoded a non-wire type")
	}
}

func TestEncode_EscapesLineBreaksInText(t *testing.T) {
	line, err := Encode(Prompt{ID: "p", Requester: "line one\nline two\r\n"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(line, []byte("\n")) != 1 || bytes.Contains(line, []byte("\r")) {
		t.Fatalf("text broke the line discipline: %q", line)
	}
}

func TestDecode_FromTheHelperAsWritten(t *testing.T) {
	// Lines as the Swift helper writes them, including a field this build
	// does not know, which must be ignored rather than rejected.
	for _, line := range []string{
		`{"type":"decision","id":"abc","approve":false,"pressed_at":"2026-11-08T21:00:00Z"}`,
		"  {\"type\":\"decision\",\"id\":\"abc\",\"approve\":false}\r\n",
	} {
		out, err := Decode([]byte(line))
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		d, ok := out.(Decision)
		if !ok || d.ID != "abc" || d.Approve {
			t.Errorf("%q: got %+v", line, out)
		}
	}
	if out, err := Decode([]byte(`{"type":"hello"}`)); err != nil || out != (Hello{Type: TypeHello}) {
		t.Errorf("hello: %+v %v", out, err)
	}
}

func TestDecode_RejectsWhatItCannotActOn(t *testing.T) {
	for name, line := range map[string]string{
		"empty":            "",
		"not json":         "hello",
		"decision no id":   `{"type":"decision","approve":true}`,
		"prompt no id":     `{"type":"prompt","requester":"x"}`,
		"approve not bool": `{"type":"decision","id":"a","approve":"yes"}`,
	} {
		if _, err := Decode([]byte(line)); err == nil {
			t.Errorf("%s: accepted %q", name, line)
		}
	}
	_, err := Decode([]byte(`{"type":"dance","id":"a"}`))
	var unknown *ErrUnknownType
	if !errors.As(err, &unknown) || unknown.Type != "dance" {
		t.Fatalf("unknown type not reported as such: %v", err)
	}
}
