package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// The hello-triggered inventory must survive the C2's strict BYOD decode
// (DisallowUnknownFields into proto.LightInventoryData). Regression for the
// empty proto.InventoryData that carried "hostname" and was rejected.
func TestLightInventoryProviderMatchesStrictBYODDecode(t *testing.T) {
	original := collectLightInventoryFn
	t.Cleanup(func() { collectLightInventoryFn = original })
	var gotVersion string
	collectLightInventoryFn = func(_ context.Context, agentVersion string, _ *solarbeam.Manager) (proto.LightInventoryData, error) {
		gotVersion = agentVersion
		return proto.LightInventoryData{
			OSFamily: "darwin", OSMajorVersion: "27", Architecture: "arm64", AgentVersion: agentVersion,
			SolarbeamHost: &proto.SolarbeamHostReport{Enabled: true, Available: true},
		}, nil
	}

	snapshot, err := lightInventoryProvider("0.3.1", nil)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotVersion != "0.3.1" {
		t.Fatalf("agent version passed to the collector = %q, want 0.3.1", gotVersion)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["hostname"]; ok {
		t.Fatalf("hello inventory carries a hostname field: %s", raw)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var light proto.LightInventoryData
	if err := dec.Decode(&light); err != nil {
		t.Fatalf("C2 strict decode would reject the hello inventory: %v (%s)", err, raw)
	}
	if light.SolarbeamHost == nil || !light.SolarbeamHost.Available {
		t.Fatalf("SolarBeam host report lost in transit: %+v", light)
	}
}
