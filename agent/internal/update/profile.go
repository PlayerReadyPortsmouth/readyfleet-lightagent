// Package update owns agent self-update: single-flight download+verify,
// a per-entrypoint cross-profile ProductPolicy guard, and delegating the
// actual binary swap to a caller-supplied ApplyFn (the swap/restart
// mechanism differs by entrypoint — a Windows service for the managed
// agent, a Task-Scheduler-launched process for the lite agent — so it
// can't live here).
//
// This replaced agent/internal/exec's UpdateManager: exec is off-limits to
// the lite entrypoint (agent/cmd/lightagent must not import it), and
// self-update is a capability both entrypoints need, so it moved out from
// under exec rather than being duplicated.
package update

import (
	"fmt"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// ProductPolicy is the entrypoint's own fixed, local answer to "which
// update am I willing to accept" — constructed once at startup from a
// hardcoded value, NEVER from server-supplied config. This is the guard
// that closes a real gap: the old exec.UpdateManager trusted whatever
// SignerFingerprint the server sent and used it directly as the
// Authenticode-expected value, with no local allowlist restricting which
// fingerprints were ever acceptable. A compromised or simply confused
// controller could not previously be distinguished, at the agent, from one
// correctly offering this exact binary's own release.
type ProductPolicy struct {
	// ProductID is this entrypoint's own product identifier (e.g.
	// "readyfleet-agent" or "readyfleet-light-agent").
	ProductID string
	// Profile is this entrypoint's own agent profile (e.g. "managed" or
	// "byod_solarbeam").
	Profile string
	// Signers is the set of Authenticode signer fingerprints (lowercase
	// hex) this entrypoint will ever trust for its own updates.
	Signers map[string]struct{}
}

// Validate rejects args unless its ProductID and AgentProfile match p
// exactly and its SignerFingerprint is in p.Signers. It does not check
// SHA256/hex format or download reachability — Manager.validate does that
// separately, before calling this.
func (p ProductPolicy) Validate(args proto.UpdateArgs) error {
	if args.ProductID != p.ProductID {
		return fmt.Errorf("update policy: product_id %q not accepted for profile %q", args.ProductID, p.Profile)
	}
	if args.AgentProfile != p.Profile {
		return fmt.Errorf("update policy: agent_profile %q does not match %q", args.AgentProfile, p.Profile)
	}
	if _, ok := p.Signers[args.SignerFingerprint]; !ok {
		return fmt.Errorf("update policy: signer_fingerprint is not in the trusted set for %q", p.Profile)
	}
	return nil
}
