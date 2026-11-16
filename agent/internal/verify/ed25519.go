package verify

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
)

// Ed25519PublicKeyFingerprint returns the lowercase SHA-256 fingerprint used
// in release targets for a raw Ed25519 public key.
func Ed25519PublicKeyFingerprint(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("ed25519 public key must be %d bytes", ed25519.PublicKeySize)
	}
	sum := sha256.Sum256(publicKey)
	return hex.EncodeToString(sum[:]), nil
}

// ParseEd25519PublicKey decodes a standard base64 raw Ed25519 public key.
func ParseEd25519PublicKey(encoded string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid base64 Ed25519 public key")
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyEd25519File verifies signature over the exact bytes at path and also
// pins publicKey to expectedFingerprint. Hash verification remains a separate,
// earlier download step; requiring both catches storage corruption and proves
// release-publisher authenticity without trusting controller-supplied key
// material.
func VerifyEd25519File(path, encodedSignature, expectedFingerprint string, publicKey ed25519.PublicKey) error {
	if !ValidLowerHex256(expectedFingerprint) {
		return errors.New("expected signer fingerprint must be 64 lowercase hexadecimal characters")
	}
	actualFingerprint, err := Ed25519PublicKeyFingerprint(publicKey)
	if err != nil {
		return err
	}
	expected, _ := hex.DecodeString(expectedFingerprint)
	actual, _ := hex.DecodeString(actualFingerprint)
	if subtle.ConstantTimeCompare(actual, expected) != 1 {
		return fmt.Errorf("Ed25519 public key fingerprint mismatch: got %s", actualFingerprint)
	}
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return errors.New("invalid base64 Ed25519 signature")
	}
	artifact, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read signed artifact: %w", err)
	}
	if !ed25519.Verify(publicKey, artifact, signature) {
		return errors.New("Ed25519 artifact signature verification failed")
	}
	return nil
}
