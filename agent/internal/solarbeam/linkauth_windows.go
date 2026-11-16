//go:build windows

package solarbeam

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	windowsLinkSecretMu  sync.Mutex
	errWindowsLinkSecret = errors.New("protected link credential operation refused")
)

// The DACL is supplied at creation, before any secret bytes exist on disk.
// Administrators ownership also permits elevated native-VM tests; both readers
// accept only SYSTEM or Administrators as owner and as allowed trustees.
const windowsLinkSecretSDDL = "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"

func windowsLinkSecretPath() string {
	return filepath.Join(bridgeEnvDir, "link-auth", "secret")
}

func writeWindowsLinkSecret(secret []byte) (string, func() error, error) {
	windowsLinkSecretMu.Lock()
	defer windowsLinkSecretMu.Unlock()
	if len(secret) != 64 || strings.IndexFunc(string(secret), func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f')
	}) >= 0 {
		return "", nil, errWindowsLinkSecret
	}
	path := windowsLinkSecretPath()
	dirs, err := pinWindowsLinkDirs(path, true)
	if err != nil {
		return "", nil, errWindowsLinkSecret
	}
	defer closeWindowsLinkDirs(dirs)
	// Refuse an existing unsafe final object; do not repair or overwrite it.
	old, err := openWindowsLinkFile(path, windows.GENERIC_READ)
	if err == nil {
		_, err = validateWindowsLinkFile(old)
		windows.CloseHandle(old)
	}
	if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return "", nil, errWindowsLinkSecret
	}
	sd, err := windows.SecurityDescriptorFromString(windowsLinkSecretSDDL)
	if err != nil {
		return "", nil, errWindowsLinkSecret
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	temp := filepath.Join(filepath.Dir(path), ".secret-"+rand.Text())
	p, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return "", nil, errWindowsLinkSecret
	}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		0, &sa, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", nil, errWindowsLinkSecret
	}
	f := os.NewFile(uintptr(h), "protected link credential")
	defer os.Remove(temp)
	info, validationErr := validateWindowsLinkFile(h)
	if validationErr != nil {
		f.Close()
		return "", nil, errWindowsLinkSecret
	}
	if n, err := f.Write(secret); err != nil || n != len(secret) {
		f.Close()
		return "", nil, errWindowsLinkSecret
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", nil, errWindowsLinkSecret
	}
	if err := f.Close(); err != nil {
		return "", nil, errWindowsLinkSecret
	}
	target, err := windows.UTF16PtrFromString(path)
	if err != nil || windows.MoveFileEx(p, target, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH) != nil {
		return "", nil, errWindowsLinkSecret
	}
	return path, func() error { return removeWindowsLinkSecretAt(path, &info) }, nil
}

// Used only after an adopted session has been stopped: a restarted agent has
// no in-memory generation handle. Ordinary cleanup always checks the file ID.
func removeWindowsLinkSecret() error {
	return removeWindowsLinkSecretAt(windowsLinkSecretPath(), nil)
}

func removeWindowsLinkSecretAt(path string, expected *windows.ByHandleFileInformation) error {
	windowsLinkSecretMu.Lock()
	defer windowsLinkSecretMu.Unlock()
	dirs, err := pinWindowsLinkDirs(path, false)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return errWindowsLinkSecret
	}
	defer closeWindowsLinkDirs(dirs)
	h, err := openWindowsLinkFile(path, windows.DELETE)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return errWindowsLinkSecret
	}
	defer windows.CloseHandle(h)
	info, err := validateWindowsLinkFile(h)
	if err != nil {
		return errWindowsLinkSecret
	}
	if expected != nil && (info.VolumeSerialNumber != expected.VolumeSerialNumber ||
		info.FileIndexHigh != expected.FileIndexHigh || info.FileIndexLow != expected.FileIndexLow) {
		return nil
	}
	// Delete the validated object by handle, never a freshly resolved pathname.
	remove := byte(1)
	if windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &remove, 1) != nil {
		return errWindowsLinkSecret
	}
	return nil
}

func pinWindowsLinkDirs(path string, create bool) ([]windows.Handle, error) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	if !filepath.IsAbs(path) || len(volume) != 2 || volume[1] != ':' || strings.Contains(path[2:], ":") {
		return nil, errWindowsLinkSecret
	}
	parts := strings.Split(strings.TrimPrefix(path[len(volume):], `\`), `\`)
	if len(parts) < 2 {
		return nil, errWindowsLinkSecret
	}
	sd, err := windows.SecurityDescriptorFromString(windowsLinkSecretSDDL)
	if err != nil {
		return nil, errWindowsLinkSecret
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	var dirs []windows.Handle
	ok := false
	defer func() {
		if !ok {
			closeWindowsLinkDirs(dirs)
		}
	}()
	current := volume + `\`
	for i := -1; i < len(parts)-1; i++ {
		if i >= 0 {
			current = filepath.Join(current, parts[i])
		}
		p, err := windows.UTF16PtrFromString(current)
		if err != nil {
			return nil, errWindowsLinkSecret
		}
		h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if create && (errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND)) && i >= len(parts)-3 {
			if e := windows.CreateDirectory(p, &sa); e != nil && !errors.Is(e, windows.ERROR_ALREADY_EXISTS) {
				return nil, errWindowsLinkSecret
			}
			h, err = windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
				windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		}
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, h)
		var info windows.ByHandleFileInformation
		if windows.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
			info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || i == len(parts)-2 && !windowsLinkSystemAdminOnly(h) {
			return nil, errWindowsLinkSecret
		}
	}
	ok = true
	return dirs, nil
}

func closeWindowsLinkDirs(dirs []windows.Handle) {
	for i := len(dirs) - 1; i >= 0; i-- {
		windows.CloseHandle(dirs[i])
	}
}

func openWindowsLinkFile(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, errWindowsLinkSecret
	}
	return windows.CreateFile(p, access|windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func validateWindowsLinkFile(h windows.Handle) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil ||
		info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 ||
		info.NumberOfLinks != 1 || info.FileSizeHigh != 0 || info.FileSizeLow > 66 || !windowsLinkSystemAdminOnly(h) {
		return info, errWindowsLinkSecret
	}
	return info, nil
}

func windowsLinkSystemAdminOnly(h windows.Handle) bool {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	return err == nil && windowsLinkProtectedDescriptor(sd)
}

func windowsLinkProtectedDescriptor(sd *windows.SECURITY_DESCRIPTOR) bool {
	if sd == nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || !windowsLinkTrustedSID(owner) {
		return false
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
			!windowsLinkTrustedSID((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
			return false
		}
	}
	return true
}

func windowsLinkTrustedSID(sid *windows.SID) bool {
	return sid != nil && (sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}
