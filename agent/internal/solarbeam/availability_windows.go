//go:build windows

package solarbeam

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

func replaceHostingState(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// This is a deliberate device-level refusal, unlike a session-scoped Stop.
// Exact configured paths define the managed device policy boundary across all
// users. BYOD additionally requires the agent token SID. This never
// terminates by basename and never trusts a loopback status/PID claim. Open
// handles retain process identity through inspection and termination.
func (m *Manager) disableConfiguredHost() error {
	if !filepath.IsAbs(m.paths.Engine) || !filepath.IsAbs(m.paths.Bridge) {
		return errors.New("hosting off: exact engine and bridge install paths are required")
	}
	var serviceErr error
	if m.paths.Mode == ModeManaged {
		serviceErr = m.stopConfiguredEngineService()
	}
	return errors.Join(serviceErr, terminateConfiguredProcesses(m.paths.Mode, m.paths.Engine, m.paths.Bridge))
}

func (m *Manager) stopConfiguredEngineService() error {
	sc, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer sc.Disconnect()
	service, err := sc.OpenService(engineServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return err
	}
	defer service.Close()
	cfg, err := service.Config()
	if err != nil {
		return err
	}
	args, err := windows.DecomposeCommandLine(cfg.BinaryPathName)
	expected := filepath.Join(filepath.Dir(m.paths.Engine), "tools", engineServiceBinName)
	if err != nil || len(args) != 1 || !sameConfiguredPath(args[0], expected) {
		return errors.New("hosting off: SolarBeam service belongs to a different or unknown installation")
	}
	return stopServiceIfRunning(service)
}

func sameConfiguredPath(actual, expected string) bool {
	if !filepath.IsAbs(actual) || !filepath.IsAbs(expected) {
		return false
	}
	return strings.EqualFold(filepath.Clean(actual), filepath.Clean(expected))
}

func terminateConfiguredProcesses(mode Mode, paths ...string) error {
	var owner *windows.SID
	if mode == ModeInteractive {
		user, err := windows.GetCurrentProcessToken().GetTokenUser()
		if err != nil {
			return fmt.Errorf("hosting off: agent owner: %w", err)
		}
		owner = user.User.Sid
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	var failures []error
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		name := windows.UTF16ToString(entry.ExeFile[:])
		candidate := false
		for _, path := range paths {
			if path != "" && strings.EqualFold(name, filepath.Base(path)) {
				candidate = true
			}
		}
		if !candidate {
			continue
		}
		if e := terminateConfiguredProcess(entry.ProcessID, mode, owner, paths); e != nil {
			failures = append(failures, e)
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func terminateConfiguredProcess(pid uint32, mode Mode, owner *windows.SID, paths []string) error {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	} // exited
	if err != nil {
		return fmt.Errorf("hosting off: inspect candidate %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)
	var buf [32768]uint16
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return err
	}
	match := false
	for _, path := range paths {
		if sameConfiguredPath(windows.UTF16ToString(buf[:n]), path) {
			match = true
		}
	}
	return stopHostProcessInScope(mode, match, func() (bool, error) {
		if owner == nil {
			return false, errors.New("hosting off: BYOD owner unavailable")
		}
		var token windows.Token
		if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token); err != nil {
			return false, err
		}
		defer token.Close()
		user, err := token.GetTokenUser()
		if err != nil {
			return false, err
		}
		return user.User.Sid.Equals(owner), nil
	}, func() error {
		if err := windows.TerminateProcess(h, 0); err != nil {
			state, _ := windows.WaitForSingleObject(h, 0)
			if state != windows.WAIT_OBJECT_0 {
				return fmt.Errorf("hosting off: terminate %d: %w", pid, err)
			}
		}
		state, err := windows.WaitForSingleObject(h, 5000)
		if err != nil {
			return err
		}
		if state != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("hosting off: process %d has not stopped", pid)
		}
		return nil
	})
}
