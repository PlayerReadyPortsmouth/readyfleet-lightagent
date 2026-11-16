//go:build linux

package lightinstall

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const systemdUnitName = "readyfleet-light.service"

func userSystemdDir() (string, error) {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// StartupUnit renders the systemd user unit the light agent runs under.
//
// Three properties of this unit are load-bearing, and all three were once got
// wrong in a way that made SolarBeam silently unusable on Linux — the machine
// joined the LiveKit room, published audio, and never published video:
//
//   - NO NoNewPrivileges. It makes the kernel ignore file capabilities on
//     exec, so getcap on the engine binary showed cap_sys_admin,cap_sys_nice
//     while getpcaps on the running process showed none — a unit that silently
//     disagrees with the filesystem about what the engine is.
//
//     Keep it out even though the engine is now expected to carry NO
//     capabilities (see the correction below). A directive whose whole effect
//     is to make a process's privileges differ from what every tool reports is
//     worth refusing on its own; and if a machine that genuinely needs kmsgrab
//     is ever granted them by hand, this unit must not quietly discard them.
//
//   - NO filesystem sandboxing at all: no PrivateTmp, no ProtectHome, no
//     ProtectSystem, no ReadWritePaths.
//
//     These directives buy this unit nothing. It is a USER unit, running as
//     the desktop user, who already cannot write /usr, /etc or /boot. All
//     ProtectSystem=strict actually adds is that $HOME and /tmp become
//     read-only — which is not a boundary against anything, and is a boundary
//     against us. It produced three outages in one day, each a path someone
//     did not think to list.
//
//     ~/.local/state: the log directory could not be created, so the engine
//     and bridge logs went to /dev/null and the original failure was
//     invisible for a day.
//
//     ~/.local/share: "mkdir ~/.local/share/.solarbeam-update-2290502795:
//     read-only file system". The engine updater stages into a SIBLING of the
//     install dir, which no ReadWritePaths entry naming the install dir could
//     ever have covered.
//
//     /tmp: "open /tmp/readyfleet-light-update-591973770.bin: read-only file
//     system". The agent could no longer download its own updates. That had
//     been masked all along by PrivateTmp handing it a writable private /tmp,
//     so removing PrivateTmp for the portal's sake is what uncovered it.
//
//     A ReadWritePaths list is a list of the places we remembered. The engine,
//     the bridge and the updater between them touch the XDG base dirs, /tmp,
//     and $XDG_RUNTIME_DIR, and any future one of them will touch somewhere
//     else. Enumerating that set correctly, forever, is not a security control
//     worth the outages — especially when the process could write all of it
//     anyway if the unit simply named the directory.
//
// CORRECTION to the reasoning first committed here: removing these directives
// was believed to also fix xdg-desktop-portal's "AccessDenied: Unable to open
// /proc/<pid>/root", on the theory that the mount namespace they create defeats
// the portal's resolution of its caller. Measured afterwards on the reference
// box, that is WRONG — the denial reproduces with no namespace at all.
//
// The actual cause is the engine's own file capabilities. Gaining privilege
// through file caps on exec clears the kernel's dumpable flag, which makes
// /proc/<pid>/ root-owned; xdg-desktop-portal runs as the user and so cannot
// open /proc/<pid>/root to identify the caller. Dropping the caps at runtime
// (which the engine does, logging "drop_elevated_privileges succeeded") does
// not restore dumpability, so it does not help. cap_sys_admin on sunshine and
// compositor screencast are mutually exclusive, by construction.
//
// The fix is therefore not in this unit and not in the engine: it is that the
// engine must not have the capabilities at all. packaging/linux/install-system-
// integration.sh no longer grants them and now strips them, and the engine on
// the reference CachyOS/KDE/Wayland/NVIDIA box then takes KWin ScreenCast and
// reaches h264_nvenc over DMA-BUF at 2560x1440. Note that /dev/uinput access —
// the other thing that integration step exists for — comes from its udev rule
// and the input group, never from the capabilities, so removing them costs
// nothing on the input side.
//
// Everything above is still correct and still required; just do not read any
// of it as a claim that this unit is what makes capture work.
//
// After=/WantedBy=graphical-session.target starts the agent as part of the
// desktop session, so it inherits WAYLAND_DISPLAY/DISPLAY/XAUTHORITY from the
// user manager's environment block. It is kept in default.target as well so
// the agent still reaches the C2 on a machine sitting at the login screen;
// there is deliberately no PartOf=, because losing C2 contact at logout would
// cost more than it buys. The agent no longer *relies* on inheritance either
// (solarbeam.resolveDisplayEnv re-resolves it per launch) — this is the belt
// to that braces.
func StartupUnit(agentPath string) string {
	return "[Unit]\n" +
		"Description=ReadyFleet BYOD SolarBeam Agent\n" +
		"After=graphical-session.target network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=" + systemdEscape(agentPath) + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=5\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n" +
		"WantedBy=graphical-session.target\n"
}

func registerLogonStartup(agentPath string) error {
	dir, err := userSystemdDir()
	if err != nil {
		return fmt.Errorf("resolve systemd user directory: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, systemdUnitName), []byte(StartupUnit(agentPath)), 0o600); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "--user", "enable", systemdUnitName).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ReconcileStartupUnit rewrites the systemd user unit if it has drifted from
// StartupUnit, and reports whether it changed anything.
//
// It exists because the unit is written by the INSTALLER and by nothing else:
// a self-update replaces the agent binary in place and never touches
// ~/.config/systemd/user/readyfleet-light.service. Without this, every machine
// installed before the capability/portal fix above would keep its broken unit
// forever unless someone re-ran the installer by hand on each one.
//
// Deliberately does not restart the agent. Restarting the process that is
// mid-startup to pick up its own new unit is a good way to build a loop, and
// the agent it would kill may be hosting a session. Rewriting plus a
// daemon-reload means the corrected unit takes effect at the next restart the
// machine was going to do anyway (logout, reboot, or the next self-update),
// and the log line says so.
//
// Errors are returned but callers should treat them as a warning: an agent
// that cannot heal its unit is still a working agent.
func ReconcileStartupUnit(agentPath string) (bool, error) {
	dir, err := userSystemdDir()
	if err != nil {
		return false, fmt.Errorf("resolve systemd user directory: %w", err)
	}
	unitPath := filepath.Join(dir, systemdUnitName)
	current, err := os.ReadFile(unitPath)
	if err != nil {
		// No unit at all means this agent was not started by the installer's
		// unit (a developer running it by hand, a container). Writing one
		// would be a surprise; leave it alone.
		return false, nil
	}
	want := StartupUnit(agentPath)
	if string(current) == want {
		return false, nil
	}
	if err := os.WriteFile(unitPath, []byte(want), 0o600); err != nil {
		return false, fmt.Errorf("rewrite %s: %w", systemdUnitName, err)
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return true, fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// enable is idempotent and is what adds the graphical-session.target want
	// that an older unit's [Install] section never had.
	if out, err := exec.Command("systemctl", "--user", "enable", systemdUnitName).CombinedOutput(); err != nil {
		return true, fmt.Errorf("systemctl enable: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return true, nil
}

func systemdEscape(path string) string {
	return strings.NewReplacer("\\", "\\\\", " ", "\\x20", "\t", "\\t").Replace(path)
}

func removeLogonStartup() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", systemdUnitName).Run()
	dir, err := userSystemdDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, systemdUnitName)); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	return nil
}

func startAgentOnce(string) error {
	out, err := exec.Command("systemctl", "--user", "restart", systemdUnitName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
