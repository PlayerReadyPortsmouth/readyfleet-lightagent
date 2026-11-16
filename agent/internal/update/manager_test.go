package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

func TestUpdate_SHA256Mismatch_DoesNotApply(t *testing.T) {
	payload := []byte("fake-binary-bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := New(Config{Policy: managedPolicy()})
	applied := false
	m.ApplyFn = func(downloadedPath, version, signerFingerprint string) error { applied = true; return nil }

	args := validManagedArgs()
	args.URL = srv.URL
	args.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("expected sha256 mismatch to be rejected")
	}
	if applied {
		t.Fatal("apply must NOT be called on sha mismatch")
	}
}

func TestUpdate_RequiredDetachedSignatureIsVerified(t *testing.T) {
	payload := []byte("signed-linux-agent")
	sum := sha256.Sum256(payload)
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := verify.Ed25519PublicKeyFingerprint(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(payload) }))
	defer srv.Close()
	policy := ProductPolicy{ProductID: "readyfleet-agent", Profile: "managed", Signers: map[string]struct{}{fingerprint: {}}}
	m := New(Config{Policy: policy, RequireDetachedSignature: true, TrustedPublicKeys: map[string]ed25519.PublicKey{fingerprint: publicKey}})
	applied := false
	m.ApplyFn = func(string, string, string) error { applied = true; return nil }
	args := validManagedArgs()
	args.URL, args.SHA256, args.SignerFingerprint = srv.URL, hex.EncodeToString(sum[:]), fingerprint
	args.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	if err := m.Update(context.Background(), args); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !applied {
		t.Fatal("signed artifact was not applied")
	}
	applied = false
	args.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte("other")))
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("bad detached signature accepted")
	}
	if applied {
		t.Fatal("bad signature reached apply")
	}
}

func TestUpdate_GoodSHA_CallsApply(t *testing.T) {
	payload := []byte("good-binary-bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := New(Config{Policy: managedPolicy()})
	var appliedPath, appliedVersion, appliedSigner string
	m.ApplyFn = func(downloadedPath, version, signerFingerprint string) error {
		appliedPath, appliedVersion, appliedSigner = downloadedPath, version, signerFingerprint
		return nil
	}

	args := validManagedArgs()
	args.URL = srv.URL
	args.SHA256 = hex.EncodeToString(sum[:])
	if err := m.Update(context.Background(), args); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if appliedPath == "" {
		t.Fatal("apply was not called for a verified download")
	}
	if appliedVersion != args.Version {
		t.Fatalf("apply version = %q, want %q", appliedVersion, args.Version)
	}
	if appliedSigner != managedSigner {
		t.Fatalf("apply signer = %q, want %q", appliedSigner, managedSigner)
	}
}

func TestUpdate_HostNotAllowed(t *testing.T) {
	m := New(Config{AllowedHost: "readyapp.player-ready.co.uk", Policy: managedPolicy()})
	m.ApplyFn = func(string, string, string) error { return nil }

	args := validManagedArgs()
	args.URL = "https://evil.example.com/x"
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("expected rejection for disallowed host")
	}
}

func TestUpdate_ApplyFailureWrapsWithApplyPrefix(t *testing.T) {
	payload := []byte("good-binary-bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := New(Config{Policy: managedPolicy()})
	m.ApplyFn = func(string, string, string) error { return errors.New("apply failed") }

	args := validManagedArgs()
	args.URL = srv.URL
	args.SHA256 = hex.EncodeToString(sum[:])
	err := m.Update(context.Background(), args)
	if err == nil {
		t.Fatal("expected apply failure to propagate")
	}
}

func TestUpdate_NoApplyFnConfigured(t *testing.T) {
	m := New(Config{Policy: managedPolicy()})
	if err := m.Update(context.Background(), validManagedArgs()); err == nil {
		t.Fatal("expected an error when ApplyFn is unset")
	}
}

func TestValidate_RequiresAllFields(t *testing.T) {
	m := New(Config{Policy: managedPolicy()})
	valid := validManagedArgs()
	if err := m.validate(valid); err != nil {
		t.Fatalf("valid args rejected: %v", err)
	}
	missing := valid
	missing.ReleaseID = ""
	if err := m.validate(missing); err == nil {
		t.Fatal("missing release_id accepted")
	}
	missing = valid
	missing.AgentProfile = ""
	if err := m.validate(missing); err == nil {
		t.Fatal("missing agent_profile accepted")
	}
}

func TestUpdate_RefusesWhileSessionActive(t *testing.T) {
	payload := []byte("good-binary-bytes")
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := New(Config{Policy: managedPolicy()})
	m.SessionActiveFn = func(ctx context.Context) bool { return true }
	applied := false
	m.ApplyFn = func(string, string, string) error { applied = true; return nil }

	args := validManagedArgs()
	args.URL = srv.URL
	args.SHA256 = hex.EncodeToString(sum[:])
	if err := m.Update(context.Background(), args); err == nil {
		t.Fatal("expected update to be refused while a session is active")
	}
	if applied {
		t.Fatal("apply must NOT be called while a session is active")
	}
}

func TestUpdate_SingleFlight(t *testing.T) {
	m := New(Config{Policy: managedPolicy()})
	if !m.TryBegin() {
		t.Fatal("first TryBegin should succeed")
	}
	if m.TryBegin() {
		t.Fatal("second TryBegin should fail while one is in flight")
	}
	m.End()
	if !m.TryBegin() {
		t.Fatal("TryBegin should succeed after End()")
	}
}
