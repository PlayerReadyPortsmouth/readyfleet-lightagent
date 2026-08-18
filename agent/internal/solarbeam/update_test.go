package solarbeam

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func validEngineUpdateArgs() proto.SolarbeamEngineUpdateArgs {
	return proto.SolarbeamEngineUpdateArgs{
		ReleaseID:         "rel-1",
		Version:           "2.0.6",
		URL:               "https://readyapp.player-ready.co.uk/rel/solarbeam-engine.exe",
		SHA256:            strings.Repeat("a", 64),
		SignerFingerprint: strings.Repeat("b", 64),
		ProductID:         solarbeamEngineProductID,
	}
}

func newTestManagerForUpdate() *Manager {
	m := New(Paths{Engine: `C:\Program Files\SolarBeam\sunshine.exe`})
	m.statusFn = func() bool { return false }
	m.applyUpdateFn = func(context.Context, Paths, proto.SolarbeamEngineUpdateArgs) error { return nil }
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
