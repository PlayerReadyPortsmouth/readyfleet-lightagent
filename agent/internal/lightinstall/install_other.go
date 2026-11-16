//go:build !windows && !linux && !darwin

// Non-Windows stubs. lightinstall's seams are only ever wired to these on a
// dev machine running the package's own tests (which inject fake seam
// functions instead) or a stray non-Windows build of light-installer — a
// mentor's PC is always Windows, so these exist purely so the package
// compiles cross-platform, matching every other _other.go in this repo.
package lightinstall

import (
	"context"
	"errors"
	"time"
)

var errUnsupportedPlatform = errors.New("lightinstall: only supported on Windows")

func verifyBundledAgent(exePath string) error {
	return errUnsupportedPlatform
}

// ProtectKeyACL is the non-Windows stub for install_windows.go's
// ProtectKeyACL. Exported for the same reason as that implementation —
// see its doc comment.
func ProtectKeyACL(keyPath string) error {
	return errUnsupportedPlatform
}

func registerLogonStartup(agentExePath string) error {
	return errUnsupportedPlatform
}

func removeLogonStartup() error {
	return errUnsupportedPlatform
}

func enrollLight(ctx context.Context, cfg Config, payload Payload, onPending func()) error {
	return errUnsupportedPlatform
}

func startAgentOnce(agentExePath string) error {
	return errUnsupportedPlatform
}

func waitForAgentConnected(ctx context.Context, logPath string, timeout time.Duration) error {
	return errUnsupportedPlatform
}

func unlinkOnline(ctx context.Context, agentExePath string) error {
	return errUnsupportedPlatform
}

func removeInstallDir(root string) error {
	return errUnsupportedPlatform
}

func writeTombstone(root string) error {
	return errUnsupportedPlatform
}
