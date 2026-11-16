//go:build linux || darwin

package lightinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

func platformPreflight(root string) error {
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
	var stat unix.Statfs_t
	if err := unix.Statfs(parent, &stat); err != nil {
		return err
	}
	if uint64(stat.Bavail)*uint64(stat.Bsize) < 128<<20 {
		return fmt.Errorf("at least 128 MB free space is required")
	}
	f, err := os.CreateTemp(parent, ".readyfleet-preflight-*")
	if err != nil {
		return err
	}
	f.Close()
	defer os.Remove(f.Name())
	if runtime.GOOS == "linux" {
		if err = exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
			return fmt.Errorf("sign in to a desktop session before setup: %w", err)
		}
	}
	return nil
}
func stopAgent(string) error {
	if runtime.GOOS == "linux" {
		if err := exec.Command("systemctl", "--user", "stop", "readyfleet-light.service").Run(); err != nil {
			// A missing unit is normal on first install. A loaded unit that refuses
			// to stop must not be overwritten.
			out, _ := exec.Command("systemctl", "--user", "show", "readyfleet-light.service", "--property=LoadState", "--value").Output()
			if string(out) != "not-found\n" {
				return err
			}
		}
		return nil
	}
	return nil
}

func acquireInstallLock(root string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return nil, err
	}
	// Keep this empty sibling lock file: unlinking an advisory lock lets a
	// second setup lock a different inode while another still holds the first.
	f, err := os.OpenFile(root+".install.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}
func snapshotStartup() (func() error, error) {
	if runtime.GOOS != "linux" {
		return nil, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	file, err := snapshot(filepath.Join(dir, "systemd", "user", "readyfleet-light.service"))
	if err != nil {
		return nil, err
	}
	enabled := exec.Command("systemctl", "--user", "is-enabled", "--quiet", "readyfleet-light.service").Run() == nil
	return func() error {
		if !file.present {
			return removeLogonStartup()
		}
		if err := restoreFile(file); err != nil {
			return err
		}
		if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
			return err
		}
		verb := "disable"
		if enabled {
			verb = "enable"
		}
		return exec.Command("systemctl", "--user", verb, "readyfleet-light.service").Run()
	}, nil
}

func reportOS() string {
	name := runtime.GOOS + " " + runtime.GOARCH
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "PRETTY_NAME=") {
				name += " " + strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
				break
			}
		}
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name
}
