//go:build windows

package solarbeam

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/svc/mgr"
)

func linkCapabilityCommand(ctx context.Context, path string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	configureLinkCapabilityCommand(cmd, linkCapabilityEnvironment())
	return cmd
}

func linkEngineMatches(paths Paths) bool {
	if paths.Mode == ModeInteractive {
		return true
	}
	manager, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(engineServiceName)
	if err != nil {
		return false
	}
	defer service.Close()
	config, err := service.Config()
	return err == nil && linkServicePathMatches(paths.Engine, config.BinaryPathName)
}

func linkServicePathMatches(engine, service string) bool {
	// The service launches Sunshine.exe relative to its own tools directory.
	// Refuse arguments, ambiguous quoting and a service from another install.
	expected := engineServicePath(engine)
	return strings.EqualFold(filepath.Base(engine), "sunshine.exe") &&
		((!strings.ContainsAny(expected, " \t\r\n\"") && strings.EqualFold(service, expected)) ||
			strings.EqualFold(service, `"`+expected+`"`))
}
