//go:build linux

package main

import (
	"crypto/ed25519"
	"fmt"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// lightLinuxPublicKey is injected as standard base64 at build time. Keeping
// the private key entirely outside the build and repository lets the agent pin
// publisher identity without giving the release pipeline signing authority.
var lightLinuxPublicKey string

func platformTrustedPublicKeys() (map[string]ed25519.PublicKey, error) {
	if lightLinuxPublicKey == "" {
		return map[string]ed25519.PublicKey{}, nil
	}
	key, err := verify.ParseEd25519PublicKey(lightLinuxPublicKey)
	if err != nil {
		return nil, fmt.Errorf("parse pinned public key: %w", err)
	}
	fingerprint, err := verify.Ed25519PublicKeyFingerprint(key)
	if err != nil {
		return nil, err
	}
	return map[string]ed25519.PublicKey{fingerprint: key}, nil
}

func platformUpdateTempPattern() string { return "readyfleet-light-update-*.bin" }
