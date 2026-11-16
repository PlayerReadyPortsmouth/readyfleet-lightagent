package config

import (
	"encoding/json"
	"path/filepath"
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

// The warm-engine kill switch is the one lever an operator has to revert a
// single venue PC without a redeploy, so all three states must behave
// exactly as documented — and in particular an ABSENT key must mean "on",
// not Go's zero value.
func TestWarmEngineEnabled_AbsentKeyMeansOn(t *testing.T) {
	tests := []struct {
		name string
		json string
		want bool
	}{
		{"absent", `{"server_url":"wss://x/agent/v1"}`, true},
		{"explicitly true", `{"server_url":"wss://x/agent/v1","warm_engine":true}`, true},
		{"explicitly false", `{"server_url":"wss://x/agent/v1","warm_engine":false}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c Config
			if err := json.Unmarshal([]byte(tc.json), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := c.WarmEngineEnabled(); got != tc.want {
				t.Fatalf("WarmEngineEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The owner identity is what the consent gate compares a connection
// request's requester against. If it does not survive a save/load it is
// silently absent, and an absent owner means the machine's own owner gets
// prompted for access to their own PC — a behaviour regression that
// nothing else would catch.
func TestOwnerActorID_SurvivesASaveRoundTrip(t *testing.T) {
	b, err := json.Marshal(Config{ServerURL: "wss://x/agent/v1", OwnerActorID: "mentor-oid"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"owner_actor_id":"mentor-oid"`) {
		t.Fatalf("owner_actor_id was dropped on marshal, got: %s", b)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OwnerActorID != "mentor-oid" {
		t.Fatalf("OwnerActorID = %q after a round trip", got.OwnerActorID)
	}
}

// A config written before the field existed must load with no owner, not
// fail — and no owner means prompt everybody.
func TestOwnerActorID_AbsentOnALegacyConfig(t *testing.T) {
	var got Config
	if err := json.Unmarshal([]byte(`{"server_url":"wss://x/agent/v1","machine_id":"m1"}`), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.OwnerActorID != "" {
		t.Fatalf("OwnerActorID = %q, want empty for a config that never carried one", got.OwnerActorID)
	}
}

// Enrollment rewrites config.json via Save, so a machine reverted with
// warm_engine:false must not silently drift back to warm. `omitempty` on a
// *bool omits only nil, never a pointer to false — this pins that.
func TestWarmEngineFalse_SurvivesASaveRoundTrip(t *testing.T) {
	off := false
	b, err := json.Marshal(Config{ServerURL: "wss://x/agent/v1", WarmEngine: &off})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"warm_engine":false`) {
		t.Fatalf("warm_engine:false was dropped on marshal, got: %s", b)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.WarmEngineEnabled() {
		t.Fatal("warm_engine:false must survive a save/load round trip")
	}
}

func TestSessionIndicatorDefaultsOff(t *testing.T) {
	// The whole Linux path is unsafe until an engine honouring
	// capture_reserve_top has reached the machine, so an absent key must
	// mean OFF, never "on because that seems useful".
	var c Config
	if c.SessionIndicatorEnabled() {
		t.Error("SessionIndicatorEnabled() is true with the key absent")
	}
	if c.SessionIndicatorRequired() {
		t.Error("SessionIndicatorRequired() is true with the key absent")
	}
}

func TestSessionIndicatorSwitchesRoundTripThroughJSON(t *testing.T) {
	tests := []struct {
		name              string
		json              string
		enabled, required bool
	}{
		{"both absent", `{}`, false, false},
		{"enabled only", `{"session_indicator":true}`, true, false},
		{"enabled and required", `{"session_indicator":true,"require_session_indicator":true}`, true, true},
		{"explicitly disabled", `{"session_indicator":false}`, false, false},
		{"required without enabled is inert", `{"require_session_indicator":true}`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Config
			if err := json.Unmarshal([]byte(tt.json), &c); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if c.SessionIndicatorEnabled() != tt.enabled {
				t.Errorf("enabled = %v, want %v", c.SessionIndicatorEnabled(), tt.enabled)
			}
			if c.SessionIndicatorRequired() != tt.required {
				t.Errorf("required = %v, want %v", c.SessionIndicatorRequired(), tt.required)
			}
		})
	}
}

func TestLinkAuthModeSurvivesSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Save(path, Config{SolarbeamLinkAuthMode: "enforce"}); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil || got.SolarbeamLinkAuthMode != "enforce" {
		t.Fatal("link authentication mode lost across save", err)
	}
}

func TestMenuPromptsDefaultsOff(t *testing.T) {
	// A Mac with no key stays decline-all; the menu-bar prompt is opt-in
	// per machine until Aurora decides ask-the-owner is the policy.
	for name, tc := range map[string]struct {
		json string
		want bool
	}{
		"absent":   {`{}`, false},
		"false":    {`{"menu_prompts":false}`, false},
		"true":     {`{"menu_prompts":true}`, true},
		"not bool": {`{"menu_prompts":"yes"}`, false},
	} {
		var c Config
		err := json.Unmarshal([]byte(tc.json), &c)
		if name == "not bool" {
			if err == nil {
				t.Errorf("%s: a non-boolean was accepted", name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := c.MenuPromptsEnabled(); got != tc.want {
			t.Errorf("%s: MenuPromptsEnabled() = %v, want %v", name, got, tc.want)
		}
	}
}

func TestSolarbeamHostDefaultsOffAndRoundTrips(t *testing.T) {
	var c Config
	if c.SolarbeamHostEnabled() {
		t.Fatal("solarbeam_host absent must be off")
	}
	if err := json.Unmarshal([]byte(`{"server_url":"wss://x","solarbeam_host":false}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.SolarbeamHostEnabled() {
		t.Fatal("explicit false must be off")
	}
	in := `{"server_url":"wss://x","solarbeam_host":true,"solarbeam_dir":"/d","solarbeam_engine_app":"/d/E.app","solarbeam_bridge":"/d/b"}`
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	if !c.SolarbeamHostEnabled() || c.SolarbeamDir != "/d" || c.SolarbeamEngineApp != "/d/E.app" || c.SolarbeamBridge != "/d/b" {
		t.Fatalf("config = %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"solarbeam_host":true`, `"solarbeam_dir":"/d"`, `"solarbeam_engine_app":"/d/E.app"`, `"solarbeam_bridge":"/d/b"`} {
		if !strings.Contains(string(out), key) {
			t.Errorf("marshal lost %s: %s", key, out)
		}
	}
	// A watch-only config emits none of the keys.
	out, _ = json.Marshal(Config{ServerURL: "wss://x"})
	if strings.Contains(string(out), "solarbeam_host") {
		t.Errorf("absent key emitted: %s", out)
	}
}
