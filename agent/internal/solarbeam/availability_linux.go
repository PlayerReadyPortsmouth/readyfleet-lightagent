//go:build linux

package solarbeam

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Retained pidfds prevent PID reuse between inspecting and stopping an exact
// executable owned by this BYOD user. No cross-user/global basename kills.
func (m *Manager) disableConfiguredHost() error {
	if !filepath.IsAbs(m.paths.Engine) || !filepath.IsAbs(m.paths.Bridge) {
		return errors.New("hosting off: exact engine and bridge install paths are required")
	}
	if m.paths.Mode != ModeInteractive {
		return errors.New("managed SolarBeam is not supported on Linux")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		var stat unix.Stat_t
		if unix.Stat(dir, &stat) != nil || stat.Uid != uint32(os.Getuid()) {
			continue
		}
		path, err := os.Readlink(filepath.Join(dir, "exe"))
		if err != nil {
			continue
		}
		path = strings.TrimSuffix(path, " (deleted)")
		if !filepath.IsAbs(path) || (path != m.paths.Engine && path != m.paths.Bridge) {
			continue
		}
		fd, err := unix.PidfdOpen(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("hosting off: retain process %d identity: %w", pid, err))
			continue
		}
		// Recheck after opening: a reused PID must not substitute a different exe.
		again, readErr := os.Readlink(filepath.Join(dir, "exe"))
		if readErr == nil && strings.TrimSuffix(again, " (deleted)") == path && unix.Stat(dir, &stat) == nil && stat.Uid == uint32(os.Getuid()) {
			err = unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0)
			if err == nil {
				poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
				_, err = unix.Poll(poll, 5000)
				if err == nil && poll[0].Revents&unix.POLLIN == 0 {
					err = errors.New("process has not stopped")
				}
			}
			if err != nil && !errors.Is(err, unix.ESRCH) {
				failures = append(failures, fmt.Errorf("hosting off: process %d: %w", pid, err))
			}
		}
		unix.Close(fd)
	}
	return errors.Join(failures...)
}
