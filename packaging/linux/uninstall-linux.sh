#!/usr/bin/env bash
# Remove ReadyFleet BYOD and SolarBeam from this Linux machine.
#
#   ./uninstall-linux.sh [--yes] [--remove-input-group] [--purge-engine-config]
#
# Run it as the desktop user who enrolled, not with sudo; it asks for sudo once,
# for the system-level part, at the end.
#
# Removes, in this order:
#   1. the ReadyFleet light agent: its systemd user service, its enrolment
#      certificate and key, and ~/.config/readyfleet-light;
#   2. the SolarBeam engine in $XDG_DATA_HOME/solarbeam, and the KWin
#      screencast permission entry the agent wrote for it;
#   3. the system integration (udev rule, receipt, and the input group if the
#      integration step added it), via the root-owned copy recorded at install.
#
# ~/.config/sunshine is left alone unless --purge-engine-config is given: it is
# the standard Sunshine configuration directory, and may belong to a Sunshine
# the mentor installed themselves.
#
# The agent's own uninstall removes this machine's credentials, but does not
# revoke it in ReadyFleet. Remove the device under "SolarBeam Devices" in
# ReadyApp as well, so the record does not linger as merely offline.
set -euo pipefail

assume_yes=0
integration_args=()
purge_engine_config=0
for arg in "$@"; do
  case ${arg} in
    --yes|-y) assume_yes=1 ;;
    --remove-input-group) integration_args+=(--remove-input-group) ;;
    --purge-engine-config) purge_engine_config=1 ;;
    -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: ${arg}" >&2; exit 2 ;;
  esac
done

if [[ ${EUID} -eq 0 ]]; then
  echo "Run this as the desktop user, not with sudo; it asks for sudo when it needs it." >&2
  exit 1
fi

config_home=${XDG_CONFIG_HOME:-${HOME}/.config}
data_home=${XDG_DATA_HOME:-${HOME}/.local/share}
agent_dir=${config_home}/readyfleet-light
agent=${agent_dir}/readyfleet-light
unit=${config_home}/systemd/user/readyfleet-light.service
engine_dir=${data_home}/solarbeam
kwin_entry=${data_home}/applications/dev.lizardbyte.app.Sunshine.kwin-readyfleet.desktop
engine_config=${config_home}/sunshine
system_uninstaller=/var/lib/readyfleet-byod/install-system-integration.sh
bundle_dir=$(cd "$(dirname "$0")" && pwd)

if [[ ${assume_yes} -ne 1 ]]; then
  echo "This removes ReadyFleet and SolarBeam from this machine for ${USER}."
  read -r -p "Continue? [y/N] " reply
  [[ ${reply} =~ ^[Yy]$ ]] || { echo "Nothing changed."; exit 0; }
fi

echo
echo "1/3 ReadyFleet agent"
if [[ -x ${agent} ]] && "${agent}" uninstall; then
  :
else
  # The agent's own uninstall is the normal path. This is the fallback for a
  # half-installed or already-broken agent: do by hand what it would have done.
  systemctl --user disable --now readyfleet-light.service >/dev/null 2>&1 || true
  rm -f -- "${unit}"
  systemctl --user daemon-reload >/dev/null 2>&1 || true
  rm -rf -- "${agent_dir}"
fi
echo "    removed"

echo "2/3 SolarBeam engine"
rm -rf -- "${engine_dir}" "${engine_dir}.previous"
find "${data_home}" -maxdepth 1 \( -name '.solarbeam-install-*' -o -name '.solarbeam-update-*' \) \
  -exec rm -rf -- {} + 2>/dev/null || true
rm -f -- "${kwin_entry}"
if [[ ${purge_engine_config} -eq 1 ]]; then
  rm -rf -- "${engine_config}"
  echo "    removed (including ${engine_config})"
else
  echo "    removed (kept ${engine_config}; pass --purge-engine-config to remove it too)"
fi

echo "3/3 System integration (needs sudo)"
if [[ -x ${system_uninstaller} ]]; then
  sudo "${system_uninstaller}" --uninstall "${USER}" "${integration_args[@]}"
elif [[ -e /etc/udev/rules.d/60-solarbeam-readyfleet.rules && -x ${bundle_dir}/integration/install-system-integration.sh ]]; then
  # Installed by a bundle from before the receipt existed.
  sudo "${bundle_dir}/integration/install-system-integration.sh" --uninstall "${USER}" "${integration_args[@]}"
elif [[ -e /etc/udev/rules.d/60-solarbeam-readyfleet.rules ]]; then
  echo "    An older install left a udev rule but no uninstaller. Remove it with:"
  echo "      sudo rm /etc/udev/rules.d/60-solarbeam-readyfleet.rules && sudo udevadm control --reload-rules"
else
  echo "    nothing installed"
fi

echo
echo "ReadyFleet and SolarBeam are removed from this machine."
echo "Also remove it under \"SolarBeam Devices\" in ReadyApp, so ReadyFleet stops listing it."
