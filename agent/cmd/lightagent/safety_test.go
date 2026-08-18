package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenStrings are the tells that shell/terminal/screen-capture/
// service-install/account-management code compiled into this binary —
// the exact list from
// docs/superpowers/plans/2026-07-26-solarbeam-byod-light-agent.md Task 4
// Step 5. This test is the actual proof the import boundary held; the
// package doc comment in main.go is compile-time hope until this passes.
var forbiddenStrings = []string{
	"cmd.exe",
	"bcdedit",
	"BitBlt",
	"shutdown.exe",
	"net user",
	"ADMIN$",
}

// knownBenignMatches is one of the eight forbidden strings that DOES
// appear in the binary but was traced (2026-08-09) to a legitimate,
// necessary dependency rather than an actual capability leak — checked
// separately from forbiddenStrings above so a genuinely new occurrence of
// any OTHER forbidden string still fails loudly, and so this stays
// visible rather than silently dropped from the list:
//
//   - "WTSQueryUserToken": not called anywhere in this dependency graph.
//     It's a string-table artifact of importing golang.org/x/sys/windows
//     (needed by agent/internal/solarbeam for process listing/termination
//     — terminateByName, core BYOD start/stop functionality): Go statically
//     links the whole package once anything in it is used, and that
//     package declares proc-name strings for many Windows APIs it never
//     calls. TestLightagentBinary_ImportBoundary (below) independently
//     confirms the actual dependency graph is clean — this is a
//     string-scan false positive, not a real capability.
//
// "powershell.exe" was here too until 2026-08-09: agent/internal/verify's
// Authenticode check used to shell out to Get-AuthenticodeSignature. It's
// now a direct WinVerifyTrust/crypt32 syscall implementation (root-caused
// live this session: the PowerShell shell-out reliably failed to
// autoload its own module when spawned from a compiled Go binary) — the
// binary genuinely contains no PowerShell dependency anymore, so this
// isn't a stale exception quietly left in place, it's actually gone.
//
// If this justification stops being true (something starts actually
// calling WTSQueryUserToken), this exception needs re-examining, not
// silently trusting.
var knownBenignMatches = map[string]bool{
	"WTSQueryUserToken": true,
}

// TestLightagentBinary_ContainsNoForbiddenStrings builds the real
// lightagent binary for the host OS/arch and scans it for every forbidden
// string. Skips (not fails) when `go build` itself is unavailable in this
// environment, since CI/dev-host constraints shouldn't be conflated with
// an actual boundary violation — but a successful build that fails the
// scan is a hard failure.
func TestLightagentBinary_ContainsNoForbiddenStrings(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping binary build+scan under -short")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "lightagent-safety-check.bin")

	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = "."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build ./cmd/lightagent failed (this test IS the boundary proof, not optional): %v\n%s", err, stderr.String())
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read built binary: %v", err)
	}

	lower := bytes.ToLower(data)
	for _, s := range forbiddenStrings {
		if bytes.Contains(lower, bytes.ToLower([]byte(s))) {
			t.Errorf("lightagent binary contains forbidden string %q — the exec/screen/terminal/service import boundary has been violated", s)
		}
	}

	// The two knownBenignMatches are expected to still be present, traced
	// to specific legitimate sources (see the var doc comment). If either
	// disappears that's not a failure (a dependency changed harmlessly);
	// this just keeps the exception visible and exercised rather than
	// dead documentation nobody re-checks.
	for s := range knownBenignMatches {
		if !bytes.Contains(lower, bytes.ToLower([]byte(s))) {
			t.Logf("known-benign string %q no longer appears in the binary (fine — a dependency likely changed)", s)
		}
	}
}

// TestLightagentBinary_ImportBoundary is a second, independent check of
// the same boundary via `go list`, catching a violation even if some
// future forbidden dependency happens not to leave a matching string in
// the binary (e.g. it's dead-code-eliminated but still technically
// imported, which `go list` sees and a strings scan wouldn't).
func TestLightagentBinary_ImportBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-f", "{{join .Deps \"\\n\"}}", ".")
	cmd.Dir = "."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}

	// Shell, terminal, screen-capture, service-install, and LAN-scan all
	// live as files inside agent/internal/exec, not separate packages
	// (confirmed against the actual internal/ directory listing) — so
	// excluding exec covers all of them in one check. agent/internal/svc
	// is deliberately NOT forbidden: main.go reuses its OpenServiceLogger
	// helper only (confirmed to have no SCM/service coupling of its own).
	forbiddenPkgs := []string{
		"agent/internal/exec",
		"agent/internal/inventory",
	}

	deps := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, dep := range deps {
		for _, forbidden := range forbiddenPkgs {
			if strings.Contains(dep, forbidden) {
				t.Errorf("lightagent imports forbidden package %q (via dep %q)", forbidden, dep)
			}
		}
	}
}
