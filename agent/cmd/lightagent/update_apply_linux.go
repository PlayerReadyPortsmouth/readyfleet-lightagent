//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func applyLightUpdate(downloadedPath, version, signerFingerprint string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate running agent: %w", err)
	}
	exePath, err = evalExecutablePath(exePath)
	if err != nil {
		return err
	}
	if err := os.Chmod(downloadedPath, 0o755); err != nil {
		return fmt.Errorf("make update executable: %w", err)
	}
	previous := exePath + ".previous"
	_ = os.Remove(previous)
	if err := os.Rename(exePath, previous); err != nil {
		return fmt.Errorf("backup running agent: %w", err)
	}
	if err := os.Rename(downloadedPath, exePath); err != nil {
		_ = os.Rename(previous, exePath)
		return fmt.Errorf("activate agent update: %w", err)
	}
	// Delay the restart until the update handler has returned its successful
	// result to the controller. systemd's Restart policy then owns subsequent
	// recovery; the previous binary remains beside the new one for rollback.
	go func() {
		time.Sleep(2 * time.Second)
		_ = exec.Command("systemctl", "--user", "restart", "readyfleet-light.service").Run()
	}()
	return nil
}

func evalExecutablePath(path string) (string, error) {
	resolved, err := filepathEvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve running agent: %w", err)
	}
	return resolved, nil
}

var filepathEvalSymlinks = func(path string) (string, error) {
	return filepath.EvalSymlinks(path)
}
