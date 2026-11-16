//go:build !windows || fakeagent

package svc

import (
	"context"
	"errors"
)

func runImpl(ctx context.Context, cancel context.CancelFunc) error {
	_ = ctx
	_ = cancel
	return errors.New("svc: Windows service mode not supported on this build")
}

func isWindowsServiceImpl() bool {
	return false
}
