//go:build windows

package solarbeam

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

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
const childLogDir = `C:\ProgramData\ReadyFleet\logs`

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
const bridgeEnvDir = `C:\ProgramData\SolarBeam`

// bridgeEnvFile is the per-session env file (KEY=VALUE\n lines) the service
// reads before launching the engine. Not a secret (only the bridge WS URL +
// machine id), so 0644 is fine.
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

// startEngine writes the per-session bridge.env then restarts
// SunshineService so the engine relaunches fresh in bridge mode pointed at
// the just-launched bridge. The bridge must already be listening
// (launchBridge blocks until it is) because the engine's WS client does not
// retry the connect.
func (m *Manager) startEngine(args proto.SolarbeamStartArgs) error {
	if err := writeBridgeEnv(args); err != nil {
		return err
	}
	// SOLARBEAM_BRIDGE_WS switches the engine into bridge mode (spike.cpp);
	// SOLARBEAM_MACHINE_ID is read by the engine's bridge code to validate
	// the per-session control-capability token before injecting input. The
	// service reads bridge.env and injects both into the engine's
	// environment.
	return restartEngineService()
}

// writeBridgeEnv writes %ProgramData%\SolarBeam\bridge.env (KEY=VALUE\n
// lines) the engine service reads before launching the engine. Overwrites
// any prior session's file.
func writeBridgeEnv(args proto.SolarbeamStartArgs) error {
	if err := os.MkdirAll(bridgeEnvDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", bridgeEnvDir, err)
	}
	contents := "SOLARBEAM_BRIDGE_WS=" + bridgeWSURL + "\n" +
		"SOLARBEAM_MACHINE_ID=" + args.MachineID + "\n"
	if err := os.WriteFile(bridgeEnvFile, []byte(contents), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", bridgeEnvFile, err)
	}
	return nil
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
	if err := s.Start(); err != nil {
		return fmt.Errorf("start %s: %w", engineServiceName, err)
	}
	if !waitForServiceState(s, svc.Running, serviceStateTimeout) {
		return fmt.Errorf("%s did not reach running within %s", engineServiceName, serviceStateTimeout)
	}
	return nil
}

// stopServiceIfRunning issues a Stop control if the service is not already
// stopped, then waits for the stopped state.
func stopServiceIfRunning(s *mgr.Service) error {
	st, err := s.Query()
	if err != nil {
		return fmt.Errorf("query %s: %w", engineServiceName, err)
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop %s: %w", engineServiceName, err)
	}
	if !waitForServiceState(s, svc.Stopped, serviceStateTimeout) {
		return fmt.Errorf("%s did not stop within %s", engineServiceName, serviceStateTimeout)
	}
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
func (m *Manager) stopSolarbeam() error {
	if err := stopEngineService(); err != nil {
		return err
	}
	if m.paths.Bridge != "" {
		terminateByName(filepath.Base(m.paths.Bridge))
	}
	if err := os.Remove(bridgeEnvFile); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", bridgeEnvFile, err)
	}
	return nil
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
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := windows.Process32First(snap, &pe); err != nil {
		return
	}
	target := strings.ToLower(name)
	for {
		if strings.ToLower(windows.UTF16ToString(pe.ExeFile[:])) == target {
			if h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pe.ProcessID); err == nil {
				_ = windows.TerminateProcess(h, 1)
				_ = windows.CloseHandle(h)
			}
		}
		if err := windows.Process32Next(snap, &pe); err != nil {
			return
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
// holds those tokens. The bridge stays agent-launched in session 0
// (networking only); it persists across the service's per-session engine
// relaunches.
func (m *Manager) launchBridge(args proto.SolarbeamStartArgs) error {
	if m.paths.Bridge == "" {
		return fmt.Errorf("bridge path not configured")
	}
	// Clear a stale bridge from a prior session so it can rebind bridgeWSAddr.
	terminateByName(filepath.Base(m.paths.Bridge))
	cmd := exec.Command(m.paths.Bridge)
	cmd.Env = append(os.Environ(),
		"READYAPP_URL="+args.ReadyappURL,
		"SESSION_ID="+args.SessionID,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
		"SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken,
	)
	// Best-effort stdout/stderr capture to bridge.log so publish/connect
	// failures are visible. os/exec passes the file's handle to the child,
	// so closing our copy after Start is safe — the child keeps its own.
	logDir := childLogDir
	if m.paths.Mode == ModeInteractive {
		logDir = childLogDirInteractive()
	}
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		if logf, ferr := os.OpenFile(filepath.Join(logDir, "bridge.log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			cmd.Stdout = logf
			cmd.Stderr = logf
			defer logf.Close()
		}
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start bridge: %w", err)
	}
	// Block until the bridge is listening on its local WS — the engine
	// launch follows and the engine does not retry the connect. The bridge
	// fetches its venue bundle and joins LiveKit before it listens, so
	// allow generous time; if it crashes (e.g. bundle/LiveKit failure) it
	// never listens and this surfaces the failure instead of starting an
	// engine that cannot connect.
	if err := waitForListener(bridgeWSAddr, 30*time.Second); err != nil {
		return fmt.Errorf("bridge not listening on %s: %w", bridgeWSAddr, err)
	}
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
func (m *Manager) startEngineInteractive(args proto.SolarbeamStartArgs) error {
	if m.interactiveEngineProc != nil {
		_ = m.interactiveEngineProc.Kill()
		m.interactiveEngineProc = nil
	}
	cmd := exec.Command(m.paths.Engine)
	cmd.Env = append(os.Environ(),
		"SOLARBEAM_BRIDGE_WS="+bridgeWSURL,
		"SOLARBEAM_MACHINE_ID="+args.MachineID,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start engine: %w", err)
	}
	m.interactiveEngineProc = cmd.Process
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
		terminateByName(filepath.Base(m.paths.Bridge))
	}
	return nil
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
