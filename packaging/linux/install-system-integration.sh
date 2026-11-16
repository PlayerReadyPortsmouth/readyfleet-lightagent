#!/usr/bin/env bash
# SolarBeam's one privileged step on a Linux BYOD machine.
#
# BYOD means the machine belongs to the mentor, not to us, so this step does
# as little as it can, writes down everything it did, and can undo it:
#
#   sudo install-system-integration.sh <SolarBeam install root> <desktop user>
#   sudo install-system-integration.sh --uninstall <desktop user> [--remove-input-group]
#        install-system-integration.sh --dry-run <SolarBeam install root> <desktop user>
#
# What an install changes on the system, and nothing else:
#
#   /etc/udev/rules.d/60-solarbeam-readyfleet.rules
#       lets the person sitting at the machine (logind's `uaccess` tag) open
#       /dev/uinput and /dev/uhid, which is how remote keyboard, mouse and
#       gamepad input reaches the desktop;
#   /etc/modules-load.d/solarbeam-readyfleet.conf
#       loads uinput and uhid at every boot. /dev/uinput is a static node that
#       loads its module on first open, but /dev/uhid is not: until something
#       running as root loads uhid, the boot-time /dev/uhid is root-only, the
#       udev rule above never gets a chance to apply, and PlayStation-style
#       virtual controllers (created through uhid) fail with "Permission
#       denied" after every reboot;
#   /var/lib/readyfleet-byod/
#       a receipt of these changes and a root-owned copy of this script, so
#       `--uninstall` works after the downloaded bundle has been deleted and
#       never has to run a file the desktop user could have edited;
#   the `input` group, ONLY as a fallback
#       when the uaccess ACL did not give the desktop user /dev/uinput (no
#       logind seat, or setup run over SSH). Membership of `input` also lets
#       every process of that user READ every keyboard and mouse on the
#       machine, which is far more than SolarBeam needs, so it is never the
#       first choice and it is recorded so uninstall can take it back.
#
# It also strips file capabilities from the engine (see the note further down)
# and loads uinput/uhid for this boot as well.
#
# --dry-run needs no root and changes nothing: it reports what an install would
# do and what state the machine is in now.
set -euo pipefail

readonly rule_dest=/etc/udev/rules.d/60-solarbeam-readyfleet.rules
readonly modules_dest=/etc/modules-load.d/solarbeam-readyfleet.conf
readonly state_dir=/var/lib/readyfleet-byod
readonly receipt=${state_dir}/integration.receipt
readonly script_copy=${state_dir}/install-system-integration.sh
readonly uinput=/dev/uinput
readonly uhid=/dev/uhid

usage() {
  cat >&2 <<EOF
usage: sudo $0 <SolarBeam install root> <desktop user> [--input-group|--no-input-group]
       sudo $0 --uninstall <desktop user> [--remove-input-group]
            $0 --dry-run <SolarBeam install root> <desktop user>
EOF
}

mode=install
input_group_policy=auto   # auto | always | never
remove_input_group=0
positional=()
for arg in "$@"; do
  case ${arg} in
    --uninstall) mode=uninstall ;;
    --dry-run) mode=dry-run ;;
    --input-group) input_group_policy=always ;;
    --no-input-group) input_group_policy=never ;;
    --remove-input-group) remove_input_group=1 ;;
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: ${arg}" >&2; usage; exit 2 ;;
    *) positional+=("${arg}") ;;
  esac
done

if [[ ${mode} != dry-run && ${EUID} -ne 0 ]]; then
  echo "Run this one integration step with sudo." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

sha256_of() { sha256sum "$1" | awk '{print $1}'; }
# By hash, not cmp(1): diffutils is not part of an Arch base system.
same_content() { [[ -f $1 && -f $2 && $(sha256_of "$1") == "$(sha256_of "$2")" ]]; }

user_in_input_group() {
  # Read from the SYSTEM, not `id` on this process: a usermod made moments ago
  # is not in any running session's group set yet.
  id -nG "$1" 2>/dev/null | tr ' ' '\n' | grep -qx input
}

# Can <user> open /dev/uinput for writing? Asked as that user — through a
# logind ACL, group membership or mode bits, whichever applies — rather than
# inferred from any one of them. runuser initialises supplementary groups from
# /etc/group, so a membership added by this run already counts.
user_can_write() {
  local user=$1 path=$2
  [[ -e ${path} ]] || return 1
  if [[ ${EUID} -eq 0 ]]; then
    runuser -u "${user}" -- test -w "${path}"
  elif [[ $(id -un) == "${user}" ]]; then
    test -w "${path}"
  else
    return 1
  fi
}

# Receipt values are parsed, never sourced: `key=value` lines, known keys only.
receipt_get() {
  local key=$1
  [[ -f ${receipt} ]] || return 0
  sed -n "s/^${key}=\\([A-Za-z0-9_.:/+-]*\\)\$/\\1/p" "${receipt}" | tail -n1
}

reload_udev() {
  udev_reload_ok=0
  udevadm control --reload-rules || udev_reload_ok=$?
  udevadm trigger --property-match=DEVNAME="${uinput}" || true
  udevadm trigger --property-match=DEVNAME="${uhid}" || true
  # trigger only queues the events; the uaccess ACL is applied when udev has
  # processed them, and the access check below must not race that.
  udevadm settle --timeout=10 || true
}

# ---------------------------------------------------------------------------
# Uninstall
# ---------------------------------------------------------------------------
if [[ ${mode} == uninstall ]]; then
  if [[ ${#positional[@]} -ne 1 ]]; then usage; exit 2; fi
  desktop_user=${positional[0]}

  recorded_user=$(receipt_get desktop_user)
  recorded_group_added=$(receipt_get input_group_added)
  if [[ -n ${recorded_user} && ${recorded_user} != "${desktop_user}" ]]; then
    echo "This machine's integration was installed for ${recorded_user}, not ${desktop_user}." >&2
    exit 3
  fi
  if [[ ! -f ${receipt} ]]; then
    echo "No integration receipt found at ${receipt}; removing the known files anyway."
  fi

  echo "Removing SolarBeam system integration:"
  if [[ -e ${rule_dest} ]]; then
    rm -f -- "${rule_dest}"
    echo "  removed ${rule_dest}"
  else
    echo "  (no udev rule installed)"
  fi
  if [[ -e ${modules_dest} ]]; then
    rm -f -- "${modules_dest}"
    echo "  removed ${modules_dest} (uinput/uhid stay loaded until the next reboot)"
  fi
  reload_udev
  [[ ${udev_reload_ok} -eq 0 ]] || echo "  warning: udev did not reload; the old rule's permissions last until reboot" >&2

  if id "${desktop_user}" >/dev/null 2>&1 && user_in_input_group "${desktop_user}"; then
    if [[ ${recorded_group_added} == 1 || ${remove_input_group} -eq 1 ]]; then
      gpasswd -d "${desktop_user}" input >/dev/null
      echo "  removed ${desktop_user} from the input group (takes effect at next sign-in)"
    else
      # Not ours to take away: either it predates us, or an older copy of this
      # script (which did not keep a receipt) added it and we cannot tell which.
      echo "  kept ${desktop_user} in the input group — this script has no record of adding it."
      echo "  If SolarBeam was the only reason for it:  sudo gpasswd -d ${desktop_user} input"
    fi
  fi

  rm -rf -- "${state_dir}"
  echo "  removed ${state_dir}"
  echo
  echo "SolarBeam system integration removed."
  exit 0
fi

# ---------------------------------------------------------------------------
# Install / dry run: validate
# ---------------------------------------------------------------------------
if [[ ${#positional[@]} -ne 2 ]]; then usage; exit 2; fi
install_root=${positional[0]}
desktop_user=${positional[1]}

engine_path=${install_root%/}/sunshine
rules_path=${install_root%/}/assets/udev/rules.d/60-sunshine.rules
if [[ ! -x ${engine_path} ]]; then
  echo "SolarBeam executable not found at ${engine_path}" >&2
  exit 3
fi
if [[ ! -f ${rules_path} ]]; then
  echo "SolarBeam udev rules not found at ${rules_path}" >&2
  exit 3
fi
if ! id "${desktop_user}" >/dev/null 2>&1; then
  echo "Unknown desktop user: ${desktop_user}" >&2
  exit 3
fi
# The rule is copied into /etc and runs as root inside udev, so check it is the
# rule we expect in shape before trusting it: only device-permission
# assignments, nothing that runs a program or loads a module.
if grep -Eq '(RUN|PROGRAM|IMPORT)[[:space:]]*(\{[^}]*\})?[[:space:]]*[+:]?=' "${rules_path}"; then
  echo "Refusing ${rules_path}: it runs programs; a SolarBeam rule only sets permissions." >&2
  exit 3
fi

if [[ ${mode} == dry-run ]]; then
  echo "Dry run — nothing will be changed. An install would:"
  if same_content "${rules_path}" "${rule_dest}"; then
    echo "  keep     ${rule_dest} (already identical)"
  else
    echo "  write    ${rule_dest} (sha256 $(sha256_of "${rules_path}"))"
  fi
  echo "  write    ${modules_dest} (load uinput and uhid at boot)"
  echo "  write    ${receipt} and ${script_copy}"
  caps=$(getcap "$(readlink -f "${engine_path}")" 2>/dev/null || true)
  if [[ -n ${caps} ]]; then
    echo "  strip    file capabilities from the engine (${caps##* })"
  fi
  echo "  load     uinput and uhid kernel modules for this boot"
  if user_in_input_group "${desktop_user}"; then
    echo "  note     ${desktop_user} is already in the input group; it will not be changed"
  elif [[ ${input_group_policy} == always ]]; then
    echo "  add      ${desktop_user} to the input group (--input-group)"
  elif user_can_write "${desktop_user}" "${uinput}"; then
    echo "  skip     the input group: ${desktop_user} can already open ${uinput}"
  else
    echo "  decide   the input group after reloading udev: added only if the"
    echo "           seat ACL still does not let ${desktop_user} open ${uinput}"
  fi
  exit 0
fi

# ---------------------------------------------------------------------------
# Install
# ---------------------------------------------------------------------------
recorded_user=$(receipt_get desktop_user)
if [[ -n ${recorded_user} && ${recorded_user} != "${desktop_user}" ]]; then
  echo "This machine's integration was installed for ${recorded_user}, not ${desktop_user}. Uninstall it for that user first." >&2
  exit 3
fi
previous_group_added=$(receipt_get input_group_added)
group_added=${previous_group_added:-0}

install -Dm0644 "${rules_path}" "${rule_dest}"
modules_tmp=$(mktemp)
cat >"${modules_tmp}" <<'MODULES'
# Written by ReadyFleet's SolarBeam BYOD integration step: the virtual
# keyboard/mouse/controller devices (uinput) and PlayStation-style controllers
# (uhid). Removed by its --uninstall.
uinput
uhid
MODULES
install -Dm0644 "${modules_tmp}" "${modules_dest}"
rm -f -- "${modules_tmp}"
# NO setcap here. This script used to run
#
#     setcap cap_sys_admin,cap_sys_nice+p "$(readlink -f "${engine_path}")"
#
# and that line was, on its own, why SolarBeam published audio and no video on
# every Linux BYOD machine.
#
# Gaining privilege through file capabilities on exec clears the kernel's
# dumpable flag, which makes /proc/<pid>/ root-owned. Both compositor capture
# paths identify their caller by opening /proc/<pid>/root, and both run as the
# user, so both are refused:
#
#     GDBus.Error:org.freedesktop.DBus.Error.AccessDenied:
#     Portal operation not allowed: Unable to open /proc/266324/root
#
# The engine drops the capabilities itself before it asks ("[misc]
# drop_elevated_privileges succeeded in dropping capabilities") and it does not
# help — dropping capabilities does not restore dumpability. cap_sys_admin on
# the engine and compositor screencast are mutually exclusive by construction.
#
# The capabilities only ever existed for kmsgrab, which is the fallback for
# compositors that offer no screencast interface. On the platform this bundle
# targets they are not merely unnecessary, they are the blocker: on the
# reference CachyOS/KDE/Wayland/NVIDIA box, the capless engine takes KWin
# ScreenCast and reaches h264_nvenc/hevc_nvenc/av1_nvenc over DMA-BUF at
# 2560x1440, while the same engine WITH capabilities finds no encoder at all
# and kmsgrab enumerates zero monitors.
#
# If a machine ever genuinely needs kmsgrab — a compositor with no screencast
# portal — grant the capabilities on that machine by hand and accept that its
# screencast paths are then unavailable. Do not put it back here.
#
# Actively strip them instead, because the machines that need it most are the
# ones an older copy of this script already broke, and re-running the setup is
# what somebody does when capture does not work. `|| true` because setcap -r
# fails when there was nothing to remove, which is the normal case.
setcap -r "$(readlink -f "${engine_path}")" 2>/dev/null || true
modprobe uinput || true
modprobe uhid || true
# Recorded rather than fatal. The reload matters — without it the rule just
# installed does not apply — but aborting here would skip the verification block
# below, which is the one moment the script can tell somebody standing at the
# machine exactly what is wrong. So the outcome is carried into that report
# instead of ending the run at the first problem.
reload_udev

# Input access: the seat ACL first, the input group only when that is not
# enough. See the header for why the group is the last resort.
access_via=
signout_needed=0
if user_in_input_group "${desktop_user}"; then
  access_via="input group (already a member)"
elif [[ ${input_group_policy} != always ]] && user_can_write "${desktop_user}" "${uinput}"; then
  access_via="seat ACL (uaccess)"
elif [[ ${input_group_policy} == never ]]; then
  access_via="none (--no-input-group)"
else
  usermod -aG input "${desktop_user}"
  group_added=1
  signout_needed=1
  access_via="input group (added by this step)"
fi

install -d -m0755 "${state_dir}"
install -m0755 "$(readlink -f "${BASH_SOURCE[0]}")" "${script_copy}.new"
mv -f -- "${script_copy}.new" "${script_copy}"
receipt_tmp=$(mktemp "${state_dir}/.receipt-XXXXXX")
cat >"${receipt_tmp}" <<EOF
# Written by ReadyFleet's SolarBeam BYOD integration step. Every system-level
# change it made is listed here; undo them all with:
#   sudo ${script_copy} --uninstall ${desktop_user}
format=1
installed_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
desktop_user=${desktop_user}
udev_rule=${rule_dest}
udev_rule_sha256=$(sha256_of "${rule_dest}")
modules_load=${modules_dest}
input_group_added=${group_added}
EOF
chmod 0644 "${receipt_tmp}"
mv -f -- "${receipt_tmp}" "${receipt}"

# ---------------------------------------------------------------------------
# Verify. Every step above can succeed and still leave a host that cannot host
# a session — a rule that does not match, a kernel with no uinput, a desktop
# user who still cannot open it. None of that surfaces until a mentor starts a
# session and the remote pointer does not move, which is the worst possible
# moment and the hardest to diagnose down a phone line. Check it here, while
# somebody is still sitting in front of the machine.
# ---------------------------------------------------------------------------
failed=0

# `check <description> <command...>` — runs the command in an `if`, which is
# what keeps `set -e` from aborting the run on the FIRST failed check. Reporting
# every failure in one pass is the whole point: telling a mentor one thing is
# wrong, then another after they fix it, is three phone calls instead of one.
check() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  ok      %s\n' "${description}"
  else
    printf '  FAILED  %s\n' "${description}" >&2
    failed=1
  fi
}

# The engine must NOT carry file capabilities — see the long note above the
# removed setcap. Worth checking rather than assuming, because they can arrive
# from outside this script: an older copy of it, or someone following a
# Sunshine guide.
engine_has_no_caps() {
  local caps
  caps=$(getcap "$(readlink -f "${engine_path}")" 2>/dev/null) || return 0
  [[ -z ${caps} ]]
}

# Advisory, never fails the run: keyboard, mouse and Xbox-style controllers
# need only /dev/uinput. Without /dev/uhid, PlayStation-style controllers do not
# work, and the viewer is told so by the engine; worth saying here too.
warned=0
advise() {
  local description=$1
  shift
  if "$@" >/dev/null 2>&1; then
    printf '  ok      %s\n' "${description}"
  else
    printf '  WARN    %s\n' "${description}" >&2
    warned=1
  fi
}

echo
echo "Verifying SolarBeam integration:"

check "udev rule installed" same_content "${rules_path}" "${rule_dest}"
check "udev rules reloaded" test "${udev_reload_ok}" -eq 0
check "engine carries no file capabilities" engine_has_no_caps
check "${uinput} exists" test -e "${uinput}"
check "${desktop_user} can open ${uinput} — via ${access_via}" user_can_write "${desktop_user}" "${uinput}"
check "uinput and uhid load at boot (${modules_dest})" test -s "${modules_dest}"
check "receipt written to ${receipt}" test -s "${receipt}"
advise "${desktop_user} can open ${uhid} (PlayStation-style controllers)" user_can_write "${desktop_user}" "${uhid}"

echo
if [[ ${failed} -ne 0 ]]; then
  echo "SolarBeam integration is INCOMPLETE — the checks above that failed must be" >&2
  echo "resolved before this machine can host a session." >&2
  if [[ ${input_group_policy} == never ]]; then
    echo "(--no-input-group: ${desktop_user} gets ${uinput} only while signed in at this" >&2
    echo " machine's own screen. Re-run from a desktop session, or without that option.)" >&2
  fi
  exit 5
fi

if [[ ${warned} -ne 0 ]]; then
  echo "Keyboard, mouse and Xbox-style controllers will work. PlayStation-style" >&2
  echo "controllers need ${uhid}: check the uhid kernel module is available." >&2
fi
echo "SolarBeam input/capture integration installed and verified."
echo "Undo it at any time with:  sudo ${script_copy} --uninstall ${desktop_user}"
if [[ ${signout_needed} -eq 1 ]]; then
  echo "Sign out and back in so input-group membership takes effect."
fi
