package solarbeam

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

var ErrHostingDisabled = errors.New("SolarBeam hosting is switched off on this PC")

const hostingStateName = "solarbeam-hosting.json"

// New loads the refusal before any preload/start. An unreadable or malformed
// existing file fails closed without taking down Fleet's transport.
func (m *Manager) loadHostingAvailability() {
	if m.paths.StateDir == "" {
		return
	} // legacy/library callers: setter still refuses persistence
	data, err := os.ReadFile(filepath.Join(m.paths.StateDir, hostingStateName))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var state proto.SetSolarbeamAvailabilityArgs
	if err == nil {
		err = json.Unmarshal(data, &state)
	}
	if err != nil {
		m.hostingOff.Store(true)
		m.availabilityLoadError = fmt.Errorf("read hosting availability: %w", err)
		m.hostingError = m.availabilityLoadError.Error()
		return
	}
	m.hostingOff.Store(!state.Enabled)
}

// SetHostingEnabled is the authorised local owner operation for BYOD; the
// managed adapter exposes it only through its narrow authorised C2 command.
// Disable closes admission immediately, including a Start already inside mu.
// Saving and cleanup then serialize with start/stop/update. A failure remains
// refused in memory and is surfaced, never represented as confirmed Off.
func (m *Manager) SetHostingEnabled(ctx context.Context, enabled bool) error {
	m.availabilityMu.Lock()
	defer m.availabilityMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !enabled {
		m.hostingOff.Store(true)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.setHostingEnabledLocked(ctx, enabled)
}

func (m *Manager) setHostingEnabledLocked(ctx context.Context, enabled bool) error {
	m.hostingGeneration++
	if enabled {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.saveHostingAvailability(true); err != nil {
			return err
		}
		m.availabilityLoadError = nil
		m.hostingError = ""
		m.hostingOff.Store(false)
		// On permits the next authorised Start; it does not silently host anyone.
		return nil
	}
	saveErr := m.saveHostingAvailability(false)
	m.availabilityLoadError = saveErr
	// Even a cancelled requester or failed disk write must not leave the
	// already-refused live host accepting input. Platform cleanup is bounded.
	stopErr := m.disableHostFn()
	err := errors.Join(saveErr, stopErr)
	if err != nil {
		m.hostingError = err.Error()
		return err
	}
	m.lastStoppedSessionID = m.currentSessionID
	m.currentSessionID = ""
	m.clearSessionRecordLocked()
	m.hostingError = ""
	m.notifySessionChangeLocked()
	return nil
}

// SetRemoteHostingEnabled cannot undo a BYOD owner's persistent refusal.
// Remote disable is allowed; only the local owner can turn BYOD hosting on.
func (m *Manager) SetRemoteHostingEnabled(ctx context.Context, enabled bool) error {
	if enabled && m.paths.Mode == ModeInteractive {
		return errors.New("BYOD SolarBeam hosting can only be enabled by the local owner")
	}
	return m.SetHostingEnabled(ctx, enabled)
}

// ReconcileHosting enforces a saved refusal at agent startup, including an
// exact configured host that survived restart. It never warms or changes Fleet.
func (m *Manager) ReconcileHosting(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hostingOff.Load() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := errors.Join(m.availabilityLoadError, m.disableHostFn())
	if err != nil {
		m.hostingError = err.Error()
		return err
	}
	m.hostingError = ""
	return nil
}

func (m *Manager) saveHostingAvailability(enabled bool) error {
	if m.paths.StateDir == "" {
		return errors.New("hosting availability state directory is not configured")
	}
	// The config directory already exists and retains its installer-controlled
	// ACL on managed Windows; never create a fallback world-writable directory.
	info, err := os.Lstat(m.paths.StateDir)
	if err != nil {
		return fmt.Errorf("hosting state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("hosting state directory is not a real directory")
	}
	data, _ := json.Marshal(proto.SetSolarbeamAvailabilityArgs{Enabled: enabled})
	file, err := os.CreateTemp(m.paths.StateDir, ".solarbeam-hosting-*")
	if err != nil {
		return fmt.Errorf("save hosting availability: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceHostingState(name, filepath.Join(m.paths.StateDir, hostingStateName))
	}
	if err != nil {
		return fmt.Errorf("save hosting availability: %w", err)
	}
	return nil
}

// Used after an installer/update may have restarted a service. Caller holds mu.
func (m *Manager) disableConfiguredHostLocked() error {
	err := errors.Join(m.availabilityLoadError, m.disableHostFn())
	if err != nil {
		m.hostingError = err.Error()
	} else {
		m.hostingError = ""
	}
	return err
}

// SetHostingEnabledIfCurrent applies a local reviewed change only to the exact
// host generation shown in the confirmation. Waiting for an in-flight Start
// must never turn a confirmation for the old host into authority over its replacement.
func (m *Manager) SetHostingEnabledIfCurrent(ctx context.Context, enabled bool, expectedRevision string) error {
	m.availabilityMu.Lock()
	defer m.availabilityMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if expectedRevision == "" || expectedRevision != m.hostingRevisionLocked() {
		return errors.New("hosting changed; review the current session again")
	}
	if !enabled {
		m.hostingOff.Store(true)
	}
	return m.setHostingEnabledLocked(ctx, enabled)
}

func (m *Manager) hostingRevisionLocked() string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%t:%s:%s", m.hostingEpoch, m.hostingGeneration, m.hostingOff.Load(), m.currentSessionID, m.hostingError)))
	return fmt.Sprintf("%x", digest)
}
