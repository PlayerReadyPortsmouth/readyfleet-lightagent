# ReadyFleet Light Agent (BYOD)

The agent that runs on a **personal** machine so it can host SolarBeam
sessions. Published so you can read it before you install it, and check that
what we ship is what is written here.

This repository is **generated** from ReadyFleet and is read-only. Nothing is
typed into it directly, so it cannot drift from the agent you are running.

## What it can do

Six commands, listed in [CONTRACT.md](CONTRACT.md). That is the whole surface.

Build and test it yourself — this is a Go workspace, so the paths are explicit:

    go build ./agent/... ./proto/...
    go test  ./agent/... ./proto/...

## What it cannot do

There is no remote shell, no screen capture, no terminal, no ability to read
your files, and no service installation. That is not a promise — it is
checked. `cmd/lightagent/safety_test.go` compiles the binary and fails
if it contains the tells of any of those capabilities:

    go test ./agent/... ./proto/...

The wire protocol here is pruned to what this agent references: 55
declarations kept, 41 belonging to the separate managed-venue fleet removed.
Their absence is deliberate and is itself part of the guarantee.

## Verify what you were given

Builds here are deterministic: build twice, or against a colleague's build,
and the bytes match. What you **cannot** do is match the hash of the binary
we publish, and the reason is deliberate rather than evasive.

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

## A known gap in the pruning

Six commands is the real surface (above) — but `proto/messages.go` here also
declares several managed-fleet DATA shapes (`HardwareInfo`, `DiskInfo`,
`NetworkInfo`, `TelemetryData`, `GPUInfo`, `ThermalInfo`) that this agent never
populates. Not a leaked capability — the forbidden-symbol check that generates
this repository would refuse to publish one of those — but worth being
straight about rather than leaving unexplained.

Why they're here: this agent and the managed one share a config struct
(`internal/runtime.Config`), and that struct declares hooks for both. This
agent's own inventory hook is a permanent stub:

    InventoryProvider: func(ctx context.Context) (proto.InventoryData, error) {
        return proto.InventoryData{}, nil
    },

— see `cmd/lightagent/main.go` — and never sets the telemetry hook at all. The
struct field's TYPE still has to be declared for the shared code to compile,
which is what pulls the shape declarations in. No instance of this agent has
ever sent one populated.

## How it runs

Current user only, from your own `%LocalAppData%` — no administrator
rights at any point, and no Windows service. It starts when you log in and
connects outbound only.
