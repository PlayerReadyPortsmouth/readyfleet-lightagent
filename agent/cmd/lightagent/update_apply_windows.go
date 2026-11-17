//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// applyLightUpdate verifies and swaps the user-owned binary, then starts the
// replacement's local helper before the delayed exit. The helper waits for this
// exact process to exit, relaunches in the same desktop session and retains the
// old binary until the replacement survives its startup window.
func applyLightUpdate(downloadedPath, version, signerFingerprint string) error {
	exePath, err := resolveExePath()
	if err != nil {
		return err
	}

	lease, err := acquireLightUpdateLease(exePath)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(lease)

	// Personal PCs never trust ReadyFleet's private signing root (see
	// lightinstall.verifyBundledAgent), so the chain-to-trusted-root check
	// would refuse every BYOD self-update. The update was already
	// SHA-256-verified against the C2's target before reaching here; the
	// signer-fingerprint pin inside VerifyAuthenticodeAllowUntrustedRoot
	// remains the authority.
	if err := verify.VerifyAuthenticodeAllowUntrustedRoot(downloadedPath, signerFingerprint); err != nil {
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

	// Start before exiting so a launch failure can still restore the current
	// agent synchronously and return a failed command result to the controller.
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE, true, uint32(os.Getpid()))
	if err != nil {
		if restoreErr := restoreLightUpdate(exePath, oldPath, downloadedPath); restoreErr != nil {
			return fmt.Errorf("retain updating process: %v; restore: %w", err, restoreErr)
		}
		return fmt.Errorf("retain updating process: %w", err)
	}
	defer windows.CloseHandle(parent)
	args := append([]string{lightUpdateHelper, strconv.FormatUint(uint64(parent), 10), strconv.FormatUint(uint64(lease), 10)}, os.Args[1:]...)
	helper := exec.Command(exePath, args...)
	helper.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, AdditionalInheritedHandles: []syscall.Handle{syscall.Handle(parent), syscall.Handle(lease)}}
	if err := helper.Start(); err != nil {
		if restoreErr := restoreLightUpdate(exePath, oldPath, downloadedPath); restoreErr != nil {
			return fmt.Errorf("start relaunch helper: %v; restore: %w", err, restoreErr)
		}
		return fmt.Errorf("start relaunch helper: %w", err)
	}
	go func() { _ = helper.Wait() }()

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

// restoreLightUpdate returns the downloaded candidate to its staging path so
// update.Manager can remove it, preserving the enrolled agent and its identity.
func restoreLightUpdate(exePath, previous, staged string) error {
	if err := os.Rename(exePath, staged); err != nil {
		return err
	}
	if err := os.Rename(previous, exePath); err != nil {
		_ = os.Rename(staged, exePath)
		return err
	}
	return nil
}

// An inherited exclusive file handle prevents a replacement from accepting a
// second update while the helper still owns its startup/rollback window. The
// file itself contains no state; a crashed helper releases the kernel lease.
func acquireLightUpdateLease(exe string) (windows.Handle, error) {
	path, err := windows.UTF16PtrFromString(exe + ".update-lock")
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE,
		0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return 0, fmt.Errorf("self-update startup or rollback already active: %w", err)
	}
	if err := windows.SetHandleInformation(h, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
		windows.CloseHandle(h)
		return 0, fmt.Errorf("inherit self-update lease: %w", err)
	}
	return h, nil
}
