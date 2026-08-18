package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The agent draws its connection-request prompt with TaskDialogIndirect,
// which only exists in ComCtl32 v6. Windows selects v5 unless the binary's
// manifest declares a dependency on v6 — and it does so silently: the dialog
// simply fails to create, the mentor is never asked, and the agent otherwise
// looks healthy. Nothing at compile time catches it.
//
// This is not hypothetical. The BYOD installer shipped on 18 Aug 2026 built
// from a .syso carrying the icon but not the manifest, and did exactly that:
// it staged its files and exited having shown the user nothing.
//
// So this asserts the property on the linked binary rather than on
// versioninfo.json, which only describes what should have been generated.
func TestLightagentBinary_CarriesComCtl32V6Manifest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "lightagent-manifest-probe.exe")

	build := exec.Command("go", "build", "-o", out, ".")
	build.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64")
	if combined, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, combined)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read built binary: %v", err)
	}
	text := string(body)

	if !strings.Contains(text, "<assembly") {
		t.Fatal("built agent has NO embedded manifest — regenerate resource_windows_amd64.syso " +
			"with goversioninfo (versioninfo.json records ManifestPath). Without it the " +
			"connection-request prompt silently never appears.")
	}
	if !strings.Contains(text, "Microsoft.Windows.Common-Controls") {
		t.Error("manifest present but does not declare Microsoft.Windows.Common-Controls; " +
			"TaskDialogIndirect will not be available")
	}
	if !strings.Contains(text, "6.0.0.0") {
		t.Error("Common-Controls dependency is not version 6.0.0.0")
	}
	// The light agent must never ask for elevation: it runs at logon where
	// nobody is present to answer a UAC prompt, and the installer promises no
	// admin rights are needed.
	//
	// Match the attribute, not the bare word. The manifest's own comment
	// explains why it is asInvoker "and never requireAdministrator", and that
	// comment is embedded in the binary — a substring check fails on the
	// explanation of the rule it is enforcing.
	if !strings.Contains(text, `<requestedExecutionLevel level="asInvoker"`) {
		t.Error("manifest does not request asInvoker")
	}
	if strings.Contains(text, `<requestedExecutionLevel level="requireAdministrator"`) {
		t.Error("manifest requests administrator; the light agent must run asInvoker")
	}
}
