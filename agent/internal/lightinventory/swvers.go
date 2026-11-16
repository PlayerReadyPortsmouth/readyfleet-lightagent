package lightinventory

import (
	"fmt"
	"strings"
)

// parseSwVers turns `sw_vers -productVersion` output ("15.1.1\n") into
// "macos:15". Untagged so it is tested on every host.
func parseSwVers(out string) (string, error) {
	version := strings.TrimSpace(out)
	major, _, _ := strings.Cut(version, ".")
	if major == "" || strings.Trim(major, "0123456789") != "" {
		return "", fmt.Errorf("lightinventory: unrecognised sw_vers output %q", version)
	}
	return "macos:" + major, nil
}
