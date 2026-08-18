package verify

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// ValidLowerHex256 reports whether value is exactly 64 lowercase hex
// characters — the shape every sha256 digest and signer fingerprint on the
// wire must take.
func ValidLowerHex256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// VerifySignerCertificateDER returns nil only if certDER's SHA-256 matches
// expectedFingerprint (constant-time compare).
func VerifySignerCertificateDER(certDER []byte, expectedFingerprint string) error {
	if len(certDER) == 0 {
		return errors.New("signer certificate is missing")
	}
	expected, err := hex.DecodeString(expectedFingerprint)
	if err != nil || len(expected) != sha256.Size ||
		strings.ToLower(expectedFingerprint) != expectedFingerprint {
		return errors.New("expected signer fingerprint must be 64 lowercase hexadecimal characters")
	}
	actual := sha256.Sum256(certDER)
	if subtle.ConstantTimeCompare(actual[:], expected) != 1 {
		return fmt.Errorf("signer fingerprint mismatch: got %s", hex.EncodeToString(actual[:]))
	}
	return nil
}
