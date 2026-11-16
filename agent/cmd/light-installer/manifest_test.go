package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Sibling of agent/cmd/lightagent's TestLightagentBinary_CarriesComCtl32V6Manifest
// — same property, same reason, different binary. The installer wizard's
// own TaskDialogIndirect calls (agent/internal/installerui) need exactly
// the same ComCtl32 v6 manifest dependency, and go build silently produces
// a binary without one if resource_windows_amd64.syso doesn't actually
// carry it — which is precisely what shipped on 19 Aug 2026: the checked-in
// .syso built and linked cleanly, embedded an icon, but never embedded the
// manifest at all. TaskDialogIndirect then failed to resolve, intermittently
// (which OS thread Go's scheduler handed the dialog call decided whether it
// happened to already have the process's activation context), producing
// anything from a silent early exit to a hard panic depending on the run.
// See scripts/build-light-installer-template.sh, which now regenerates and
// verifies this exact property before any release is published — this test
// is the same check enforced at `go test` time instead of release time.
func TestLightInstallerBinary_CarriesComCtl32V6Manifest(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "light-installer-manifest-probe.exe")

	build := exec.Command("go", "build", "-buildvcs=false", "-o", out, ".")
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
		t.Fatal("built light-installer has NO embedded manifest — regenerate " +
			"resource_windows_amd64.syso with `go-winres make` from " +
			"winres/winres.json (use-common-controls-v6 must be true). " +
			"Without it, TaskDialogIndirect intermittently fails to resolve " +
			"and the wizard shows nothing, or crashes.")
	}
	if !strings.Contains(text, "Microsoft.Windows.Common-Controls") {
		t.Error("manifest present but does not declare Microsoft.Windows.Common-Controls; " +
			"TaskDialogIndirect will not be available")
	}
	if !strings.Contains(text, "6.0.0.0") {
		t.Error("Common-Controls dependency is not version 6.0.0.0")
	}
	// The installer must never ask for elevation — the whole BYOD design
	// promise is no admin rights required (current-user install, HKCU Run
	// key startup).
	if !strings.Contains(text, `<requestedExecutionLevel level="asInvoker"`) {
		t.Error("manifest does not request asInvoker")
	}
	if strings.Contains(text, `<requestedExecutionLevel level="requireAdministrator"`) {
		t.Error("manifest requests administrator; the light installer must run asInvoker")
	}
}
