#!/usr/bin/env bash
# Exercise the setup ordering in a disposable Ubuntu container, with fixture
# executables and sudo intercepted; no packages, enrollment or host integration.
set -euo pipefail
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
docker run --rm -i --network none -v "$repo/packaging/linux:/recipes:ro" \
  readyfleet-ubuntu24-builder:personal-access bash -euo pipefail -s <<'CONTAINER'
mkdir -p /tmp/payload/engine /tmp/payload/integration /tmp/stubs /tmp/desktop
cp /recipes/setup-ubuntu.sh /tmp/payload/setup-ubuntu.sh
for file in engine/sunshine engine/solarbeam-bridge readyfleet-light; do cp /bin/true "/tmp/payload/$file"; done
cat > /tmp/payload/readyfleet-light-installer <<'INSTALLER'
#!/bin/bash
set -euo pipefail
: > "$HOME/installer-ran"
INSTALLER
cat > /tmp/payload/integration/install-system-integration.sh <<'INTEGRATION'
#!/bin/bash
exit 0
INTEGRATION
cp /bin/true /tmp/payload/uninstall-linux.sh
cat > /tmp/stubs/sudo <<'SUDO'
#!/bin/bash
if [[ $1 == apt-get ]]; then exit 0; fi
exec "$@"
SUDO
chmod +x /tmp/stubs/sudo /tmp/payload/readyfleet-light-installer /tmp/payload/integration/install-system-integration.sh
export PATH="/tmp/stubs:$PATH" HOME=/tmp/desktop USER=desktop
printf '{}\n' > /tmp/enrollment.json
bash /tmp/payload/setup-ubuntu.sh /tmp/enrollment.json >/dev/null
[[ -f "$HOME/installer-ran" && -x "$HOME/.local/share/solarbeam/sunshine" ]]
rm -rf /tmp/desktop
mkdir /tmp/desktop
# A failed loader check must never copy user state or consume the enrolment.
printf '#!/bin/bash\necho "libmissing.so => not found"\nexit 0\n' > /tmp/stubs/ldd
chmod +x /tmp/stubs/ldd
if bash /tmp/payload/setup-ubuntu.sh /tmp/enrollment.json >/dev/null 2>&1; then
  echo 'accepted a missing runtime library' >&2; exit 1
fi
[[ ! -e "$HOME/installer-ran" && ! -e "$HOME/.local/share/solarbeam" ]]
printf 'ID=ubuntu\nVERSION_ID="22.04"\nPRETTY_NAME="Ubuntu 22.04"\n' > /etc/os-release
if bash /tmp/payload/setup-ubuntu.sh /tmp/enrollment.json >/dev/null 2>&1; then
  echo 'accepted Ubuntu older than baseline' >&2; exit 1
fi
[[ ! -e "$HOME/installer-ran" ]]
echo 'Ubuntu setup fixture, loader refusal and old-version refusal passed'
CONTAINER
