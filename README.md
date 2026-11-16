# ReadyFleet Light Agent (BYOD)

The agent that runs on a **personal** machine so it can host SolarBeam
sessions. Published so you can read it before you install it, and check that
what we ship is what is written here.

This repository is **generated** from ReadyFleet and is read-only. Nothing is
typed into it directly, so it cannot drift from the agent you are running.

## What it can do

The accepted commands are listed in [CONTRACT.md](CONTRACT.md), generated from
the source. The light installer is included so enrolment is auditable too.

Build and test it yourself — this is a Go workspace, so the paths are explicit:

    go build ./agent/... ./proto/...
    go test  ./agent/... ./proto/...

## What it cannot do

There is no remote shell, no screen capture, no terminal, no ability to read
your files, and no service installation. That is not a promise — it is
checked. `cmd/lightagent/safety_test.go` compiles the binary and fails
if it contains the tells of any of those capabilities:

    go test ./agent/... ./proto/...

Runtime source files are copied verbatim. Two test-only adaptations are made:
the shared staff-message test's rejected shell fixture uses the same typed wire
literal instead of exporting the managed command constant; the companion
browser source-wiring test retains its BYOD assertion and omits only the
assertion reading the separate managed venue executable. All other assertions,
including the binary capability checks, remain.
Safe protocol data referenced by these tests is retained too.

The wire protocol here is pruned to what this agent and its tests reference: 168
declarations kept, 426 belonging to the separate managed-venue fleet removed.
Their absence is deliberate and is itself part of the guarantee.

## Verify what you were given

Use the same Go version, build flags, dependencies and source revision when
comparing local builds. This snapshot does **not** reproduce the exact hash of
the binary we publish.

Our released build compiles the full ReadyFleet protocol package, which
declares commands for the separate managed-venue fleet. Those declarations are
removed here on purpose — see above — and Go derives part of a binary's
identity from its source, so different source means different bytes even
though no removed declaration is reachable from this agent. Measured, not
assumed: the two builds differ.

So the guarantee is this instead:

- **Every line that runs on your machine is here.** Only the unused protocol
  declarations differ, and removing them is what stops this repository
  disclosing capabilities aimed at machines that are not yours.
- **The capability boundary is testable.** `go test ./agent/... ./proto/...` runs the same checks we do.
- **The file you were given is the file we published.** The download endpoint
  returns both values as response headers — not just a claim, a value you can
  fetch and diff yourself:

      curl -sI https://readyapp.player-ready.co.uk/releases/agent-download/windows-byod-agent/<versionCode>

  gives `x-artifact-sha256` and `x-artifact-signer-fingerprint`. Hash the file you were
  given and compare; open its Properties → Digital Signatures in Windows
  Explorer and confirm the signer thumbprint matches. Either mismatching means
  the download was tampered with in transit — stop and don't run it.

## Privacy-safe inventory

The light agent reports its OS family/version, CPU/GPU model, agent and engine
versions, and SolarBeam availability. Unlike the August snapshot's stub, its
current `lightInventoryProvider` supplies that limited snapshot; it does
not enable managed hardware or telemetry collection. Shared protocol DATA
shapes can still appear because Go must compile shared types. They do not
add a command handler. The command contract and safety tests are the boundary.

## How it runs

Windows runs from your own `%LocalAppData%` and starts at login,
without a Windows service. Linux runs as your desktop user under a systemd
user unit, with SolarBeam under `$XDG_DATA_HOME/solarbeam`. Its setup
scripts ask for sudo only for host input integration and distribution packages.
The agent itself does not run as root. Pause/availability and connection-consent
controls remain in the agent; owner self-connect does not bypass pause.

Linux prebuilt bundles are signed with Ed25519. Compare the download's SHA-256
to `x-artifact-sha256` from the release endpoint, and verify the detached
signature with [linux-release-public.pem](linux-release-public.pem), whose raw
Ed25519 public-key SHA-256 is
`5961b14ed61f81602d7a48e65884bd3f20e4bb9dde2e4a2a6cefb61a29800c0a`.
The key was matched to the committed release pin and the existing public Linux
agent download; no private key is present. For an artifact with its detached
signature alongside it:

    openssl pkeyutl -verify -rawin -pubin -inkey linux-release-public.pem -sigfile artifact.sig -in artifact

Native package recipes live in ReadyFleet `packaging/linux`; they wrap that same payload.

This snapshot includes the macOS source paths reached by the same agent as
well; publishing this source does not publish a signed installer for any OS.
