//go:build linux

package lightinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stubEngineEnvDirs pins the three resolution seams at temporary directories
// for the duration of a test. Without it these tests would read and WRITE the
// developer's real ~/.config/sunshine and ~/.local/share/applications — this
// package's code is the same code that runs on a real BYOD machine, and its
// whole job is to edit those files.
//
// Returns the engine config directory and the desktop entry directory, in that
// order. The engine binary path is fixed at engine/sunshine under the same
// temporary root so a test can create (or not create) it.
func stubEngineEnvDirs(t *testing.T) (confDir, appsDir, enginePath string) {
	t.Helper()
	root := t.TempDir()
	confDir = filepath.Join(root, "config", "sunshine")
	appsDir = filepath.Join(root, "share", "applications")
	enginePath = filepath.Join(root, "share", "solarbeam", "sunshine")

	prevConf, prevApps, prevEngine := engineConfigDirFn, desktopEntryDirFn, engineBinaryPathFn
	engineConfigDirFn = func() (string, error) { return confDir, nil }
	desktopEntryDirFn = func() (string, error) { return appsDir, nil }
	engineBinaryPathFn = func() (string, error) { return enginePath, nil }
	t.Cleanup(func() {
		engineConfigDirFn, desktopEntryDirFn, engineBinaryPathFn = prevConf, prevApps, prevEngine
	})
	return confDir, appsDir, enginePath
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// freezeModTime backdates a file so a later "was it rewritten?" assertion is
// deterministic — comparing against time.Now() would depend on filesystem
// timestamp granularity.
func freezeModTime(t *testing.T, path string) time.Time {
	t.Helper()
	frozen := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, frozen, frozen); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.ModTime()
}

func assertUnmodifiedSince(t *testing.T, path string, before time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !info.ModTime().Equal(before) {
		t.Fatalf("%s was rewritten (mtime %v -> %v) despite already being correct", path, before, info.ModTime())
	}
}

// A fresh BYOD machine has no engine config at all — the engine writes one only
// once it has run. If the installer does not create it, the engine
// auto-detects a capture backend and on KDE Wayland picks one that cannot run
// unattended.
func TestEnsureEngineEnvironmentCreatesTheConfWhenAbsent(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	if got, want := readFile(t, filepath.Join(confDir, "sunshine.conf")), "capture = kwin\n"; got != want {
		t.Fatalf("sunshine.conf = %q, want %q", got, want)
	}
}

// The config file belongs to the engine, not to us. Anything already in it was
// put there by the engine or by a human, so rewriting it from a template would
// silently drop settings — bitrate, port, encoder — that nothing in ReadyFleet
// knows about.
func TestEnsureEngineEnvironmentPreservesUnrelatedKeysAndComments(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := "# tuned by hand, do not lose this\nmin_log_level = debug\nport = 47989\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	got := readFile(t, path)
	if !strings.HasPrefix(got, existing) {
		t.Fatalf("existing config was not preserved verbatim:\n%s", got)
	}
	if !strings.Contains(got, "capture = kwin\n") {
		t.Fatalf("capture backend was not appended:\n%s", got)
	}
}

// A machine configured for the old X11 path must be moved onto kwin, in place,
// without gaining a second contradictory capture line.
func TestEnsureEngineEnvironmentReplacesAnExistingCaptureLine(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("capture = x11\nport = 47989\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	got := readFile(t, path)
	if want := "capture = kwin\nport = 47989\n"; got != want {
		t.Fatalf("sunshine.conf = %q, want %q", got, want)
	}
	if strings.Count(got, "capture = ") != 1 {
		t.Fatalf("capture assigned more than once:\n%s", got)
	}
}

// A commented-out capture line is a note, not a setting. Rewriting it would
// destroy the note and still leave the engine auto-detecting, so the comment
// must survive and an active line must appear alongside it.
func TestEnsureEngineEnvironmentLeavesACommentedCaptureLineAlone(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# capture = x11\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	if got, want := readFile(t, path), "# capture = x11\ncapture = kwin\n"; got != want {
		t.Fatalf("sunshine.conf = %q, want %q", got, want)
	}
}

// This runs at every agent startup, not just at install. Rewriting a correct
// file on every boot would churn its mtime for nothing and, worse, would mean
// a bug in the writer could damage a working machine at any restart.
func TestEnsureEngineEnvironmentDoesNotRewriteACorrectConf(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("capture = kwin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := freezeModTime(t, path)

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	assertUnmodifiedSince(t, path, before)
}

// KWin grants zkde_screencast_unstable_v1 only to a binary named in a desktop
// entry carrying the interface line, and the engine matches the Exec= path
// byte-for-byte against its own /proc/self/exe — which is fully symlink
// resolved. An entry naming an unresolved path reads as "not mine", and the
// engine falls back to writing a temporary one and sleeping three seconds on
// every launch.
func TestEnsureEngineEnvironmentWritesTheDesktopEntryWithAResolvedExec(t *testing.T) {
	_, appsDir, enginePath := stubEngineEnvDirs(t)
	// A symlinked install directory is the case that catches an unresolved
	// path: the engine reports the real one.
	realDir := filepath.Join(t.TempDir(), "solarbeam-real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realEngine := filepath.Join(realDir, "sunshine")
	if err := os.WriteFile(realEngine, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(enginePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realEngine, enginePath); err != nil {
		t.Fatal(err)
	}

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	got := readFile(t, filepath.Join(appsDir, kwinPermissionFileName))
	resolvedReal, err := filepath.EvalSymlinks(realEngine)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Exec="+resolvedReal+"\n") {
		t.Fatalf("desktop entry does not name the resolved engine path %q:\n%s", resolvedReal, got)
	}
	if !strings.Contains(got, "X-KDE-Wayland-Interfaces=zkde_screencast_unstable_v1\n") {
		t.Fatalf("desktop entry is missing the interface declaration KWin reads:\n%s", got)
	}
	// The engine's stale-entry sweep keeps files whose name starts with this
	// prefix and whose Exec matches; ours has to be recognisable to it.
	if !strings.HasPrefix(kwinPermissionFileName, "dev.lizardbyte.app.Sunshine.kwin") {
		t.Fatalf("entry name %q would be deleted by the engine's own cleanup", kwinPermissionFileName)
	}
}

// The engine is not installed yet when the installer runs, so EvalSymlinks
// fails. The absolute path is still the right answer — that is where the engine
// will be — and refusing to write the entry would leave the three-second sleep
// on every launch forever.
func TestEnsureEngineEnvironmentWritesTheEntryBeforeTheEngineExists(t *testing.T) {
	_, appsDir, enginePath := stubEngineEnvDirs(t)

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	got := readFile(t, filepath.Join(appsDir, kwinPermissionFileName))
	if !strings.Contains(got, "Exec="+enginePath+"\n") {
		t.Fatalf("desktop entry does not name the engine path %q:\n%s", enginePath, got)
	}
}

// Same reasoning as the conf: this runs at every startup, and a correct entry
// must not be touched.
func TestEnsureEngineEnvironmentDoesNotRewriteACorrectDesktopEntry(t *testing.T) {
	_, appsDir, enginePath := stubEngineEnvDirs(t)
	if err := os.MkdirAll(appsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(appsDir, kwinPermissionFileName)
	if err := os.WriteFile(path, []byte(kwinPermissionEntry(enginePath)), 0o600); err != nil {
		t.Fatal(err)
	}
	before := freezeModTime(t, path)

	if err := EnsureEngineEnvironment(); err != nil {
		t.Fatalf("EnsureEngineEnvironment: %v", err)
	}
	assertUnmodifiedSince(t, path, before)
}

// The LocalAppData override is what keeps Wine/dev runs deterministic
// (platformLightEngineDir honours it too). An entry naming the XDG path while
// the engine actually runs from LocalAppData would not match /proc/self/exe.
func TestDefaultEngineBinaryPathHonoursLocalAppData(t *testing.T) {
	t.Setenv("LocalAppData", filepath.Join(t.TempDir(), "AppData", "Local"))
	got, err := defaultEngineBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(os.Getenv("LocalAppData"), "SolarBeam", "sunshine"); got != want {
		t.Fatalf("defaultEngineBinaryPath = %q, want %q", got, want)
	}
}

// The engine resolves its config as $XDG_CONFIG_HOME/sunshine before
// ~/.config/sunshine; writing to the wrong one is a silent no-op from the
// engine's point of view.
func TestDefaultDirsFollowTheXDGEnvironment(t *testing.T) {
	t.Setenv("LocalAppData", "")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	t.Setenv("XDG_DATA_HOME", "/xdg/data")

	confDir, err := defaultEngineConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/xdg/config/sunshine"; confDir != want {
		t.Errorf("defaultEngineConfigDir = %q, want %q", confDir, want)
	}
	appsDir, err := defaultDesktopEntryDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/xdg/data/applications"; appsDir != want {
		t.Errorf("defaultDesktopEntryDir = %q, want %q", appsDir, want)
	}
	enginePath, err := defaultEngineBinaryPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/xdg/data/solarbeam/sunshine"; enginePath != want {
		t.Errorf("defaultEngineBinaryPath = %q, want %q", enginePath, want)
	}
}

func TestEnsureCaptureReserveTopWritesTheKeyAndKeepsTheRest(t *testing.T) {
	// The band the session indicator draws into is the engine's half of
	// the contract; writing it must not disturb the capture backend or
	// anything else a human put in the file.
	confDir, _, _ := stubEngineEnvDirs(t)
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.WriteFile(path, []byte("# hand-edited\ncapture = kwin\nmin_log_level = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := EnsureCaptureReserveTop(40); err != nil {
		t.Fatalf("EnsureCaptureReserveTop: %v", err)
	}
	got := readConf(t, path)
	for _, want := range []string{"# hand-edited", "capture = kwin", "min_log_level = 2", "capture_reserve_top = 40"} {
		if !strings.Contains(got, want) {
			t.Errorf("conf is missing %q:\n%s", want, got)
		}
	}
}

func TestEnsureCaptureReserveTopReplacesAnExistingValue(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	if err := os.MkdirAll(confDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(confDir, "sunshine.conf")
	if err := os.WriteFile(path, []byte("capture_reserve_top = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCaptureReserveTop(40); err != nil {
		t.Fatalf("EnsureCaptureReserveTop: %v", err)
	}
	got := readConf(t, path)
	if strings.Contains(got, "capture_reserve_top = 0") || !strings.Contains(got, "capture_reserve_top = 40") {
		t.Fatalf("stale reserve left behind:\n%s", got)
	}
}

func TestEnsureCaptureReserveTopIsIdempotent(t *testing.T) {
	confDir, _, _ := stubEngineEnvDirs(t)
	if err := EnsureCaptureReserveTop(40); err != nil {
		t.Fatalf("first: %v", err)
	}
	path := filepath.Join(confDir, "sunshine.conf")
	first, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The agent calls this at every startup; a rewrite every time would
	// churn the engine's own config file for no reason.
	time.Sleep(10 * time.Millisecond)
	if err := EnsureCaptureReserveTop(40); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("a correct conf was rewritten on the second call")
	}
}

func readConf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read conf: %v", err)
	}
	return string(b)
}
