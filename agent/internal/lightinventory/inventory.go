// Package lightinventory collects the six-field, privacy-safe snapshot a
// byod_solarbeam-profiled agent reports — proto.LightInventoryData, and
// nothing else. It deliberately does NOT import agent/internal/inventory
// (the managed agent's WMI/hardware/network/installed-software collectors)
// — that package can read a personal PC's hostname, MAC addresses, and
// installed-software list, none of which may ever leave a BYOD device. See
// docs/superpowers/specs/2026-07-26-all-venue-rollout-and-byod-light-agent-design.md
// §7.1/§7.4.
package lightinventory

import (
	"context"
	"runtime"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/solarbeam"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// Collect returns the minimal inventory snapshot: OS family/major
// version/architecture, this agent's own version, and — best-effort, via
// manager.Status — the local SolarBeam engine's version/running state. It
// never fails on the engine status being unknown; only a hard OS-detection
// error is returned.
func Collect(ctx context.Context, agentVersion string, manager *solarbeam.Manager) (proto.LightInventoryData, error) {
	majorVersion, err := osMajorVersion()
	if err != nil {
		return proto.LightInventoryData{}, err
	}

	data := proto.LightInventoryData{
		OSFamily:       osFamily,
		OSMajorVersion: majorVersion,
		Architecture:   runtime.GOARCH,
		AgentVersion:   agentVersion,
	}

	if manager != nil {
		status := manager.Status(ctx)
		data.SolarbeamVersion = status.EngineVersion
		if status.Running {
			data.SolarbeamStatus = "running"
		} else {
			data.SolarbeamStatus = "stopped"
		}
	}

	return data, nil
}
