//go:build linux

package lightinventory

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

const osFamily = "linux"

// osMajorVersion reports the distribution and its major release without
// collecting a hostname or any other identifying machine data. CachyOS and
// Arch are rolling releases, so VERSION_ID is commonly absent; in that case
// the stable distro ID is the most useful honest value.
func osMajorVersion() (string, error) {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return "", fmt.Errorf("read /etc/os-release: %w", err)
	}
	defer f.Close()
	values := make(map[string]string)
	s := bufio.NewScanner(f)
	for s.Scan() {
		key, value, ok := strings.Cut(s.Text(), "=")
		if ok {
			values[key] = strings.Trim(strings.TrimSpace(value), `"`)
		}
	}
	if err := s.Err(); err != nil {
		return "", fmt.Errorf("scan /etc/os-release: %w", err)
	}
	id := values["ID"]
	if id == "" {
		id = "linux"
	}
	if version := values["VERSION_ID"]; version != "" {
		if major, _, ok := strings.Cut(version, "."); ok {
			version = major
		}
		return id + ":" + version, nil
	}
	return id + ":rolling", nil
}
