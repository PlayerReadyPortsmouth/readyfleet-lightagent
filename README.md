# ReadyFleet Light Agent (BYOD)

This is the generated public source for ReadyFleet's personal-device light
agent and installer. Regenerate it from ReadyFleet; do not hand-edit runtime
files in this mirror. The current source can be newer than a published release.

The controller commands are listed in [CONTRACT.md](CONTRACT.md). Managed venue
commands are removed from the protocol: 168 declarations kept, 426 omitted.
The agent reports only its OS, architecture, its own version and SolarBeam host
readiness. The full venue hardware/software inventory collector is excluded.

Build and run the checks from this workspace:

    go build ./agent/... ./proto/...
    go test ./agent/... ./proto/...

The copied safety tests compile the agent and check its import boundary and
forbidden capability strings. There is no venue remote shell, terminal or
Windows service installation in this agent. SolarBeam's separate streaming
engine supplies the screen stream with the consent policy in this source.

Windows runs as the current user from LocalAppData with an HKCU login entry.
Linux runs as a desktop-user systemd service. Its optional system integration
asks for sudo to configure input devices; enrolment and the agent remain under
the desktop account. macOS uses the platform-specific current-user startup path.

The resilient installer canary is selected by the signed enrolment payload.
It checks prerequisites, retries safe network operations, repairs an install
using credentials for the expected device owner, and rolls back its own files
when it cannot finish. Installer reports contain the version, OS, failed step,
error category and redacted diagnostic lines. A scoped, short-lived reporting
credential sends them to ReadyApp; Discord webhook secrets exist only on the
server. Enrolment credentials are never part of a diagnostic report.

## Verify a download

The published Windows agent download returns x-artifact-sha256 and
x-artifact-signer-fingerprint headers. Compare the downloaded bytes' SHA-256
and its Windows Digital Signatures signer with those values before running it.
Linux release bundles are signed separately and verified by the downloader.

A mirror build uses pruned protocol source, so it does not reproduce the exact
bytes of the private full-protocol release build. The import-boundary tests
and the published artifact signatures are separate checks.
