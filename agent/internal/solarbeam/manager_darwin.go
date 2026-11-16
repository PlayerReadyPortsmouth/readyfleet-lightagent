//go:build darwin

// macOS SolarBeam host: the process half.
//
// The Mac is a BYOD light agent (ModeInteractive): engine and bridge are
// per-session direct children of the agent, as on Linux. What differs:
//
//   - The engine is an .app bundle. It is run by exec'ing
//     Contents/MacOS/<CFBundleExecutable> directly, not through `open`.
//     `open` hands the app to LaunchServices, which gives us no pipe to read
//     the engine's own "entering bridge mode" / "No screen capture
//     permission!" lines from (the whole reason the launch is not reported
//     as started on fork+exec alone), no PID to stop cleanly, and no way to
//     pass the launch credential except `--env` on a command line, which the
//     link-auth rules forbid. The cost is TCC attribution: macOS keys a
//     Screen Recording grant to the RESPONSIBLE process, and a child exec'd
//     by a LaunchAgent is the LaunchAgent's responsibility, so the grant
//     may be recorded against readyfleet-light rather than the engine app.
//     That is a verification item for the first run with the real engine
//     (docs/macos-light-agent.md, "Verify with the real engine"); if it
//     bites, the fallback is to launch via `open -W` with a 0600 credential
//     file once the bridge's darwin file backend exists.
//   - A supervisor relaunches an engine that dies mid-session (bounded,
//     engineRestartPolicy) and ends the session with a reason when it gives
//     up or when the exit was a permission loss, which relaunching cannot fix.
//   - A permission fault is remembered for faultCooldown so a C2 retry loop
//     cannot become a prompt storm, and reported through HostAvailability.
//
// Everything is off unless Paths.DarwinHostEnabled (config.json
// `solarbeam_host`) and both files are present and executable.
package solarbeam

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// darwinBridgeWS is the loopback address the bridge listens on for the engine
// (the bridge's BRIDGE_WS_ADDR, default 127.0.0.1:9092) and the URL the engine
// dials. Variables rather than constants so the fake-engine dry run can use a
// free port on a Mac where a hand-launched bridge already holds 9092;
// production never changes them (the status probe is fixed to 9092 too).
var (
	bridgeWSAddr = "127.0.0.1:9092"
	bridgeWSURL  = "ws://127.0.0.1:9092/bridge"
)

const (
	// darwinHostDisabledReason is the start refusal while solarbeam_host is
	// off. It names the key so the operator knows what to flip.
	darwinHostDisabledReason = "SolarBeam hosting is not enabled on this Mac (set \"solarbeam_host\": true in config.json and restart the agent)"
)

func init() { platformHostAvailability = darwinHostAvailability }

// Managed mode does not exist on a Mac: no SYSTEM service, no secure
// desktop, no warm engine. The light agent never asks for it.
func (m *Manager) prepareEngine() error {
	return errors.New("managed SolarBeam is not supported on macOS")
}
func (m *Manager) startEngine(proto.SolarbeamStartArgs) error {
	return errors.New("managed SolarBeam is not supported on macOS")
}
func (m *Manager) stopSolarbeam() error {
	return errors.New("managed SolarBeam is not supported on macOS")
}
func (m *Manager) engineServiceRunning() bool { return false }
func (m *Manager) preloadEngine(string) error {
	return errors.New("managed SolarBeam is not supported on macOS")
}
func (m *Manager) engineHealthy() bool { return false }
func restartEngineService() error      { return errors.New("managed SolarBeam is not supported on macOS") }
func stopEngineService() error         { return errors.New("managed SolarBeam is not supported on macOS") }

// darwinSolarbeamRoot is the SolarBeam directory: Paths.Root when the caller
// set it (the light agent does), else the default under Application Support.
func (m *Manager) darwinSolarbeamRoot() string {
	if m.paths.Root != "" {
		return m.paths.Root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "solarbeam"
	}
	return DarwinSolarbeamDir(home)
}

func (m *Manager) darwinLogDir() string { return DarwinLogDir(m.darwinSolarbeamRoot()) }

// openDarwinChildLog attaches a child to its size-capped rotating log (see
// childlog.go). Never fails; an unwritable directory drops output.
func (m *Manager) openDarwinChildLog(name string) io.WriteCloser {
	return openChildLog(m.darwinLogDir(), name, 0o700, 0o600)
}

// darwinFilesReady checks the installed files without running anything: the
// engine executable inside an .app with an Info.plist, and the bridge binary.
// Each failure names the path, because "not installed" on a Mac means "put
// it here".
func (m *Manager) darwinFilesReady() error {
	if err := engineBinaryExists(m.paths.Engine); err != nil {
		return err
	}
	if err := engineBinaryExists(m.paths.Bridge); err != nil {
		return fmt.Errorf("bridge not installed: %w", err)
	}
	for _, path := range []string{m.paths.Engine, m.paths.Bridge} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("binary %s is not executable", path)
		}
	}
	app := darwinAppBundleOf(m.paths.Engine)
	if app == "" {
		return fmt.Errorf("engine %s is not inside an .app bundle (expected <App>.app/Contents/MacOS/<exe>)", m.paths.Engine)
	}
	if _, err := os.Stat(DarwinInfoPlistPath(app)); err != nil {
		return fmt.Errorf("engine bundle %s has no Contents/Info.plist: %w", app, err)
	}
	return nil
}

// darwinAvailabilityLocked is HostAvailability's body. Caller holds mu.
func (m *Manager) darwinAvailabilityLocked() HostAvailability {
	a := HostAvailability{Enabled: m.paths.DarwinHostEnabled, Capabilities: DarwinHostCapabilities()}
	switch {
	case !a.Enabled:
		a.Reason = darwinHostDisabledReason
	case m.hostingOff.Load():
		a.Reason = ErrHostingDisabled.Error()
	default:
		if err := m.darwinFilesReady(); err != nil {
			a.Reason = err.Error()
		} else if f := m.darwin.fault; f.blocks(time.Now()) {
			a.Reason = f.Reason
		} else {
			a.Available = true
		}
	}
	return a
}

func darwinHostAvailability(m *Manager) HostAvailability {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.darwinAvailabilityLocked()
}

// prepareEngineInteractive runs before the bridge touches LiveKit, so a Mac
// that cannot host says so first (Linux rule, same reason).
func (m *Manager) prepareEngineInteractive() error {
	if !m.paths.DarwinHostEnabled {
		return errors.New(darwinHostDisabledReason)
	}
	if err := m.darwinFilesReady(); err != nil {
		return err
	}
	if f := m.darwin.fault; f.blocks(time.Now()) {
		return fmt.Errorf("%s (the engine reported it %s ago; grant it in System Settings > Privacy & Security, then try again)",
			f.Reason, time.Since(f.At).Truncate(time.Second))
	}
	return nil
}

// recordDarwinFaultLocked remembers why the engine could not host. Caller
// holds mu.
func (m *Manager) recordDarwinFaultLocked(reason string) {
	m.darwin.fault = hostFault{Reason: reason, At: time.Now()}
	slog.Error("solarbeam: this Mac cannot host right now", "phase", "darwin_host_fault", "reason", reason)
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

// stopChild ends a direct child: SIGTERM, then SIGKILL after 3 s.
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

// exitCodeOf extracts a process exit code from exec.Cmd.Wait's error.
func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return -1
}

func (m *Manager) launchBridge(args proto.SolarbeamStartArgs) error {
	// Privacy mode blanks the physical monitors through the Windows venue
	// companion. Nothing on a Mac can do that, and a session that was asked
	// for privacy and streams without it is exactly the "stub that reports
	// success" the plan forbids.
	if args.PrivacyMode || args.PrivacyInput {
		return errors.New("privacy mode is not available on this Mac; start the session without privacy_mode")
	}
	stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
	if err := m.provisionPlatformLinkAuth(); err != nil {
		return err
	}
	// Keep the credential for the engine supervisor: a mid-session engine
	// relaunch must present the same key the bridge holds, and Start forgets
	// the credential once both children are up. Cleared at stop. Linux keeps
	// the same secret in a 0600 file for the session; this is no wider.
	if m.linkCredential != nil {
		m.darwin.linkEnv = append([]string(nil), m.linkCredential.env...)
	}
	cmd := exec.Command(m.paths.Bridge)
	cmd.Dir = filepath.Dir(m.paths.Bridge)
	cmd.Env = append(m.linkAuthEnvironment(darwinChildEnvironment(os.Environ())),
		"BRIDGE_WS_ADDR="+bridgeWSAddr,
		"READYAPP_URL="+args.ReadyappURL,
		"SESSION_ID="+args.SessionID,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
		"SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken,
	)
	logw := m.openDarwinChildLog("bridge.log")
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
	sid := args.SessionID
	go func() {
		code := exitCodeOf(cmd.Wait())
		_ = logw.Close()
		slog.Info("solarbeam: bridge exited", "phase", "bridge_exit", "session_id", sid, "code", code, "meaning", BridgeExitMeaning(code))
		close(done)
	}()
	if err := waitForListener(bridgeWSAddr, 30*time.Second); err != nil {
		stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
		return fmt.Errorf("bridge not listening on %s: %w", bridgeWSAddr, err)
	}
	slog.Info("solarbeam: macOS bridge listener confirmed", "session_id", sid)
	return nil
}

// buildDarwinEngineCmd prepares the engine process. No DYLD_* variables: the
// bundle resolves its libraries by rpath, and injecting loader variables into
// a signed app is both unnecessary and refused by hardened runtimes.
func buildDarwinEngineCmd(enginePath string, args proto.SolarbeamStartArgs) *exec.Cmd {
	cmd := exec.Command(enginePath)
	cmd.Dir = filepath.Dir(enginePath)
	cmd.Env = append(darwinChildEnvironment(os.Environ()),
		"SOLARBEAM_BRIDGE_WS="+bridgeWSURL,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
	)
	return cmd
}

// launchDarwinEngine starts one engine process and waits for its verdict.
// Caller holds mu. On success the watcher is returned so the supervisor can
// read a later reason; on failure the child is already stopped and a
// permission reason has been recorded.
func (m *Manager) launchDarwinEngine(args proto.SolarbeamStartArgs) (*darwinEngineWatcher, error) {
	cmd := buildDarwinEngineCmd(m.paths.Engine, args)
	cmd.Env = append(cmd.Env, m.darwin.linkEnv...)
	logPath := filepath.Join(m.darwinLogDir(), "engine.log")
	logw := m.openDarwinChildLog("engine.log")
	watcher := newDarwinEngineWatcher(logw)
	cmd.Stdout, cmd.Stderr = watcher, watcher
	cmd.WaitDelay = childPipeWaitDelay
	startErr := cmd.Start()
	cmd.Env = withoutLinkAuth(cmd.Env)
	if err := startErr; err != nil {
		_ = logw.Close()
		return nil, fmt.Errorf("start engine: %w", err)
	}
	m.interactiveEngineProc = cmd.Process
	done := make(chan struct{})
	m.interactiveEngineDone = done
	sid := args.SessionID
	go func() {
		code := exitCodeOf(cmd.Wait())
		_ = logw.Close()
		slog.Info("solarbeam: engine exited", "phase", "engine_exit", "session_id", sid, "code", code, "reason", watcher.Reason())
		close(done)
	}()
	if err := watcher.wait(darwinEngineReadyTimeout, done, logPath); err != nil {
		stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
		if reason := watcher.Reason(); isPermissionReason(reason) {
			m.recordDarwinFaultLocked(reason)
		}
		return nil, err
	}
	// Proven: the engine captured and dialled the bridge, so any earlier
	// fault is history.
	m.darwin.fault = hostFault{}
	slog.Info("solarbeam: engine reached bridge mode", "phase", "engine_ready", "session_id", sid)
	return watcher, nil
}

func (m *Manager) startEngineInteractive(args proto.SolarbeamStartArgs) error {
	stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
	caps := DarwinHostCapabilities()
	slog.Info("solarbeam: macOS host capabilities", "phase", "darwin_capabilities", "unsupported", caps.Unsupported())
	watcher, err := m.launchDarwinEngine(args)
	if err != nil {
		return err
	}
	m.darwin.restarts = 0
	m.darwin.supervisorGen++
	go m.superviseDarwinEngine(m.darwin.supervisorGen, args, watcher, m.interactiveEngineDone)
	return nil
}

// superviseDarwinEngine owns the engine for one session: when it exits while
// the session is still bound and the bridge still runs, relaunch it within
// the policy; otherwise end the session locally with the reason. A newer
// supervisor generation (a stop, a new start) retires this one.
func (m *Manager) superviseDarwinEngine(gen uint64, args proto.SolarbeamStartArgs, watcher *darwinEngineWatcher, done <-chan struct{}) {
	for {
		<-done
		m.mu.Lock()
		if m.darwin.supervisorGen != gen || m.currentSessionID != args.SessionID {
			m.mu.Unlock()
			return
		}
		if !childRunning(m.interactiveBridgeProc, m.interactiveBridgeDone) {
			// Bridge gone too: the session is over and ReapSession ends it.
			m.mu.Unlock()
			return
		}
		reason := ""
		if watcher != nil {
			reason = watcher.Reason()
		}
		if isPermissionReason(reason) {
			m.recordDarwinFaultLocked(reason)
			m.endDarwinSessionLocked(args.SessionID, reason)
			m.mu.Unlock()
			return
		}
		m.darwin.restarts++
		attempt := m.darwin.restarts
		policy := m.darwin.restartPolicy()
		if !policy.Allow(attempt) {
			why := fmt.Sprintf("engine exited %d times in this session; giving up", attempt)
			if reason != "" {
				why += " (last: " + reason + ")"
			}
			m.recordDarwinFaultLocked(why)
			m.endDarwinSessionLocked(args.SessionID, why)
			m.mu.Unlock()
			return
		}
		delay := policy.Delay(attempt)
		slog.Warn("solarbeam: engine exited mid-session; relaunching", "phase", "engine_relaunch",
			"session_id", args.SessionID, "attempt", attempt, "of", policy.MaxRestarts, "after", delay.String(), "reason", reason)
		m.mu.Unlock()
		time.Sleep(delay)
		m.mu.Lock()
		if m.darwin.supervisorGen != gen || m.currentSessionID != args.SessionID {
			m.mu.Unlock()
			return
		}
		stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
		w, err := m.launchDarwinEngine(args)
		if err != nil {
			slog.Warn("solarbeam: engine relaunch failed", "phase", "engine_relaunch_failed", "session_id", args.SessionID, "attempt", attempt, "err", err)
			closed := make(chan struct{})
			close(closed)
			watcher, done = nil, closed
			m.mu.Unlock()
			continue
		}
		m.hostingGeneration++
		watcher, done = w, m.interactiveEngineDone
		m.mu.Unlock()
	}
}

// endDarwinSessionLocked ends the bound session the way ReapSession does, with
// a reason: stop both children, drop the credential, remember the session as
// stopped so ReadyApp's later stop_solarbeam replays as success. Caller holds mu.
func (m *Manager) endDarwinSessionLocked(sid, reason string) {
	m.hostingGeneration++
	if err := m.stopSolarbeamInteractive(); err != nil {
		slog.Warn("solarbeam: session end cleanup failed", "phase", "darwin_session_end_failed", "session_id", sid, "err", err)
	}
	if err := m.cleanupLinkAuth(); err != nil {
		slog.Warn("solarbeam: link credential cleanup failed", "session_id", sid, "err", err)
	}
	m.currentSessionID = ""
	m.lastStoppedSessionID = sid
	m.clearSessionRecordLocked()
	m.notifySessionChangeLocked()
	slog.Error("solarbeam: session ended locally", "phase", "darwin_session_ended", "session_id", sid, "reason", reason)
}

func (m *Manager) stopSolarbeamInteractive() error {
	m.darwin.supervisorGen++ // retire the supervisor before its child goes
	stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
	stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
	m.darwin.linkEnv = nil
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

// engineFileVersion reads the bundle's CFBundleShortVersionString (falling
// back to CFBundleVersion, then a VERSION file beside the executable). It
// never runs the engine: `--version` can start legacy initialisation, and on
// a Mac that can raise a TCC prompt.
func engineFileVersion(path string) (string, error) {
	if app := darwinAppBundleOf(path); app != "" {
		f, err := os.Open(DarwinInfoPlistPath(app))
		if err == nil {
			defer f.Close()
			plist, err := parseInfoPlist(f)
			if err == nil {
				if plist.ShortVersion != "" {
					return plist.ShortVersion, nil
				}
				if plist.Version != "" {
					return plist.Version, nil
				}
			}
		}
	}
	for _, candidate := range []string{filepath.Join(filepath.Dir(path), "VERSION"), filepath.Join(filepath.Dir(path), "version.txt")} {
		if raw, err := os.ReadFile(candidate); err == nil {
			if version := trimVersion(raw); version != "" {
				return version, nil
			}
		}
	}
	return "", fmt.Errorf("read engine version: no Info.plist version or VERSION file for %s", path)
}

func trimVersion(raw []byte) string {
	s := string(raw)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
