# ReadyFleet BYOD on CachyOS/Arch and Ubuntu

This is the first Linux profile. It runs `readyfleet-light`, SolarBeam, and
the LiveKit bridge as the logged-in desktop user. It does not install or enable
the managed ReadyFleet agent.

The SolarBeam CMake install tree, bridge, and light agent are assembled with
`build-cachyos-bundle.sh`. Production artifacts must include the detached
Ed25519 `.sig`; unsigned output is for local development only.

## Staging the engine

The bundle is relocatable — it unpacks under the mentor's
`$XDG_DATA_HOME/solarbeam` — so the engine must be configured to match. Three
of these flags are not optional:

The engine is `solarbeam-engine-v2` (branch `solarbeam`), the fork of
`Nonary/vibeshine`. The superseded `solarbeam-engine` is reference only.

```console
cmake -B build -S . \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_INSTALL_PREFIX=/path/to/stage \
  -DCMAKE_INSTALL_RPATH='$ORIGIN/lib' \
  -DSUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF \
  -DSUNSHINE_ENABLE_TRAY=OFF \
  -DBUILD_TESTS=OFF -DBUILD_DOCS=OFF
cmake --build build --parallel
cmake --install build
```

`SUNSHINE_ENABLE_TRAY=OFF` avoids a hard dependency on
`libayatana-appindicator`, which the engine's tray requires alongside libnotify
and which the bundle cannot guarantee is present on a mentor's machine. The
trade-off is that nothing on the host then indicates a session is live — the
light agent has no Linux UI of its own — so revisit this before rollout.

`CMAKE_INSTALL_RPATH` keeps the engine able to find the libraries the bundle
carries beside it; the default install step strips the build tree's runpath and
leaves nothing in its place. `SUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF` stages
the udev rule inside the assets tree instead of writing it to `/usr/lib/udev`,
which both escapes the staging prefix and needs root.

Both `release-cachyos.sh` and `build-cachyos-bundle.sh` need the build tree as
well as the stage (`--engine-build`): the engine links Boost, libdatachannel,
glad, libdisplaydevice and inputtino as shared libraries that are built there
and that no install step copies out.

On an Arch-family build host, add `-DGLAD_SKIP_PIP_INSTALL=ON`. glad probes for
`pkg_resources`, which setuptools 81 deprecated and 84 removed, and otherwise
tries to pip-install into the system interpreter.

`release-cachyos.sh` is the production entrypoint. It derives the public key
from an externally supplied Ed25519 PEM, embeds only that public key in the
agent, builds the agent/installer/bridge, signs both uploadable artifacts, and
prints the signer fingerprint ReadyApp must store. The private key is never
copied into the repository or output directory.

```console
./release-cachyos.sh --version 0.6.0 \
  --engine-stage /path/to/stage --engine-build /path/to/solarbeam-engine/build \
  --bridge-repo /path/to/solarbeam-livekit-bridge \
  --signing-key /path/to/ed25519.pem --output ./dist
```

Pass `--installer` for a mentor-facing bundle. The Linux user-facing entrypoint
is a terminal script, not the Windows-style installer UI:

```console
./setup-cachyos.sh /path/to/enrollment.json
```

It installs the bundled engine under `$XDG_DATA_HOME/solarbeam`, invokes the
internal console enrollment helper, registers and starts the systemd user
service, confirms the C2 connection, and then requests `sudo` only for the
udev/input integration described below. The Windows BYOD release remains its existing
installer flow.

The same signed Linux bundle also contains an Ubuntu entrypoint:

```console
./setup-ubuntu.sh /path/to/enrollment.json
```

It verifies the host is Ubuntu and installs the small set of host integration
prerequisites with `apt` before following the same unprivileged enrollment and
systemd-user-service flow. Build production Linux payloads on the oldest
supported Ubuntu LTS baseline so the resulting glibc-linked engine remains
portable to newer Ubuntu and rolling Arch-family systems.

### The privileged step

`integration/install-system-integration.sh` is the only part that runs as root.
A BYOD machine belongs to the mentor, so it changes as little as it can and
records all of it:

| Change | Why | Undone by `--uninstall` |
|---|---|---|
| `/etc/udev/rules.d/60-solarbeam-readyfleet.rules` | lets the person at the machine (logind `uaccess`) open `/dev/uinput`/`/dev/uhid` for remote input | removed, udev reloaded |
| `/etc/modules-load.d/solarbeam-readyfleet.conf` | loads `uinput` and `uhid` at every boot; without it `/dev/uhid` stays root-only after a reboot and PlayStation-style controllers fail | removed (modules stay loaded until reboot) |
| `/var/lib/readyfleet-byod/` | receipt of these changes + a root-owned copy of the script | removed |
| `input` group membership — **fallback only** | when the seat ACL did not grant `/dev/uinput` (no logind seat, setup over SSH) | removed only if the receipt says this step added it |

It also strips any file capabilities from the engine (they break compositor
screencast — see the note in the script) and loads `uinput`/`uhid` for this
boot. It refuses a bundled rule that runs programs, and ends with a
verification pass that asks, as the desktop user, whether `/dev/uinput` can
actually be opened (and warns, without failing, if `/dev/uhid` cannot:
keyboard, mouse and Xbox-style controllers need only `/dev/uinput`).

`input` is a last resort because it also lets every process of that user read
every keyboard and mouse on the machine. With the seat ACL no sign-out is
needed; the script only asks for one when it had to add the group.
`--dry-run` runs without root and changes nothing; `--input-group` and
`--no-input-group` override the automatic choice.

To remove everything, the setup scripts leave the user-level uninstaller at
`~/.config/readyfleet-light/uninstall-linux.sh`. It removes the agent (and its
credentials), the engine and the KWin permission entry, then runs
`sudo /var/lib/readyfleet-byod/install-system-integration.sh --uninstall`.
`~/.config/sunshine` is kept unless `--purge-engine-config` is passed, since a
mentor may run their own Sunshine. The device still has to be removed under
"SolarBeam Devices" in ReadyApp.

Machines set up before the receipt existed (up to bundle 0.8.x) have no
record of who added `input`; uninstall keeps that membership and prints the
`gpasswd` command, or removes it with `--remove-input-group`.

`test/run-container-tests.sh` exercises install, fallback, uninstall, legacy
and refusal paths for real inside throwaway Ubuntu 24.04 and Arch containers.
Never run the integration step on a developer machine to test it.

Publish the bundle only to the `linux` `byod_solarbeam` target. ReadyFleet
partitions convergence targets by platform, so Windows clients remain on their
existing Windows artifact.

## Publishing

`release-cachyos.sh` stops at a local directory. `scripts/release-linux-byod.sh`
is the step that takes it the rest of the way: it runs the build, verifies both
detached signatures against the public half of the key before anything leaves
the machine, uploads to ReadyApp's release store, and registers the two
`ReleaseArtifact` rows. It is the Linux counterpart of `scripts/release-agent.sh`.

```console
VERSION=0.8.0 VERSION_CODE=1 \
  ENGINE_STAGE=/path/to/stage ENGINE_BUILD=/path/to/solarbeam-engine-v2/build-linux \
  BRIDGE_REPO=/path/to/solarbeam-livekit-bridge \
  SIGNING_KEY=~/.config/readyfleet/linux-byod-signing.pem \
  VPS=readyapp-newvps RELEASES_DIR=/srv/readyapp/data/releases \
  scripts/release-linux-byod.sh
```

Two rows, because they do different jobs:

| Platform | What it is | Who fetches it |
|---|---|---|
| `linux-byod-bundle` | the whole `.tar.zst` | the setup script `POST /solarbeam-devices/enroll?platform=linux` generates, unauthenticated, pinning this row's sha256. Bootstrap only — never a convergence target. |
| `linux-byod-agent` | the bare `readyfleet-light` binary | the agent's own self-update, from the `fleet_agent_targets` row the script prints at the end. |

`DRY_RUN=1` builds, signs and verifies without uploading or registering.

### The release key

The private Ed25519 key is deliberately not in this repository, not in the
output directory, and not in any environment file: it is a path passed on the
command line. Its **public** half is compiled into the agent
(`-X main.lightLinuxPublicKey`), which is what makes the key that signs a
release and the key an installed agent will accept the same key by construction.

The practical consequence is worth stating plainly, because it is not obvious
and it is not recoverable: an agent only trusts the key that was compiled into
*it*. Signing a new release with a different key does not roll the fleet over to
that key — it makes the update unverifiable on every machine already out there,
and those machines have to be re-enrolled from a fresh bundle. Treat the key as
something to keep, not something to regenerate.

- Working copy: `~/.config/readyfleet/linux-byod-signing.pem`, mode 0600.
- Backup: `/srv/readyfleet-codesign/secrets/linux-byod-signing.pem` on the
  production VPS, alongside the Authenticode material.
- Current signer fingerprint (public, and the value the target rows carry):
  `5961b14ed61f81602d7a48e65884bd3f20e4bb9dde2e4a2a6cefb61a29800c0a`

Derive the fingerprint from the key at any time with the same steps
`release-cachyos.sh` uses — public key to DER, last 32 bytes, SHA-256.


## Resilient setup canary and native-package handoff (ri / pa, 16 Nov 2026)

The generated ReadyApp downloader and these entrypoints opt into the resilient
path only when the enrolment JSON contains `resilient_install: true`. ReadyApp
mints that payload behind its runtime per-person installer flag. Old payloads
keep the existing path; never add the flag manually to a published bundle.

The downloader makes at most three complete HTTPS downloads, with one- and
two-second backoff, discarding each unverified partial file. Every attempt is
checked against the release SHA-256. It checks free disk space and server time;
a final failure sends only a canonical diagnostic through the independent,
short-lived reporting capability in its payload. Curl/proxy errors are never
included in the report.

The distro entrypoint preflights the desktop user service, distribution, disk
space and sudo approval. It keeps the previous engine in a unique transaction
directory until both system integration and agent enrolment succeed. Integration
runs before enrolment, so a failed privileged step cannot leave a newly running
agent. Failures restore the previous engine, and a fresh integration is removed
using its own receipt. Existing integration and unrelated packages are preserved.
If rollback fails, the backup stays on disk and setup explicitly asks for help.
An interrupted process that was killed without a catchable signal can leave the
empty `.readyfleet-setup.lock` folder; the next run explains how to remove it
once no other setup is running.

Package-manager transactions are deliberately not undone: removing shared
Ubuntu prerequisites or rolling back Arch packages could break other software.
An existing integration is not uninstalled on failure. A privileged integration
failure before it writes its receipt can also leave changes that its uninstaller
cannot attribute, particularly input-group membership; support must inspect
those rather than blindly revoking the user's pre-existing access. This draft
has not been exercised against a real desktop's sudo/systemd/udev stack.

Coordination evidence: the live local `pa` job is **SolarBeam personal access
for mentors**. Its `feature/solarbeam-personal-linux-20261116` worktree adds native
packages whose `readyfleet-light-enrol` launcher delegates to these same distro
entrypoints. Its native builder copies the full verified bundle tree, so
`setup-resilient.sh` travels with them automatically once ri's bundle-builder
change is included. Preserve that sibling when wrapping/extracting the bundle;
no package-manager root hook should run enrolment or hold its token. No pa-owned
source was edited by ri. At bundle assembly, run both jobs' targeted tests and
verify the helper exists in the Debian and Arch payloads before publishing.

Local checks (no host privilege changes):

```console
python3 packaging/linux/test/setup-resilience-test.py
shellcheck -x -P packaging/linux packaging/linux/setup-ubuntu.sh packaging/linux/setup-cachyos.sh
```
