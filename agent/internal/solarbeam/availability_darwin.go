//go:build darwin

package solarbeam

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// disableConfiguredHost is hosting-off on a Mac: stop the children this
// manager owns, then end any survivor from an earlier agent that is running
// EXACTLY the configured engine or bridge path as this user. No /proc and no
// pidfd here, so survivors are found with ps and matched on the exact path
// and uid (parsePSList, survivorsOf); a basename match or another user's
// process is never signalled. Between listing and signalling a PID could in
// principle be reused; the list is re-read immediately before each signal to
// narrow that window, and this runs only on an explicit hosting-off.
func (m *Manager) disableConfiguredHost() error {
	if m.paths.Mode != ModeInteractive {
		return errors.New("managed SolarBeam is not supported on macOS")
	}
	if !filepath.IsAbs(m.paths.Engine) || !filepath.IsAbs(m.paths.Bridge) {
		return errors.New("hosting off: exact engine and bridge install paths are required")
	}
	m.darwin.supervisorGen++
	stopChild(&m.interactiveEngineProc, &m.interactiveEngineDone)
	stopChild(&m.interactiveBridgeProc, &m.interactiveBridgeDone)
	m.darwin.linkEnv = nil
	return endDarwinSurvivors(os.Getuid(), []string{m.paths.Engine, m.paths.Bridge}, listDarwinProcesses)
}

// listDarwinProcesses runs ps; the seam exists so tests never do.
func listDarwinProcesses() ([]psEntry, error) {
	out, err := exec.Command("/bin/ps", "-axww", "-o", "pid=,uid=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("hosting off: list processes: %w", err)
	}
	return parsePSList(string(out)), nil
}

func endDarwinSurvivors(uid int, paths []string, list func() ([]psEntry, error)) error {
	entries, err := list()
	if err != nil {
		return err
	}
	victims := survivorsOf(entries, uid, paths)
	if len(victims) == 0 {
		return nil
	}
	var failures []error
	for _, v := range victims {
		if err := syscall.Kill(v.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			failures = append(failures, fmt.Errorf("hosting off: process %d: %w", v.PID, err))
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err = list()
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		remaining := survivorsOf(entries, uid, paths)
		if len(remaining) == 0 {
			return errors.Join(failures...)
		}
		if time.Now().After(deadline) {
			for _, v := range remaining {
				// Re-checked by the list just taken: same uid, same exact path.
				if err := syscall.Kill(v.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					failures = append(failures, fmt.Errorf("hosting off: process %d: %w", v.PID, err))
				}
			}
			time.Sleep(200 * time.Millisecond)
			entries, err = list()
			if err == nil && len(survivorsOf(entries, uid, paths)) > 0 {
				failures = append(failures, errors.New("hosting off: a SolarBeam process has not stopped: "+strconv.Itoa(len(survivorsOf(entries, uid, paths)))))
			}
			return errors.Join(failures...)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
