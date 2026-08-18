//go:build windows

package lightinventory

import (
	"fmt"
	"regexp"

	"golang.org/x/sys/windows/registry"
)

const osFamily = "windows"

// majorVersionPattern extracts the major version token from a
// ProductName like "Windows 11 Pro" or "Windows Server 2022 Standard".
// CurrentMajorVersionNumber is NOT used here — it reports 10 for both
// Windows 10 and 11, a known Windows registry quirk that makes it useless
// for telling them apart.
var majorVersionPattern = regexp.MustCompile(`Windows (?:Server )?(\d+)`)

// osMajorVersion reads ProductName from
// HKLM\Software\Microsoft\Windows NT\CurrentVersion (the same key the
// managed agent's inventory collector reads, but this is the only value
// lightinventory ever touches there) and extracts the major version token.
func osMajorVersion() (string, error) {
	const path = `SOFTWARE\Microsoft\Windows NT\CurrentVersion`
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", fmt.Errorf("lightinventory: open %s: %w", path, err)
	}
	defer key.Close()

	productName, _, err := key.GetStringValue("ProductName")
	if err != nil {
		return "", fmt.Errorf("lightinventory: read ProductName: %w", err)
	}

	m := majorVersionPattern.FindStringSubmatch(productName)
	if m == nil {
		return "", fmt.Errorf("lightinventory: could not parse major version from ProductName %q", productName)
	}
	return m[1], nil
}
