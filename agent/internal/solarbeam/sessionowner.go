package solarbeam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Session ownership that survives an agent restart, and reaping of a session
// whose bridge has gone.
//
// Incident, Ports-Admin, 24 Sep 2026: a scheduled service restart killed the
// agent while it was mid-way through start_solarbeam. The bridge it had just
// launched kept running, but the restarted agent had no currentSessionID, so
// ReadyApp's stop_solarbeam nine minutes later failed ("current session
// identity unknown; refusing destructive cleanup") and the bridge ran for
// eight hours. SessionActive saw a live bridge the whole time and refused
// engine installs and service changes.
//
// Two changes close that:
//
//  1. Start records the session it is about to launch in StateDir (the
//     agent's own ACL-protected config directory) BEFORE launching the
//     bridge. A restarted agent adopts that record while a bridge is still
//     running, so the matching stop is honoured. This is the agent's own
//     authenticated state, not a loopback observation: the rule that an
//     unauthenticated observation can never create stop authority stands,
//     and a stop must still name the recorded session exactly.
//  2. A reaper notices the bridge has gone (it now exits by itself when the
//     room is deleted or LiveKit is lost) and ends the session locally: it
//     clears the binding, stops a per-session engine where there is one, and
//     remembers the session as stopped so ReadyApp's later stop succeeds
//     idempotently.

const sessionRecordName = "solarbeam-session.json"

// reapConfirmations is how many consecutive reaper passes must see no bridge
// before the session is ended locally. The process check is a snapshot that
// reports "absent" on failure; one bad snapshot must not end a live session.
const reapConfirmations = 2

// DefaultReapInterval is how often RunSessionReaper checks the bridge.
const DefaultReapInterval = 10 * time.Second

type sessionRecord struct {
	SessionID string `json:"session_id"`
}

// saveSessionRecordLocked persists the session about to be launched. Caller
// holds mu. Best-effort: a failure is logged and the start proceeds, which is
// exactly today's behaviour (no restart recovery).
func (m *Manager) saveSessionRecordLocked(sessionID string) {
	if m.paths.StateDir == "" {
		return
	}
	if err := m.writeSessionRecord(sessionID); err != nil {
		slog.Warn("solarbeam: could not record session ownership; an agent restart will lose it",
			"phase", "session_record_failed", "session_id", sessionID, "err", err)
	}
}

func (m *Manager) writeSessionRecord(sessionID string) error {
	info, err := os.Lstat(m.paths.StateDir)
	if err != nil {
		return fmt.Errorf("session state directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session state directory is not a real directory")
	}
	data, _ := json.Marshal(sessionRecord{SessionID: sessionID})
	file, err := os.CreateTemp(m.paths.StateDir, ".solarbeam-session-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = replaceHostingState(name, filepath.Join(m.paths.StateDir, sessionRecordName))
	}
	return err
}

// loadSessionRecord returns the recorded session id, or "" when there is none
// or it cannot be read (unreadable means no recovery, never a guess).
func (m *Manager) loadSessionRecord() string {
	if m.paths.StateDir == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(m.paths.StateDir, sessionRecordName))
	if err != nil {
		return ""
	}
	var rec sessionRecord
	if json.Unmarshal(data, &rec) != nil {
		return ""
	}
	return rec.SessionID
}

func (m *Manager) clearSessionRecordLocked() {
	if m.paths.StateDir == "" {
		return
	}
	if err := os.Remove(filepath.Join(m.paths.StateDir, sessionRecordName)); err != nil && !os.IsNotExist(err) {
		slog.Warn("solarbeam: could not clear session record", "phase", "session_record_clear_failed", "err", err)
	}
}

// recoverSessionLocked adopts a recorded session after an agent restart (or a
// start that failed after its bridge launched) while that bridge is still
// running. With no bridge running the record is stale: it is cleared and the
// session is remembered as stopped. Caller holds mu; only acts when no
// session is bound.
func (m *Manager) recoverSessionLocked() {
	if m.currentSessionID != "" {
		return
	}
	sid := m.loadSessionRecord()
	if sid == "" {
		return
	}
	if m.bridgeRunningFn() {
		m.currentSessionID = sid
		m.lastStoppedSessionID = ""
		m.hostingGeneration++
		slog.Info("solarbeam: adopted recorded session with a running bridge",
			"phase", "session_adopted", "session_id", sid)
		return
	}
	m.clearSessionRecordLocked()
	if m.lastStoppedSessionID == "" {
		m.lastStoppedSessionID = sid
	}
}

// ReapSession ends the bound session locally once its bridge has gone, and
// adopts a recorded session whose bridge survived an agent restart. Safe to
// call at any time; it serialises with Start/Stop on mu.
func (m *Manager) ReapSession(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return
	}
	if m.currentSessionID == "" {
		m.bridgeGoneCount = 0
		m.recoverSessionLocked()
		return
	}
	if m.bridgeRunningFn() {
		m.bridgeGoneCount = 0
		return
	}
	m.bridgeGoneCount++
	if m.bridgeGoneCount < reapConfirmations {
		return
	}
	m.bridgeGoneCount = 0
	sid := m.currentSessionID
	m.hostingGeneration++
	// Warm engine: the bridge is gone and the engine is meant to stay up, so
	// there is nothing to stop (and stopFn's by-name bridge kill must not run
	// on a process-snapshot verdict). Per-session engines (warm disabled,
	// BYOD) are stopped exactly as a stop_solarbeam would.
	if !m.warmEngineEnabled() {
		if err := m.stopFn(); err != nil {
			slog.Warn("solarbeam: bridge gone but session cleanup failed; will retry",
				"phase", "session_reap_failed", "session_id", sid, "err", err)
			return
		}
	}
	if err := m.cleanupStoppedLinkAuth(); err != nil {
		slog.Warn("solarbeam: link credential cleanup failed; will retry")
		return
	}
	m.currentSessionID = ""
	m.lastStoppedSessionID = sid
	m.clearSessionRecordLocked()
	m.notifySessionChangeLocked()
	slog.Info("solarbeam: bridge exited; session ended locally",
		"phase", "session_reaped", "session_id", sid)
}

// RunSessionReaper calls ReapSession every interval until ctx ends. The first
// pass runs immediately so a session orphaned by an agent restart is adopted
// (or cleared) at startup.
func (m *Manager) RunSessionReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultReapInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.ReapSession(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// BridgeExitMeaning names the solarbeam-livekit-bridge exit codes for logs.
func BridgeExitMeaning(code int) string {
	switch code {
	case 0:
		return "exited"
	case 1:
		return "failed or was terminated"
	case 3:
		return "session ended (room deleted/closed, removed, or authority ended)"
	case 4:
		return "LiveKit connection lost"
	default:
		return "unexpected exit"
	}
}
