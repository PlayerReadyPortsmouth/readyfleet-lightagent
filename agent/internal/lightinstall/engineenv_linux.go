//go:build linux

package lightinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// captureBackend is the value written into the engine's sunshine.conf.
//
// KWin's native screencast protocol (zkde_screencast_unstable_v1) is the only
// capture backend that works for an unattended BYOD machine on KDE Wayland: it
// needs no file capabilities, no xdg-desktop-portal, no consent dialog and no
// restore token, and it keeps capturing with the screen locked and the monitors
// asleep. The two alternatives are both disqualified — the portal cannot run
// unattended (it asks a human, every session, at the device), and KMS needs
// cap_sys_admin on the engine binary, which is exactly what makes /proc/<pid>
// root-owned and breaks the portal fallback as well (see StartupUnit's
// correction note in install_linux.go).
//
// The engine will happily pick something else if left to auto-detect, so the
// choice has to be pinned in its config rather than assumed.
const captureBackend = "kwin"

// captureKey is the sunshine.conf key holding the backend. The file format is
// `key = value`, one per line, with `#` starting a comment.
const captureKey = "capture"

// captureReserveTopKey is the engine's `capture_reserve_top` — the height,
// in LOGICAL pixels, of a band along the top edge of the streamed output
// that the KWin capture leaves out of the stream entirely (it captures
// with stream_region over the output rectangle minus that band, instead of
// stream_output over the whole thing).
//
// It exists for exactly one caller: the agent's on-screen session
// indicator (agent/internal/sessionindicator), which draws its "your
// screen is being viewed remotely" bar inside that band. KWin's screencast
// protocol has no way to exclude a window from a capture — the whole of
// zkde_screencast_unstable_v1 is stream_output, stream_window,
// stream_virtual_output and stream_region, with no per-window skip flag
// anywhere — so a geometric reservation agreed with the engine is the ONLY
// mechanism available on Linux.
//
// The two halves must agree: sessionindicator.ReserveHeight is the number
// written here, and a bar taller than the band would be partly inside the
// stream. That is why the value is passed in from the one constant rather
// than duplicated.
const captureReserveTopKey = "capture_reserve_top"

// kwinPermissionFileName is the desktop entry that grants the engine the KWin
// screencast interface.
//
// The `-readyfleet` suffix is deliberate and load-bearing. The engine's own
// stale-file cleanup (src/platform/linux/kwingrab.cpp,
// screencast_permission_helper_t) scans the applications directory for names
// beginning with `dev.lizardbyte.app.Sunshine.kwin`, keeps any whose Exec=
// matches the running binary, and deletes the rest. Ours matches that prefix,
// so the engine recognises it as its own and leaves it alone; the suffix keeps
// it distinguishable from the temporary one the engine writes for itself.
const kwinPermissionFileName = "dev.lizardbyte.app.Sunshine.kwin-readyfleet.desktop"

// Directory and binary resolution live behind package-level seams so the tests
// can point them at t.TempDir() instead of the developer's real ~/.config and
// ~/.local/share. Same idiom as solarbeam's inputReadyFn.
var (
	engineConfigDirFn  = defaultEngineConfigDir
	desktopEntryDirFn  = defaultDesktopEntryDir
	engineBinaryPathFn = defaultEngineBinaryPath
)

// EnsureEngineEnvironment prepares the two pieces of per-user state the
// SolarBeam engine needs on a Linux BYOD desktop, both of which live outside
// the engine's own install directory and so survive every engine update:
//
//   - $XDG_CONFIG_HOME/sunshine/sunshine.conf gets `capture = kwin`, pinning
//     the only capture backend that works unattended (see captureBackend).
//
//   - $XDG_DATA_HOME/applications/dev.lizardbyte.app.Sunshine.kwin-readyfleet.desktop
//     declares X-KDE-Wayland-Interfaces=zkde_screencast_unstable_v1 for the
//     engine binary, which is what makes KWin hand out that interface at all.
//
// Both steps are idempotent: a second run against a correct machine writes
// nothing. It is called by the installer and again by the agent at every
// startup, so a machine installed before this existed heals itself rather than
// waiting for someone to re-run the installer by hand.
//
// Errors from the two steps are joined rather than short-circuited: they are
// independent, and a machine that got one of them is better off than a machine
// that got neither.
func EnsureEngineEnvironment() error {
	return errors.Join(ensureEngineCaptureBackend(), ensureKWinScreencastPermission())
}

// ensureEngineCaptureBackend pins `capture = kwin` in the engine's config.
//
// The file is edited line-by-line rather than rewritten from a template
// because it is the ENGINE's config, not ours: anything else in it was put
// there by the engine or by a human, and clobbering it would be a silent
// regression of settings we know nothing about. So every other line — comments
// included — is preserved verbatim, an existing active `capture` line is
// replaced in place, a commented-out one is left as the comment it is and an
// active line appended after it, and a file that is already correct is not
// rewritten at all.
func ensureEngineCaptureBackend() error {
	return ensureEngineConfKey(captureKey, captureBackend)
}

// EnsureCaptureReserveTop pins `capture_reserve_top = px` in the engine's
// config, so the engine keeps that band out of the captured region and the
// session indicator can draw into it. See captureReserveTopKey.
//
// Called from the light agent at startup, and ONLY when the indicator is
// actually enabled on this machine — writing it unconditionally would cost
// every BYOD machine 40 logical pixels off the top of the stream for a
// feature it is not running. Idempotent, and preserves the rest of the
// file exactly like ensureEngineCaptureBackend does.
func EnsureCaptureReserveTop(px int) error {
	return ensureEngineConfKey(captureReserveTopKey, strconv.Itoa(px))
}

// ensureEngineConfKey sets one key in sunshine.conf, preserving everything
// else. Shared by both callers so there is one conf editor, not two that
// can drift.
func ensureEngineConfKey(key, value string) error {
	dir, err := engineConfigDirFn()
	if err != nil {
		return fmt.Errorf("lightinstall: resolve engine config directory: %w", err)
	}
	path := filepath.Join(dir, "sunshine.conf")

	current, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lightinstall: read %s: %w", path, err)
	}
	want := setConfKey(string(current), key, value)
	if err == nil && want == string(current) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lightinstall: create engine config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		return fmt.Errorf("lightinstall: write %s: %w", path, err)
	}
	return nil
}

// setConfKey returns conf with every active `key = …` line set to value,
// appending one if the file has none. Commented-out lines are left
// untouched — a `#capture = x11` line is documentation, not a setting, and
// rewriting it would destroy the note without changing any behaviour.
func setConfKey(conf, key, value string) string {
	assignment := key + " = " + value
	lines := strings.Split(conf, "\n")
	found := false
	for i, line := range lines {
		if !isActiveAssignment(line, key) {
			continue
		}
		found = true
		lines[i] = assignment
	}
	if found {
		return strings.Join(lines, "\n")
	}
	if conf != "" && !strings.HasSuffix(conf, "\n") {
		conf += "\n"
	}
	return conf + assignment + "\n"
}

// isActiveAssignment reports whether line assigns key and is not commented out.
func isActiveAssignment(line, key string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return false
	}
	name, _, ok := strings.Cut(trimmed, "=")
	return ok && strings.TrimSpace(name) == key
}

// ensureKWinScreencastPermission installs the desktop entry KWin reads before
// granting zkde_screencast_unstable_v1.
//
// Without it the engine is not merely slower to start: it writes its own
// temporary entry and then SLEEPS THREE SECONDS on every single launch waiting
// for KDE to notice the new file, on the way into every session. A permanent
// entry removes that wait entirely.
//
// The Exec= path must be the fully symlink-resolved absolute path, because the
// engine compares it byte-for-byte against readlink("/proc/self/exe"), which is
// always resolved. A path that differs only by a symlink hop reads to the
// engine as "no entry for me" and it falls back to the temporary-file sleep.
//
// The entry lives outside the engine install directory on purpose: an engine
// update replaces that directory wholesale (solarbeam.applyEngineUpdate renames
// the whole thing into place), but the install PATH is stable, so this file
// stays valid across every update and never needs rewriting.
func ensureKWinScreencastPermission() error {
	dir, err := desktopEntryDirFn()
	if err != nil {
		return fmt.Errorf("lightinstall: resolve desktop entry directory: %w", err)
	}
	enginePath, err := engineBinaryPathFn()
	if err != nil {
		return fmt.Errorf("lightinstall: resolve engine binary: %w", err)
	}
	path := filepath.Join(dir, kwinPermissionFileName)
	want := kwinPermissionEntry(resolveEnginePath(enginePath))

	if current, err := os.ReadFile(path); err == nil && string(current) == want {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("lightinstall: read %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("lightinstall: create desktop entry directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		return fmt.Errorf("lightinstall: write %s: %w", path, err)
	}
	return nil
}

// resolveEnginePath returns the symlink-resolved absolute path of the engine
// binary. EvalSymlinks fails when the binary is not installed yet (the
// installer runs before the first engine install), in which case the absolute
// unresolved path is still the right answer: that is where the engine will be,
// and on a machine with no symlink in the way the two are identical.
func resolveEnginePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// kwinPermissionEntry renders the desktop entry. Only the Exec= line is read by
// KWin's permission check, but the rest matches the engine's own writer's
// format so the two files are recognisably the same thing.
func kwinPermissionEntry(execPath string) string {
	return "[Desktop Entry]\n" +
		"Exec=" + execPath + "\n" +
		"X-KDE-Wayland-Interfaces=zkde_screencast_unstable_v1\n" +
		"Type=Application\n" +
		"Name=SolarBeam KWin screen permission\n" +
		"Comment=SolarBeam KWin screencast permission\n" +
		"NoDisplay=true\n"
}

// defaultEngineConfigDir mirrors the engine's own platf::appdata() resolution
// for the two cases that can apply here: $XDG_CONFIG_HOME/sunshine, else
// ~/.config/sunshine. The engine checks $CONFIGURATION_DIRECTORY first, but the
// agent never sets it (the systemd user unit has no ConfigurationDirectory=),
// so it can never win.
func defaultEngineConfigDir() (string, error) {
	return xdgDir("XDG_CONFIG_HOME", ".config", "sunshine")
}

// defaultDesktopEntryDir is $XDG_DATA_HOME/applications, else
// ~/.local/share/applications — the per-user half of the XDG desktop-entry
// search path, and where the engine looks for (and prunes) its own entries.
func defaultDesktopEntryDir() (string, error) {
	return xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share"), "applications")
}

// defaultEngineBinaryPath mirrors platformLightEngineDir() in
// cmd/lightagent/platform_paths_linux.go, LocalAppData override included —
// that override is what keeps Wine/dev runs deterministic, and an engine
// launched from there must get a permission entry naming that same path.
// Duplicated rather than shared because the original lives in package main.
func defaultEngineBinaryPath() (string, error) {
	if base := os.Getenv("LocalAppData"); base != "" {
		return filepath.Join(base, "SolarBeam", "sunshine"), nil
	}
	dir, err := xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share"), "solarbeam")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sunshine"), nil
}

// xdgDir resolves $env/leaf, falling back to $HOME/fallback/leaf.
func xdgDir(env, fallback, leaf string) (string, error) {
	if base := os.Getenv(env); base != "" {
		return filepath.Join(base, leaf), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback, leaf), nil
}
