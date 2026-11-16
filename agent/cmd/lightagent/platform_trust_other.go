//go:build !linux && !darwin

package main

import "crypto/ed25519"

func platformTrustedPublicKeys() (map[string]ed25519.PublicKey, error) {
	return map[string]ed25519.PublicKey{}, nil
}

func platformUpdateTempPattern() string { return "readyfleet-light-update-*.exe" }
