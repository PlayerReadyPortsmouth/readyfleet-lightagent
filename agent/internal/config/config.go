// Package config loads agent configuration from disk.
//
// On Windows the config file lives at:
//
//	%PROGRAMDATA%\fleetmgr\config.json
//
// In dev / fake-agent mode it falls back to ./fleetmgr-config.json next
// to the binary, so a developer can iterate without touching system
// directories.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/winpaths"
)

// Config is the on-disk agent configuration.
type Config struct {
	// VenueCompanion overrides managed Windows tray provisioning. Absent uses
	// the interactive console account; enabled:false disables provisioning and
	// the status pipe. Existing files/configuration are never rewritten here.
	VenueCompanion *VenueCompanion `json:"venue_companion,omitempty"`
	// ServerURL is the wss:// URL of the C2 agent listener.
	ServerURL string `json:"server_url"`

	// EnrollmentURL is the https:// URL of the C2 enrollment endpoint.
	// Falls back to ServerURL with /enroll/v1/redeem appended if empty.
	EnrollmentURL string `json:"enrollment_url,omitempty"`

	// EnrollmentSecret is the bootstrap credential the agent presents to
	// /enroll/v1/redeem as a Bearer token. Accepts either:
	//   - rfk_... a long-lived venue API key (legacy / scripted path), or
	//   - rft_... a single-use enrollment token (the standard flow).
	// The server dispatches by prefix; the agent treats this field as
	// opaque. Cleared from disk after a successful enrollment.
	//
	// JSON key is `enrollment_secret`. The legacy key `venue_api_key` is
	// accepted on unmarshal for one release to preserve existing on-disk
	// configs; UnmarshalJSON below handles the alias.
	EnrollmentSecret string `json:"enrollment_secret,omitempty"`

	// IntendedLabel optionally pre-fills the machine label, set at install
	// time by the IT admin. Cleared after enrollment.
	IntendedLabel string `json:"intended_label,omitempty"`

	// CertPath is the path to the client cert PEM file. Set after
	// enrollment.
	CertPath string `json:"cert_path,omitempty"`

	// KeyPath is the path to the private key. Set after enrollment.
	KeyPath string `json:"key_path,omitempty"`

	// CAPath is the path to the internal CA cert PEM. Set after enrollment.
	CAPath string `json:"ca_path,omitempty"`

	// MachineID is the UUID issued by the server at enrollment.
	MachineID string `json:"machine_id,omitempty"`

	// VenueID is the UUID of the venue this machine belongs to.
	VenueID string `json:"venue_id,omitempty"`

	// OwnerActorID is the ReadyApp actor (Entra OID) this machine belongs
	// to, copied from the controller's machines.owner_actor_id in the
	// enrollment response. On a BYOD light agent that is the mentor whose
	// personal PC this is.
	//
	// Its one job is the consent gate (agent/internal/consent): when a
	// connection request names a requester, the agent compares it to this
	// value to tell "the owner is connecting to their own device" (no
	// prompt — staff have unattended access to their own machine) from
	// "somebody else is asking" (prompt, and nothing proceeds until they
	// answer).
	//
	// Empty on a machine that enrolled before this field existed, or with
	// no actor attribution. Empty never matches anyone, so an agent with
	// no owner recorded prompts for every request — the fail-closed
	// direction. Written once at enrollment; if an operator edits it by
	// hand they are editing who this device answers to, which is exactly
	// the local-operator posture warm_engine already has.
	OwnerActorID string `json:"owner_actor_id,omitempty"`

	// WarmEngine is the per-machine kill switch for the managed agent's
	// warm-engine behaviour: preloading the SolarBeam engine at agent
	// start, skipping the per-session engine restart when the engine is
	// already healthy, and leaving the engine running at session end
	// (see agent/internal/solarbeam's warmEngineEnabled).
	//
	// A pointer so an ABSENT key means "the shipped default" rather than
	// Go's zero value: warm-engine is ON by default on ModeManaged venue
	// PCs, and an operator reverts one machine by writing
	// `"warm_engine": false` into config.json and restarting the service —
	// no agent redeploy, no controller round-trip. It is on-disk only and
	// never accepted from the wire, same posture as every other local
	// operator control here.
	//
	// No effect on the BYOD light agent, which runs ModeInteractive and
	// never goes warm at all (2026-08-20 warm-engine design, §5.1).
	WarmEngine *bool `json:"warm_engine,omitempty"`

	// SolarbeamWatchdog turns ON the host watchdog that restarts a wedged
	// SolarBeam engine (no frames for 60 s with a viewer connected, or a
	// helper-pipe loss that never recovers) through the preload path.
	// OFF unless present and true: canary per machine by writing
	// `"solarbeam_watchdog": true` into config.json and restarting the service.
	SolarbeamWatchdog *bool `json:"solarbeam_watchdog,omitempty"`

	// ViewingBar turns ON the "Staff are viewing this screen" bar drawn by the
	// venue companion. OFF unless present and true (default off since 29 Oct
	// 2026). The companion cannot read this file, so the service republishes it
	// in the status snapshot; restart the agent service after changing it.
	ViewingBar *bool `json:"viewing_bar,omitempty"`

	// PrivacyMode is the per-machine kill switch for SolarBeam privacy mode
	// (full-screen notice + optional local input lock). Allowed unless present
	// and false; false refuses locally and reports it. Restart the service
	// after changing it.
	PrivacyMode *bool `json:"privacy_mode,omitempty"`

	// ActivitySignals is the local kill switch for activity signals on a
	// managed Windows venue PC (internal/usagestats): false stops the
	// in-session helper entirely, so no attended state, no foreground app and
	// no DNS counts. Absent or true leaves the helper running for attended
	// state; foreground and DNS stay off unless the C2 turns them on for a
	// staff device (apply_activity_signals). On-disk only, never from the
	// wire; restart the service after changing it.
	ActivitySignals *bool `json:"activity_signals,omitempty"`
	// UsageStats is the pre-0.63 name of ActivitySignals, read as an alias
	// for one release (ActivitySignals wins when both are set). It no longer
	// turns foreground sampling on: that is the C2's staff-device toggle.
	UsageStats *bool `json:"usage_stats,omitempty"`

	// ElevationMode arms the WATCH-ONLY admin-prompt observer
	// (internal/elevation). Only the exact string "watch" does anything;
	// absent, "", "off", "broker" (phase 2, not built) and unknown values are
	// off. On-disk only, never from the wire; restart the service after
	// changing it. Runbook: docs/runbooks/elevation-watch.md.
	ElevationMode string `json:"elevation_mode,omitempty"`

	// ElevationRPCSpike additionally records Microsoft-Windows-RPC AppInfo
	// events near each prompt to elevation-rpc-spike.jsonl (research only,
	// never used for attribution). Off unless true AND ElevationMode is watch.
	ElevationRPCSpike bool `json:"elevation_rpc_spike,omitempty"`

	// SolarbeamLinkAuthMode selects warn (initial rollout) or enforce for both
	// children. Set per machine or distribute to a reviewed cohort; restart the
	// agent after changing it. The managed engine service restarts when needed.
	SolarbeamLinkAuthMode string `json:"solarbeam_link_auth_mode,omitempty"`

	// SolarbeamRestrictedBridge runs the managed SolarBeam bridge as a
	// restricted Low-integrity LocalService process instead of LocalSystem.
	// Absent ⇒ false (today's behaviour). On-disk only, never from the wire;
	// restart the agent after changing it. Runbook:
	// docs/runbooks/solarbeam-bridge-least-privilege.md.
	SolarbeamRestrictedBridge bool `json:"solarbeam_restricted_bridge,omitempty"`

	// ComputeEnabled turns on ReadyCloudCompute polling (agent/internal/
	// compute). Absent => false. On-disk only, never from the wire. Even when
	// on, nothing runs until the controller has this machine in its compute
	// pool, the global switch is on and an approved connector has a job.
	ComputeEnabled bool `json:"compute_enabled,omitempty"`

	// SessionIndicator turns on the on-screen "your screen is being
	// viewed remotely" bar (agent/internal/sessionindicator) for the whole
	// time a remote viewer is connected.
	//
	// A pointer, and ABSENT ⇒ OFF. This ships inert, which in this repo is
	// what a feature flag would be elsewhere. The reason is platform-
	// specific and worth stating:
	//
	//   - Windows needs nothing else: the bar excludes itself from capture
	//     with SetWindowDisplayAffinity, so turning it on is safe the
	//     moment the agent carrying it lands.
	//   - Linux CANNOT exclude a window from a KWin capture at all. The
	//     bar is kept out of the stream by the ENGINE reserving a band at
	//     the top of the captured region (`capture_reserve_top`). An
	//     engine build without that key streams the bar straight to the
	//     viewer, which is worse than showing nothing, because it tells
	//     the remote party whether the local person has been alerted. So
	//     this must not be turned on for a Linux machine until an engine
	//     that honours capture_reserve_top has converged onto it.
	//
	// On-disk only, never accepted from the wire — same posture as
	// WarmEngine.
	SessionIndicator *bool `json:"session_indicator,omitempty"`

	// RequireSessionIndicator decides what happens when the indicator is
	// enabled but CANNOT be shown — no Wayland, an X11 session, no
	// zwlr_layer_shell_v1, or SetWindowDisplayAffinity failing on a
	// Windows build older than 10 2004.
	//
	// It does NOT fire when the indicator is deliberately SKIPPED because
	// the machine is locked or its displays are asleep. That is not an
	// inability to show, and treating it as one would turn every locked
	// BYOD machine into a machine that refuses support — see
	// sessionindicator.Decide and the skip-rule section of that package's
	// doc.
	//
	// ⚠ THIS IS AN OPEN POLICY DECISION, deliberately left at the
	// permissive default rather than chosen here:
	//
	//   - true — refuse to host the session. The honest privacy posture:
	//     nobody is ever watched without the machine saying so. Its cost
	//     is that a compositor quirk or an old Windows build turns "no
	//     indicator" into "no support session", on precisely the machine
	//     that already has something wrong with it.
	//   - false (absent ⇒ this) — log the failure loudly and host the
	//     session anyway. Support keeps working and the failure is
	//     auditable, but a session can then run with no local indication,
	//     which is the status quo the indicator exists to end.
	//
	// Absent ⇒ false. Flipping it is a one-key edit plus a restart, no
	// redeploy, exactly like WarmEngine.
	RequireSessionIndicator *bool `json:"require_session_indicator,omitempty"`

	// MenuPrompts (macOS light agent only) routes connection requests to
	// the menu-bar helper (agent/internal/menuhelper, "ReadyFleet Menu.app")
	// so the person at the Mac is asked Approve / Decline, instead of the
	// darwin default of declining every request because the build has
	// nothing to ask with.
	//
	// A pointer, and ABSENT ⇒ OFF: decline-all stays until Aurora decides
	// ask-the-owner is wanted on Macs (brief
	// ops/byod-device-names-and-mac-tray-2026-11-08, section 2). With the
	// key on and no helper running or connected, requests are still
	// declined, with the reason logged — the helper is how the question is
	// asked, never a reason to answer it. On-disk only, never from the
	// wire; takes effect at the next agent start, like WarmEngine.
	MenuPrompts *bool `json:"menu_prompts,omitempty"`

	// SolarbeamHost (macOS light agent only) turns the Mac from watch-only
	// into a SolarBeam host: with it on, and the engine app and bridge
	// present under SolarbeamDir, start_solarbeam launches them as the
	// agent's own children (agent/internal/solarbeam, manager_darwin.go).
	//
	// A pointer, and ABSENT ⇒ OFF. Off, every start is refused with a
	// message naming this key and nothing is ever launched, whatever files
	// are installed. On-disk only, never from the wire; takes effect at the
	// next agent start. Windows and Linux ignore it: hosting is what those
	// agents are for. Plan: ops/mac-mini-2026-11-08/SOLARBEAM-HOST-PLAN.md.
	SolarbeamHost *bool `json:"solarbeam_host,omitempty"`

	// SolarbeamDir (macOS) overrides where the host files live. Absent ⇒
	// ~/Library/Application Support/ReadyFleet/solarbeam. The engine app,
	// bridge and child logs default to paths under it.
	SolarbeamDir string `json:"solarbeam_dir,omitempty"`

	// SolarbeamEngineApp (macOS) is the engine .app bundle. Absent ⇒
	// <SolarbeamDir>/Vibeshine.app. The executable run is
	// Contents/MacOS/<CFBundleExecutable> from its Info.plist.
	SolarbeamEngineApp string `json:"solarbeam_engine_app,omitempty"`

	// SolarbeamBridge (macOS) is the bridge binary. Absent ⇒
	// <SolarbeamDir>/solarbeam-bridge.
	SolarbeamBridge string `json:"solarbeam_bridge,omitempty"`
}

type VenueCompanion struct {
	Enabled bool   `json:"enabled"`
	CYPSID  string `json:"cyp_sid"`
	// ServiceNames is the locally reviewed set of mode-owned services to query.
	// No service control operations are implemented by this preview.
	ServiceNames []string `json:"service_names,omitempty"`
	// ModeSwitchIcon provisions the protected public "Switch AP ↔ VR" desktop
	// shortcut (opens the mode picker). Absent ⇒ false: the icon is added
	// only to PCs being prepared for operating mode enforcement, never as a
	// side effect of the tray. Turning it off leaves an existing icon alone.
	ModeSwitchIcon bool `json:"mode_switch_icon,omitempty"`
}

// SessionIndicatorEnabled reports whether the on-screen viewer indicator
// runs on this machine. Absent key ⇒ disabled; see the field doc.
func (c Config) SessionIndicatorEnabled() bool {
	return c.SessionIndicator != nil && *c.SessionIndicator
}

// MenuPromptsEnabled reports whether connection requests go to the macOS
// menu-bar helper. Absent ⇒ false (decline-all, the shipped default).
func (c Config) MenuPromptsEnabled() bool {
	return c.MenuPrompts != nil && *c.MenuPrompts
}

// SolarbeamHostEnabled reports whether this Mac may host SolarBeam sessions.
// Absent ⇒ false (watch-only, the shipped default).
func (c Config) SolarbeamHostEnabled() bool {
	return c.SolarbeamHost != nil && *c.SolarbeamHost
}

// SessionIndicatorRequired reports whether a session may proceed when the
// indicator cannot be shown. Absent key ⇒ not required (log only); see
// the RequireSessionIndicator field doc for the trade-off.
func (c Config) SessionIndicatorRequired() bool {
	return c.RequireSessionIndicator != nil && *c.RequireSessionIndicator
}

// WarmEngineEnabled reports whether warm-engine behaviour is enabled on
// this machine. Absent key ⇒ enabled, the shipped default; see the
// WarmEngine field doc.
func (c Config) WarmEngineEnabled() bool {
	if c.WarmEngine == nil {
		return true
	}
	return *c.WarmEngine
}

// ActivitySignalsEnabled reports the local kill switch: ON unless
// activity_signals (or its alias usage_stats) is explicitly false.
func (c Config) ActivitySignalsEnabled() bool {
	switch {
	case c.ActivitySignals != nil:
		return *c.ActivitySignals
	case c.UsageStats != nil:
		return *c.UsageStats
	}
	return true
}

// SolarbeamWatchdogEnabled reports the per-machine watchdog switch (default OFF).
func (c Config) SolarbeamWatchdogEnabled() bool {
	return c.SolarbeamWatchdog != nil && *c.SolarbeamWatchdog
}

// PrivacyModeAllowed reports the privacy-mode kill switch (default allowed).
func (c Config) PrivacyModeAllowed() bool { return c.PrivacyMode == nil || *c.PrivacyMode }

// ViewingBarEnabled reports the per-machine viewing-bar switch (default OFF).
func (c Config) ViewingBarEnabled() bool { return c.ViewingBar != nil && *c.ViewingBar }

// DefaultPath returns the canonical config path for the current platform.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		return filepath.Join(winpaths.ProgramData(), "fleetmgr", "config.json")
	default:
		// Dev / fake-agent fallback. Production agents only ship for
		// Windows.
		return "fleetmgr-config.json"
	}
}

// Load reads and parses the config file at path. A missing file is not
// an error — it returns a zero Config and the caller can decide whether
// to enter the enrollment flow.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("agent/config: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("agent/config: parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path atomically. The parent directory must exist; on
// Windows it is created during MSI install.
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("agent/config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("agent/config: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("agent/config: rename: %w", err)
	}
	return nil
}

// IsEnrolled reports whether c contains the post-enrollment material
// needed to connect via mTLS.
func (c Config) IsEnrolled() bool {
	return c.MachineID != "" && c.CertPath != "" && c.KeyPath != ""
}

// UnmarshalJSON implements json.Unmarshaler so Config accepts either
// `enrollment_secret` (preferred) or the legacy `venue_api_key` JSON
// key. When both are present, `enrollment_secret` wins. Drop the
// legacy branch after one release.
func (c *Config) UnmarshalJSON(data []byte) error {
	type configAlias Config // shadow to avoid recursion
	aux := struct {
		LegacyVenueAPIKey string `json:"venue_api_key,omitempty"`
		*configAlias
	}{configAlias: (*configAlias)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Legacy key only wins if the new key was not supplied.
	if c.EnrollmentSecret == "" && aux.LegacyVenueAPIKey != "" {
		c.EnrollmentSecret = aux.LegacyVenueAPIKey
	}
	return nil
}
