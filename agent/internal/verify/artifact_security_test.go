package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestVerifySignerCertificateDER(t *testing.T) {
	der := []byte("test signer certificate DER")
	sum := sha256.Sum256(der)
	fingerprint := hex.EncodeToString(sum[:])
	if err := VerifySignerCertificateDER(der, fingerprint); err != nil {
		t.Fatalf("matching signer rejected: %v", err)
	}
	if err := VerifySignerCertificateDER(der,
		"0000000000000000000000000000000000000000000000000000000000000000",
	); err == nil {
		t.Fatal("mismatched signer fingerprint accepted")
	}
	if err := VerifySignerCertificateDER(nil, fingerprint); err == nil {
		t.Fatal("missing signer certificate accepted")
	}
}
