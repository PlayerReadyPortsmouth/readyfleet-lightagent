//go:build !windows && !linux

package connectionprompt

import (
	"log/slog"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/consent"
)

// NewAsker has nothing to ask with on a platform the light agent
// does not ship for. Returning nil is deliberate and is not a gap:
// consent.Gate treats a nil Asker as OutcomeNoPrompter, which DECLINES and
// logs why.
//
// The alternative is what this build used to do — fall through to
// installerui's console stub, which printed to a stdout nobody was reading
// and returned true. A build that cannot ask must not answer.
func NewAsker(*slog.Logger) consent.Asker { return nil }
