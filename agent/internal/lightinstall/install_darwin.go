//go:build darwin

package lightinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// launchdDomain is the per-user GUI domain the agent job is bootstrapped into.
func launchdDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }

func launchctl(args ...string) error {
	out, err := exec.Command("launchctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// registerLogonStartup writes the LaunchAgent plist. launchd loads it at the
// next login; startAgentOnce loads it now.
func registerLogonStartup(agentPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	plist := LaunchAgentPlistPath(home)
	logPath := LaunchdLogPath(home)
	for _, dir := range []string{filepath.Dir(plist), filepath.Dir(logPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(plist, []byte(LaunchAgentPlist(agentPath, logPath)), 0o644)
}

func removeLogonStartup() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Not loaded is not an error: unlink runs this on machines in any state.
	_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+LaunchdLabel).Run()
	if err := os.Remove(LaunchAgentPlistPath(home)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// startAgentOnce (re)loads the job. bootstrap fails if the label is already
// loaded, so bootout first (its failure just means it was not loaded).
func startAgentOnce(string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", launchdDomain()+"/"+LaunchdLabel).Run()
	if err := launchctl("bootstrap", launchdDomain(), LaunchAgentPlistPath(home)); err != nil {
		return err
	}
	return launchctl("kickstart", "-k", launchdDomain()+"/"+LaunchdLabel)
}
