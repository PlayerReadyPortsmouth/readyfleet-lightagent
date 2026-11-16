#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 /path/to/enrollment.json" >&2
  exit 2
fi
enrollment_file=$1
[[ -f ${enrollment_file} ]] || { echo "enrollment file not found: ${enrollment_file}" >&2; exit 3; }

if [[ ! -r /etc/os-release ]]; then
  echo "Ubuntu release information was not found." >&2
  exit 3
fi
# shellcheck disable=SC1091
. /etc/os-release
if [[ ${ID:-} != ubuntu ]]; then
  echo "This setup targets Ubuntu; detected ${PRETTY_NAME:-unknown Linux}." >&2
  exit 3
fi

if [[ ! ${VERSION_ID:-} =~ ^[0-9]+\.[0-9]+$ ]] || [[ $(printf '%s\n' 24.04 "$VERSION_ID" | sort -V | head -1) != 24.04 ]]; then
  echo "SolarBeam personal access requires Ubuntu 24.04 or newer; detected ${PRETTY_NAME:-unknown Ubuntu}." >&2
  exit 3
fi

bundle_dir=$(cd "$(dirname "$0")" && pwd)
for path in "${bundle_dir}/engine/sunshine" "${bundle_dir}/engine/solarbeam-bridge" \
            "${bundle_dir}/readyfleet-light" "${bundle_dir}/readyfleet-light-installer"; do
  [[ -x ${path} ]] || { echo "bundle is incomplete: ${path}" >&2; exit 3; }
done

echo "Installing Ubuntu integration prerequisites..."
sudo apt-get update
# wl-clipboard / xclip back SolarBeam clipboard sync. The engine shells out to
# one of them rather than linking Xlib or implementing wlr-data-control, so that
# one build works under whatever compositor a mentor happens to run — see
# src/solarbeam/clipboard.h in solarbeam-engine-v2. Both are installed because
# the session type is not known at install time and a mentor may switch between
# an X11 and a Wayland session on the same machine. Without them the engine
# still runs; clipboard sync reports itself unavailable.
runtime_packages=()
if [[ -f "$bundle_dir/ubuntu-runtime-packages" ]]; then
  while IFS= read -r package; do
    [[ $package =~ ^[a-z0-9][a-z0-9+.-]*$ ]] || { echo 'Invalid Ubuntu runtime package manifest.' >&2; exit 3; }
    runtime_packages+=("$package")
  done < "$bundle_dir/ubuntu-runtime-packages"
fi
sudo apt-get install -y libcap2-bin kmod udev wl-clipboard xclip "${runtime_packages[@]}"
# Never consume the one-use enrolment until the engine's loader can resolve
# every dependency on this distribution. This catches a mislabeled Arch build
# as well as a missing Ubuntu runtime package, before copying any user state.
loader_report=$(mktemp)
if ! LD_LIBRARY_PATH="$bundle_dir/engine/lib" ldd "$bundle_dir/engine/sunshine" > "$loader_report" 2>&1 || grep -q 'not found' "$loader_report"; then
  cat "$loader_report" >&2
  rm -f "$loader_report"
  echo 'SolarBeam runtime libraries are unavailable; enrolment was not attempted.' >&2
  exit 3
fi
rm -f "$loader_report"

data_home=${XDG_DATA_HOME:-${HOME}/.local/share}
engine_dir=${data_home}/solarbeam
install -d -m0700 "$(dirname "${engine_dir}")"
stage=$(mktemp -d "$(dirname "${engine_dir}")/.solarbeam-install-XXXXXX")
cleanup() { rm -rf -- "${stage}"; }
trap cleanup EXIT
cp -a "${bundle_dir}/engine/." "${stage}/"

previous=${engine_dir}.previous
rm -rf -- "${previous}"
if [[ -d ${engine_dir} ]]; then mv -- "${engine_dir}" "${previous}"; fi
if ! mv -- "${stage}" "${engine_dir}"; then
  if [[ -d ${previous} ]]; then mv -- "${previous}" "${engine_dir}"; fi
  echo "could not activate SolarBeam" >&2
  exit 4
fi
rm -rf -- "${previous}"
trap - EXIT

"${bundle_dir}/readyfleet-light-installer" "${enrollment_file}"

echo
echo "ReadyFleet is enrolled. SolarBeam now needs one privileged integration step."
sudo "${bundle_dir}/integration/install-system-integration.sh" "${engine_dir}" "${USER}"

# Keep the uninstaller beside the agent, so removing SolarBeam does not depend on
# the mentor still having the downloaded bundle. The system-level half of it is
# kept root-owned under /var/lib/readyfleet-byod by the integration step itself.
config_home=${XDG_CONFIG_HOME:-${HOME}/.config}
install -Dm0755 "${bundle_dir}/uninstall-linux.sh" "${config_home}/readyfleet-light/uninstall-linux.sh"

echo
echo "Ubuntu BYOD setup is complete."
# ---------------------------------------------------------------------------
# Final readiness report. The integration step normally grants /dev/uinput
# through the seat ACL, which applies at once; only when it had to fall back to
# the input group does THIS session still carry the group set it logged in with,
# so the mentor's very next action, starting a session, would fail on a machine
# whose setup genuinely succeeded. The only honest way to tell the two apart is
# to try the device the way the engine will, and to say which situation it is.
# ---------------------------------------------------------------------------
echo
echo "Readiness:"
if [[ -w /dev/uinput ]]; then
  echo "  ok      this machine can host a session now"
else
  echo "  ACTION  sign out and back in before hosting a session"
  echo "          (the input group applies to new logins only)"
fi

if command -v wl-copy >/dev/null 2>&1 || command -v xclip >/dev/null 2>&1 || command -v xsel >/dev/null 2>&1; then
  echo "  ok      clipboard sync available"
else
  echo "  note    clipboard sync unavailable (no wl-clipboard/xclip/xsel)"
  echo "          everything else works; only clipboard sharing is off"
fi
echo
echo "To remove ReadyFleet and SolarBeam later:  ${config_home}/readyfleet-light/uninstall-linux.sh"

