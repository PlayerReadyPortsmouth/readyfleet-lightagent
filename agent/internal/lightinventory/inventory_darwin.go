//go:build darwin

package lightinventory

import (
	"fmt"
	"os/exec"
)

const osFamily = "darwin"

// osMajorVersion runs `sw_vers -productVersion` and reports "macos:<major>".
// No hostname or serial number is read: the light inventory carries six
// declared fields and nothing identifying.
func osMajorVersion() (string, error) {
	out, err := exec.Command("/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return "", fmt.Errorf("lightinventory: sw_vers: %w", err)
	}
	return parseSwVers(string(out))
}
