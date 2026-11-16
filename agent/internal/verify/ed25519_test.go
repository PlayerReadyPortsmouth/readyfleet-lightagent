package verify

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyEd25519File(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	artifact := []byte("readyfleet linux release")
	path := filepath.Join(t.TempDir(), "release.tar.zst")
	if err := os.WriteFile(path, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := Ed25519PublicKeyFingerprint(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, artifact))
	if err := VerifyEd25519File(path, signature, fingerprint, publicKey); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyEd25519File(path, signature, fingerprint, publicKey); err == nil {
		t.Fatal("tampered artifact accepted")
	}
}

func TestVerifyEd25519FileRejectsWrongPinnedKey(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "release")
	artifact := []byte("release")
	if err := os.WriteFile(path, artifact, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongFingerprint, _ := Ed25519PublicKeyFingerprint(otherKey)
	signature := base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, artifact))
	if err := VerifyEd25519File(path, signature, wrongFingerprint, publicKey); err == nil {
		t.Fatal("wrong pinned key accepted")
	}
}
