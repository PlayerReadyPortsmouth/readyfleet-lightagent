//go:build !windows && !linux

package main

import "errors"

// applyLightUpdate on non-Windows (dev/fakeagent) hosts is unsupported:
// the light agent only runs on real Windows personal PCs.
func applyLightUpdate(downloadedPath, version, signerFingerprint string) error {
	return errors.New("self-update is only supported on Windows")
}
