//go:build linux

package solarbeam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	bridgeWSAddr = "127.0.0.1:9092"
	bridgeWSURL  = "ws://127.0.0.1:9092/bridge"
)

func linuxLogDir() string {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "readyfleet-light", "logs")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("lightagent-data", "logs")
	}
	return filepath.Join(home, ".local", "state", "readyfleet-light", "logs")
}

func (m *Manager) prepareEngine() error {
	return errors.New("managed SolarBeam is not supported on Linux")
}
func (m *Manager) startEngine(proto.SolarbeamStartArgs) error {
	return errors.New("managed SolarBeam is not supported on Linux")
}
func (m *Manager) stopSolarbeam() error {
	return errors.New("managed SolarBeam is not supported on Linux")
}
func (m *Manager) engineServiceRunning() bool { return false }
func (m *Manager) preloadEngine(string) error {
	return errors.New("managed SolarBeam is not supported on Linux")
}
func (m *Manager) engineHealthy() bool { return false }
func restartEngineService() error      { return errors.New("managed SolarBeam is not supported on Linux") }
func stopEngineService() error         { return errors.New("managed SolarBeam is not supported on Linux") }

// uinputPath is the kernel device inputtino opens to synthesise the remote
// keyboard, mouse and gamepad. Everything the privileged setup step does — the
// udev rule's seat ACL, with the input group only as a fallback — exists to
// make this one file openable by an unprivileged desktop process.
const uinputPath = "/dev/uinput"

// openUinput reports whether this process can actually open /dev/uinput for
// writing.
//
// Deliberately the real thing rather than its proxies. Checking group
// membership, or the engine's capabilities, or the presence of the udev rule,
// each answers a question ABOUT the setup; opening the device answers the
// question that matters. It also catches the case those proxies all miss and
// that will be the single most common support call: the mentor ran the setup,
// everything succeeded, and they have not signed out and back in — so the
// running session still carries the old group set and cannot open the device
// even though `id` on a fresh login would say it can.
//
// Opening uinput is side-effect free. A virtual device only appears after a
// UI_DEV_CREATE ioctl, which we never issue.
// inputReadyFn is the injectable seam for the check below, matching this
// package's prepareFn/launchFn convention. Tests swap it so they never depend
// on the host: /dev/uinput is openable on a developer's desktop (logind grants
// the active seat a uaccess ACL) and is not in CI, so a test calling the real
// thing would pass locally and fail on the build machine — or worse, the other
// way round.
var inputReadyFn = openUinput

func openUinput() error {
	f, err := os.OpenFile(uinputPath, os.O_WRONLY, 0)
	if err == nil {
		_ = f.Close()
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist — the uinput kernel module is not loaded", uinputPath)
	}
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("no permission to open %s — run the SolarBeam integration step "+
			"(sudo integration/install-system-integration.sh), sign in at this machine's own screen, "+
			"and sign out and back in if that step says to", uinputPath)
	}
	return fmt.Errorf("cannot open %s: %w", uinputPath, err)
}

func (m *Manager) prepareEngineInteractive() error {
	if err := engineBinaryExists(m.paths.Engine); err != nil {
		return err
	}
	if err := engineBinaryExists(m.paths.Bridge); err != nil {
		return err
	}
	for _, path := range []string{m.paths.Engine, m.paths.Bridge} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("binary %s is not executable", path)
		}
	}
	// Fail HERE rather than at first keystroke. Without this the session starts,
	// the young person sees a picture, and the mentor discovers the pointer does
	// not move — remotely, with nothing on screen explaining why. prepareFn runs
	// before the bridge touches LiveKit precisely so a machine that cannot host
	// says so first.
	if err := inputReadyFn(); err != nil {
		return fmt.Errorf("solarbeam input is not set up on this machine: %w", err)
	}
	return nil
}

func waitForListener(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// openLinuxChildLog attaches a child to its size-capped rotating log under
// linuxLogDir (see childlog.go). It never fails; an unwritable directory
// yields a writer that drops output.
func openLinuxChildLog(name string) io.WriteCloser {
	return openChildLog(linuxLogDir(), name, 0o700, 0o600)
}

func stopChild(proc **os.Process, done *<-chan struct{}) {
	if *proc == nil {
		return
	}
	_ = (*proc).Signal(syscall.SIGTERM)
	if *done != nil {
		select {
		case <-*done:
		case <-time.After(3 * time.Second):
			_ = (*proc).Kill()
			<-*done
		}
	} else {
		_ = (*proc).Kill()
	}
	*proc = nil
	*done = nil
}

func (m *Manager) launchBridge(args proto.SolarbeamStartArgs) error {
	stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
	if err := m.provisionPlatformLinkAuth(); err != nil {
		return err
	}
	cmd := exec.Command(m.paths.Bridge)
	cmd.Dir = filepath.Dir(m.paths.Bridge)
	cmd.Env = append(m.linkAuthEnvironment(os.Environ()),
		"READYAPP_URL="+args.ReadyappURL,
		"SESSION_ID="+args.SessionID,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
		"SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken,
	)
	logw := openLinuxChildLog("bridge.log")
	cmd.Stdout, cmd.Stderr = logw, logw
	cmd.WaitDelay = childPipeWaitDelay
	startErr := cmd.Start()
	cmd.Env = withoutLinkAuth(cmd.Env)
	if err := startErr; err != nil {
		_ = logw.Close()
		return fmt.Errorf("start bridge: %w", err)
	}
	m.interactiveBridgeProc = cmd.Process
	done := make(chan struct{})
	m.interactiveBridgeDone = done
	go func() { _ = cmd.Wait(); _ = logw.Close(); close(done) }()
	if err := waitForListener(bridgeWSAddr, 30*time.Second); err != nil {
		stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
		return fmt.Errorf("bridge not listening on %s: %w", bridgeWSAddr, err)
	}
	slog.Info("solarbeam: Linux bridge listener confirmed", "session_id", args.SessionID)
	return nil
}

// engineLibraryPath prepends the bundle's own lib directory to LD_LIBRARY_PATH.
//
// The engine's runpath finds its direct dependencies via $ORIGIN/lib, but those
// libraries were linked inside the engine's build tree and carry runpaths
// pointing back at it, so their own dependencies (e.g. libboost_charconv, which
// only libboost_log asks for) would not resolve on a mentor's machine.
func engineLibraryPath(enginePath string) string {
	libDir := filepath.Join(filepath.Dir(enginePath), "lib")
	if existing := os.Getenv("LD_LIBRARY_PATH"); existing != "" {
		return libDir + string(os.PathListSeparator) + existing
	}
	return libDir
}

// displayEnvVars are the variables the engine needs in order to find the
// user's compositor, in the order resolveDisplayEnv reports them.
//
// WAYLAND_DISPLAY is the one that actually mattered: without it the engine
// logs "[wayland] Environment variable WAYLAND_DISPLAY has not been defined",
// every encoder probe then fails, and it dies with "Unable to find display or
// encoder during startup". DISPLAY and XAUTHORITY are here for the XWayland
// and pure-X11 capture paths, XDG_SESSION_TYPE and XDG_CURRENT_DESKTOP because
// the engine and the portal both branch on them.
var displayEnvVars = []string{
	"WAYLAND_DISPLAY",
	"DISPLAY",
	"XAUTHORITY",
	"XDG_SESSION_TYPE",
	"XDG_CURRENT_DESKTOP",
}

// resolveDisplayEnv works out the display environment to hand the engine,
// as KEY=VALUE strings ready to append to cmd.Env.
//
// Inheriting the agent's own environment is NOT enough, and assuming it was is
// half of why Linux SolarBeam published audio and never video. The agent's
// systemd unit is in default.target as well as graphical-session.target, so on
// a machine that boots to a login screen the agent starts before the
// compositor exists and its environ has no WAYLAND_DISPLAY — permanently, for
// the life of that process, no matter who logs in afterwards. The user systemd
// MANAGER learns the session's variables at login (the desktop calls
// `dbus-update-activation-environment --systemd`), so asking it per launch
// gets the right answer even for an agent that started hours earlier.
//
// Order is most-authoritative-last so a later source overwrites an earlier
// one: the process environment first, then the systemd manager's block, then
// a direct probe of $XDG_RUNTIME_DIR for a wayland-N socket for the case where
// nothing exported anything at all. Values are only ever overwritten by a
// non-empty value, so a good inherited variable survives a source that has
// nothing to say about it.
//
// Pure function over its three inputs precisely so it is testable without a
// desktop session; the real sources are read by displayEnvSources.
func resolveDisplayEnv(processEnv map[string]string, managerEnv map[string]string, waylandSockets []string) []string {
	resolved := make(map[string]string, len(displayEnvVars))
	for _, source := range []map[string]string{processEnv, managerEnv} {
		for _, key := range displayEnvVars {
			if v := strings.TrimSpace(source[key]); v != "" {
				resolved[key] = v
			}
		}
	}
	if resolved["WAYLAND_DISPLAY"] == "" && len(waylandSockets) > 0 {
		resolved["WAYLAND_DISPLAY"] = waylandSockets[0]
		if resolved["XDG_SESSION_TYPE"] == "" {
			resolved["XDG_SESSION_TYPE"] = "wayland"
		}
	}
	out := make([]string, 0, len(displayEnvVars))
	for _, key := range displayEnvVars {
		if v := resolved[key]; v != "" {
			out = append(out, key+"="+v)
		}
	}
	return out
}

// systemdManagerEnv reads the user systemd manager's environment block.
//
// `systemctl --user show-environment` prints one KEY=VALUE per line, shell
// quoting a value only when it needs it. Best-effort: a machine with no user
// manager (a container, a bare ssh login) just gets nothing back, which
// resolveDisplayEnv handles.
func systemdManagerEnv() map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show-environment").Output()
	if err != nil {
		return nil
	}
	env := make(map[string]string)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok || key == "" {
			continue
		}
		env[key] = unquoteSystemdValue(value)
	}
	return env
}

// unquoteSystemdValue undoes the shell quoting show-environment applies to
// values containing spaces or specials. Anything unquoted is returned as-is.
func unquoteSystemdValue(v string) string {
	if len(v) < 2 {
		return v
	}
	if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
		inner := v[1 : len(v)-1]
		return strings.NewReplacer(`\"`, `"`, `\'`, `'`, `\\`, `\`).Replace(inner)
	}
	return v
}

// waylandSocketsIn lists the wayland-N socket names in dir, newest-looking
// first (wayland-0 before wayland-1), skipping the .lock files beside them.
func waylandSocketsIn(dir string) []string {
	if dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "wayland-") && !strings.HasSuffix(name, ".lock") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// engineDisplayEnv is the injectable seam over the three real sources, so
// tests never depend on the host having (or not having) a desktop session.
var engineDisplayEnv = func() []string {
	processEnv := make(map[string]string, len(displayEnvVars))
	for _, key := range displayEnvVars {
		processEnv[key] = os.Getenv(key)
	}
	return resolveDisplayEnv(processEnv, systemdManagerEnv(), waylandSocketsIn(os.Getenv("XDG_RUNTIME_DIR")))
}

func buildEngineCmd(enginePath string, args proto.SolarbeamStartArgs) *exec.Cmd {
	cmd := exec.Command(enginePath)
	cmd.Dir = filepath.Dir(enginePath)
	display := engineDisplayEnv()
	cmd.Env = append(os.Environ(),
		"SOLARBEAM_BRIDGE_WS="+bridgeWSURL,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
		"LD_LIBRARY_PATH="+engineLibraryPath(enginePath),
	)
	cmd.Env = append(cmd.Env, display...)
	slog.Info("solarbeam: engine display environment resolved", "vars", display)
	return cmd
}

func (m *Manager) startEngineInteractive(args proto.SolarbeamStartArgs) error {
	stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
	cmd := buildEngineCmd(m.paths.Engine, args)
	cmd.Env = m.linkAuthEnvironment(cmd.Env)
	logw := openLinuxChildLog("engine.log")
	watcher := newEngineWatcher(logw)
	cmd.Stdout, cmd.Stderr = watcher, watcher
	cmd.WaitDelay = childPipeWaitDelay
	startErr := cmd.Start()
	cmd.Env = withoutLinkAuth(cmd.Env)
	if err := startErr; err != nil {
		_ = logw.Close()
		return fmt.Errorf("start engine: %w", err)
	}
	m.interactiveEngineProc = cmd.Process
	done := make(chan struct{})
	m.interactiveEngineDone = done
	go func() {
		_ = cmd.Wait()
		_ = logw.Close()
		close(done)
	}()

	// Do not report the session started just because fork+exec worked.
	//
	// cmd.Start() returning nil says the kernel created a process. It says
	// nothing about whether that process can capture the screen, and for a
	// whole day it said "success, exit_code=0" for an engine that was wedged
	// in an XDG portal call, had published no video track, and never would.
	// ReadyApp believed the launch, the room timed out the video track ~31s
	// later, and nothing anywhere recorded a failure. Wait for the engine's
	// own bridge-mode line instead.
	if err := watcher.wait(engineReadyTimeout, done); err != nil {
		stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
		return err
	}
	slog.Info("solarbeam: engine reached bridge mode", "phase", "engine_ready", "session_id", args.SessionID)
	return nil
}

func (m *Manager) stopSolarbeamInteractive() error {
	stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
	stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
	return m.cleanupStoppedLinkAuth()
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func (m *Manager) engineProcessRunning() bool {
	return childRunning(m.interactiveEngineProc, m.interactiveEngineDone)
}

func (m *Manager) bridgeProcessRunning() bool {
	return childRunning(m.interactiveBridgeProc, m.interactiveBridgeDone)
}

func childRunning(proc *os.Process, done <-chan struct{}) bool {
	if proc == nil {
		return false
	}
	if done != nil {
		select {
		case <-done:
			return false
		default:
			return true
		}
	}
	return processAlive(proc.Pid)
}

func engineFileVersion(path string) (string, error) {
	for _, candidate := range []string{filepath.Join(filepath.Dir(path), "VERSION"), filepath.Join(filepath.Dir(path), "version.txt")} {
		if raw, err := os.ReadFile(candidate); err == nil {
			if version := strings.TrimSpace(string(raw)); version != "" {
				return version, nil
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read engine version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
