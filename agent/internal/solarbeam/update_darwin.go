//go:build darwin

package solarbeam

import (
	"context"
	"errors"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// applyEngineUpdate is deliberately unsupported on macOS for now: the engine
// is a hand-installed, locally signed .app (docs/macos-light-agent.md,
// "SolarBeam host"), there is no darwin ReleaseArtifact in the C2, and a
// downloaded bundle would need a Developer ID + notarisation story before it
// could be trusted (plan decision D7). A solarbeam_engine_update aimed at a
// Mac fails with this message instead of pretending.
func applyEngineUpdate(_ context.Context, _ Paths, _ proto.SolarbeamEngineUpdateArgs) error {
	return errors.New("solarbeam engine update is not available on macOS: install the engine app by hand (no darwin update channel yet)")
}
