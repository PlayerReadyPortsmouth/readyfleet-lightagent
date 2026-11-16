#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --stage DIR --engine-build DIR --bridge FILE --agent FILE [--installer FILE] --version VERSION --output DIR [--signing-key ED25519_PEM]" >&2
}

stage=''
engine_build=''
bridge=''
agent=''
version=''
output=''
signing_key=''
installer=''
while (($#)); do
  case "$1" in
    --stage) stage=$2; shift 2 ;;
    --engine-build) engine_build=$2; shift 2 ;;
    --bridge) bridge=$2; shift 2 ;;
    --agent) agent=$2; shift 2 ;;
    --installer) installer=$2; shift 2 ;;
    --version) version=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    --signing-key) signing_key=$2; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done
if [[ -z ${stage} || -z ${engine_build} || -z ${bridge} || -z ${agent} || -z ${version} || -z ${output} ]]; then
  usage
  exit 2
fi
for path in "${stage}" "${engine_build}" "${bridge}" "${agent}"; do
  [[ -e ${path} ]] || { echo "missing input: ${path}" >&2; exit 3; }
done

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
bundle=${work}/readyfleet-solarbeam-cachyos-x86_64
install -d "${bundle}/engine" "${bundle}/integration"
engine_source=${stage%/}/bin/sunshine
[[ -x ${engine_source} ]] || { echo "missing staged engine: ${engine_source}" >&2; exit 3; }
install -Dm0755 "${engine_source}" "${bundle}/engine/sunshine"

# The engine resolves its assets (web UI, shaders, tray icons, the staged udev
# rule) relative to its own executable, so the assets tree must travel with it.
# Stage the engine with -DSUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF so the udev
# rule lands here rather than in /usr/lib.
[[ -d ${stage%/}/assets ]] || { echo "staged engine has no assets tree: ${stage%/}/assets" >&2; exit 3; }
[[ -f ${stage%/}/assets/udev/rules.d/60-sunshine.rules ]] || {
  echo "staged assets are missing the udev rule; rebuild with -DSUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF" >&2
  exit 3
}
cp -a "${stage%/}/assets" "${bundle}/engine/assets"
if [[ -d ${stage%/}/share ]]; then cp -a "${stage%/}/share" "${bundle}/engine/"; fi
if [[ -d ${stage%/}/lib ]]; then
  # -L, not -a: the agent's engine updater (extractLinuxEngine) rejects any
  # archive entry that is not a regular file or a directory, so a development
  # symlink such as libdatachannel.so would fail every future update while the
  # first install, which is unpacked by tar(1), succeeded.
  cp -aL "${stage%/}/lib" "${bundle}/engine/"
  # Development files for linking against the staged libraries; a mentor
  # machine only ever loads them.
  rm -rf -- "${bundle}/engine/lib/cmake" "${bundle}/engine/lib/pkgconfig"
fi

# The engine links shared libraries that are built inside its own build tree
# (Boost, libdatachannel, glad, libdisplaydevice, inputtino) and that no install
# step copies out. Without them the engine cannot start on a machine that has no
# checkout. Resolve them against the build tree and carry them in engine/lib,
# which the executable's $ORIGIN/lib runpath searches. Stage the engine with
# -DCMAKE_INSTALL_RPATH='$ORIGIN/lib' so that runpath survives installation.
install -d "${bundle}/engine/lib"
engine_build_abs=$(cd "${engine_build}" && pwd)
build_lib_path=$(find "${engine_build_abs}" -name '*.so*' -type f -printf '%h\n' | sort -u | paste -sd:)
while read -r resolved; do
  case ${resolved} in
    "${engine_build_abs}"/*) install -Dm0755 "${resolved}" "${bundle}/engine/lib/$(basename "${resolved}")" ;;
  esac
done < <(LD_LIBRARY_PATH=${build_lib_path} ldd "${bundle}/engine/sunshine" | awk '$3 ~ /^\// {print $3}')

if LD_LIBRARY_PATH=${bundle}/engine/lib ldd "${bundle}/engine/sunshine" | grep -q 'not found'; then
  echo "bundled engine still has unresolved shared libraries:" >&2
  LD_LIBRARY_PATH=${bundle}/engine/lib ldd "${bundle}/engine/sunshine" | grep 'not found' >&2
  exit 4
fi
install -Dm0755 "${bridge}" "${bundle}/engine/solarbeam-bridge"
install -Dm0755 "${agent}" "${bundle}/readyfleet-light"
if [[ -n ${installer} ]]; then
  [[ -x ${installer} ]] || { echo "installer is not executable: ${installer}" >&2; exit 3; }
  install -Dm0755 "${installer}" "${bundle}/readyfleet-light-installer"
fi
install -Dm0755 "$(dirname "$0")/install-system-integration.sh" "${bundle}/integration/install-system-integration.sh"
install -Dm0755 "$(dirname "$0")/setup-cachyos.sh" "${bundle}/setup-cachyos.sh"
install -Dm0755 "$(dirname "$0")/setup-ubuntu.sh" "${bundle}/setup-ubuntu.sh"
install -Dm0755 "$(dirname "$0")/uninstall-linux.sh" "${bundle}/uninstall-linux.sh"
printf '%s\n' "${version}" > "${bundle}/engine/VERSION"

install -d "${output}"
artifact=${output%/}/readyfleet-solarbeam-cachyos-x86_64-${version}.tar.zst
tar --zstd --hard-dereference -C "${work}" -cf "${artifact}" "$(basename "${bundle}")"
# The agent's engine updater (extractLinuxEngine) accepts only regular files and
# directories, and fails the whole update on anything else. The first install is
# unpacked by tar(1) and would not notice, so check here rather than discover it
# the first time a mentor's machine tries to update.
if tar --zstd -tvf "${artifact}" | awk '$1 !~ /^[-d]/ { print; found = 1 } END { exit !found }'; then
  echo "bundle contains archive entries the agent's engine updater cannot apply (shown above)" >&2
  exit 4
fi

# Record the checksum against the bare filename so `sha256sum -c` works
# wherever the artifact is later copied to, not only on this build machine.
(cd "${output%/}" && sha256sum "$(basename "${artifact}")" > "$(basename "${artifact}").sha256")
if [[ -n ${signing_key} ]]; then
  openssl pkeyutl -sign -rawin -inkey "${signing_key}" -in "${artifact}" -out "${artifact}.sig"
  base64 -w0 "${artifact}.sig" > "${artifact}.sig.b64"
fi
echo "${artifact}"
