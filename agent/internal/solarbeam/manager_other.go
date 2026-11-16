//go:build !windows && !linux && !darwin

package solarbeam

import (
	"errors"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// prepareEngine is a no-op on non-Windows dev hosts: the engine only
// launches on Windows. Returning nil keeps the start path testable.
func (m *Manager) prepareEngine() error { return nil }

// startEngine is a no-op on non-Windows dev hosts (see prepareEngine).
func (m *Manager) startEngine(_ proto.SolarbeamStartArgs) error { return nil }

// stopSolarbeam is a no-op on non-Windows dev hosts: there is no
// SunshineService or bridge to tear down. Returning nil keeps the stop
// path testable.
func (m *Manager) stopSolarbeam() error { return nil }

// engineServiceRunning is always false on non-Windows dev hosts: there is
// no SCM to query.
func (m *Manager) engineServiceRunning() bool { return false }

// preloadEngine, engineHealthy and bridgeProcessRunning are the warm-engine
// stubs for non-Windows dev hosts: there is no SunshineService to preload,
// no engine process to find, and no bridge process to enumerate. preload is
// a no-op so the managed start path stays callable; the two predicates
// report false, which is the safe direction for each — engineHealthy false
// means "don't assume a warm engine", and bridgeProcessRunning false means
// SessionActive short-circuits to inactive on a host that cannot host a
// session in the first place. Tests inject all three.
func (m *Manager) preloadEngine(_ string) error { return nil }
func (m *Manager) engineHealthy() bool          { return false }
func (m *Manager) bridgeProcessRunning() bool   { return false }

// restartEngineService and stopEngineService exist here only so New can
// wire restartFn/stopServiceFn identically on every platform; there is no
// SCM on a dev host to drive.
func restartEngineService() error { return nil }
func stopEngineService() error    { return nil }

// prepareEngineInteractive, startEngineInteractive, stopSolarbeamInteractive,
// engineProcessRunning mirror the ModeManaged stubs above — no real engine
// process exists to manage on a non-Windows dev host either way.
func (m *Manager) prepareEngineInteractive() error                         { return nil }
func (m *Manager) startEngineInteractive(_ proto.SolarbeamStartArgs) error { return nil }
func (m *Manager) stopSolarbeamInteractive() error                         { return nil }
func (m *Manager) engineProcessRunning() bool                              { return false }

// engineFileVersion is unsupported on non-Windows hosts: there is no Win32
// version resource to read. Tests inject fileVersionFn, so this stub is
// only hit if a real Status() reaches a dev host.
func engineFileVersion(_ string) (string, error) {
	return "", errors.New("solarbeam engine file version is only readable on Windows")
}

// launchBridge is unsupported on non-Windows hosts. Tests inject launchFn,
// so this stub is only hit if a real start reaches a dev host.
func (m *Manager) launchBridge(_ proto.SolarbeamStartArgs) error {
	return errors.New("solarbeam bridge launch is only supported on Windows")
}

func (m *Manager) removeRecoveredLinkAuth() error { return nil }
