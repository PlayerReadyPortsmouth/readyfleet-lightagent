#!/usr/bin/env bash
# Sourced by the distro entrypoints only for a resilient_install payload.
# All paths are per-user, except the integration script's recorded changes.
resilient_begin() {
  local distro=$1 available_kb
  if [[ ${EUID} -eq 0 ]]; then
    echo "Run setup from your desktop account. It will ask for sudo only for the Linux integration step." >&2
    return 2
  fi
  for command in systemctl sudo df awk mktemp cp mv install grep; do
    command -v "$command" >/dev/null || { echo "Missing required command: $command. Install it and run setup again." >&2; return 2; }
  done
  # shellcheck disable=SC1091
  . /etc/os-release
  if [[ $distro == ubuntu ]]; then
    if [[ ${ID:-} != ubuntu || ${VERSION_ID%%.*} -lt 22 ]]; then
      echo "This setup needs Ubuntu 22.04 or newer. Download the matching installer in ReadyApp." >&2
      return 2
    fi
  elif [[ ${ID:-} != cachyos && ${ID:-} != arch && ${ID_LIKE:-} != *arch* ]]; then
    echo "This setup needs CachyOS or Arch Linux. Download the matching installer in ReadyApp." >&2
    return 2
  fi
  if ! systemctl --user show-environment >/dev/null 2>&1; then
    echo "The desktop user service is unavailable. Sign in to your desktop and run setup there, without sudo." >&2
    return 2
  fi
  if ! sudo -v; then
    echo "Linux integration needs administrator permission. Ask the PC owner to approve sudo, then run setup again." >&2
    return 2
  fi
  data_home=${XDG_DATA_HOME:-${HOME}/.local/share}
  (umask 077; mkdir -p "$data_home")
  available_kb=$(df -Pk "$data_home" | awk 'NR==2 {print $4}')
  if [[ ! $available_kb =~ ^[0-9]+$ ]] || (( available_kb < 1048576 )); then
    echo "Setup needs at least 1 GB of free space. Free some space and run setup again." >&2
    return 2
  fi
  lock_dir=${data_home}/.readyfleet-setup.lock
  if ! mkdir "$lock_dir" 2>/dev/null; then
    echo "Another ReadyFleet setup may be running. Close it before trying again. If setup was interrupted, remove the empty .readyfleet-setup.lock folder in your data directory." >&2
    return 2
  fi
  transaction=
  engine_activated=0
  integration_started=0
  integration_preexisting=0
  committed=0
  trap resilient_cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM HUP
  transaction=$(mktemp -d "$data_home/.readyfleet-setup-XXXXXX")
  engine_dir=${data_home}/solarbeam
  previous=${transaction}/previous
  stage=${transaction}/engine
  mkdir "$stage"
  # Never remove another install's integration or inherited group membership.
  if sudo test -e /var/lib/readyfleet-byod/integration.receipt ||
      sudo test -e /etc/udev/rules.d/60-solarbeam-readyfleet.rules ||
      sudo test -e /etc/modules-load.d/solarbeam-readyfleet.conf; then
    integration_preexisting=1
  fi
}

resilient_activate() {
  # shellcheck disable=SC2154 # The entrypoint owns bundle_dir.
  cp -a "${bundle_dir}/engine/." "$stage/" || return
  if [[ -e $engine_dir || -L $engine_dir ]]; then
    mv -- "$engine_dir" "$previous" || return
  fi
  # Record ownership before the move: cleanup must restore the backup even
  # if activating the new tree itself fails.
  engine_activated=1
  mv -- "$stage" "$engine_dir" || return
  integration_started=1
  sudo "${bundle_dir}/integration/install-system-integration.sh" "$engine_dir" "$(id -un)"
}

resilient_cleanup() {
  local result=$? rollback_failed=0
  trap - EXIT INT TERM HUP
  if (( ! committed )); then
    if (( integration_started && ! integration_preexisting )); then
      sudo "${bundle_dir}/integration/install-system-integration.sh" --uninstall "$(id -un)" || rollback_failed=1
    fi
    if (( engine_activated )); then
      if rm -rf -- "$engine_dir"; then
        if [[ -e $previous || -L $previous ]]; then
          mv -- "$previous" "$engine_dir" || rollback_failed=1
        fi
      else
        rollback_failed=1
      fi
    fi
    if (( result != 0 )); then
      echo "Setup could not finish. The previous SolarBeam files were restored; request a fresh setup link in ReadyApp before retrying." >&2
    fi
  fi
  if (( rollback_failed )); then
    echo "Automatic cleanup needs help. Keep the .readyfleet-setup backup folder and contact readyapp@player-ready.co.uk." >&2
    result=11
  elif [[ -n $transaction ]]; then
    rm -rf -- "$transaction"
  fi
  rmdir "$lock_dir" || true
  exit "$result"
}
