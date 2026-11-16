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

## Native Ubuntu and CachyOS/Arch packages

`build-native-packages.sh` wraps an **existing prebuilt** signed Linux bundle;
no engine compilation happens in this step. Build the payload on the oldest
supported Ubuntu LTS as above. This prevents a package from silently depending
on a newer glibc from the Arch build host.

```console
packaging/linux/build-native-packages.sh \
  dist/linux-byod/readyfleet-solarbeam-cachyos-x86_64-0.65.0.tar.zst \
  packaging/linux/release-public.pem \
  5961b14ed61f81602d7a48e65884bd3f20e4bb9dde2e4a2a6cefb61a29800c0a \
  0.65.0 dist/native
```

The builder checks the independently pinned key fingerprint and detached
Ed25519 signature, validates the bundle layout/version, and emits:

- `readyfleet-light_<version>_amd64.deb` and its SHA-256 file for Ubuntu.
- `readyfleet-light-<version>-arch/PKGBUILD`, the same prebuilt bundle and a
  launcher, with pinned SHA-256 sources. Run `makepkg -si` as a normal user on
  CachyOS/Arch. The recipe uses local sources so an unpublished build never
  invents a public download URL.

The native packages have no maintainer hooks and never enrol a device, start a
root service, or copy somebody's enrolment token into system directories.
They place the payload at `/usr/lib/readyfleet-light`. Once installed, the
logged-in desktop user runs `readyfleet-light-enrol /path/to/enrollment.json`
without sudo. This calls the existing distribution setup flow, which copies
agent/engine state into that user's XDG directories, enrols that person, and
asks for sudo only for host integration. The package is bootstrap media:
removing it does not revoke or uninstall the separately enrolled user agent.
Use ReadyApp's revoke control and the agent's uninstall flow for that.

The public key in `release-public.pem` was extracted from public
`linux-byod-agent` version code 10 on 16 Nov 2026 (download SHA-256
`83dc5da43d0b454f907a340215b3f70b2a4530f123b011fc8ead990838b699e8`)
and matched against the fingerprint committed above. It contains no secret.
A release operator must sign the resulting native packages before publishing
them; this script deliberately does neither. Existing public release storage
uses `linux-byod-bundle` and `linux-byod-agent`, and no native package release
platform is assumed.

Run the offline smoke test with:

```console
packaging/linux/test/native-packages-test.sh
```

It builds a fixture package and executes the PKGBUILD packaging function,
checks archive contents/checksums, and rejects a wrong key or tampered bundle.
It does not install packages or prove capture on an Ubuntu/CachyOS desktop.

### Ubuntu 24.04 build lane and compatibility boundary

The published `linux-byod-bundle` version code 10 was inspected on 16 Nov 2026:
its engine version is `0.8.11`, SHA-256 is
`8c0c361fa93096c95aa41aae84eca5b6a2de6cccdd987955997b0e8780cacb53`, and
`readelf --version-info` shows `GLIBC_2.44`. It also links Arch's miniupnpc 21
and ICU 78 sonames. It **cannot** be used on Ubuntu 24.04 (glibc 2.39), even
inside a `.deb`. The native Ubuntu package builder rejects that payload.

Use the dedicated Ubuntu build lane, which copies the source into a disposable
container, builds against Ubuntu 24.04, and leaves only the stage/build output
on the host:

```console
BUILD_VERSION=0.65.0 packaging/linux/build-ubuntu-engine.sh /path/to/solarbeam-engine-v2 /path/to/ubuntu-output
```

This requires Docker, downloads build dependencies, limits compilation to two
jobs/six GiB by default, and never touches the source checkout or a release
server. This lane disables CUDA toolkit code; CPU/VAAPI/Vulkan support and
NVIDIA behavior still need validation on actual desktops. A build alone does
not prove screen capture, input, audio, or wake.

Run the existing local release assembly inside the Ubuntu builder environment
with this stage and build tree, the bridge sources and the release operator's
signing key. That keeps the loader's dependency resolution on Ubuntu, too.
Register its **signed tar bundle** as `linux-byod-ubuntu-bundle`, which the
ReadyApp personal Ubuntu wizard selects. Keep the Arch/CachyOS payload under
`linux-byod-bundle`. Neither platform is published or registered by this PR.

`build-native-packages.sh` accepts a final `ubuntu`, `arch` or `both` argument
(default `both`). Ubuntu packaging checks every ELF file for `GLIBC <= 2.39`
and requires Ubuntu's `dpkg-shlibdeps` for a dynamically linked real engine,
so the resulting `.deb` declares its actual runtime dependencies. An Arch
package can be generated from its own signed bundle with `arch` without
claiming Ubuntu compatibility. The GLIBC check is a necessary baseline check,
not a replacement for running the installed package on the target desktop.

`build-ubuntu-bundle.sh VERSION ENGINE_REPO BRIDGE_REPO OUTPUT_DIR` also builds
the Go light agent/installer and LiveKit bridge inside that environment (using
your installed Go toolchain), then assembles an unsigned developer tar bundle.
It records the engine source commit and stamps the explicit build version; it
never uses a production signing key or publishes anything. A release operator
must sign the verified output before native release packaging.

The Ubuntu developer bundle includes an `ubuntu-runtime-packages` manifest
resolved against Ubuntu's shlibs database before enrolment. The setup script
installs those packages and checks the engine's loader before consuming the
one-use enrolment. The versioned miniupnpc DSO travels with the payload so a
newer Ubuntu/Arch release does not need the old distro package name. The build
also prefers the source-pinned NVIDIA codec headers over FFmpeg's downloaded
headers; it keeps the engine's backwards-compatibility assertion enabled.
