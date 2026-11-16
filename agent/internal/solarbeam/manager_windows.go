//go:build windows

package solarbeam

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/winpaths"
	"github.com/playerreadyportsmouth/readyfleet/proto"
)

// engineServiceName is the bundled engine service Start installs once and
// (re)starts per session. The service (session 0, LocalSystem) re-targets
// the SYSTEM token to the active console session and launches the engine
// there as SYSTEM, so it follows the input desktop and captures the secure
// desktop.
const engineServiceName = "SunshineService"

// engineServiceBinName is the service host bundled with the engine, located
// in the engine install dir's tools\ subfolder.
const engineServiceBinName = "sunshinesvc.exe"

// bridgeWSAddr is the localhost address the Go bridge listens on for the
// engine (BRIDGE_WS_ADDR default in the bridge), and bridgeWSURL is the
// matching ws:// URL the engine dials when SOLARBEAM_BRIDGE_WS is set
// (spike.cpp → bridge mode).
const (
	bridgeWSAddr = "127.0.0.1:9092"
	bridgeWSURL  = "ws://127.0.0.1:9092/bridge"
)

// childLogDir is where the agent captures the bridge's stdout/stderr,
// alongside the agent's own log. SYSTEM-writable, ModeManaged only — the
// engine is launched by the service there, which writes its own log; only
// the bridge is agent-launched. ModeInteractive uses childLogDirInteractive
// instead: a plain user account has no guaranteed write access to
// %ProgramData%, and this is best-effort diagnostic capture, not something
// worth failing the whole start over, so getting the directory wrong would
// otherwise just silently lose the log rather than error loudly.
var childLogDir = filepath.Join(winpaths.ProgramData(), "ReadyFleet", "logs")

// childLogDirInteractive is ModeInteractive's per-user equivalent of
// childLogDir — always writable by the account that's running it.
func childLogDirInteractive() string {
	base := os.Getenv("LocalAppData")
	if base == "" {
		base = "lightagent-data" // dev fallback, mirrors lightagent's own convention
	}
	return filepath.Join(base, "ReadyFleet Light", "logs")
}

// bridgeEnvDir is the engine-owned, generic SolarBeam config location Start
// writes bridge.env into. sunshinesvc.exe reads it and injects the
// SOLARBEAM_* vars into the engine's environment; if absent the engine runs
// stock (standalone behaviour preserved). The path uses generic
// SOLARBEAM_* naming and no fleetmgr/ReadyApp coupling, keeping the GPL
// seam clean.
//
// Known gap: sunshinesvc.exe (engine 2.1.25 and earlier) opens
// C:\ProgramData\SolarBeam\bridge.env literally, so on a PC whose Windows is
// not on C: it needs the matching engine change to see this file.
var bridgeEnvDir = filepath.Join(winpaths.ProgramData(), "SolarBeam")

// bridgeEnvFile is the per-session env file (KEY=VALUE\n lines) the service
// reads before launching the engine. Not a secret, but a trust root: it and
// its folder are SYSTEM/Administrators-only (see bridgeenv_windows.go).
var bridgeEnvFile = filepath.Join(bridgeEnvDir, "bridge.env")

// serviceStateTimeout bounds how long we wait for a service stop/start to
// take effect before giving up and surfacing the failure.
const serviceStateTimeout = 15 * time.Second

// prepareEngine validates the engine can be launched WITHOUT starting it:
// the engine binary is installed AND the bundled SunshineService is
// installed (installing it if missing). Running this before the bridge
// means a machine that cannot host fails cleanly without leaving an
// orphaned bridge connected to LiveKit.
func (m *Manager) prepareEngine() error {
	if err := engineBinaryExists(m.paths.Engine); err != nil {
		return err
	}
	return ensureEngineService(m.paths.Engine)
}

// engineServicePath is the absolute path to the bundled sunshinesvc.exe,
// derived from the engine binary's install dir (tools\sunshinesvc.exe).
func engineServicePath(enginePath string) string {
	return filepath.Join(filepath.Dir(enginePath), "tools", engineServiceBinName)
}

// ensureEngineService makes sure SunshineService is installed, registering
// it against the bundled sunshinesvc.exe if missing. It does not start the
// service (startEngine restarts it per session). A missing sunshinesvc.exe
// is a clear error rather than a silent skip — without it secure-desktop
// capture cannot work.
func ensureEngineService(enginePath string) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(engineServiceName); err == nil {
		s.Close()
		return nil
	}

	svcBin := engineServicePath(enginePath)
	if _, err := os.Stat(svcBin); err != nil {
		return fmt.Errorf("engine service host %s not installed: %w", svcBin, err)
	}
	s, err := m.CreateService(engineServiceName, svcBin, mgr.Config{
		DisplayName:  engineServiceName,
		Description:  "SolarBeam engine host (secure-desktop capture).",
		StartType:    mgr.StartManual,
		ErrorControl: mgr.ErrorNormal,
	})
	if err != nil {
		return fmt.Errorf("create %s service: %w", engineServiceName, err)
	}
	s.Close()
	return nil
}

// startEngine applies the nonsecret service environment and reuses a healthy
// warm engine only after that configuration has been imported successfully.
// Secret rotation keeps a stable path; the engine reloads its bytes on reconnect.
func (m *Manager) startEngine(args proto.SolarbeamStartArgs) error {
	// SOLARBEAM_BRIDGE_WS switches the engine into bridge mode (spike.cpp);
	// SOLARBEAM_MACHINE_ID is read by the engine's bridge code to validate
	// the per-session control-capability token before injecting input. The
	// service reads bridge.env and injects both into the engine's
	// environment.
	mode := ""
	if m.linkCredential != nil {
		mode = m.linkCredential.mode
	}
	changed, err := writeBridgeEnv(args.MachineID, mode)
	if err != nil {
		return err
	}
	if m.warmEngineEnabled() && !changed && (mode == "" || m.linkEngineVerified) && m.healthyFn() {
		slog.Info("solarbeam: engine already warm, skipping restart",
			"phase", "engine_warm_reused", "session_id", args.SessionID)
		return nil
	}
	if err := m.restartFn(); err != nil {
		return err
	}
	if err := markBridgeEnvLoaded(); err != nil {
		return err
	}
	m.linkEngineVerified = mode != ""
	return nil
}

// engineHealthy reports whether the resident engine is healthy enough to
// hand a new session to without restarting it.
//
// Increment 1 is liveness only (warm-engine design §4.3, Aurora's ruling):
// the service is Running AND a real sunshine.exe process exists. That is
// cheap and it catches the failure that actually bit us — a bad binary
// crash-looping under sunshinesvc, where the SERVICE reports Running while
// no engine survives. It does not catch a hung-but-alive engine.
//
// TODO(warm-engine §4.3 option 2): the durable answer is connect-observed
// health — after the bridge is up, if no engine WS connection arrives
// within N seconds, restart the service and retry once. That directly
// observes the property we care about instead of proxying it, and the
// bridge already logs "engine connected"; it needs a bridge→agent signal
// that does not exist yet, so it is deliberately out of scope here.
func (m *Manager) engineHealthy() bool {
	if !m.engineServiceRunning() {
		return false
	}
	if m.paths.Engine == "" {
		return false
	}
	return processExistsByName(filepath.Base(m.paths.Engine))
}

// preloadEngine is the warm-engine start-of-day path (design §5.1): write
// the machine-constant bridge.env and bring the engine up if it is not
// already healthy, so the first session of the day does not pay the ~4.8s
// cold start. An unhealthy engine (service Running but crash-looping) is
// restarted rather than left alone, which keeps agent restart working as a
// coarse self-heal for the wedge §4.3 option 1 cannot otherwise detect.
func (m *Manager) preloadEngine(machineID string) error {
	mode, err := m.compatibleLinkAuthMode()
	if err != nil {
		return err
	}
	if mode != "" && !m.linkEngineVerified && m.bridgeRunningFn() {
		return fmt.Errorf("link authentication configuration pending until the active session ends")
	}
	changed, err := writeBridgeEnvWhenIdle(machineID, mode, m.bridgeRunningFn)
	if err != nil {
		return err
	}
	if !changed && (mode == "" || m.linkEngineVerified) && m.healthyFn() {
		slog.Info("solarbeam: engine already warm at preload",
			"phase", "engine_preload_skipped")
		return nil
	}
	if err := m.restartFn(); err != nil {
		return err
	}
	if err := markBridgeEnvLoaded(); err != nil {
		return err
	}
	m.linkEngineVerified = mode != ""
	return nil
}

// writeBridgeEnv returns whether the running service still needs to import
// this configuration. The path is stable across sessions; mode is per machine.
// The folder is re-secured on every call: an older agent may have written
// identical legacy bytes into an inherited, user-writable folder.
func writeBridgeEnv(machineID, mode string) (bool, error) {
	return writeBridgeEnvWhenIdle(machineID, mode, nil)
}

func writeBridgeEnvWhenIdle(machineID, mode string, bridgeRunning func() bool) (bool, error) {
	contents, err := linkBridgeEnvironment(machineID, windowsLinkSecretPath(), mode)
	if err != nil {
		return false, err
	}
	if _, err := secureBridgeEnvDir(); err != nil {
		return false, fmt.Errorf("secure %s: %w", bridgeEnvDir, err)
	}
	old, _ := os.ReadFile(bridgeEnvFile)
	applied, _ := os.ReadFile(bridgeEnvFile + ".applied")
	if (string(old) != contents || string(applied) != contents) && bridgeRunning != nil && bridgeRunning() {
		return false, fmt.Errorf("link authentication configuration pending until the active session ends")
	}
	if string(old) == contents {
		return string(applied) != contents, nil
	}
	// Invalidate the acknowledgement before changing what the service reads.
	if err := os.Remove(bridgeEnvFile + ".applied"); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if err := writeBridgeEnvFile(bridgeEnvFile, []byte(contents)); err != nil {
		return false, err
	}
	return true, nil
}

// A failed service restart must remain pending across agent restarts. Merely
// comparing bridge.env cannot prove the running service imported those bytes.
func markBridgeEnvLoaded() error {
	contents, err := os.ReadFile(bridgeEnvFile)
	if err != nil {
		return err
	}
	return writeBridgeEnvFile(bridgeEnvFile+".applied", contents)
}

// restartEngineService stops SunshineService if running, then starts it,
// waiting for each state transition. Restarting (rather than a plain
// start) guarantees a fresh engine that re-reads bridge.env and dials the
// new bridge.
func restartEngineService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(engineServiceName)
	if err != nil {
		return fmt.Errorf("open %s: %w", engineServiceName, err)
	}
	defer s.Close()

	if err := stopServiceIfRunning(s); err != nil {
		return err
	}
	slog.Info("solarbeam: engine service start starting", "phase", "service_start_start")
	if err := s.Start(); err != nil {
		// Something else may have started it between our stop and this
		// start — an engine MSI finishing its own service handling, or
		// sunshinesvc's recovery action. That is the outcome we wanted, so
		// only treat the error as fatal if the service is not on its way
		// up. Queried once rather than waited on, so a genuine failure
		// still surfaces immediately.
		if st, qerr := s.Query(); qerr != nil ||
			(st.State != svc.Running && st.State != svc.StartPending) {
			return fmt.Errorf("start %s: %w", engineServiceName, err)
		}
	}
	if !waitForServiceState(s, svc.Running, serviceStateTimeout) {
		return fmt.Errorf("%s did not reach running within %s", engineServiceName, serviceStateTimeout)
	}
	slog.Info("solarbeam: engine service start confirmed", "phase", "service_start_confirmed")
	return nil
}

// serviceStateIsPending reports whether state is one of the SCM's
// transitional states — a service that is on its way somewhere and cannot
// reliably be sent a control right now.
//
// This matters because the warm-engine work added a caller that arrives at
// an UNKNOWN moment: the re-preload that runs straight after an engine MSI
// install, while the installer may still be stopping or starting
// SunshineService itself. Issuing a Stop control to a StopPending service
// fails with ERROR_SERVICE_CANNOT_ACCEPT_CTRL, which would turn a perfectly
// normal race into a preload error.
func serviceStateIsPending(state svc.State) bool {
	switch state {
	case svc.StartPending, svc.StopPending, svc.ContinuePending, svc.PausePending:
		return true
	default:
		return false
	}
}

// waitForServiceToSettle polls until the service leaves any transitional
// state or timeout elapses, returning the last state it managed to read. A
// query failure ends the wait — the caller's next operation will surface
// the real error with better context than this helper could.
func waitForServiceToSettle(s *mgr.Service, timeout time.Duration) svc.State {
	deadline := time.Now().Add(timeout)
	last := svc.Stopped
	for {
		st, err := s.Query()
		if err != nil {
			return last
		}
		last = st.State
		if !serviceStateIsPending(st.State) || time.Now().After(deadline) {
			return st.State
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// stopServiceIfRunning issues a Stop control if the service is not already
// stopped, then waits for the stopped state. A service found mid-transition
// is allowed to settle first rather than being sent a control it cannot
// accept — see serviceStateIsPending.
func stopServiceIfRunning(s *mgr.Service) error {
	slog.Info("solarbeam: engine service stop starting", "phase", "service_stop_start")
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query %s: %w", engineServiceName, err)
	}
	state := st.State
	if serviceStateIsPending(state) {
		state = waitForServiceToSettle(s, serviceStateTimeout)
	}
	if state == svc.Stopped {
		slog.Info("solarbeam: engine service stop confirmed", "phase", "service_stop_confirmed", "already_stopped", true)
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		// It may have reached Stopped on its own between the query above
		// and this control (an installer completing its own stop), which
		// is the state we were asking for.
		if st, qerr := s.Query(); qerr == nil && st.State == svc.Stopped {
			return nil
		}
		return fmt.Errorf("stop %s: %w", engineServiceName, err)
	}
	if !waitForServiceState(s, svc.Stopped, serviceStateTimeout) {
		return fmt.Errorf("%s did not stop within %s", engineServiceName, serviceStateTimeout)
	}
	slog.Info("solarbeam: engine service stop confirmed", "phase", "service_stop_confirmed", "already_stopped", false)
	return nil
}

// waitForServiceState polls until the service reaches want or timeout
// elapses. Returns true if it reached want.
func waitForServiceState(s *mgr.Service, want svc.State, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, err := s.Query()
		if err == nil && st.State == want {
			return true
		}
		time.Sleep(250 * time.Millisecond)
	}
	st, err := s.Query()
	return err == nil && st.State == want
}

// stopSolarbeam tears the session down: stop SunshineService (the engine
// dies via the service's job object), kill the agent-launched bridge, and
// remove bridge.env so a stale per-session config cannot leak into the
// next stock launch. Best-effort on the bridge/env cleanup; a failure to
// stop the service is the only hard error (it would otherwise keep
// capturing).
//
// With warm-engine enabled the engine stays up (design §4.2) and only the
// bridge is torn down. bridge.env stays too: under §5.1 it is written at
// agent start and its contents are machine-constant, so it no longer marks
// a session, and deleting it would leave an engine respawned by
// sunshinesvc running STOCK instead of in bridge mode — i.e. it would
// silently un-warm the machine until the next agent restart. The original
// "don't leak a stale per-session config into the next stock launch"
// rationale does not apply when there is no next stock launch by design.
//
// Killing the bridge is what actually ends the session: it owns the
// LiveKit room, so no viewer survives it, and the engine drops to its
// reconnect loop and (with the disconnect gate reset, §4.1) back to
// unauthorised input.
func (m *Manager) stopSolarbeam() error {
	if m.warmEngineEnabled() {
		if m.paths.Bridge != "" {
			if err := stopBridgeForLinkRotation(filepath.Base(m.paths.Bridge)); err != nil {
				return err
			}
		}
		slog.Info("solarbeam: session stopped, engine left warm",
			"phase", "stop_engine_left_warm")
		return m.cleanupStoppedLinkAuth()
	}
	if err := m.stopServiceFn(); err != nil {
		return err
	}
	if m.paths.Bridge != "" {
		if err := stopBridgeForLinkRotation(filepath.Base(m.paths.Bridge)); err != nil {
			return err
		}
	}
	if err := os.Remove(bridgeEnvFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", bridgeEnvFile, err)
	}
	return m.cleanupStoppedLinkAuth()
}

// stopEngineService stops SunshineService if it is installed and running. A
// missing service is not an error (nothing to stop).
func stopEngineService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect SCM: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(engineServiceName)
	if err != nil {
		// Service not installed → nothing to stop.
		return nil
	}
	defer s.Close()
	return stopServiceIfRunning(s)
}

// engineServiceRunning reports whether SunshineService is currently in the
// Running state. A missing/unqueryable service reports false rather than
// erroring — Status has no error return, so "not running" is the honest
// answer for "we can't tell."
func (m *Manager) engineServiceRunning() bool {
	sc, err := mgr.Connect()
	if err != nil {
		return false
	}
	defer sc.Disconnect()

	s, err := sc.OpenService(engineServiceName)
	if err != nil {
		return false
	}
	defer s.Close()

	st, err := s.Query()
	return err == nil && st.State == svc.Running
}

// terminateByName force-terminates every process whose image name matches
// name (case-insensitive). Best-effort: failures to open/terminate an
// individual process are ignored. Used to clear a stale bridge before a
// per-session relaunch and on teardown. The agent runs as LocalSystem,
// which can terminate processes in other sessions.
func terminateByName(name string) {
	forEachProcessNamed(name, func(pid uint32) bool {
		if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid); err == nil {
			_ = windows.TerminateProcess(h, 1)
			_ = windows.CloseHandle(h)
		}
		return true // keep going: kill every matching instance
	})
}

// processExistsByName reports whether at least one process with image name
// name (case-insensitive) exists. Used by engineHealthy to tell a genuinely
// resident engine apart from a service the SCM calls Running while
// sunshinesvc crash-loops the binary underneath it, and by
// bridgeProcessRunning as SessionActive's outer gate.
//
// A snapshot failure reports false. That is the safe direction for both
// callers: engineHealthy then restarts rather than trusting a warm engine
// it cannot see, and bridgeProcessRunning is only reached in ModeManaged
// where an unreadable process table means we are not going to be able to
// establish a session anyway.
func processExistsByName(name string) bool {
	if name == "" {
		return false
	}
	found := false
	forEachProcessNamed(name, func(uint32) bool {
		found = true
		return false // first hit is enough
	})
	return found
}

// bridgeProcessRunning reports whether the agent-launched bridge process
// exists — SessionActive's outer gate (see its doc comment). An unset
// bridge path reports false: a Manager with no bridge configured cannot
// have launched one, so it cannot be hosting a session.
func (m *Manager) bridgeProcessRunning() bool {
	if m.paths.Bridge == "" {
		return false
	}
	return processExistsByName(filepath.Base(m.paths.Bridge))
}

// forEachProcessNamed walks the process table and calls visit with the pid
// of every process whose image name matches name (case-insensitive),
// stopping early when visit returns false. Callers that establish teardown
// authority must check the error: an unreadable snapshot is not an empty one.
func forEachProcessNamed(name string, visit func(pid uint32) bool) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil
		}
		return err
	}
	target := strings.ToLower(name)
	for {
		if strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])) == target {
			if !visit(pe.ProcessID) {
				return nil
			}
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				return nil
			}
			return err
		}
	}
}

// waitForListener blocks until addr accepts a TCP connection or timeout
// elapses. The engine's WS client does not retry, so the agent waits for
// the bridge to be listening before restarting the engine service.
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

// launchBridge starts the bridge process with the per-session context in
// its environment. The bridge presents BundleToken to ReadyApp's
// /venue/bundle to fetch its engine + publisher tokens; the agent never
// newHiddenCmd builds an exec.Cmd for a ReadyFleet-shipped child binary with
// CREATE_NO_WINDOW set. Both the engine and the bridge are console-subsystem
// binaries (built without -H=windowsgui), and this agent is itself
// GUI-subsystem with no console of its own for a child to inherit — so
// without this, Windows allocates each one a brand new, visible console
// window on every session start. Regression test:
// TestNewHiddenCmd_SuppressesConsoleWindow.
//
// A seam rather than setting SysProcAttr inline at each call site, so the
// property is asserted directly on the constructed *exec.Cmd instead of by
// spawning a real process and inferring it from window/console state after
// the fact — the same "injectable function seam, never spawn a real process
// in a test" convention this repo already uses everywhere else (see
// ReadyFleet's CLAUDE.md). Same idiom exec/screencap_windows.go already uses
// for its own child process, via the raw CreateProcess flag directly.
func newHiddenCmd(path string) *exec.Cmd {
	cmd := exec.Command(path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

// holds those tokens. The bridge stays agent-launched in session 0
// (networking only); it persists across the service's per-session engine
// relaunches.
func (m *Manager) launchBridge(args proto.SolarbeamStartArgs) error {
	if m.paths.Bridge == "" {
		return fmt.Errorf("bridge path not configured")
	}
	// Clear a stale bridge from a prior session so it can rebind bridgeWSAddr.
	if err := stopBridgeForLinkRotation(filepath.Base(m.paths.Bridge)); err != nil {
		return err
	}
	if err := m.provisionPlatformLinkAuth(); err != nil {
		return err
	}
	env := append(m.linkAuthEnvironment(os.Environ()),
		"READYAPP_URL="+args.ReadyappURL,
		"SESSION_ID="+args.SessionID,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
		"SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken,
	)
	// Best-effort stdout/stderr capture to bridge.log so publish/connect
	// failures are visible. The child writes into a pipe the agent drains
	// into a size-capped rotating log (childlog.go) — it used to be handed an
	// O_APPEND file directly, which nothing ever rotated (97 MB on
	// Ports-Admin, 26 Sep 2026). A log failure never fails the launch.
	logDir := childLogDir
	if m.paths.Mode == ModeInteractive {
		logDir = childLogDirInteractive()
	}
	logw := openChildLog(logDir, "bridge.log", 0o755, 0o644)
	restricted := m.restrictedBridge()
	slog.Info("solarbeam: bridge process spawn starting", "phase", "bridge_spawn_start",
		"session_id", args.SessionID, "restricted", restricted)
	var pid int
	var wait func() int
	if restricted {
		proc, copied, err := startRestrictedBridgeFn(m.paths.Bridge, env, logw)
		if err != nil {
			_ = logw.Close()
			return fmt.Errorf("start restricted bridge: %w", err)
		}
		pid = proc.Pid
		wait = func() int {
			state, err := proc.Wait()
			<-copied
			if err != nil {
				return -1
			}
			return state.ExitCode()
		}
	} else {
		cmd := newHiddenCmd(m.paths.Bridge)
		cmd.Env = env
		cmd.Stdout, cmd.Stderr = logw, logw
		cmd.WaitDelay = childPipeWaitDelay
		startErr := cmd.Start()
		cmd.Env = withoutLinkAuth(cmd.Env)
		if err := startErr; err != nil {
			_ = logw.Close()
			return fmt.Errorf("start bridge: %w", err)
		}
		pid = cmd.Process.Pid
		wait = func() int {
			_ = cmd.Wait()
			if cmd.ProcessState == nil {
				return -1
			}
			return cmd.ProcessState.ExitCode()
		}
	}
	// Reap the child, detach its log and record why it ended: the bridge
	// exits by itself with a distinct code when its session ends (3) or
	// LiveKit is lost (4). Wait also releases the pipe and process handle,
	// which a fire-and-forget Start would otherwise leak. Session state is
	// cleared by the reaper (sessionowner.go), which also covers a bridge
	// this agent instance did not launch.
	go func(sessionID string, pid int) {
		code := wait()
		_ = logw.Close()
		slog.Info("solarbeam: bridge process exited", "phase", "bridge_exited",
			"session_id", sessionID, "pid", pid, "exit_code", code, "meaning", BridgeExitMeaning(code))
	}(args.SessionID, pid)
	// Block until the bridge is listening on its local WS — the engine
	// launch follows and the engine does not retry the connect.
	//
	// ⚠ Stale assumption, corrected 2026-08-19: this comment used to say the
	// bridge fetches its venue bundle and joins LiveKit BEFORE it listens, so
	// "listening" implied "LiveKit-viable". The codec-negotiation branch
	// (solarbeam-livekit-bridge) reordered that — the bridge now listens
	// FIRST (so the engine can complete the codec-resolution handshake
	// before any LiveKit publish is shaped) and only connects to LiveKit
	// afterward. So `waitForListener` returning success here no longer
	// guarantees the bridge can actually reach LiveKit — a connect failure
	// can still happen after this call returns, and the engine we launch
	// next has no retry, so it inherits that failure with no clean signal
	// back to us. The bridge repo mitigates this on its side (closes its
	// listener immediately on a post-listen LiveKit failure, so a
	// subsequent engine connect attempt fails fast rather than hanging) —
	// see solarbeam-livekit-bridge's own fix for the matching finding. A
	// real fix here would need a genuine post-listen liveness signal from
	// the bridge (not just "the TCP port is open") — tracked as a follow-up,
	// not solved by this comment alone.
	if err := waitForListener(bridgeWSAddr, 30*time.Second); err != nil {
		return fmt.Errorf("bridge not listening on %s: %w", bridgeWSAddr, err)
	}
	slog.Info("solarbeam: bridge listener confirmed", "phase", "bridge_listener_confirmed", "session_id", args.SessionID)
	return nil
}

// prepareEngineInteractive validates just the engine binary — ModeInteractive
// launches it directly in the caller's own session, so there's no service
// to install/check (see the Mode type doc in manager.go).
func (m *Manager) prepareEngineInteractive() error {
	return engineBinaryExists(m.paths.Engine)
}

// startEngineInteractive launches the engine directly as a normal child
// process, in bridge mode, in the caller's own session — no service, no
// SYSTEM, no secure-desktop capture. Kills any previously
// interactively-launched instance first, matching the service path's
// restart-for-a-fresh-session semantics. The bridge.env file the service
// reads doesn't apply here — the env vars go straight into the child
// process's own environment instead.
// buildEngineCmd constructs (without starting) the interactive engine
// process. A pure function, factored out of startEngineInteractive purely so
// cmd.Dir can be asserted directly in a test — see
// TestBuildEngineCmd_RunsFromItsOwnDirectory — rather than inferred from a
// spawned process's behaviour, which for a config-relative-path bug like this
// one is exactly the kind of thing that "worked" by accident before and
// could again.
func buildEngineCmd(enginePath string, args proto.SolarbeamStartArgs) *exec.Cmd {
	cmd := newHiddenCmd(enginePath)
	// The engine resolves its own asset paths (assets/apps.json etc.)
	// relative to its working directory, not relative to its own binary
	// location — a Start Menu shortcut normally sets "Start in" to the
	// install dir, which is what makes that assumption invisible. Without
	// this the child inherits the AGENT's cwd instead, and the engine fails
	// during config setup with "cannot copy file: No such file or
	// directory" for a file that is genuinely sitting right next to it — it
	// just never looked there. Caught live on 2026-08-18 via engine.log
	// (added the same day this bug was found), on a real BYOD test session
	// that never got past "waiting for the screen".
	cmd.Dir = filepath.Dir(enginePath)
	cmd.Env = append(os.Environ(),
		"SOLARBEAM_BRIDGE_WS="+bridgeWSURL,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
	)
	return cmd
}

func (m *Manager) startEngineInteractive(args proto.SolarbeamStartArgs) error {
	if m.interactiveEngineProc != nil {
		_ = m.interactiveEngineProc.Kill()
		m.interactiveEngineProc = nil
	}
	cmd := buildEngineCmd(m.paths.Engine, args)
	cmd.Env = m.linkAuthEnvironment(cmd.Env)
	// Best-effort stdout/stderr capture to engine.log, mirroring
	// launchBridge's bridge.log (same size-capped rotating log). Hiding the
	// console window above means a crash or startup failure would otherwise
	// leave NO trace anywhere — the only place the engine's own diagnostics
	// went was the console window this same change just suppressed.
	logw := openChildLog(childLogDirInteractive(), "engine.log", 0o755, 0o644)
	cmd.Stdout, cmd.Stderr = logw, logw
	cmd.WaitDelay = childPipeWaitDelay
	startErr := cmd.Start()
	cmd.Env = withoutLinkAuth(cmd.Env)
	if err := startErr; err != nil {
		_ = logw.Close()
		return fmt.Errorf("start engine: %w", err)
	}
	m.interactiveEngineProc = cmd.Process
	go func() { _ = cmd.Wait(); _ = logw.Close() }()
	return nil
}

// stopSolarbeamInteractive kills the interactively-launched engine (if
// any) and the bridge, mirroring stopSolarbeam's managed-mode behavior
// without any service involved.
func (m *Manager) stopSolarbeamInteractive() error {
	if m.interactiveEngineProc != nil {
		_ = m.interactiveEngineProc.Kill()
		m.interactiveEngineProc = nil
	}
	if m.paths.Bridge != "" {
		if err := stopBridgeForLinkRotation(filepath.Base(m.paths.Bridge)); err != nil {
			return err
		}
	}
	return m.cleanupStoppedLinkAuth()
}

// engineProcessRunning reports whether the interactively-launched engine
// process is still alive. A nil interactiveEngineProc (never started, or
// already reaped by a Stop) reports false, same "not running" convention
// engineServiceRunning uses for a missing/unqueryable service.
func (m *Manager) engineProcessRunning() bool {
	if m.interactiveEngineProc == nil {
		return false
	}
	return processAlive(m.interactiveEngineProc.Pid)
}

// processAlive checks liveness via OpenProcess+GetExitCodeProcess rather
// than os.Process.Signal, which on Windows only supports os.Kill/os.Interrupt
// in the standard library — no signal-0-style liveness probe.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// engineFileVersion reads path's own Win32 version resource
// (VS_FIXEDFILEINFO) directly — real at any time regardless of whether
// this Manager instance ever ran Update, unlike lastUpdatedVersion (see
// the Status type doc). GetFileVersionInfoSize/GetFileVersionInfo/
// VerQueryValue are already wrapped by golang.org/x/sys/windows, so this
// needs no hand-declared syscalls.
func engineFileVersion(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return "", fmt.Errorf("get file version info size: %w", err)
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("get file version info: %w", err)
	}
	var fixedInfo unsafe.Pointer
	var fixedInfoLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixedInfo), &fixedInfoLen); err != nil {
		return "", fmt.Errorf("ver query value: %w", err)
	}
	if fixedInfoLen < uint32(unsafe.Sizeof(windows.VS_FIXEDFILEINFO{})) {
		return "", fmt.Errorf("unexpected VS_FIXEDFILEINFO size %d", fixedInfoLen)
	}
	info := (*windows.VS_FIXEDFILEINFO)(fixedInfo)
	major := info.FileVersionMS >> 16
	minor := info.FileVersionMS & 0xFFFF
	build := info.FileVersionLS >> 16
	revision := info.FileVersionLS & 0xFFFF
	if revision == 0 {
		return fmt.Sprintf("%d.%d.%d", major, minor, build), nil
	}
	return fmt.Sprintf("%d.%d.%d.%d", major, minor, build, revision), nil
}
