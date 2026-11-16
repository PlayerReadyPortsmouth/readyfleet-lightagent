//go:build !windows && !linux && !darwin

package solarbeam

import "errors"

func (m *Manager) disableConfiguredHost() error {
	return errors.New("hosting cleanup unsupported on this platform")
}
