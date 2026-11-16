#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 /path/to/enrollment.json" >&2
  exit 2
fi
enrollment_file=$1
[[ -f ${enrollment_file} ]] || { echo "enrollment file not found: ${enrollment_file}" >&2; exit 3; }

bundle_dir=$(cd "$(dirname "$0")" && pwd)
for path in "${bundle_dir}/engine/sunshine" "${bundle_dir}/engine/solarbeam-bridge" \
            "${bundle_dir}/readyfleet-light" "${bundle_dir}/readyfleet-light-installer"; do
  [[ -x ${path} ]] || { echo "bundle is incomplete: ${path}" >&2; exit 3; }
done

# Clipboard-sync helpers. The engine shells out to wl-copy/wl-paste (Wayland) or
# xclip (X11) rather than linking Xlib or implementing wlr-data-control, so that
# one build works under whatever compositor a mentor happens to run — see
# src/solarbeam/clipboard.h in solarbeam-engine-v2. Both are installed because
# the session type is not known at install time and a mentor may switch between
# an X11 and a Wayland session on the same machine.
#
# Unlike the Ubuntu script this only OFFERS to install them: an Arch-family
# machine is somebody's own rolling system, and a setup script that runs pacman
# unasked is a setup script that can pull in a partial upgrade. Declining is
# fine — the engine still runs, and clipboard sync reports itself unavailable.
missing=()
for pkg in wl-clipboard xclip; do
  pacman -Qq "${pkg}" >/dev/null 2>&1 || missing+=("${pkg}")
done
if (( ${#missing[@]} )); then
  echo
  echo "SolarBeam clipboard sync needs: ${missing[*]}"
  read -r -p "Install them now with pacman? [y/N] " reply
  if [[ ${reply} =~ ^[Yy]$ ]]; then
    sudo pacman -S --needed --noconfirm "${missing[@]}"
  else
    echo "Skipping — clipboard sync will be unavailable until they are installed."
  fi
fi

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
echo "CachyOS BYOD setup is complete."
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

