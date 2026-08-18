//go:build windows && !fakeagent

package svc

import (
	"context"

	"golang.org/x/sys/windows/svc"
)

type handler struct {
	cancel context.CancelFunc
}

// Execute is invoked by svc.Run. It transitions the service through
// StartPending → Running, then translates SCM control codes into
// context cancellation when asked to stop.
func (h *handler) Execute(args []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (svcSpecificEC bool, exitCode uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending}
	status <- svc.Status{State: svc.Running, Accepts: accepted}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			h.cancel()
			return false, 0
		}
	}
	return false, 0
}

func runImpl(ctx context.Context, cancel context.CancelFunc) error {
	return svc.Run(ServiceName, &handler{cancel: cancel})
}

func isWindowsServiceImpl() bool {
	yes, _ := svc.IsWindowsService()
	return yes
}
