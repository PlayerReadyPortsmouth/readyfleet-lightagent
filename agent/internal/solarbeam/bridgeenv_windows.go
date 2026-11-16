//go:build windows

package solarbeam

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// bridge.env is a trust root: sunshinesvc injects its variables into the
// engine, which runs with a SYSTEM token. %ProgramData% lets Users create
// files and folders in any subfolder that inherits from it, so whoever made
// %ProgramData%\SolarBeam (or anything in it) first could control the
// engine. Every write therefore re-owns the folder and everything in it to
// SYSTEM with a protected SYSTEM/Administrators-only DACL, which also
// repairs folders older agents created by inheritance.
const bridgeEnvSDDL = "O:SYG:SYD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"

// SeRestore lets SYSTEM (and an elevated test) set SYSTEM as owner and take
// back an object whose DACL shuts it out; SeBackup lets it list one. Both
// are held by LocalSystem but disabled by default.
var enableBridgeEnvPrivileges = sync.OnceValue(func() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()
	for _, name := range []string{"SeBackupPrivilege", "SeRestorePrivilege"} {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(name), &luid); err != nil {
			return fmt.Errorf("lookup %s: %w", name, err)
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1,
			Privileges: [1]windows.LUIDAndAttributes{{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}}}
		if err := windows.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
			return fmt.Errorf("enable %s: %w", name, err)
		}
	}
	return nil
})

// secureBridgeEnvDir creates bridgeEnvDir locked down, or repairs it and
// every entry under it. A link, junction or hard-linked file anywhere in it
// is refused, never followed or re-owned.
func secureBridgeEnvDir() (*windows.SECURITY_DESCRIPTOR, error) {
	if !filepath.IsAbs(bridgeEnvDir) {
		return nil, errors.New("ProgramData could not be resolved")
	}
	if err := enableBridgeEnvPrivileges(); err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString(bridgeEnvSDDL)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	if err := windows.CreateDirectory(windows.StringToUTF16Ptr(bridgeEnvDir), sa); err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	// Top-down: each folder is locked before it is listed.
	return sd, filepath.WalkDir(bridgeEnvDir, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return lockDownBridgeEnvPath(path, sd)
	})
}

// lockDownBridgeEnvPath applies sd to path's own object through a handle
// that does not follow reparse points. SetSecurityInfo reads the existing
// descriptor to apply the new DACL, so the handle needs READ_CONTROL too:
// without it every call fails "Access is denied", even as SYSTEM.
func lockDownBridgeEnvPath(path string, sd *windows.SECURITY_DESCRIPTOR) error {
	h, err := windows.CreateFile(windows.StringToUTF16Ptr(path),
		windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("%s is a link or junction; refusing it", path)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 && info.NumberOfLinks != 1 {
		return fmt.Errorf("%s has %d hard links; refusing it", path, info.NumberOfLinks)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		owner, nil, dacl, nil); err != nil {
		return fmt.Errorf("lock down %s: %w", path, err)
	}
	return nil
}

// writeBridgeEnvFile writes dst (bridge.env or its .applied marker) through a
// locked-down temp file renamed into place, so the old directory entry
// (whatever it was) is replaced rather than written through.
func writeBridgeEnvFile(dst string, contents []byte) error {
	sd, err := secureBridgeEnvDir()
	if err != nil {
		return fmt.Errorf("secure %s: %w", bridgeEnvDir, err)
	}
	f, err := os.CreateTemp(bridgeEnvDir, "bridge.env.*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(contents)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = lockDownBridgeEnvPath(tmp, sd)
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", dst, err)
	}
	return nil
}
