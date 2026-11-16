//go:build windows

package gamepaddriver

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/playerreadyportsmouth/readyfleet/agent/internal/verify"
)

// DefaultDeps wires the real Windows side effects.
func DefaultDeps(sessionActive func() bool) Deps {
	return Deps{
		Detect:               detectViGEmBus,
		Download:             download,
		Hold:                 holdFile,
		VerifySigner:         verify.VerifyAuthenticode,
		RunElevated:          runElevated,
		Msiexec:              msiexecPath,
		InstalledProductCode: installedProductCode,
		SessionActive:        sessionActive,
	}
}

func download(ctx context.Context, url, sha256Hex, dir string, maxBytes int64) (string, error) {
	return verify.DownloadVerified(ctx, verify.DownloadParams{
		URL:         url,
		SHA256:      sha256Hex,
		DownloadDir: dir,
		// .exe: ShellExecuteEx picks the handler by extension.
		TempPattern: "vigembus-setup-*.exe",
		MaxBytes:    maxBytes,
		Client:      verify.NewTransferClient(),
	})
}

// driverPath is %SystemRoot%\System32\drivers\ViGEmBus.sys, the file the
// engine's getViGEmBusStatus reads.
func driverPath() (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(sys, "drivers", "ViGEmBus.sys"), nil
}

// detectViGEmBus needs no rights: any user can stat a driver file and read
// its version resource.
func detectViGEmBus() (Status, error) {
	path, err := driverPath()
	if err != nil {
		return Status{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Status{}, nil
		}
		return Status{}, err
	}
	return Status{Installed: true, Version: fileVersion(path)}, nil
}

func fileVersion(path string) string {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return ""
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return ""
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var fixedLen uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&fixed), &fixedLen); err != nil || fixed == nil {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d.%d",
		fixed.FileVersionMS>>16, fixed.FileVersionMS&0xffff,
		fixed.FileVersionLS>>16, fixed.FileVersionLS&0xffff)
}

// holdFile opens path for reading with FILE_SHARE_READ only: while the
// handle is open no other process — including one running as this same user
// — can open it for writing, rename it or delete it. The loader's own open
// for execution asks only for read/execute, which FILE_SHARE_READ allows.
func holdFile(path string) (io.ReadCloser, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

func msiexecPath() (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(sys, "msiexec.exe"), nil
}

// installedProductCode finds the ViGEm Bus Driver entry under the machine
// Uninstall keys (read-only; HKLM is readable by users) and returns its key
// name when that is an MSI product code.
func installedProductCode() (string, error) {
	const uninstall = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`
	for _, access := range []uint32{registry.WOW64_64KEY, registry.WOW64_32KEY} {
		root, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall, registry.ENUMERATE_SUB_KEYS|registry.READ|access)
		if err != nil {
			continue
		}
		names, _ := root.ReadSubKeyNames(-1)
		root.Close()
		for _, name := range names {
			if !productCodeRE.MatchString(name) {
				continue
			}
			k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstall+`\`+name, registry.QUERY_VALUE|access)
			if err != nil {
				continue
			}
			display, _, _ := k.GetStringValue("DisplayName")
			k.Close()
			if strings.HasPrefix(strings.ToLower(display), "vigem bus driver") {
				return name, nil
			}
		}
	}
	return "", nil
}

// shellExecuteInfo is SHELLEXECUTEINFOW. Go's natural alignment of these
// fields matches the Windows SDK layout on both amd64 and arm64.
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           windows.Handle
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       windows.Handle
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      windows.Handle
	dwHotKey       uint32
	hIconOrMonitor windows.Handle
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	swShowNormal          = 1
	// elevatedRunLimit bounds the wait for the installer. The UAC prompt
	// itself has no timeout, so this also covers an owner who walked away.
	elevatedRunLimit = 15 * time.Minute
)

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// runElevated is the ONE place this package asks for administrator rights,
// and it can only ask: ShellExecuteEx with the "runas" verb makes Windows
// show the UAC prompt, and ERROR_CANCELLED comes back if the owner says No.
func runElevated(ctx context.Context, exe, args string) (uint32, error) {
	// ShellExecuteEx wants an STA COM apartment on the calling thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// S_FALSE (already initialised on this thread) still needs the matching
	// CoUninitialize; RPC_E_CHANGED_MODE (an MTA thread) must not get one.
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err == nil || err == windows.Errno(1) {
		defer windows.CoUninitialize()
	}

	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	params, err := windows.UTF16PtrFromString(args)
	if err != nil {
		return 0, err
	}
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		lpDirectory:  dir,
		nShow:        swShowNormal,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	r, _, callErr := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info)))
	if r == 0 {
		if callErr == windows.ERROR_CANCELLED {
			return 0, ErrElevationDeclined
		}
		return 0, callErr
	}
	if info.hProcess == 0 {
		return 0, fmt.Errorf("Windows started the installer but returned no process handle")
	}
	defer windows.CloseHandle(info.hProcess)

	deadline := time.Now().Add(elevatedRunLimit)
	for {
		ev, err := windows.WaitForSingleObject(info.hProcess, 500)
		if err != nil {
			return 0, err
		}
		if ev == windows.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			// Never kill an installer mid-transaction; say so instead.
			return 0, fmt.Errorf("stopped waiting for the installer (%w); it may still be running", ctx.Err())
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("the installer has not finished after %s; it may still be running", elevatedRunLimit)
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return 0, err
	}
	return code, nil
}
