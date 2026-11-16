package config

import (
	"encoding/json"
	"testing"
)

func TestVenueCompanionAbsentPreservesAutomaticPolicy(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"server_url":"wss://example.invalid","machine_id":"existing"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.VenueCompanion != nil {
		t.Fatal("old config acquired an explicit companion override")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["venue_companion"]; ok {
		t.Fatal("save introduced companion config")
	}
}

func TestVenueCompanionConfigRoundTrip(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"venue_companion":{"enabled":true,"cyp_sid":"S-1-5-21-100-200-300-1001","service_names":["ReviewedService"],"mode_switch_icon":true}}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.VenueCompanion == nil || !c.VenueCompanion.Enabled || len(c.VenueCompanion.ServiceNames) != 1 || !c.VenueCompanion.ModeSwitchIcon {
		t.Fatal("opt-in lost")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var restored Config
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.VenueCompanion.CYPSID != c.VenueCompanion.CYPSID || !restored.VenueCompanion.ModeSwitchIcon {
		t.Fatal("identity changed")
	}
}
