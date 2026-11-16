//go:build windows

package solarbeam

// Native link-auth / restricted-bridge acceptance for the Windows build VM.
// Skipped unless SOLARBEAM_VM_E2E=1, and it must run as LocalSystem with the
// SolarBeam MSI installed: it drives the real Manager.Start against the
// installed engine and bridge, SunshineService and %ProgramData%\SolarBeam.
//
//	SOLARBEAM_VM_READYAPP   fake ReadyApp base URL serving /solarbeam/venue/bundle
//	SOLARBEAM_VM_ENGINE_LOG the engine's sunshine.log
//
// The bridge gets a real LiveKit (dev server) through the bundle. Runbook:
// docs/runbooks/solarbeam-bridge-least-privilege.md.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/playerreadyportsmouth/readyfleet/proto"
)

const (
	vmEngine = `C:\Program Files\SolarBeam\sunshine.exe`
	vmBridge = `C:\Program Files\SolarBeam\solarbeam-bridge.exe`
)

func TestVMLinkAuthAcceptance(t *testing.T) {
	if os.Getenv("SOLARBEAM_VM_E2E") != "1" {
		t.Skip("VM acceptance only (SOLARBEAM_VM_E2E=1, as SYSTEM)")
	}
	if self, err := windows.GetCurrentProcessToken().GetTokenUser(); err != nil || !self.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		t.Fatal("run as LocalSystem")
	}
	readyapp, engineLog := os.Getenv("SOLARBEAM_VM_READYAPP"), os.Getenv("SOLARBEAM_VM_ENGINE_LOG")
	childLogDir = t.TempDir()
	bridgeLog := filepath.Join(childLogDir, "bridge.log")
	args := proto.SolarbeamStartArgs{SessionID: "vm-session", MachineID: "vm-machine", ReadyappURL: readyapp, BundleToken: "vm-bundle"}
	manager := func(mode string, restricted bool) *Manager {
		return New(Paths{Mode: ModeManaged, StateDir: t.TempDir(), Engine: vmEngine, Bridge: vmBridge,
			LinkAuthMode: mode, RestrictedBridge: restricted})
	}
	stop := func(m *Manager) {
		if err := m.Stop(context.Background(), proto.StopSolarbeamArgs{SessionID: args.SessionID}); err != nil {
			t.Errorf("stop: %v", err)
		}
	}

	// An old agent left legacy bytes in a folder Users can write to.
	t.Run("upgrade from old-agent state", func(t *testing.T) {
		if err := os.MkdirAll(bridgeEnvDir, 0o755); err != nil {
			t.Fatal(err)
		}
		loose, _ := windows.SecurityDescriptorFromString("D:(A;OICI;FA;;;BU)(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
		dacl, _, _ := loose.DACL()
		if err := windows.SetNamedSecurityInfo(bridgeEnvDir, windows.SE_FILE_OBJECT,
			windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
			t.Fatal(err)
		}
		legacy := "SOLARBEAM_BRIDGE_WS=" + bridgeWSURL + "\nSOLARBEAM_MACHINE_ID=" + args.MachineID + "\n"
		if err := os.WriteFile(bridgeEnvFile, []byte(legacy), 0o644); err != nil {
			t.Fatal(err)
		}
		m := manager("enforce", true)
		bOff, eOff := logSize(bridgeLog), logSize(engineLog)
		if err := m.Start(context.Background(), args); err != nil {
			t.Fatalf("enforce start: %v", err)
		}
		defer stop(m)
		assertSystemAdminsOnly(t, bridgeEnvDir)
		assertSystemAdminsOnly(t, windowsLinkSecretPath())
		env, _ := os.ReadFile(bridgeEnvFile)
		if !strings.Contains(string(env), "SOLARBEAM_LINK_AUTH_MODE=enforce\n") || strings.Contains(string(env), "SOLARBEAM_LINK_SECRET=") {
			t.Fatalf("bridge.env = %q", env)
		}
		assertRestrictedBridgeProcess(t)
		waitLog(t, bridgeLog, bOff, "link auth: engine authenticated")
		waitLog(t, engineLog, eOff, "solarbeam ws: link authenticated")
	})

	t.Run("enforce with missing secret refuses", func(t *testing.T) {
		// Engine side: provisioned enforce config, protected file absent.
		if err := removeWindowsLinkSecret(); err != nil {
			t.Fatal(err)
		}
		if _, err := writeBridgeEnv(args.MachineID, "enforce"); err != nil {
			t.Fatal(err)
		}
		eOff := logSize(engineLog)
		if err := restartEngineService(); err != nil {
			t.Fatal(err)
		}
		waitLog(t, engineLog, eOff, "solarbeam ws: link authentication configuration unavailable or invalid")
		_ = stopEngineService()
		// Bridge side: enforce with no credential refuses to start at all.
		bOff := logSize(bridgeLog)
		proc, done := startVMBridge(t, bridgeLog, args, "SOLARBEAM_LINK_AUTH_MODE=enforce")
		exited := make(chan *os.ProcessState, 1)
		go func() { state, _ := proc.Wait(); exited <- state }()
		select {
		case state := <-exited:
			<-done
			if state == nil || state.ExitCode() == 0 {
				t.Fatalf("bridge exit = %v, want a failure", state)
			}
		case <-time.After(15 * time.Second):
			_ = proc.Kill()
			t.Fatal("bridge kept running in enforce without a credential")
		}
		waitLog(t, bridgeLog, bOff, "link authentication credential unavailable or invalid")
		if waitForListener(bridgeWSAddr, time.Second) == nil {
			t.Fatal("something is listening on the bridge port")
		}
	})

	t.Run("wrong secret refuses", func(t *testing.T) {
		m := manager("enforce", true)
		if err := m.Start(context.Background(), args); err != nil {
			t.Fatalf("enforce start: %v", err)
		}
		defer stop(m)
		// Replace the engine's file with a different valid key.
		if _, _, err := writeWindowsLinkSecret([]byte(strings.Repeat("ab", 32))); err != nil {
			t.Fatal(err)
		}
		bOff, eOff := logSize(bridgeLog), logSize(engineLog)
		if err := restartEngineService(); err != nil {
			t.Fatal(err)
		}
		waitLog(t, bridgeLog, bOff, "link auth: engine authentication failed")
		waitLog(t, engineLog, eOff, "solarbeam ws: upgrade or link authentication failed")
		assertNoLog(t, engineLog, eOff, "solarbeam ws: link authenticated")
	})

	t.Run("enforce refuses an engine without the capability", func(t *testing.T) {
		m := New(Paths{Mode: ModeManaged, StateDir: t.TempDir(), Engine: vmEngine,
			Bridge: filepath.Join(os.Getenv("SystemRoot"), `System32\whoami.exe`), LinkAuthMode: "enforce", RestrictedBridge: true})
		if err := m.Start(context.Background(), args); err == nil || !strings.Contains(err.Error(), "refusing to start SolarBeam unauthenticated") {
			t.Fatalf("start = %v, want enforce refusal", err)
		}
	})

	for _, restricted := range []bool{true, false} {
		name := map[bool]string{true: "rollback to warn (restricted)", false: "rollback to warn and SYSTEM bridge"}[restricted]
		t.Run(name, func(t *testing.T) {
			m := manager("warn", restricted)
			bOff, eOff := logSize(bridgeLog), logSize(engineLog)
			if err := m.Start(context.Background(), args); err != nil {
				t.Fatalf("warn start: %v", err)
			}
			defer stop(m)
			if restricted {
				assertRestrictedBridgeProcess(t)
			}
			// A provisioned warn host still authenticates both ends.
			waitLog(t, bridgeLog, bOff, "link auth: engine authenticated")
			waitLog(t, engineLog, eOff, "solarbeam ws: link authenticated")
		})
	}

	// Agent rollback: the pre-link-auth agent writes byte-identical legacy
	// bridge.env and runs the bridge as SYSTEM with no credential. Leftover
	// link-auth files from this agent must not stop that pair linking.
	t.Run("rollback to the previous agent", func(t *testing.T) {
		if _, err := writeBridgeEnv(args.MachineID, ""); err != nil {
			t.Fatal(err)
		}
		if err := stopBridgeForLinkRotation(filepath.Base(vmBridge)); err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(bridgeLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer out.Close()
		bOff, eOff := logSize(bridgeLog), logSize(engineLog)
		cmd := newHiddenCmd(vmBridge)
		cmd.Env = append(withoutLinkAuth(os.Environ()), "READYAPP_URL="+args.ReadyappURL, "SESSION_ID="+args.SessionID,
			"SOLARBEAM_MACHINE_ID="+args.MachineID, "SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = stopEngineService() }()
		if err := waitForListener(bridgeWSAddr, 30*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := restartEngineService(); err != nil {
			t.Fatal(err)
		}
		waitLog(t, bridgeLog, bOff, "link auth: WARNING allowing unauthenticated legacy engine (warn mode)")
		assertNoLog(t, engineLog, eOff, "solarbeam ws: link authentication configuration unavailable or invalid")
	})
}

// startVMBridge launches the installed bridge restricted with a hand-built
// link-auth environment, as launchBridge would.
func startVMBridge(t *testing.T, logPath string, args proto.SolarbeamStartArgs, extra ...string) (*os.Process, <-chan struct{}) {
	t.Helper()
	if err := stopBridgeForLinkRotation(filepath.Base(vmBridge)); err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	env := append(withoutLinkAuth(os.Environ()), "READYAPP_URL="+args.ReadyappURL, "SESSION_ID="+args.SessionID,
		"SOLARBEAM_MACHINE_ID="+args.MachineID, "SOLARBEAM_BUNDLE_TOKEN="+args.BundleToken)
	proc, copied, err := startRestrictedBridge(vmBridge, append(env, extra...), out)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { <-copied; out.Close(); close(done) }()
	return proc, done
}

// assertRestrictedBridgeProcess inspects the running bridge from outside:
// LocalService, Low integrity, SeChangeNotify only (the "whoami" check).
func assertRestrictedBridgeProcess(t *testing.T) {
	t.Helper()
	var pid uint32
	_ = forEachProcessNamed(filepath.Base(vmBridge), func(p uint32) bool { pid = p; return false })
	if pid == 0 {
		t.Fatal("bridge not running")
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h)
	var token windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	account, domain, _, _ := user.User.Sid.LookupAccount("")
	label := (*windows.Tokenmandatorylabel)(unsafe.Pointer(&tokenInfo(t, token, windows.TokenIntegrityLevel)[0]))
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&tokenInfo(t, token, windows.TokenPrivileges)[0]))
	var changeNotify windows.LUID
	if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr("SeChangeNotifyPrivilege"), &changeNotify); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range privileges.AllPrivileges() {
		names = append(names, map[bool]string{true: "SeChangeNotifyPrivilege", false: "other"}[p.Luid == changeNotify])
	}
	t.Logf("bridge pid %d runs as %s\\%s, integrity %s, privileges %v", pid, domain, account, label.Label.Sid, names)
	if !user.User.Sid.IsWellKnown(windows.WinLocalServiceSid) || !label.Label.Sid.IsWellKnown(windows.WinLowLabelSid) ||
		len(names) != 1 || names[0] != "SeChangeNotifyPrivilege" {
		t.Fatal("bridge is not a Low-integrity LocalService process holding only SeChangeNotify")
	}
}

func logSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func logSince(path string, offset int64) string {
	data, _ := os.ReadFile(path)
	if int64(len(data)) < offset {
		return string(data) // rotated
	}
	return string(data[offset:])
}

func waitLog(t *testing.T, path string, offset int64, want string) {
	t.Helper()
	for deadline := time.Now().Add(45 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if strings.Contains(logSince(path, offset), want) {
			t.Logf("%s: %q", filepath.Base(path), want)
			return
		}
	}
	t.Fatalf("%s never logged %q; tail:\n%s", path, want, tail(logSince(path, offset)))
}

func assertNoLog(t *testing.T, path string, offset int64, unwanted string) {
	t.Helper()
	time.Sleep(3 * time.Second)
	if strings.Contains(logSince(path, offset), unwanted) {
		t.Fatalf("%s logged %q", path, unwanted)
	}
}

func tail(s string) string {
	if len(s) > 3000 {
		return s[len(s)-3000:]
	}
	return s
}
