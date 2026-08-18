package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfigUnmarshal_AcceptsNewTag(t *testing.T) {
	in := `{"server_url":"ws://x","enrollment_secret":"rft_abc"}`
	var c Config
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.EnrollmentSecret != "rft_abc" {
		t.Errorf("EnrollmentSecret: got %q want %q", c.EnrollmentSecret, "rft_abc")
	}
}

func TestConfigUnmarshal_AcceptsLegacyTag(t *testing.T) {
	in := `{"server_url":"ws://x","venue_api_key":"rfk_legacy"}`
	var c Config
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.EnrollmentSecret != "rfk_legacy" {
		t.Errorf("EnrollmentSecret: got %q want %q (legacy tag should populate the new field)",
			c.EnrollmentSecret, "rfk_legacy")
	}
}

func TestConfigUnmarshal_NewTagWinsWhenBothPresent(t *testing.T) {
	in := `{"enrollment_secret":"rft_new","venue_api_key":"rfk_old"}`
	var c Config
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.EnrollmentSecret != "rft_new" {
		t.Errorf("EnrollmentSecret: got %q want %q", c.EnrollmentSecret, "rft_new")
	}
}

func TestConfigMarshal_EmitsNewTagOnly(t *testing.T) {
	c := Config{ServerURL: "ws://x", EnrollmentSecret: "rft_new"}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(b)
	if !strings.Contains(out, `"enrollment_secret":"rft_new"`) {
		t.Errorf("expected enrollment_secret in output, got: %s", out)
	}
	if strings.Contains(out, "venue_api_key") {
		t.Errorf("legacy venue_api_key should not appear in marshalled output, got: %s", out)
	}
}

func TestConfigUnmarshal_RoundTrip(t *testing.T) {
	original := Config{
		ServerURL:        "wss://example.com:8443/agent/v1",
		EnrollmentSecret: "rft_abcdef",
		IntendedLabel:    "pilot-01",
		MachineID:        "550e8400-e29b-41d4-a716-446655440000",
		VenueID:          "660e8400-e29b-41d4-a716-446655440001",
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != original {
		t.Errorf("round-trip mismatch:\n got:  %+v\n want: %+v", got, original)
	}
}
