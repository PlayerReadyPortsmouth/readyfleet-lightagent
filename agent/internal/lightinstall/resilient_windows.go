//go:build windows

package lightinstall

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func platformPreflight(root string) error {
	v := windows.RtlGetVersion()
	if v.MajorVersion < 10 {
		return fmt.Errorf("Windows 10 or later is required")
	}
	return writableSpace(root)
}
func writableSpace(root string) error {
	parent := root
	for {
		if _, err := os.Stat(parent); err == nil {
			break
		}
		next := filepath.Dir(parent)
		if next == parent {
			return fmt.Errorf("app-data directory unavailable")
		}
		parent = next
	}
	path, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return err
	}
	var free uint64
	if err = windows.GetDiskFreeSpaceEx(path, &free, nil, nil); err != nil {
		return err
	}
	if free < 128<<20 {
		return fmt.Errorf("at least 128 MB free space is required")
	}
	f, err := os.CreateTemp(parent, ".readyfleet-preflight-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// Stop only an executable at this exact per-user installation path, owned by
// the same SID. Never kill by image name: another Windows account can be using
// its own light agent at the same time.
func stopAgent(path string) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	current, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		h, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, entry.ProcessID)
		if e != nil {
			continue
		}
		var name [32768]uint16
		n := uint32(len(name))
		e = windows.QueryFullProcessImageName(h, 0, &name[0], &n)
		if e == nil && strings.EqualFold(filepath.Clean(windows.UTF16ToString(name[:n])), filepath.Clean(path)) {
			var token windows.Token
			e = windows.OpenProcessToken(h, windows.TOKEN_QUERY, &token)
			if e == nil {
				u, ue := token.GetTokenUser()
				token.Close()
				if ue == nil && u.User.Sid.Equals(current.User.Sid) {
					e = windows.TerminateProcess(h, 0)
					if e == nil {
						_, e = windows.WaitForSingleObject(h, 5000)
					}
				} else {
					e = fmt.Errorf("existing agent belongs to another account")
				}
			}
			windows.CloseHandle(h)
			if e != nil {
				return e
			}
		} else {
			windows.CloseHandle(h)
		}
	}
	if err != windows.ERROR_NO_MORE_FILES {
		return err
	}
	return nil
}

func snapshotStartup() (func() error, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err == registry.ErrNotExist {
		return removeLogonStartup, nil
	}
	if err != nil {
		return nil, err
	}
	defer key.Close()
	previous, _, err := key.GetStringValue(runValueName)
	if err == registry.ErrNotExist {
		return removeLogonStartup, nil
	}
	if err != nil {
		return nil, err
	}
	return func() error {
		k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer k.Close()
		return k.SetStringValue(runValueName, previous)
	}, nil
}

func acquireInstallLock(root string) (func(), error) {
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`Local\ReadyFleetInstaller-%x`, digest))
	h, err := windows.CreateMutex(nil, true, name)
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("another setup is running")
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = windows.ReleaseMutex(h); _ = windows.CloseHandle(h) }, nil
}

func reportOS() string {
	v := windows.RtlGetVersion()
	return fmt.Sprintf("windows %d.%d build %d %s", v.MajorVersion, v.MinorVersion, v.BuildNumber, runtime.GOARCH)
}
