package solarbeam

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// defaultBridgeStatusURL is the bridge's loopback status endpoint, served
// off the same 127.0.0.1:9092 listener that already carries the engine's
// /bridge WebSocket route.
const defaultBridgeStatusURL = "http://127.0.0.1:9092/status"

// bridgeStatusTimeout bounds the /status probe. Deliberately short: the
// callers are an agent self-update and an engine install deciding whether
// they may proceed, and a WEDGED bridge must not stall them — it must fail
// fast and be treated as "session active" (see SessionActive). Anything
// longer just converts a wedge into a hang.
const bridgeStatusTimeout = 2 * time.Second

// maxBridgeStatusBytes caps the /status body we will read. The real
// response is a couple of hundred bytes; the cap stops a misbehaving or
// impersonating local listener from feeding us an unbounded body.
const maxBridgeStatusBytes = 64 * 1024

// bridgeStatus mirrors the bridge's GET /status response:
//
//	{"engine_connected":true,"livekit_connected":true,
//	 "viewers":2,"session_id":"...","uptime_seconds":137}
//
// Viewers is the count of REMOTE LiveKit participants — actual humans
// watching — read live from the room rather than hand-tracked.
//
// It is a *int, not an int, on purpose: a plain int cannot tell
// `{"viewers":0}` (a well-formed answer meaning "nobody is watching",
// which permits an update) apart from `{}` or any other JSON object that
// happens to decode without error but carries no viewer count at all
// (which must NOT permit an update). Absent ⇒ nil ⇒ malformed ⇒ active.
type bridgeStatus struct {
	ConfirmedEmptySeconds *int64         `json:"confirmed_empty_seconds"`
	PresenceKnown         *bool          `json:"viewer_presence_known"`
	Remote                []RemoteViewer `json:"remote_viewers"`
	EngineConnected       bool           `json:"engine_connected"`
	LiveKitConnected      bool           `json:"livekit_connected"`
	Viewers               *int           `json:"viewers"`
	// VideoFramesTotal is the bridge's monotonic count of relayed video frames
	// (optional: older bridges omit it, which disables the watchdog stall trigger).
	VideoFramesTotal *uint64 `json:"video_frames_total"`
	SessionID        string  `json:"session_id"`
	UptimeSeconds    int     `json:"uptime_seconds"`
}

// SessionActive reports whether a SolarBeam session is in progress on this
// machine, for the interlock that refuses an agent self-update or a
// SolarBeam engine install mid-session.
//
// ⚠ This is the change that makes a warm engine safe to ship. Before the
// warm-engine work the interlock asked "is the engine process running?",
// which was a fair proxy because the engine was killed and respawned per
// session. A warm engine is running permanently, so that proxy answers
// "active" forever and the machine would silently stop receiving agent
// updates and engine installs — the same class of silent stall as an
// empty signer fingerprint blocking a rollout with no error anywhere.
//
// So it is redefined as "is anyone actually connected", which is what the
// interlock was always protecting: a stream a young person is on. That is
// strictly more precise than the old proxy, not a loosening.
//
// Three layers, and every ambiguous layer resolves to ACTIVE:
//
//	| bridge process | /status                    | verdict            |
//	|----------------|----------------------------|--------------------|
//	| not running    | —                          | inactive           |
//	| running        | 200, viewers > 0           | ACTIVE             |
//	| running        | 200, viewers == 0          | inactive           |
//	| running        | unreachable / timeout      | ACTIVE (fail safe) |
//	| running        | non-200                    | ACTIVE (fail safe) |
//	| running        | malformed / no viewers key | ACTIVE (fail safe) |
//
// The controlling principle: an update applied mid-session interrupts a
// live session with a young person on it, so only a clear, well-formed
// "nobody is watching" may permit one. A wedged bridge, an old bridge
// that predates /status, a truncated body — all of those keep the update
// window shut, which is exactly how the agent behaves today.
//
// The process check has to be the OUTER gate: without it "cannot reach
// the endpoint" is indistinguishable from "there is no bridge because
// there is no session", and the fail-safe answer to the former would
// block updates forever.
//
// In ModeInteractive (BYOD) and on a venue PC reverted with
// `warm_engine: false`, the engine is still stopped at session end, so
// engine liveness remains a truthful session signal and is kept
// unchanged — reverting the kill switch reverts this too, in one lever.
func (m *Manager) SessionActive(ctx context.Context) bool {
	if !m.warmEngineEnabled() {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.statusFn()
	}

	// Layer 1 — no bridge process, no session. The only branch that can
	// answer "inactive" without hearing from the bridge itself.
	if !m.bridgeRunningFn() {
		return false
	}

	// Layers 2 and 3 — ask the bridge who is watching.
	viewers, err := m.bridgeViewers(ctx)
	if err != nil {
		slog.Warn("solarbeam: bridge status unreadable; treating session as active",
			"phase", "session_active_failsafe", "err", err)
		return true
	}
	return viewers > 0
}

// RemoteViewers reports how many remote LiveKit participants the bridge
// currently has, or an error if that cannot be established.
//
// It is deliberately NOT SessionActive. SessionActive answers "may I
// interrupt this machine", and in ModeInteractive it short-circuits to
// engine-process liveness, which on a BYOD box means "the engine is
// running" — true for the whole of a session that nobody has joined yet.
// The on-screen indicator needs the narrower, truthful question "is a
// human watching right now", on every mode, so it takes the viewer count
// directly.
//
// It also does NOT fail safe the way SessionActive does, and must not:
// SessionActive's ambiguity resolves to "active" because the cost of
// being wrong is interrupting a young person's session. Here the cost of
// being wrong is telling somebody they are being watched when they are
// not, which erodes the one signal this whole mechanism depends on. So
// ambiguity is returned as an error and the caller decides.
func (m *Manager) RemoteViewers(ctx context.Context) (int, error) {
	if !m.bridgeRunningFn() {
		return 0, nil
	}
	return m.bridgeViewers(ctx)
}

// bridgeViewers probes the bridge's /status endpoint and returns its
// remote-viewer count. Every failure mode — transport error, timeout,
// non-200, unreadable or malformed body, missing viewers field, negative
// count — is returned as an error, and SessionActive turns any error into
// "active". Nothing here may return (0, nil) unless the bridge genuinely
// said zero viewers.
func (m *Manager) readBridgeStatus(ctx context.Context) (bridgeStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, bridgeStatusTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.bridgeStatusURL, nil)
	if err != nil {
		return bridgeStatus{}, fmt.Errorf("build bridge status request: %w", err)
	}
	resp, err := m.statusClient.Do(req)
	if err != nil {
		return bridgeStatus{}, fmt.Errorf("get %s: %w", m.bridgeStatusURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Includes the 404 an engine MSI predating the /status route
		// serves, which is why the agent half must not ship ahead of the
		// engine half: until it does, this row keeps today's behaviour.
		return bridgeStatus{}, fmt.Errorf("bridge status: unexpected HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBridgeStatusBytes+1))
	if err != nil {
		return bridgeStatus{}, fmt.Errorf("read bridge status body: %w", err)
	}
	if len(body) > maxBridgeStatusBytes {
		return bridgeStatus{}, fmt.Errorf("bridge status: oversized response")
	}
	var st bridgeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return bridgeStatus{}, fmt.Errorf("decode bridge status: %w", err)
	}
	if st.Viewers == nil {
		return bridgeStatus{}, fmt.Errorf("bridge status: no viewers field")
	}
	if *st.Viewers < 0 {
		return bridgeStatus{}, fmt.Errorf("bridge status: negative viewer count %d", *st.Viewers)
	}
	return st, nil
}

func (m *Manager) bridgeViewers(ctx context.Context) (int, error) {
	st, err := m.readBridgeStatus(ctx)
	if err != nil {
		return 0, err
	}
	return *st.Viewers, nil
}
