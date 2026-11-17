package solarbeam

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

func TestDarwinLayoutLivesUnderTheAgentDirectory(t *testing.T) {
	dir := DarwinSolarbeamDir("/Users/aurora")
	if dir != filepath.Join("/Users/aurora", "Library", "Application Support", "ReadyFleet", "solarbeam") {
		t.Fatalf("dir = %q", dir)
	}
	if got := DarwinLogDir(dir); got != filepath.Join(dir, "logs") {
		t.Errorf("log dir = %q", got)
	}
	app := filepath.Join(dir, DarwinEngineAppName)
	if got := DarwinEngineExecutable(app, "Vibeshine"); got != filepath.Join(app, "Contents", "MacOS", "Vibeshine") {
		t.Errorf("executable = %q", got)
	}
	// Unknown CFBundleExecutable falls back to Apple's default, the bundle name.
	if got := DarwinEngineExecutable(app, ""); got != filepath.Join(app, "Contents", "MacOS", "Vibeshine") {
		t.Errorf("default executable = %q", got)
	}
	if got := DarwinInfoPlistPath(app); got != filepath.Join(app, "Contents", "Info.plist") {
		t.Errorf("plist = %q", got)
	}
	if got := darwinAppBundleOf(filepath.Join(app, "Contents", "MacOS", "Vibeshine-0.0.0")); got != app {
		t.Errorf("bundle of executable = %q, want %q", got, app)
	}
	if got := darwinAppBundleOf("/usr/local/bin/sunshine"); got != "" {
		t.Errorf("bare binary reported a bundle %q", got)
	}
}

const testInfoPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key>
  <string>dev.lizardbyte.app.Sunshine</string>
  <key>CFBundleName</key>
  <string>SolarBeam</string>
  <key>CFBundleExecutable</key>
  <string>Vibeshine</string>
  <key>CFBundleShortVersionString</key>
  <string>2.1.18</string>
  <key>CFBundleVersion</key>
  <string>2118</string>
  <key>LSUIElement</key>
  <true/>
</dict>
</plist>`

func TestParseInfoPlistReadsTheEngineBundle(t *testing.T) {
	got, err := parseInfoPlist(strings.NewReader(testInfoPlist))
	if err != nil {
		t.Fatal(err)
	}
	want := infoPlist{Executable: "Vibeshine", ShortVersion: "2.1.18", Version: "2118", Identifier: "dev.lizardbyte.app.Sunshine"}
	if got != want {
		t.Fatalf("plist = %+v, want %+v", got, want)
	}
	if _, err := parseInfoPlist(strings.NewReader("bplist00\x00\x01")); err == nil || !strings.Contains(err.Error(), "binary plist") {
		t.Errorf("binary plist accepted: %v", err)
	}
	if _, err := parseInfoPlist(strings.NewReader(`<plist><dict><key>CFBundleName</key><string>X</string></dict></plist>`)); err == nil {
		t.Error("plist without CFBundleExecutable accepted")
	}
	exe, err := DarwinBundleExecutable(strings.NewReader(testInfoPlist))
	if err != nil || exe != "Vibeshine" {
		t.Errorf("DarwinBundleExecutable = %q, %v", exe, err)
	}
}

// The Mac advertises exactly what it can do. Every feature in plan section 3
// that the engine stubs out on macOS must be false here; a true that is not
// backed by an implementation is the bug the plan calls out.
func TestDarwinHostCapabilitiesAreHonest(t *testing.T) {
	caps := DarwinHostCapabilities()
	if caps.Gamepad || caps.PenTouch || caps.HostAudioMode || caps.Clipboard || caps.DisplayModes || caps.Consent || caps.PrivacyMode {
		t.Fatalf("a Mac claimed an unsupported capability: %+v", caps)
	}
	if !caps.SessionIndicator {
		t.Fatal("the menu-bar LIVE label is the Mac's session indicator and must be reported")
	}
	want := []string{"gamepad", "pen_touch", "host_audio_mode", "clipboard", "display_modes", "consent", "privacy_mode"}
	if got := caps.Unsupported(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Unsupported = %v, want %v", got, want)
	}
}

// Whatever is in the agent's environment, the children must not inherit a
// switch that advertises pen, touch, pad motion, display changes or
// multi-display to a viewer the Mac cannot serve. Tuning passes through.
func TestDarwinChildEnvironmentStripsCapabilityOptIns(t *testing.T) {
	in := []string{
		"PATH=/usr/bin", "HOME=/Users/aurora",
		"SOLARBEAM_INPUT_POINTER=1", "solarbeam_input_motion=1", "SOLARBEAM_DISPLAY_CONSENT=owner", "SOLARBEAM_MULTI_DISPLAY=1",
		"SOLARBEAM_LINK_SECRET=stale", "SOLARBEAM_LINK_AUTH_MODE=enforce",
		"SOLARBEAM_ABR_MAX_KBPS=6000", "SOLARBEAM_BRIDGE_AUDIO_STEREO=1",
	}
	want := []string{"PATH=/usr/bin", "HOME=/Users/aurora", "SOLARBEAM_ABR_MAX_KBPS=6000", "SOLARBEAM_BRIDGE_AUDIO_STEREO=1"}
	if got := darwinChildEnvironment(in); !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v, want %v", got, want)
	}
}

func TestClassifyDarwinEngineLine(t *testing.T) {
	cases := []struct {
		line   string
		class  darwinEngineLineClass
		reason string
	}{
		{"[2026-11-09 09:12:01.001]: Info: SolarBeam: entering bridge mode (SOLARBEAM_BRIDGE_WS=ws://127.0.0.1:9092/bridge)", darwinLineReady, ""},
		{"[2026-11-09 09:12:00.900]: Error: No screen capture permission!", darwinLineFatal, ReasonScreenRecording},
		{"Error: Please activate it in 'System Preferences' -> 'Privacy' -> 'Screen Recording'", darwinLineFatal, ReasonScreenRecording},
		{"Error: Platform failed to initialize", darwinLineFatal, ReasonScreenRecording},
		{"Error: SolarBeam: Accessibility permission is required for input injection", darwinLineFatal, ReasonAccessibility},
		{"Fatal: Unable to find display or encoder during startup", darwinLineFatal, ReasonNoCapture},
		{"Info: Found display: Built-in Retina Display", darwinLineNone, ""},
		{"Warning: solarbeam bridge: ws connect to ws://127.0.0.1:9092/bridge failed; retrying in 2s", darwinLineNone, ""},
	}
	for _, tc := range cases {
		class, reason := classifyDarwinEngineLine(tc.line)
		if class != tc.class || reason != tc.reason {
			t.Errorf("%q -> %v %q, want %v %q", tc.line, class, reason, tc.class, tc.reason)
		}
	}
	if !isPermissionReason(ReasonScreenRecording) || !isPermissionReason(ReasonAccessibility) || isPermissionReason(ReasonNoCapture) || isPermissionReason("") {
		t.Error("isPermissionReason misclassifies")
	}
}

func TestEngineRestartPolicyBoundsRelaunches(t *testing.T) {
	p := defaultEngineRestartPolicy
	for attempt, want := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 10: 30 * time.Second} {
		if got := p.Delay(attempt); got != want {
			t.Errorf("Delay(%d) = %s, want %s", attempt, got, want)
		}
	}
	if !p.Allow(1) || !p.Allow(3) || p.Allow(4) || p.Allow(0) {
		t.Error("Allow budget is not exactly three relaunches")
	}
	var zero darwinHostState
	if zero.restartPolicy() != defaultEngineRestartPolicy {
		t.Error("zero state must use the default policy")
	}
}

func TestHostFaultBlocksOnlyWithinTheCooldown(t *testing.T) {
	now := time.Now()
	f := hostFault{Reason: ReasonScreenRecording, At: now}
	if !f.blocks(now.Add(faultCooldown / 2)) {
		t.Error("a fresh fault must refuse a start")
	}
	if f.blocks(now.Add(faultCooldown + time.Second)) {
		t.Error("an old fault must let a start try again: the person may have granted the permission")
	}
	if (hostFault{}).blocks(now) {
		t.Error("no fault must not block")
	}
}

func TestParsePSListAndSurvivorScope(t *testing.T) {
	out := "  412   501 /Users/aurora/Library/Application Support/ReadyFleet/solarbeam/Vibeshine.app/Contents/MacOS/Vibeshine\n" +
		"  413   501 /Users/aurora/Library/Application Support/ReadyFleet/solarbeam/solarbeam-bridge\n" +
		"  414   502 /Users/aurora/Library/Application Support/ReadyFleet/solarbeam/solarbeam-bridge\n" + // another user
		"  415   501 /opt/other/solarbeam-bridge\n" + // same basename, different path
		"garbage line\n"
	entries := parsePSList(out)
	if len(entries) != 4 || entries[0].PID != 412 || entries[0].UID != 501 || !strings.HasSuffix(entries[0].Comm, "MacOS/Vibeshine") {
		t.Fatalf("entries = %+v", entries)
	}
	paths := []string{
		"/Users/aurora/Library/Application Support/ReadyFleet/solarbeam/Vibeshine.app/Contents/MacOS/Vibeshine",
		"/Users/aurora/Library/Application Support/ReadyFleet/solarbeam/solarbeam-bridge",
	}
	got := survivorsOf(entries, 501, paths)
	if len(got) != 2 || got[0].PID != 412 || got[1].PID != 413 {
		t.Fatalf("survivors = %+v", got)
	}
	if n := len(survivorsOf(entries, 501, []string{"solarbeam-bridge"})); n != 0 {
		t.Errorf("a relative path matched %d processes; only exact absolute paths may", n)
	}
}

// Off macOS the manager reports the pre-existing truth: always available,
// everything supported, mirrors hostingOff. The darwin file replaces this
// through platformHostAvailability.
func TestHostAvailabilityDefaultIsPermissive(t *testing.T) {
	if platformHostAvailability != nil {
		t.Skip("platform availability installed (darwin)")
	}
	m := New(Paths{})
	a := m.HostAvailability()
	if !a.Enabled || !a.Available || a.Reason != "" || len(a.Capabilities.Unsupported()) != 0 {
		t.Fatalf("availability = %+v", a)
	}
}

func TestSessionListenerFiresOnStartAndStop(t *testing.T) {
	m := New(Paths{Mode: ModeInteractive})
	m.prepareFn = func() error { return nil }
	m.launchFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.engineFn = func(proto.SolarbeamStartArgs) error { return nil }
	m.stopFn = func() error { return nil }
	m.recoveredLinkCleanupFn = func() error { return nil }
	m.bridgeRunningFn = func() bool { return false }
	fired := make(chan struct{}, 8)
	m.SetSessionListener(func() { fired <- struct{}{} })
	expect := func(what string) {
		select {
		case <-fired:
		case <-time.After(2 * time.Second):
			t.Fatalf("listener not told about %s", what)
		}
	}
	ctx := context.Background()
	if err := m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "s1", MachineID: "m1"}); err != nil {
		t.Fatal(err)
	}
	expect("start")
	if err := m.Stop(ctx, proto.StopSolarbeamArgs{SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	expect("stop")
	// A cleared listener is never called.
	m.SetSessionListener(nil)
	if err := m.Start(ctx, proto.SolarbeamStartArgs{SessionID: "s2", MachineID: "m1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-fired:
		t.Fatal("cleared listener was called")
	case <-time.After(50 * time.Millisecond):
	}
}
