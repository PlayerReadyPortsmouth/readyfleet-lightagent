package solarbeam

import "errors"

// The caller holds the inspected process handle through this decision and stop.
// Managed Off covers the configured installation on this device, including a
// survivor launched by another user. BYOD Off covers only its local owner.
func stopHostProcessInScope(mode Mode, matchesConfiguredPath bool, sameOwner func() (bool, error), stop func() error) error {
	if !matchesConfiguredPath {
		return nil
	}
	switch mode {
	case ModeManaged:
		return stop()
	case ModeInteractive:
		owned, err := sameOwner()
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		return stop()
	default:
		return errors.New("hosting off: unknown execution mode")
	}
}
