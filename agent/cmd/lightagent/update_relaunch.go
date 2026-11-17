package main

import "fmt"

// finishLightUpdate keeps the backup until the replacement has survived its
// startup window. A parent that has not exited must never be rolled back under.
// The operations are injected so unit tests neither exit nor launch a process.
func finishLightUpdate(waitParent, startNew, rollback, removeBackup func() error) error {
	if err := waitParent(); err != nil {
		return fmt.Errorf("wait for updating agent: %w", err)
	}
	if err := startNew(); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return fmt.Errorf("replacement startup: %v; rollback: %w", err, rollbackErr)
		}
		return fmt.Errorf("replacement startup: %w (previous agent restored and relaunched)", err)
	}
	if err := removeBackup(); err != nil {
		return fmt.Errorf("replacement running; retain backup: %w", err)
	}
	return nil
}
