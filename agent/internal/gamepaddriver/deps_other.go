//go:build !windows

package gamepaddriver

import (
	"context"
	"io"
)

// DefaultDeps off Windows reports "not installed" and refuses every change:
// Linux BYOD gets gamepads through uinput/uhid, which the sudo integration
// step (packaging/linux/install-system-integration.sh) sets up.
func DefaultDeps(sessionActive func() bool) Deps {
	return Deps{
		Detect: func() (Status, error) { return Status{}, nil },
		Download: func(context.Context, string, string, string, int64) (string, error) {
			return "", ErrUnsupported
		},
		Hold:         func(string) (io.ReadCloser, error) { return nil, ErrUnsupported },
		VerifySigner: func(string, string) error { return ErrUnsupported },
		RunElevated: func(context.Context, string, string) (uint32, error) {
			return 0, ErrUnsupported
		},
		Msiexec:              func() (string, error) { return "", ErrUnsupported },
		InstalledProductCode: func() (string, error) { return "", nil },
		SessionActive:        sessionActive,
	}
}
