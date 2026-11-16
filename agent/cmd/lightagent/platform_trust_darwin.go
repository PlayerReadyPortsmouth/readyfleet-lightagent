//go:build darwin

package main

import (
	"crypto/ed25519"
	"fmt"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// lightDarwinPublicKey is injected as standard base64 at build time
// (-X main.lightDarwinPublicKey), and release builds pass the same Ed25519
// public key as the Linux build, so one signer covers both. No darwin update
// channel exists yet; this keeps a future one verifying against a pinned key
// and, unset, fails closed with an empty trust set. Keeping
// the private key entirely outside the build and repository lets the agent pin
// publisher identity without giving the release pipeline signing authority.
var lightDarwinPublicKey string

func platformTrustedPublicKeys() (map[string]ed25519.PublicKey, error) {
	if lightDarwinPublicKey == "" {
		return map[string]ed25519.PublicKey{}, nil
	}
	key, err := verify.ParseEd25519PublicKey(lightDarwinPublicKey)
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
