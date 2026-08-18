package update

import (
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	managedSigner = "5b2e7596208e78bfd59d6e8d08844d2e3760614540040ca7f48e2dcb698be027"
	lightSigner   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

func managedPolicy() ProductPolicy {
	return ProductPolicy{
		ProductID: "readyfleet-agent",
		Profile:   "managed",
		Signers:   map[string]struct{}{managedSigner: {}},
	}
}

func lightPolicy() ProductPolicy {
	return ProductPolicy{
		ProductID: "readyfleet-light-agent",
		Profile:   "byod_solarbeam",
		Signers:   map[string]struct{}{lightSigner: {}},
	}
}

func validManagedArgs() proto.UpdateArgs {
	return proto.UpdateArgs{
		ReleaseID: "rel-1", Version: "1.0.0", URL: "https://readyapp.player-ready.co.uk/rel/agent.exe",
		SHA256: managedSigner, SignerFingerprint: managedSigner,
		AgentProfile: "managed", ProductID: "readyfleet-agent",
	}
}

func validLightArgs() proto.UpdateArgs {
	return proto.UpdateArgs{
		ReleaseID: "rel-2", Version: "1.0.0", URL: "https://readyapp.player-ready.co.uk/rel/lightagent.exe",
		SHA256: lightSigner, SignerFingerprint: lightSigner,
		AgentProfile: "byod_solarbeam", ProductID: "readyfleet-light-agent",
	}
}

func TestProductPolicy_AcceptsItsOwnMatchingUpdate(t *testing.T) {
	if err := managedPolicy().Validate(validManagedArgs()); err != nil {
		t.Fatalf("managed policy rejected its own valid update: %v", err)
	}
	if err := lightPolicy().Validate(validLightArgs()); err != nil {
		t.Fatalf("light policy rejected its own valid update: %v", err)
	}
}

// This is Task 5's explicit acceptance bar: each profile must reject the
// other's product, both directions.
func TestProductPolicy_RejectsOtherProfilesProduct(t *testing.T) {
	if err := managedPolicy().Validate(validLightArgs()); err == nil {
		t.Fatal("managed policy accepted a readyfleet-light-agent update")
	}
	if err := lightPolicy().Validate(validManagedArgs()); err == nil {
		t.Fatal("light policy accepted a readyfleet-agent update")
	}
}

func TestProductPolicy_RejectsMismatchedAgentProfileEvenWithRightProductID(t *testing.T) {
	args := validManagedArgs()
	args.AgentProfile = "byod_solarbeam" // product_id says managed, agent_profile claims BYOD
	if err := managedPolicy().Validate(args); err == nil {
		t.Fatal("accepted mismatched product_id/agent_profile combination")
	}
}

func TestProductPolicy_RejectsUnlistedSigner(t *testing.T) {
	args := validManagedArgs()
	args.SignerFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := managedPolicy().Validate(args); err == nil {
		t.Fatal("accepted a signer fingerprint outside the trusted set")
	}
}

func TestProductPolicy_EmptySignerSetRejectsEverything(t *testing.T) {
	failClosed := ProductPolicy{ProductID: "readyfleet-agent", Profile: "managed", Signers: map[string]struct{}{}}
	if err := failClosed.Validate(validManagedArgs()); err == nil {
		t.Fatal("empty signer set must fail closed, not accept")
	}
}
