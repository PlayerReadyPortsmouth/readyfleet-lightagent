//go:build windows

package winpaths

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// ProgramFiles is the machine's (64-bit) Program Files folder.
func ProgramFiles() string { return knownFolder(windows.FOLDERID_ProgramFiles, "ProgramFiles") }

// ProgramData is the machine's ProgramData folder.
func ProgramData() string { return knownFolder(windows.FOLDERID_ProgramData, "ProgramData") }

// Windows is the Windows directory (%SystemRoot%).
func Windows() string { return knownFolder(windows.FOLDERID_Windows, "SystemRoot") }

// UserProfiles is the folder holding user profiles (normally X:\Users).
func UserProfiles() string { return knownFolder(windows.FOLDERID_UserProfiles, "") }

// SystemDrive is the root of the drive Windows runs from, e.g. `D:\`.
func SystemDrive() string { return filepath.VolumeName(Windows()) + `\` }

func knownFolder(id *windows.KNOWNFOLDERID, env string) string {
	if p, err := windows.KnownFolderPath(id, windows.KF_FLAG_DEFAULT); err == nil && p != "" {
		return p
	}
	if env == "" {
		return ""
	}
	return os.Getenv(env)
}

const uninstallKey = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`

// solarBeamInstallLocation reads InstallLocation from the engine MSI's
// uninstall entry (64-bit view). The entry's key is the per-build
// ProductCode, so it is found by DisplayName. Empty when absent.
func solarBeamInstallLocation() string {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKey,
		registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer root.Close()
	names, _ := root.ReadSubKeyNames(-1)
	for _, name := range names {
		k, err := registry.OpenKey(root, name, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		display, _, _ := k.GetStringValue("DisplayName")
		loc, _, _ := k.GetStringValue("InstallLocation")
		k.Close()
		if display == solarBeamDisplayName && loc != "" {
			return loc
		}
	}
	return ""
}
