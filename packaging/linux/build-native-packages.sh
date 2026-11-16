#!/usr/bin/env bash
# Wrap the existing signed BYOD bundle; never enrol or start a service as root.
set -euo pipefail
if (( $# < 5 || $# > 6 )); then
  echo "usage: $0 BUNDLE.tar.zst ED25519_PUBLIC.pem EXPECTED_KEY_FINGERPRINT VERSION OUTPUT_DIR [ubuntu|arch|both]" >&2
  exit 2
fi
bundle=$(realpath "$1")
bundle_name=$(basename "$bundle")
[[ $bundle_name =~ ^[A-Za-z0-9][A-Za-z0-9._-]*\.tar\.zst$ ]] || { echo 'invalid bundle basename' >&2; exit 2; }
public_key=$(realpath "$2")
expected_fingerprint=$3
version=$4
output=$5
sha=$(sha256sum "$bundle" | cut -d' ' -f1)
target=${6:-both}
case "$target" in ubuntu|arch|both) ;; *) echo "unknown target: $target" >&2; exit 2 ;; esac
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'version must be x.y.z' >&2; exit 2; }
[[ $expected_fingerprint =~ ^[a-f0-9]{64}$ ]] || { echo 'invalid public key fingerprint' >&2; exit 2; }
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
openssl pkey -pubin -in "$public_key" -outform DER -out "$work/public.der"
[[ $(wc -c < "$work/public.der") == 44 && $(od -An -tx1 -N12 "$work/public.der" | tr -d '[:space:]') == 302a300506032b6570032100 ]] || {
  echo 'release public key must be an Ed25519 SPKI key' >&2; exit 3;
}
# A PEM chosen by an attacker is not a trust anchor; pin it before verification.
[[ $(tail -c 32 "$work/public.der" | sha256sum | cut -d' ' -f1) == "$expected_fingerprint" ]] || { echo 'signer fingerprint mismatch' >&2; exit 3; }
openssl pkeyutl -verify -rawin -pubin -inkey "$public_key" -sigfile "$bundle.sig" -in "$bundle"
# The release bundle intentionally has only regular files and directories.
# Refuse unsafe paths/types before tar can write through one as the builder.
tar --zstd -tf "$bundle" > "$work/paths"
if ! awk '/^\// || /(^|\/)\.\.(\/|$)/ { bad=1 } END { exit bad }' "$work/paths"; then
  echo 'unsafe bundle path' >&2; exit 3
fi
if tar --zstd -tvf "$bundle" | awk '$1 !~ /^[-d]/ { bad=1 } END { exit !bad }'; then
  echo 'bundle contains links or special files' >&2; exit 3
fi
mkdir "$work/extracted"
tar --zstd --no-same-owner -xf "$bundle" -C "$work/extracted"
payload="$work/extracted/readyfleet-solarbeam-cachyos-x86_64"
for file in readyfleet-light readyfleet-light-installer engine/sunshine engine/solarbeam-bridge setup-ubuntu.sh setup-cachyos.sh integration/install-system-integration.sh; do
  [[ -x "$payload/$file" ]] || { echo "incomplete bundle: $file" >&2; exit 3; }
done
[[ $(cat "$payload/engine/VERSION") == "$version" ]] || { echo 'bundle version mismatch' >&2; exit 3; }
if [[ $target != arch ]]; then
python3 "$(dirname "$0")/check-ubuntu-abi.py" "$payload"
runtime_deps=''
# Resolve the ABI's actual runtime packages against Ubuntu's shlibs database.
# A portable shell-only package must not guess libminiupnpc/ICU sonames from
# the Arch build host. Static test fixtures need no shlibs metadata.
if readelf -d "$payload/engine/sunshine" 2>/dev/null | grep -q NEEDED; then
  runtime_deps=$("$(dirname "$0")/ubuntu/runtime-dependencies.sh" "$payload")
  [[ -n $runtime_deps ]] || { echo 'Ubuntu runtime dependencies could not be resolved' >&2; exit 3; }
fi
root="$work/deb"
mkdir -p "$root/usr/lib/readyfleet-light" "$root/usr/bin" "$root/usr/share/doc/readyfleet-light" "$work/control" "$output"
cp -a "$payload/." "$root/usr/lib/readyfleet-light/"
install -m0755 "$(dirname "$0")/readyfleet-light-enrol" "$root/usr/bin/readyfleet-light-enrol"
cp "$bundle.sig" "$root/usr/share/doc/readyfleet-light/bundle.sig"
cp "$public_key" "$root/usr/share/doc/readyfleet-light/release-public.pem"
printf '%s  %s\n' "$sha" "$(basename "$bundle")" > "$root/usr/share/doc/readyfleet-light/bundle.sha256"
cat > "$work/control/control" <<CONTROL
Package: readyfleet-light
Version: $version
Architecture: amd64
Maintainer: Player Ready <support@player-ready.co.uk>
Section: net
Priority: optional
Depends: libc6 (>= 2.39), bash, sudo, systemd, libcap2-bin, kmod, udev, wl-clipboard, xclip${runtime_deps:+, $runtime_deps}
Installed-Size: $(du -sk "$root" | cut -f1)
Homepage: https://github.com/PlayerReadyPortsmouth/readyfleet-lightagent
Description: Personal SolarBeam host setup for the logged-in desktop user
 Installs a verified prebuilt BYOD payload without enrolling a device or
 starting a root service. Run readyfleet-light-enrol as the desktop user.
CONTROL
# Debian's 2.0 ar format needs no maintainer hooks: dpkg never sees a token.
printf '2.0\n' > "$work/debian-binary"
tar --owner=0 --group=0 -C "$work/control" -czf "$work/control.tar.gz" .
tar --owner=0 --group=0 -C "$root" -czf "$work/data.tar.gz" .
(cd "$work" && ar crD readyfleet-light.deb debian-binary control.tar.gz data.tar.gz)
install -m0644 "$work/readyfleet-light.deb" "$output/readyfleet-light_${version}_amd64.deb"
fi
if [[ $target != ubuntu ]]; then
mkdir -p "$output"
# A local-source PKGBUILD avoids inventing a download URL for an unpublished
# release. The pinned source can be copied alongside it on CachyOS or Arch.
arch_dir="$output/readyfleet-light-${version}-arch"
mkdir -p "$arch_dir"
cp "$bundle" "$arch_dir/$(basename "$bundle")"
install -m0755 "$(dirname "$0")/readyfleet-light-enrol" "$arch_dir/readyfleet-light-enrol"
sed -e "s/@VERSION@/$version/g" -e "s/@BUNDLE@/$(basename "$bundle")/g" -e "s/@SHA256@/$sha/g" \
  -e "s/@LAUNCHER_SHA256@/$(sha256sum "$arch_dir/readyfleet-light-enrol" | cut -d' ' -f1)/g" \
  "$(dirname "$0")/arch/PKGBUILD.in" > "$arch_dir/PKGBUILD"
fi
if [[ $target != arch ]]; then
(cd "$output" && sha256sum "readyfleet-light_${version}_amd64.deb" > "readyfleet-light_${version}_amd64.deb.sha256")
fi
echo "Built target $target in $output"
echo 'Native packages are unsigned until the release operator signs/publishes them.'
