package solarbeam

// macOS SolarBeam host: the policy half.
//
// Everything in this file is pure (paths, parsing, classification, the
// restart policy and the capability report) so it compiles and is tested on
// every platform, including the Linux CI runner. The process half — exec,
// signals, pipes — lives in manager_darwin.go and friends behind the darwin
// build tag. SOLARBEAM-HOST-PLAN.md (ops/mac-mini-2026-11-08) Phase B item 3,
// pulled into Phase A.
//
// Layout on the Mac, all under the agent's own Application Support directory
// so one `rm -rf` and a config edit is the whole rollback:
//
//	~/Library/Application Support/ReadyFleet/solarbeam/
//	    Vibeshine.app/           the engine, a signed .app bundle (manual install)
//	    solarbeam-bridge         the LiveKit bridge, a darwin/arm64 binary
//	    logs/engine.log          size-capped rotating child logs (childlog.go)
//	    logs/bridge.log
//
// Nothing here runs unless config.json says `"solarbeam_host": true` AND the
// engine bundle and bridge are present and executable. A Mac without both is
// the watch-only light agent of docs/macos-light-agent.md, unchanged.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// DarwinEngineAppName is the engine bundle's default name. The real name is
// whatever the engine build produces (the fork's Info.plist says Vibeshine);
// config.json `solarbeam_engine_app` points elsewhere if it changes.
const DarwinEngineAppName = "Vibeshine.app"

// DarwinBridgeName is the bridge binary's default file name.
const DarwinBridgeName = "solarbeam-bridge"

// DarwinSolarbeamDir is where the Mac keeps its SolarBeam host files, a
// subdirectory of the light agent's own data directory (lightinstall.DarwinPaths).
func DarwinSolarbeamDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "ReadyFleet", "solarbeam")
}

// DarwinLogDir is the child-log directory under the SolarBeam directory.
func DarwinLogDir(solarbeamDir string) string { return filepath.Join(solarbeamDir, "logs") }

// DarwinEngineExecutable is the Mach-O inside an .app bundle. exe is the
// bundle's CFBundleExecutable; when unknown, pass "" and the bundle's own
// base name (Vibeshine.app -> Vibeshine) is used, which is Apple's default.
func DarwinEngineExecutable(appPath, exe string) string {
	if exe == "" {
		exe = strings.TrimSuffix(filepath.Base(appPath), ".app")
	}
	return filepath.Join(appPath, "Contents", "MacOS", exe)
}

// DarwinInfoPlistPath is the bundle's property list.
func DarwinInfoPlistPath(appPath string) string {
	return filepath.Join(appPath, "Contents", "Info.plist")
}

// darwinAppBundleOf walks up from an executable inside Contents/MacOS to the
// .app directory, or returns "" when the path is not inside a bundle.
func darwinAppBundleOf(executable string) string {
	macos := filepath.Dir(executable)
	contents := filepath.Dir(macos)
	app := filepath.Dir(contents)
	if filepath.Base(macos) != "MacOS" || filepath.Base(contents) != "Contents" || !strings.HasSuffix(app, ".app") {
		return ""
	}
	return app
}

// infoPlist is the subset of an XML property list we read. Apple's plist is a
// flat <dict> of alternating <key> and value elements; we only need strings.
type infoPlist struct {
	Executable   string
	ShortVersion string
	Version      string
	Identifier   string
}

// parseInfoPlist reads CFBundleExecutable, CFBundleShortVersionString,
// CFBundleVersion and CFBundleIdentifier from an XML Info.plist. A binary
// plist (bplist00) is refused with a clear error rather than misread; the
// engine build writes XML, and `plutil -convert xml1` fixes a binary one.
func parseInfoPlist(r io.Reader) (infoPlist, error) {
	var out infoPlist
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return out, err
	}
	if strings.HasPrefix(string(data), "bplist") {
		return out, errors.New("Info.plist is a binary plist; expected XML")
	}
	var doc struct {
		Dict struct {
			Items []struct {
				XMLName xml.Name
				Value   string `xml:",chardata"`
			} `xml:",any"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return out, fmt.Errorf("parse Info.plist: %w", err)
	}
	items := doc.Dict.Items
	for i := 0; i+1 < len(items); i++ {
		if items[i].XMLName.Local != "key" {
			continue
		}
		value := items[i+1]
		if value.XMLName.Local != "string" {
			continue
		}
		switch strings.TrimSpace(items[i].Value) {
		case "CFBundleExecutable":
			out.Executable = strings.TrimSpace(value.Value)
		case "CFBundleShortVersionString":
			out.ShortVersion = strings.TrimSpace(value.Value)
		case "CFBundleVersion":
			out.Version = strings.TrimSpace(value.Value)
		case "CFBundleIdentifier":
			out.Identifier = strings.TrimSpace(value.Value)
		}
		i++
	}
	if out.Executable == "" {
		return out, errors.New("Info.plist has no CFBundleExecutable")
	}
	return out, nil
}

// HostCapabilities is what this host can actually do, as the manager knows
// it. The viewer greys out what is false and the bridge refuses the message;
// a stub that reports true for something the platform cannot do is a bug
// (plan section 6, "capability honesty"). The platform-neutral agent never
// consults this; the darwin manager logs it, exposes it through
// HostAvailability, and keeps the bridge environment consistent with it.
type HostCapabilities struct {
	// Gamepad: a virtual pad on the host (ViGEm on Windows, uinput on Linux).
	Gamepad bool `json:"gamepad"`
	// PenTouch: pen and multi-touch injection.
	PenTouch bool `json:"pen_touch"`
	// HostAudioMode: staff host+stream / stream-only switching.
	HostAudioMode bool `json:"host_audio_mode"`
	// Clipboard: staff-only clipboard sync.
	Clipboard bool `json:"clipboard"`
	// DisplayModes: virtual display, rotation and refresh changes.
	DisplayModes bool `json:"display_modes"`
	// Consent: a local owner-consent prompt the bridge can ask through.
	Consent bool `json:"consent"`
	// PrivacyMode: blank the physical monitors while a remote viewer is on.
	PrivacyMode bool `json:"privacy_mode"`
	// SessionIndicator: an on-screen "session live" signal the local person
	// can see and the viewer cannot.
	SessionIndicator bool `json:"session_indicator"`
}

// DarwinHostCapabilities is the Mac host today: picture, system audio,
// keyboard and mouse through the engine; nothing else. The session indicator
// is the menu bar's red LIVE label drawn by ReadyFleet Menu.app, which the
// engine's capture does include (the menu bar is on screen), so it is a
// signal to the local person, not a capture-excluded bar.
func DarwinHostCapabilities() HostCapabilities {
	return HostCapabilities{SessionIndicator: true}
}

// Unsupported lists the false capabilities by name, for logs and error text.
func (c HostCapabilities) Unsupported() []string {
	var out []string
	for _, f := range []struct {
		name string
		ok   bool
	}{
		{"gamepad", c.Gamepad}, {"pen_touch", c.PenTouch}, {"host_audio_mode", c.HostAudioMode},
		{"clipboard", c.Clipboard}, {"display_modes", c.DisplayModes}, {"consent", c.Consent},
		{"privacy_mode", c.PrivacyMode}, {"session_indicator", c.SessionIndicator},
	} {
		if !f.ok {
			out = append(out, f.name)
		}
	}
	return out
}

// darwinCapabilityOptInVars are bridge/engine environment switches that
// ADVERTISE a host capability to the viewer or ReadyApp. None of them may
// reach the children on a Mac, however they got into the agent's environment
// (a hand-edited LaunchAgent plist, a shell that started the agent for a
// test): the bridge would then offer pen/touch, pad motion, display changes
// or multi-display to a viewer the engine cannot serve.
var darwinCapabilityOptInVars = map[string]bool{
	"SOLARBEAM_INPUT_POINTER":   true, // pen + touch (bridge README)
	"SOLARBEAM_INPUT_MOTION":    true, // DS4/DualSense motion
	"SOLARBEAM_DISPLAY_CONSENT": true, // owner consent prompt for display changes
	"SOLARBEAM_MULTI_DISPLAY":   true, // multi-display grants
}

// darwinChildEnvironment removes the capability opt-ins (and, defensively,
// any link-auth variable: the manager adds its own) from an environment
// about to be handed to the engine or the bridge. Tuning variables
// (SOLARBEAM_ABR_*, audio canaries) pass through: the bridge README documents
// setting them on the per-user agent's environment.
func darwinChildEnvironment(base []string) []string {
	out := make([]string, 0, len(base))
	for _, entry := range withoutLinkAuth(base) {
		key, _, _ := strings.Cut(entry, "=")
		if darwinCapabilityOptInVars[strings.ToUpper(key)] {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Engine startup verdicts on macOS.
//
// The engine (upstream Sunshine's macOS layer) checks Screen Recording in
// platf::init() with CGPreflightScreenCaptureAccess, logs the two lines
// below, calls CGRequestScreenCaptureAccess (which raises the macOS prompt
// the first time) and then CONTINUES: main() only logs "Platform failed to
// initialize" and carries on into bridge mode with no capture. Left alone
// that is a session that "started" and shows nothing, so these lines decide
// the launch immediately instead. Accessibility (CGEvent injection) has no
// check in the engine today; the line is a contract with the engine build
// (docs/macos-light-agent.md, "What the engine must do"), classified here so
// the agent is ready the day it appears.
const (
	darwinEngineReadyMarker = "SolarBeam: entering bridge mode"

	// ReasonScreenRecording is the availability reason for a missing Screen
	// Recording grant. The exact text is part of the C2-visible error.
	ReasonScreenRecording = "Screen Recording not granted"
	// ReasonAccessibility is the availability reason for a missing
	// Accessibility grant.
	ReasonAccessibility = "Accessibility not granted"
	// ReasonNoCapture is the generic capture failure the engine itself reports.
	ReasonNoCapture = "engine found no display or encoder"
)

// darwinEngineLineClass is the verdict a single engine log line carries.
type darwinEngineLineClass int

const (
	darwinLineNone darwinEngineLineClass = iota
	darwinLineReady
	darwinLineFatal
)

// darwinFatalMarkers maps a substring of the engine's own output to the
// reason the agent reports. Matching is case-sensitive on the engine's exact
// wording except where noted.
var darwinFatalMarkers = []struct {
	marker string
	reason string
	fold   bool
}{
	{marker: "No screen capture permission!", reason: ReasonScreenRecording},
	{marker: "'Privacy' -> 'Screen Recording'", reason: ReasonScreenRecording},
	{marker: "Platform failed to initialize", reason: ReasonScreenRecording},
	{marker: "accessibility permission", reason: ReasonAccessibility, fold: true},
	{marker: "accessibility not granted", reason: ReasonAccessibility, fold: true},
	{marker: "AXIsProcessTrusted", reason: ReasonAccessibility},
	{marker: "Unable to find display or encoder during startup", reason: ReasonNoCapture},
}

// classifyDarwinEngineLine is the whole startup decision for one line, kept
// pure so the marker set is tested without spawning anything.
func classifyDarwinEngineLine(line string) (darwinEngineLineClass, string) {
	if strings.Contains(line, darwinEngineReadyMarker) {
		return darwinLineReady, ""
	}
	folded := strings.ToLower(line)
	for _, m := range darwinFatalMarkers {
		if m.fold {
			if strings.Contains(folded, strings.ToLower(m.marker)) {
				return darwinLineFatal, m.reason
			}
			continue
		}
		if strings.Contains(line, m.marker) {
			return darwinLineFatal, m.reason
		}
	}
	return darwinLineNone, ""
}

// isPermissionReason reports whether a reason names a TCC grant the person
// at the Mac has to give by hand. Restarting cannot fix those, so the
// supervisor stops at once instead of using its restart budget.
func isPermissionReason(reason string) bool {
	return reason == ReasonScreenRecording || reason == ReasonAccessibility
}

// engineRestartPolicy bounds the per-session engine supervisor
// (manager_darwin.go). An engine that dies mid-session while the bridge is
// still up is relaunched after a growing delay, at most MaxRestarts times per
// session; after that the session is ended locally with the last reason, so
// the C2 sees a stopped session and a reason instead of a flapping one.
type engineRestartPolicy struct {
	MaxRestarts int
	Base        time.Duration
	Cap         time.Duration
}

// defaultEngineRestartPolicy: three relaunches, 2 s, 4 s, 8 s apart.
var defaultEngineRestartPolicy = engineRestartPolicy{MaxRestarts: 3, Base: 2 * time.Second, Cap: 30 * time.Second}

// Allow reports whether relaunch number attempt (1-based) is within budget.
func (p engineRestartPolicy) Allow(attempt int) bool {
	return attempt >= 1 && attempt <= p.MaxRestarts
}

// Delay is the wait before relaunch number attempt (1-based): Base doubled
// per attempt, never above Cap.
func (p engineRestartPolicy) Delay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := p.Base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.Cap {
			return p.Cap
		}
	}
	if d > p.Cap {
		return p.Cap
	}
	return d
}

// faultCooldown is how long a recorded permission fault refuses the next
// start outright. After it, a start is allowed to try again: the person may
// have granted the permission since, and nothing tells the agent when they
// do. Short enough that a fix is picked up on the next attempt, long enough
// that a C2 retry loop cannot turn into a prompt storm.
const faultCooldown = 60 * time.Second

// hostFault is the last reason the engine could not host, with its time.
type hostFault struct {
	Reason string
	At     time.Time
}

// blocks reports whether the fault is recent enough to refuse a start now.
func (f hostFault) blocks(now time.Time) bool {
	return f.Reason != "" && now.Sub(f.At) < faultCooldown
}

// HostAvailability is the manager's own statement of whether this machine
// can host a session right now, and why not when it cannot. On Windows and
// Linux it is always available (those agents exist to host); on a Mac it
// carries the opt-in, the installed files and the last permission fault.
type HostAvailability struct {
	// Enabled is the local opt-in (config.json `solarbeam_host` on a Mac).
	Enabled bool `json:"enabled"`
	// Available is true when a start_solarbeam would be attempted.
	Available bool `json:"available"`
	// Reason is empty when Available, else a short human-readable cause.
	Reason string `json:"reason,omitempty"`
	// Capabilities is what the host can do; see HostCapabilities.
	Capabilities HostCapabilities `json:"capabilities"`
}

// platformHostAvailability is set by a platform file's init when the platform
// has anything to say; nil means "always available, everything supported",
// which is the pre-existing Windows and Linux behaviour.
var platformHostAvailability func(*Manager) HostAvailability

// HostAvailability reports whether this machine can host now. Never blocks on
// a child process; it reads the manager's own state.
func (m *Manager) HostAvailability() HostAvailability {
	if platformHostAvailability != nil {
		return platformHostAvailability(m)
	}
	return HostAvailability{Enabled: true, Available: !m.hostingOff.Load(), Reason: m.hostingError, Capabilities: HostCapabilities{
		Gamepad: true, PenTouch: true, HostAudioMode: true, Clipboard: true, DisplayModes: true, Consent: true, PrivacyMode: true, SessionIndicator: true,
	}}
}

// SetSessionListener registers fn to be called (on its own goroutine) each
// time a session starts, stops, is reaped or is ended by the supervisor. The
// macOS menu-bar status loop uses it to publish session_live promptly instead
// of waiting for its next poll. One listener; nil clears it.
func (m *Manager) SetSessionListener(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionListener = fn
}

// notifySessionChangeLocked fires the listener. Caller holds mu; the listener
// runs detached so it can call Status() without deadlocking.
func (m *Manager) notifySessionChangeLocked() {
	if m.sessionListener != nil {
		go m.sessionListener()
	}
}

// darwinHostState is the macOS manager's per-Manager state. Declared on every
// platform because it is a field of Manager; only manager_darwin.go writes it.
type darwinHostState struct {
	// fault is the last reason the engine could not host (see hostFault).
	fault hostFault
	// restarts counts the supervisor's relaunches in the current session.
	restarts int
	// supervisorGen identifies the supervisor goroutine that owns the
	// current session; a stale goroutine compares and leaves.
	supervisorGen uint64
	// policy is the restart policy in force; zero means the default.
	policy engineRestartPolicy
	// linkEnv is the session's launch credential, kept for engine
	// relaunches (see launchBridge in manager_darwin.go). Cleared at stop.
	linkEnv []string
}

func (s *darwinHostState) restartPolicy() engineRestartPolicy {
	if s.policy.MaxRestarts == 0 && s.policy.Base == 0 {
		return defaultEngineRestartPolicy
	}
	return s.policy
}

// psEntry is one line of `ps -axww -o pid=,uid=,comm=`: pid, owning uid and
// the executable path as the process was started (argv[0] for exec.Command
// with an absolute path; the kernel's path for launchd children).
type psEntry struct {
	PID  int
	UID  int
	Comm string
}

// parsePSList parses that ps output. Hosting-off on a Mac has no /proc and no
// pidfd, so a survivor from an earlier agent is found this way and matched on
// the EXACT configured path and this user's uid only; a basename match or
// another user's process is never touched (same scope rule as Linux).
func parsePSList(out string) []psEntry {
	var entries []psEntry
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		var pid, uid int
		if _, err := fmt.Sscanf(fields[0], "%d", &pid); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(fields[1], "%d", &uid); err != nil {
			continue
		}
		// comm may contain spaces ("ReadyFleet Menu"); it is everything after
		// the two numeric columns.
		idx := strings.Index(line, fields[1])
		comm := strings.TrimSpace(line[idx+len(fields[1]):])
		entries = append(entries, psEntry{PID: pid, UID: uid, Comm: comm})
	}
	return entries
}

// survivorsOf picks the processes owned by uid whose executable is exactly one
// of paths.
func survivorsOf(entries []psEntry, uid int, paths []string) []psEntry {
	var out []psEntry
	for _, e := range entries {
		if e.UID != uid || e.PID <= 1 {
			continue
		}
		for _, p := range paths {
			if p != "" && filepath.IsAbs(p) && e.Comm == p {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// DarwinBundleExecutable reads CFBundleExecutable from an XML Info.plist.
func DarwinBundleExecutable(r io.Reader) (string, error) {
	plist, err := parseInfoPlist(r)
	if err != nil {
		return "", err
	}
	return plist.Executable, nil
}
