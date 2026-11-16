//go:build linux

package lightinstall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// None of these are style preferences.
//
// NoNewPrivileges makes the kernel drop the engine's file capabilities on
// exec. The other four each create a mount namespace the XDG portal refuses
// to resolve the caller through, and each additionally makes some directory
// the agent needs read-only: ~/.local/state (the log dir — which is why this
// was invisible for a day), ~/.local/share (the engine updater's staging
// sibling), and /tmp (the agent's own update download, masked until
// PrivateTmp was removed for the portal's sake).
//
// Any one of them alone is enough to break Linux BYOD silently. If someone
// reintroduces one to "harden" a unit that runs as the desktop user and
// therefore gains nothing from it, this fails first.
func TestStartupUnitCarriesNoSandboxingDirectives(t *testing.T) {
	unit := StartupUnit("/home/mentor/.config/readyfleet-light/readyfleet-light")
	for _, forbidden := range []string{
		"NoNewPrivileges",
		"PrivateTmp",
		"ProtectHome",
		"ProtectSystem",
		"ReadWritePaths",
	} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("unit contains %s:\n%s", forbidden, unit)
		}
	}
}

func TestStartupUnitJoinsTheGraphicalSession(t *testing.T) {
	unit := StartupUnit("/home/mentor/.config/readyfleet-light/readyfleet-light")
	for _, want := range []string{
		"After=graphical-session.target",
		"WantedBy=graphical-session.target",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit is missing %q:\n%s", want, unit)
		}
	}
	// PartOf would stop the agent at logout and cost C2 contact; the display
	// environment is re-resolved per launch instead.
	if strings.Contains(unit, "PartOf=") {
		t.Errorf("unit binds its lifetime to the session:\n%s", unit)
	}
}

func TestStartupUnitEscapesTheExecStartPath(t *testing.T) {
	unit := StartupUnit("/home/test/Ready Fleet/agent")
	if !strings.Contains(unit, `ExecStart=/home/test/Ready\x20Fleet/agent`) {
		t.Errorf("ExecStart not escaped:\n%s", unit)
	}
}

func TestReconcileStartupUnitLeavesAnAbsentUnitAlone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	changed, err := ReconcileStartupUnit("/opt/agent")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("wrote a unit for an agent that was never installed by the installer")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", systemdUnitName)); !os.IsNotExist(err) {
		t.Fatalf("unit file was created: %v", err)
	}
}

func TestReconcileStartupUnitIsANoOpWhenAlreadyCurrent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	unitDir := filepath.Join(dir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(unitDir, systemdUnitName)
	if err := os.WriteFile(path, []byte(StartupUnit("/opt/agent")), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := ReconcileStartupUnit("/opt/agent")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("reported a change for an already-current unit")
	}
}

// The whole point of reconciliation: a machine installed before the fix has a
// unit with NoNewPrivileges in it, and a self-update never rewrites the unit.
func TestReconcileStartupUnitRepairsALegacyUnit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	unitDir := filepath.Join(dir, "systemd", "user")
	if err := os.MkdirAll(unitDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(unitDir, systemdUnitName)
	legacy := "[Unit]\nDescription=ReadyFleet BYOD SolarBeam Agent\n\n[Service]\nExecStart=/opt/agent\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectHome=read-only\n\n[Install]\nWantedBy=default.target\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	// systemctl is invoked after the write; it is allowed to fail here (no
	// user manager in CI) as long as the file itself has been repaired.
	changed, _ := ReconcileStartupUnit("/opt/agent")
	if !changed {
		t.Fatal("did not report rewriting a legacy unit")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != StartupUnit("/opt/agent") {
		t.Fatalf("unit not rewritten to the current template:\n%s", got)
	}
}
