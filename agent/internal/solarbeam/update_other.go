//go:build !windows && !linux && !darwin

package solarbeam

import (
	"context"
	"errors"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// applyEngineUpdate is unsupported on non-Windows hosts: the engine only
// runs on Windows. Tests inject applyUpdateFn, so this stub is only hit if
// a real update reaches a dev host.
func applyEngineUpdate(_ context.Context, _ Paths, _ proto.SolarbeamEngineUpdateArgs) error {
	return errors.New("solarbeam engine update is only supported on Windows")
}
