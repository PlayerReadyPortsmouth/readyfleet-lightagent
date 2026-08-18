//go:build windows

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// applyLightUpdate is the light entrypoint's update.Manager.ApplyFn.
//
// PROVISIONAL for this increment, explicitly by design decision (see
// docs/superpowers/plans/2026-07-26-solarbeam-byod-light-agent.md Task 6,
// not yet built): the lightagent runs as a current-user process launched
// from an HKCU Run key, not a service, so there is no service to
// stop/restart the way the managed agent's apply does. This swaps the
// binary file in place, verifies it, then exits — relying on the next
// logon to relaunch under the new binary via that same Run key entry. No
// self-heal-on-failed-restart dance (the managed apply's updater .bat)
// exists yet; if the new binary fails to start, the machine simply stays
// down until Task 6 lands a real relaunch/rollback story. Flagged here
// rather than silently decided.
func applyLightUpdate(downloadedPath, version, signerFingerprint string) error {
	exePath, err := resolveExePath()
	if err != nil {
		return err
	}

	if err := verify.VerifyAuthenticode(downloadedPath, signerFingerprint); err != nil {
		return fmt.Errorf("authenticode: %w", err)
	}

	oldPath := exePath + ".old"
	_ = os.Remove(oldPath)

	if err := os.Rename(exePath, oldPath); err != nil {
		return fmt.Errorf("rename running exe: %w", err)
	}
	if err := os.Rename(downloadedPath, exePath); err != nil {
		_ = os.Rename(oldPath, exePath)
		return fmt.Errorf("move new exe into place: %w", err)
	}

	// No self-heal: unlike the managed agent's updater .bat, there is no
	// verification here that the new binary actually starts successfully
	// before committing to it. Acceptable for this provisional path (see
	// doc comment above) — a real answer is Task 6's job.
	_ = os.Remove(oldPath)

	// Delayed exit, not immediate: Update()/runSelfUpdate still need to
	// emit the success MsgResult back to the C2 after ApplyFn returns —
	// an immediate os.Exit(0) here would race that send and could kill
	// the process before the controller ever sees the update as
	// successful.
	go func() {
		time.Sleep(2 * time.Second)
		os.Exit(0)
	}()
	return nil
}

func resolveExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return p, nil
}
