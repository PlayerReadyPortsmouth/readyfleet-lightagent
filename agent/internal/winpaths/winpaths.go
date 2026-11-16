// Package winpaths resolves the machine folders the agent uses on Windows
// from the OS (known folders), never from a hardcoded drive letter: Windows
// is not always on C: (TAUNTON-PC05 runs from D:).
package winpaths

import (
	"path"
	"path/filepath"
	"strings"
)

// solarBeamDisplayName is the engine MSI's product name, as shown in its
// Add/Remove Programs (uninstall) entry.
const solarBeamDisplayName = "SolarBeam Engine"

// SolarBeamDir is the SolarBeam engine's install directory: the engine
// MSI's registered InstallLocation when it lies under this machine's
// Program Files, else Program Files\SolarBeam (the MSI's own default).
// trusted is the on-disk check (links, user-writable folders) a location
// that passes the text check must also pass. refused is a registry
// location that was ignored as untrusted, with the reason, for the caller
// to log; empty otherwise.
func SolarBeamDir(trusted func(dir string) error) (dir, refused string) {
	return solarBeamDir(ProgramFiles(), solarBeamInstallLocation(), trusted)
}

func solarBeamDir(programFiles, installLocation string, trusted func(dir string) error) (dir, refused string) {
	fallback := filepath.Join(programFiles, "SolarBeam")
	if installLocation == "" {
		return fallback, ""
	}
	// The agent runs what it finds here as SYSTEM: only an admin-only
	// location is acceptable, whatever the registry says.
	if !UnderRoot(installLocation, programFiles) {
		return fallback, installLocation
	}
	if err := trusted(clean(installLocation)); err != nil {
		return fallback, installLocation + ": " + err.Error()
	}
	return clean(installLocation), ""
}

// UnderRoot reports whether p is strictly inside root. Both must be
// absolute drive paths; the comparison is case-insensitive and made after
// resolving "." and "..", so `D:\Program Files\..\Users` is not inside
// `D:\Program Files`. UNC, device and relative paths are never inside.
func UnderRoot(p, root string) bool {
	p, root = clean(p), clean(root)
	if !isDrivePath(p) || !isDrivePath(root) {
		return false
	}
	root = strings.TrimSuffix(root, `\`) + `\`
	return len(p) > len(root) && strings.EqualFold(p[:len(root)], root)
}

// clean normalises a Windows path independently of the host OS, so the
// checks behave the same in Linux tests and on the venue PC.
func clean(p string) string {
	return strings.ReplaceAll(path.Clean(strings.ReplaceAll(p, `\`, "/")), "/", `\`)
}

// isDrivePath accepts `X:` and `X:\...` only (not drive-relative `X:foo`).
func isDrivePath(p string) bool {
	if len(p) < 2 || p[1] != ':' {
		return false
	}
	c := p[0] | 0x20
	return c >= 'a' && c <= 'z' && (len(p) == 2 || p[2] == '\\')
}
