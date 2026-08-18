// Package svc wraps golang.org/x/sys/windows/svc with a context-flavoured
// API the agent's main() can use without caring about the SCM details.
//
// On non-Windows builds (and the fakeagent tag), IsWindowsService always
// returns false and Run returns a clear error, so the agent's main()
// can safely call them on any platform.
package svc

import "context"

// Run executes the agent under SCM control. It blocks until the SCM
// issues Stop or Shutdown, at which point it calls cancel() and
// returns. Only call this when IsWindowsService() returned true.
//
// Implemented in service_windows.go for the production build and
// service_other.go for everything else.
func Run(ctx context.Context, cancel context.CancelFunc) error {
	return runImpl(ctx, cancel)
}

// IsWindowsService reports whether the current process was started by
// the Windows Service Control Manager. Returns false on non-Windows
// builds.
func IsWindowsService() bool {
	return isWindowsServiceImpl()
}
