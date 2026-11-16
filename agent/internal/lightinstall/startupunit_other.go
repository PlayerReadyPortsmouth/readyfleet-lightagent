//go:build !linux

package lightinstall

// ReconcileStartupUnit is a no-op off Linux.
//
// The drift it repairs is specific to the systemd user unit the Linux
// installer writes: on Windows the equivalent registration is a Run-key value
// holding only the agent path, which carries no sandboxing policy that could
// go stale. The light agent calls this unconditionally at startup, so the
// stub keeps that call site platform-neutral.
func ReconcileStartupUnit(string) (bool, error) { return false, nil }
