package solarbeam

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// testTrustedSigner is the fingerprint newTestManagerForUpdate configures
// as trusted; testUntrustedSigner is a distinct, equally hex-shaped
// fingerprint that is deliberately never in that allowlist.
var (
	testTrustedSigner   = strings.Repeat("b", 64)
	testUntrustedSigner = strings.Repeat("c", 64)
)

func validEngineUpdateArgs() proto.SolarbeamEngineUpdateArgs {
	return proto.SolarbeamEngineUpdateArgs{
		ReleaseID:         "rel-1",
		Version:           "2.0.6",
		URL:               "https://readyapp.player-ready.co.uk/rel/solarbeam-engine.exe",
		SHA256:            strings.Repeat("a", 64),
		SignerFingerprint: testTrustedSigner,
		ProductID:         solarbeamEngineProductID,
	}
}

func newTestManagerForUpdate() *Manager {
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	m.statusFn = func() bool { return false }
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error { return nil }
	m.TrustedSigners = map[string]struct{}{testTrustedSigner: {}}
	return m
}

func TestUpdate_MissingFieldsRejected(t *testing.T) {
	m := newTestManagerForUpdate()
	applyCalled := false
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error {
		applyCalled = true
		return nil
	}
	args := validEngineUpdateArgs()
	args.ReleaseID = ""
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("missing release_id accepted")
	}
	if applyCalled {
		t.Fatal("apply must not run when validation fails")
	}
}

func TestUpdate_BadSHA256Rejected(t *testing.T) {
	m := newTestManagerForUpdate()
	args := validEngineUpdateArgs()
	args.SHA256 = "not-hex"
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("malformed sha256 accepted")
	}
}

func TestUpdate_BadSignerFingerprintRejected(t *testing.T) {
	m := newTestManagerForUpdate()
	args := validEngineUpdateArgs()
	args.SignerFingerprint = "TOOSHORT"
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("malformed signer_fingerprint accepted")
	}
}

// TestUpdate_SignerTrustAllowlist is the asymmetric-trust-pinning
// regression: a validly hex-shaped signer_fingerprint must still be
// rejected unless it is in this Manager's local TrustedSigners allowlist
// — hex shape alone (verify.ValidLowerHex256) proves nothing about which
// signer this agent has actually agreed to trust. Mirrors
// update.ProductPolicy.Validate's own signer check for self-update.
func TestUpdate_SignerTrustAllowlist(t *testing.T) {
	tests := []struct {
		name      string
		signer    string
		wantErr   bool
		wantApply bool
	}{
		{
			name:      "untrusted but validly hex-shaped signer rejected",
			signer:    testUntrustedSigner,
			wantErr:   true,
			wantApply: false,
		},
		{
			name:      "allowlisted signer accepted",
			signer:    testTrustedSigner,
			wantErr:   false,
			wantApply: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := newTestManagerForUpdate()
			applyCalled := false
			m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error {
				applyCalled = true
				return nil
			}
			args := validEngineUpdateArgs()
			args.SignerFingerprint = tt.signer

			err := m.Update(context.Background(), args)
			if tt.wantErr && err == nil {
				t.Fatal("untrusted signer_fingerprint accepted")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("trusted signer_fingerprint rejected: %v", err)
			}
			if applyCalled != tt.wantApply {
				t.Fatalf("applyUpdateFn called = %v, want %v", applyCalled, tt.wantApply)
			}
		})
	}
}

func TestUpdate_WrongProductIDRejected(t *testing.T) {
	m := newTestManagerForUpdate()
	args := validEngineUpdateArgs()
	args.ProductID = "readyfleet-agent"
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("wrong product_id accepted")
	}
}

// The safety guard this task's design decision hinges on: Update must
// refuse rather than silently stop a running engine.
func TestUpdate_RefusesWhileEngineRunning(t *testing.T) {
	m := newTestManagerForUpdate()
	m.statusFn = func() bool { return true }
	applyCalled := false
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error {
		applyCalled = true
		return nil
	}
	err := m.Update(context.Background(), validEngineUpdateArgs())
	if err == nil {
		t.Fatal("Update must refuse while the engine is running")
	}
	if applyCalled {
		t.Fatal("apply must not run when the engine is running")
	}
}

func TestUpdate_ApplyFailurePropagatedAndVersionNotRecorded(t *testing.T) {
	m := newTestManagerForUpdate()
	applyErr := errors.New("download failed")
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error { return applyErr }

	err := m.Update(context.Background(), validEngineUpdateArgs())
	if !errors.Is(err, applyErr) {
		t.Fatalf("err = %v, want wrapping %v", err, applyErr)
	}
	if got := m.Status(context.Background()); got.EngineVersion != "" {
		t.Fatalf("EngineVersion = %q after a failed update, want empty", got.EngineVersion)
	}
}

func TestUpdate_SuccessSetsStatusEngineVersion(t *testing.T) {
	m := newTestManagerForUpdate()
	args := validEngineUpdateArgs()

	if err := m.Update(context.Background(), args); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := m.Status(context.Background())
	if got.EngineVersion != args.Version {
		t.Fatalf("EngineVersion = %q, want %q", got.EngineVersion, args.Version)
	}
}
