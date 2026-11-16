#!/usr/bin/env bash
# Exercises install-system-integration.sh and uninstall-linux.sh for real — as
# root, against the real /etc, /var/lib, /dev and /etc/group — which is only
# safe inside a throwaway container. run-container-tests.sh is the entrypoint;
# this file refuses to run anywhere else.
#
# The only things faked are the ones a container cannot provide: udevadm and
# modprobe are PATH stubs (there is no udevd and no module loading), /dev/uinput
# is a plain file (access(2) is all the script asks of it), and logind's uaccess
# ACL is emulated by the udevadm stub when FAKE_SEAT_USER is set.
set -euo pipefail

if [[ ! -f /.dockerenv && ${SOLARBEAM_TEST_IN_CONTAINER:-} != 1 ]]; then
  echo "refusing to run outside a container: this test rewrites /etc and /dev" >&2
  exit 1
fi
[[ ${EUID} -eq 0 ]] || { echo "run as root inside the container" >&2; exit 1; }

src=${1:?usage: $0 <packaging/linux directory>}
script=${src}/install-system-integration.sh
uninstaller=${src}/uninstall-linux.sh
rule=/etc/udev/rules.d/60-solarbeam-readyfleet.rules
modules=/etc/modules-load.d/solarbeam-readyfleet.conf
state=/var/lib/readyfleet-byod
receipt=${state}/integration.receipt
user=mentor

pass=0
fail=0
ok() { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  FAIL  %s\n' "$1" >&2; fail=$((fail + 1)); }
expect() { local d=$1; shift; if "$@"; then ok "$d"; else bad "$d"; fi; }
in_input() { id -nG "${user}" | tr ' ' '\n' | grep -qx input; }

# --- fixtures ---------------------------------------------------------------
stub_dir=$(mktemp -d)
chmod 0755 "${stub_dir}"   # the mentor runs the sudo stub in case 10
cat >"${stub_dir}/udevadm" <<'EOF'
#!/usr/bin/env bash
# `settle` is where the real uaccess ACL would have been applied.
if [[ $1 == settle && -n ${FAKE_SEAT_USER:-} ]]; then
  chown "${FAKE_SEAT_USER}" /dev/uinput /dev/uhid
fi
exit "${FAKE_UDEV_RC:-0}"
EOF
printf '#!/bin/sh\nexit 0\n' >"${stub_dir}/modprobe"
chmod +x "${stub_dir}"/*
export PATH=${stub_dir}:${PATH}

getent group input >/dev/null || groupadd -r input
id "${user}" >/dev/null 2>&1 || useradd -m "${user}"

engine=$(mktemp -d)/solarbeam
mkdir -p "${engine}/assets/udev/rules.d"
printf '#!/bin/sh\n' >"${engine}/sunshine"
chmod 0755 "${engine}/sunshine"
cat >"${engine}/assets/udev/rules.d/60-sunshine.rules" <<'EOF'
KERNEL=="uinput", SUBSYSTEM=="misc", OPTIONS+="static_node=uinput", GROUP="input", MODE="0660", TAG+="uaccess"
KERNEL=="uhid", GROUP="input", MODE="0660", TAG+="uaccess"
EOF

reset() {
  rm -rf -- "${rule}" "${state}" "${modules}"
  gpasswd -d "${user}" input >/dev/null 2>&1 || true
  for d in /dev/uinput /dev/uhid; do
    rm -f "$d"; : >"$d"; chown root:input "$d"; chmod 0660 "$d"
  done
  unset FAKE_SEAT_USER FAKE_UDEV_RC
}

run() { out=$("$@" 2>&1); rc=$?; }

# --- 1. seat ACL available: no group change --------------------------------
echo "1. desktop session present (uaccess)"
reset
export FAKE_SEAT_USER=${user}
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "rule installed" test "$(sha256sum <"${engine}/assets/udev/rules.d/60-sunshine.rules")" = "$(sha256sum <"${rule}")"
expect "user NOT added to input" bash -c "! id -nG ${user} | tr ' ' '\n' | grep -qx input"
expect "receipt records no group change" grep -qx 'input_group_added=0' "${receipt}"
expect "uinput loads at boot" grep -qx uinput "${modules}"
expect "uhid loads at boot (PlayStation-style pads after a reboot)" grep -qx uhid "${modules}"
expect "receipt records the modules-load entry" grep -qx "modules_load=${modules}" "${receipt}"
expect "uhid access reported ok" grep -q "ok      ${user} can open /dev/uhid" <<<"${out}"
expect "receipt records the rule hash" grep -q "^udev_rule_sha256=$(sha256sum "${rule}" | cut -d' ' -f1)\$" "${receipt}"
expect "reports access via the seat ACL" grep -q 'via seat ACL' <<<"${out}"
# shellcheck disable=SC2016 # expanded by the inner shell
expect "no sign-out needed" bash -c '! grep -q "Sign out" <<<"$1"' _ "${out}"
expect "root-owned uninstaller copy" test "$(stat -c %U:%a "${state}/install-system-integration.sh")" = root:755

# --- 2. no seat: input-group fallback ---------------------------------------
echo "2. no desktop session (SSH or no logind) — input group fallback"
reset
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "user added to input" in_input
expect "receipt records the group change" grep -qx 'input_group_added=1' "${receipt}"
expect "asks for sign-out" grep -q 'Sign out' <<<"${out}"

echo "3. re-running keeps the record that WE added the group"
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "still input_group_added=1" grep -qx 'input_group_added=1' "${receipt}"

receipt_before=$(sha256sum "${receipt}")
set +e; run "${script}" "${engine}" root; set -e
expect "install for a different user is refused (exit 3)" test "${rc}" -eq 3
expect "original receipt is preserved" test "$(sha256sum "${receipt}")" = "${receipt_before}"
expect "original user's group membership is preserved" in_input

echo "4. uninstall from the recorded root-owned copy reverses everything"
set +e; run "${state}/install-system-integration.sh" --uninstall "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "rule removed" test ! -e "${rule}"
expect "modules-load entry removed" test ! -e "${modules}"
expect "user removed from input" bash -c "! id -nG ${user} | tr ' ' '\n' | grep -qx input"
expect "state dir removed" test ! -e "${state}"

# --- 5. pre-existing input membership is never ours to remove ---------------
echo "5. user already in input before install"
reset
usermod -aG input "${user}"
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "receipt says the group was not ours" grep -qx 'input_group_added=0' "${receipt}"
set +e; run "${script}" --uninstall "${user}"; set -e
expect "uninstall keeps the membership" in_input
expect "and says how to drop it" grep -q "gpasswd -d ${user} input" <<<"${out}"

# --- 6. legacy install (pre-receipt script) ---------------------------------
echo "6. legacy install with no receipt"
reset
usermod -aG input "${user}"
install -Dm0644 "${engine}/assets/udev/rules.d/60-sunshine.rules" "${rule}"
set +e; run "${script}" --uninstall "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "rule removed" test ! -e "${rule}"
expect "membership kept without --remove-input-group" in_input
set +e; run "${script}" --uninstall "${user}" --remove-input-group; set -e
expect "--remove-input-group removes it" bash -c "! id -nG ${user} | tr ' ' '\n' | grep -qx input"

# --- 7. capabilities are stripped -------------------------------------------
if command -v setcap >/dev/null && setcap cap_sys_nice+p "${engine}/sunshine" 2>/dev/null; then
  echo "7. engine with file capabilities from an older script"
  reset; export FAKE_SEAT_USER=${user}
  set +e; run "${script}" "${engine}" "${user}"; set -e
  expect "exit 0" test "${rc}" -eq 0
  expect "capabilities stripped" test -z "$(getcap "${engine}/sunshine")"
else
  echo "7. skipped: this filesystem cannot hold file capabilities"
fi

# --- 8. refusal and failure modes -------------------------------------------
echo "8. refusals"
reset
bad_engine=$(mktemp -d)/solarbeam
cp -a "${engine}" "${bad_engine}"
echo 'KERNEL=="uinput", RUN+="/bin/sh -c id"' >>"${bad_engine}/assets/udev/rules.d/60-sunshine.rules"
set +e; run "${script}" "${bad_engine}" "${user}"; set -e
expect "a rule that runs programs is refused (exit 3)" test "${rc}" -eq 3
expect "and nothing is installed" test ! -e "${rule}"

set +e; run "${script}" "${engine}" "${user}" --no-input-group; set -e
expect "--no-input-group with no seat ACL fails verification (exit 5)" test "${rc}" -eq 5
expect "and does not touch the group" bash -c "! id -nG ${user} | tr ' ' '\n' | grep -qx input"

reset; export FAKE_SEAT_USER=${user}
"${script}" "${engine}" "${user}" >/dev/null
set +e; run "${script}" --uninstall root; set -e
expect "uninstall for a different user than recorded is refused (exit 3)" test "${rc}" -eq 3
expect "and leaves the install alone" test -e "${rule}"

reset; export FAKE_UDEV_RC=1 FAKE_SEAT_USER=${user}
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "a failed udev reload is reported (exit 5)" test "${rc}" -eq 5
expect "in the verification list" grep -q 'FAILED  udev rules reloaded' <<<"${out}"

echo "8b. no /dev/uhid: warns, still installs (keyboard, mouse, Xbox pads work)"
reset; export FAKE_SEAT_USER=${user}
rm -f /dev/uhid
set +e; run "${script}" "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "uhid problem is a WARN, not a FAILED" grep -q "WARN    ${user} can open /dev/uhid" <<<"${out}"
expect "and names the PlayStation-style pads" grep -q "PlayStation-style" <<<"${out}"

# --- 9. dry run needs no root and changes nothing ---------------------------
echo "9. dry run as the unprivileged desktop user"
reset
chmod -R a+rX "${engine%/*}"
set +e; run runuser -u "${user}" -- "${script}" --dry-run "${engine}" "${user}"; set -e
expect "exit 0" test "${rc}" -eq 0
expect "nothing written" test ! -e "${rule}" -a ! -e "${state}" -a ! -e "${modules}"
expect "describes the modules-load entry" grep -q "write    ${modules}" <<<"${out}"
expect "describes the rule it would write" grep -q "write    ${rule}" <<<"${out}"

# --- 10. the user-level uninstaller -----------------------------------------
echo "10. uninstall-linux.sh as the desktop user"
reset
home=$(getent passwd "${user}" | cut -d: -f6)
# shellcheck disable=SC2016 # expanded by the mentor's shell
runuser -u "${user}" -- bash -c '
  set -e
  mkdir -p ~/.config/readyfleet-light ~/.local/share/solarbeam ~/.local/share/applications ~/.config/sunshine
  # A fake agent whose `uninstall` does what the real one does to its own files.
  printf "#!/bin/sh\n[ \"\$1\" = uninstall ] && rm -rf \"\$(dirname \"\$0\")\" && echo uninstalled\n" >~/.config/readyfleet-light/readyfleet-light
  chmod +x ~/.config/readyfleet-light/readyfleet-light
  touch ~/.local/share/solarbeam/sunshine ~/.local/share/applications/dev.lizardbyte.app.Sunshine.kwin-readyfleet.desktop ~/.config/sunshine/sunshine.conf
'
export FAKE_SEAT_USER=${user}
"${script}" "${engine}" "${user}" >/dev/null
# sudo is a stub that only records its arguments: runuser has dropped us to the
# mentor and a script cannot really escalate. The recorded command is then run
# as root below, which is what the real sudo would have done.
sudo_log=$(mktemp); chmod 0666 "${sudo_log}"
printf '#!/bin/sh\necho "$*" >>%s\n' "${sudo_log}" >"${stub_dir}/sudo"; chmod +x "${stub_dir}/sudo"
set +e; run runuser -u "${user}" -- env PATH="${PATH}" HOME="${home}" "${uninstaller}" --yes; set -e
expect "sudo invoked the recorded system uninstaller" grep -qx "${state}/install-system-integration.sh --uninstall ${user}" "${sudo_log}"
expect "agent directory removed" test ! -e "${home}/.config/readyfleet-light"
expect "engine removed" test ! -e "${home}/.local/share/solarbeam"
expect "KWin permission entry removed" test ! -e "${home}/.local/share/applications/dev.lizardbyte.app.Sunshine.kwin-readyfleet.desktop"
expect ".config/sunshine kept by default" test -e "${home}/.config/sunshine/sunshine.conf"
expect "uninstall-linux.sh exit 0" test "${rc}" -eq 0
read -r -a recorded <"${sudo_log}"
"${recorded[@]}" >/dev/null
expect "system integration removed" test ! -e "${rule}" -a ! -e "${state}" -a ! -e "${modules}"

echo
echo "${pass} passed, ${fail} failed"
[[ ${fail} -eq 0 ]]
