//go:build windows

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	svcpkg "github.com/playerreadyportsmouth/readyfleet/agent/internal/svc"
	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

const lightUpdateHelper = "finish-self-update"

// The replacement runs this private local helper in the same desktop session.
// It owns no C2 connection/tray and waits for the updater before relaunching.
func handleLightUpdateHelper(args []string) (bool, int) {
	if len(args) == 0 || args[0] != lightUpdateHelper {
		return false, 0
	}
	if logger, closer, err := svcpkg.OpenServiceLogger(defaultLightLogPath()); err == nil {
		defer closer.Close()
		slog.SetDefault(logger)
	}
	if len(args) < 3 {
		return true, 2
	}
	parentValue, err := strconv.ParseUint(args[1], 10, strconv.IntSize)
	if err != nil || parentValue == 0 {
		return true, 2
	}
	leaseValue, err := strconv.ParseUint(args[2], 10, strconv.IntSize)
	if err != nil || leaseValue == 0 {
		return true, 2
	}
	defer windows.CloseHandle(windows.Handle(leaseValue))
	exe, err := resolveExePath()
	if err != nil {
		return true, 1
	}
	// The updater opens and explicitly inherits this SYNCHRONIZE-only handle.
	// It remains valid even if the updater exits before this helper runs;
	// no lookup by PID, with its reuse race, is needed.
	parent := windows.Handle(parentValue)
	defer windows.CloseHandle(parent)
	previous := exe + ".old"
	err = finishLightUpdate(func() error {
		state, err := windows.WaitForSingleObject(parent, 30_000)
		if err != nil {
			return err
		}
		if state != windows.WAIT_OBJECT_0 {
			return errors.New("updating agent did not exit within 30 seconds")
		}
		return nil
	}, func() error {
		return startLightReplacement(exe, args[3:], 10*time.Second)
	}, func() error {
		if err := verify.VerifyAuthenticodeAllowUntrustedRoot(previous, lightTrustedSigner); err != nil {
			return fmt.Errorf("verify previous agent: %w", err)
		}
		failed := exe + ".failed"
		if err := os.Rename(exe, failed); err != nil {
			return fmt.Errorf("set failed replacement aside: %w", err)
		}
		if err := os.Rename(previous, exe); err != nil {
			_ = os.Rename(failed, exe)
			return err
		}
		return startLightReplacement(exe, args[3:], 10*time.Second)
	}, func() error { return os.Remove(previous) })
	if err != nil {
		slog.Error("self-update relaunch", "err", err)
		return true, 1
	}
	slog.Info("self-update replacement survived startup")
	return true, 0
}

func startLightReplacement(exe string, args []string, startupWindow time.Duration) error {
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(startupWindow)
	defer timer.Stop()
	select {
	case err := <-done:
		return fmt.Errorf("agent exited during startup: %v", err)
	case <-timer.C:
		// This is process liveness, not a claim about controller reachability.
		return nil
	}
}
