#!/usr/bin/env bash
# Runs integration-step-test.sh in throwaway Ubuntu 24.04 and Arch Linux
# containers. The privileged step is never run on the host.
#
#   packaging/linux/test/run-container-tests.sh [image ...]
set -euo pipefail
here=$(cd "$(dirname "$0")/.." && pwd)
images=("$@")
(( ${#images[@]} )) || images=(ubuntu:24.04 archlinux:latest)
status=0
for image in "${images[@]}"; do
  echo "=== ${image}"
  # libcap's setcap/getcap are what setup-ubuntu.sh installs and what an Arch
  # base system already has; install them so the capability case runs too.
  prep='true'
  case ${image} in
    ubuntu*|debian*) prep='apt-get update -qq >/dev/null && apt-get install -y -qq libcap2-bin >/dev/null' ;;
    archlinux*) prep='command -v setcap >/dev/null || pacman -Sy --noconfirm --needed libcap >/dev/null' ;;
  esac
  if ! docker run --rm -v "${here}:/src:ro" "${image}" \
      bash -c "${prep}; exec bash /src/test/integration-step-test.sh /src"; then
    status=1
  fi
done
exit "${status}"
