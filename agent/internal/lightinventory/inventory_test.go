package lightinventory

import (
	"context"
	"encoding/json"
	"testing"
)

// TestLightInventoryData_OnlyDeclaredFieldsCanEverAppear is the
// privacy-shape test the design spec calls for: LightInventoryData has no
// hostname/username/network/installed_software fields to begin with, so
// this proves the JSON wire shape is exactly the six declared keys — no
// personal field can silently reappear through a future edit without this
// test catching it.
func TestLightInventoryData_OnlyDeclaredFieldsCanEverAppear(t *testing.T) {
	data, err := Collect(context.Background(), "1.2.3", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]json.RawMessage
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	allowed := map[string]bool{
		"os_family": true, "os_major_version": true, "architecture": true,
		"agent_version": true, "solarbeam_version": true, "solarbeam_status": true,
	}
	for key := range asMap {
		if !allowed[key] {
			t.Errorf("LightInventoryData JSON contains undeclared key %q", key)
		}
	}
	forbidden := []string{"hostname", "username", "network", "installed_software", "mac", "ip", "fingerprint"}
	for _, f := range forbidden {
		if _, ok := asMap[f]; ok {
			t.Errorf("LightInventoryData JSON contains forbidden key %q", f)
		}
	}
}

func TestCollect_PopulatesAgentVersionAndPlatformFields(t *testing.T) {
	data, err := Collect(context.Background(), "9.9.9", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if data.AgentVersion != "9.9.9" {
		t.Errorf("AgentVersion = %q, want %q", data.AgentVersion, "9.9.9")
	}
	if data.Architecture == "" {
		t.Error("Architecture should never be empty")
	}
	if data.OSFamily == "" {
		t.Error("OSFamily should never be empty")
	}
}

func TestCollect_NilManagerLeavesSolarbeamFieldsEmpty(t *testing.T) {
	data, err := Collect(context.Background(), "1.0.0", nil)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if data.SolarbeamVersion != "" || data.SolarbeamStatus != "" {
		t.Errorf("expected empty SolarBeam fields with a nil manager, got version=%q status=%q",
			data.SolarbeamVersion, data.SolarbeamStatus)
	}
}
