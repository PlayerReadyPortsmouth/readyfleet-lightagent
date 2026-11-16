// Package gamepaddriver is the BYOD light agent's opt-in "Install gamepad
// support" flow: it puts the ViGEmBus virtual-gamepad driver on a mentor's
// own Windows PC, but only when the owner asks for it, and only through a
// Windows administrator (UAC) prompt they answer themselves.
//
// Why the light agent can start this at all. The light agent is asInvoker
// and always will be (agent/cmd/lightagent/agent.manifest); it cannot
// install a kernel driver and never tries. What it can do is what any
// program on the desktop can do: download a file, check it, and ask Windows
// to run it elevated with ShellExecuteEx's "runas" verb. Windows then shows
// the owner a UAC prompt naming the file's publisher, and nothing happens
// unless they approve it. Nothing here elevates silently, keeps elevated
// rights, or runs anything elevated except the pinned vendor installer (and,
// to remove it, msiexec with the pinned product code).
//
// The trust chain, in order, all before the prompt appears:
//
//  1. the owner chose "Install gamepad support…" and pressed Continue on a
//     plain-language consent screen (cmd/lightagent/gamepad_windows.go);
//  2. the file's SHA-256 equals Spec.SHA256 — the exact 1.22.0 release,
//     cross-checked against the winget manifest;
//  3. the file stays open with a deny-write/deny-delete share mode from the
//     re-hash through to the end of the installer, so a same-user process
//     cannot swap it between the check and the elevation;
//  4. its Authenticode signature is Valid (timestamped; the leaf expired in
//     Feb 2025) and the signer certificate's SHA-256 equals
//     Spec.SignerSHA256 — Nefarius Software Solutions e.U., which chains to
//     the public DigiCert Trusted Root G4, so the UAC prompt shows a named,
//     verified publisher rather than "Unknown publisher".
//
// Design context: ReadyFleet PR #93 (docs/gamepad-driver-rollout) proposed a
// separate requireAdministrator "SolarBeam system integration" binary for
// BYOD. This flow meets the same constraints (asInvoker agent, one UAC
// prompt at a moment the owner chose, nothing else registered) without a
// second signed binary to build and distribute; the UAC prompt names the
// driver's own publisher instead of ours.
package gamepaddriver

// Spec pins one driver package. Everything that decides whether bytes may be
// run elevated lives here as a constant, never as something the controller
// or a config file can change.
type Spec struct {
	// Name is the product name shown to the owner.
	Name string
	// Version is the package release (not the .sys file version).
	Version string
	// Publisher is the Authenticode signer the UAC prompt will name.
	Publisher string
	// URLs are tried in order. The first is our own release store; the
	// last is the vendor's original release asset. Integrity never depends
	// on which one answers: SHA256 and SignerSHA256 are checked either way.
	URLs []string
	// SHA256 is the lowercase-hex digest of the installer.
	SHA256 string
	// SizeBytes is the installer's exact size; MaxBytes caps the download.
	SizeBytes int64
	// SignerSHA256 is the SHA-256 of the Authenticode signer certificate DER
	// (verify.VerifySignerCertificateDER's format).
	SignerSHA256 string
	// InstallArgs are the silent switches for the vendor bootstrapper.
	InstallArgs string
	// ProductCode is the MSI product code, the fallback for uninstall when
	// the Uninstall registry key cannot be read.
	ProductCode string
	// MinMajor/MinMinor are the lowest driver file version the engine
	// accepts (its own version_compatible check: 1.17).
	MinMajor, MinMinor int
}

// ReleaseStoreURL is where the owner's PC fetches the driver first: the
// same public, query-string-free /releases/agent-download route the light
// agent's own updates use, under the windows-vigembus platform.
const ReleaseStoreURL = "https://readyapp.player-ready.co.uk/releases/agent-download/windows-vigembus/latest"

// ViGEmBus is ViGEmBus 1.22.0, the final release (the upstream repository
// was archived on 2 Nov 2023). The values match
// ops/solarbeam-gamepad-portsadmin-20260926/artefact/artefact.json; the
// signer hash was recomputed independently from the PE certificate table.
var ViGEmBus = Spec{
	Name:      "ViGEmBus",
	Version:   "1.22.0",
	Publisher: "Nefarius Software Solutions e.U.",
	URLs: []string{
		ReleaseStoreURL,
		"https://github.com/nefarius/ViGEmBus/releases/download/v1.22.0/ViGEmBus_1.22.0_x64_x86_arm64.exe",
	},
	SHA256:       "89220a7865076b342892f98865f3499fb7c4cfd673159e89d352c360fd014c6a",
	SizeBytes:    6278576,
	SignerSHA256: "e84e8537968cdc234e17a232bd813e3ea4e5b7826a8062f5cb033047e1ce3da0",
	InstallArgs:  "/exenoui /qn /norestart",
	ProductCode:  "{9C581C76-2D68-40F8-AA6F-94D3C5215C05}",
	MinMajor:     1,
	MinMinor:     17,
}
