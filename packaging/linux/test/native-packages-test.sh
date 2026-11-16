#!/usr/bin/env bash
# Offline format/verification smoke: no package install, enrolment or network.
set -euo pipefail
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
payload="$work/readyfleet-solarbeam-cachyos-x86_64"
mkdir -p "$payload/engine" "$payload/integration"
# A static, dependency-free ELF makes the format fixture portable across the
# build host's glibc, rather than copying its possibly newer /bin/true.
printf '.global _start\n_start:\n mov $60, %%rax\n xor %%rdi, %%rdi\n syscall\n' > "$work/stub.s"
cc -nostdlib -static -o "$work/stub" "$work/stub.s"
for file in readyfleet-light readyfleet-light-installer engine/sunshine engine/solarbeam-bridge; do
  cp "$work/stub" "$payload/$file"
done
for file in setup-ubuntu.sh setup-cachyos.sh integration/install-system-integration.sh; do
  printf '#!/bin/sh\nexit 0\n' > "$payload/$file"
  chmod +x "$payload/$file"
done
printf '1.2.3\n' > "$payload/engine/VERSION"
tar --zstd -C "$work" -cf "$work/bundle.tar.zst" "$(basename "$payload")"
openssl genpkey -algorithm ED25519 -out "$work/test-key.pem" >/dev/null 2>&1
openssl pkey -in "$work/test-key.pem" -pubout -out "$work/public.pem"
fingerprint=$(openssl pkey -pubin -in "$work/public.pem" -outform DER | tail -c32 | sha256sum | cut -d' ' -f1)
openssl pkeyutl -sign -rawin -inkey "$work/test-key.pem" -in "$work/bundle.tar.zst" -out "$work/bundle.tar.zst.sig"
cp "$work/bundle.tar.zst.sig" "$work/ed25519.sig"
build=("$repo/packaging/linux/build-native-packages.sh" "$work/bundle.tar.zst" "$work/public.pem" "$fingerprint" 1.2.3)
"${build[@]}" "$work/out"
"${build[@]}" "$work/arch-only" arch
[[ -f "$work/arch-only/readyfleet-light-1.2.3-arch/PKGBUILD" ]]
[[ ! -f "$work/arch-only/readyfleet-light_1.2.3_amd64.deb" ]]
(cd "$work/out" && sha256sum -c readyfleet-light_1.2.3_amd64.deb.sha256)
mkdir "$work/deb"
(cd "$work/deb" && ar x "$work/out/readyfleet-light_1.2.3_amd64.deb")
[[ $(cat "$work/deb/debian-binary") == 2.0 ]]
tar -xzf "$work/deb/control.tar.gz" -C "$work/deb"
grep -q '^Architecture: amd64$' "$work/deb/control"
[[ ! -e "$work/deb/postinst" && ! -e "$work/deb/prerm" ]]
mkdir "$work/root"
tar -xzf "$work/deb/data.tar.gz" -C "$work/root"
[[ -x "$work/root/usr/bin/readyfleet-light-enrol" ]]
[[ -x "$work/root/usr/lib/readyfleet-light/readyfleet-light" ]]
arch="$work/out/readyfleet-light-1.2.3-arch"
(
  cd "$arch"
  source ./PKGBUILD
  [[ $pkgver == 1.2.3 && ${arch[0]} == x86_64 ]]
  for i in 0 1; do echo "${sha256sums[$i]}  ${source[$i]}" | sha256sum -c -; done
  mkdir src pkg
  tar --zstd -xf bundle.tar.zst -C src
  cp readyfleet-light-enrol src/
  srcdir="$PWD/src" pkgdir="$PWD/pkg" package
  [[ -x pkg/usr/bin/readyfleet-light-enrol ]]
  [[ -x pkg/usr/lib/readyfleet-light/engine/sunshine ]]
)
if command -v makepkg >/dev/null 2>&1; then
  (cd "$arch" && makepkg --nodeps --noconfirm --force >/dev/null)
  bsdtar -tf "$arch/readyfleet-light-1.2.3-1-x86_64.pkg.tar.zst" | grep -q 'usr/bin/readyfleet-light-enrol'
fi
if "$repo/packaging/linux/build-native-packages.sh" "$work/bundle.tar.zst" "$work/public.pem" "$(printf '%064d' 0)" 1.2.3 "$work/bad-pin" >/dev/null 2>&1; then
  echo 'accepted wrong signer fingerprint' >&2; exit 1
fi
# An alternative algorithm must fail before interpreting a tail-of-DER pin.
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$work/rsa.pem" >/dev/null 2>&1
openssl pkey -in "$work/rsa.pem" -pubout -out "$work/rsa-public.pem"
rsa_pin=$(openssl pkey -pubin -in "$work/rsa-public.pem" -outform DER | tail -c32 | sha256sum | cut -d' ' -f1)
openssl pkeyutl -sign -rawin -inkey "$work/rsa.pem" -in "$work/bundle.tar.zst" -out "$work/bundle.tar.zst.sig"
if "$repo/packaging/linux/build-native-packages.sh" "$work/bundle.tar.zst" "$work/rsa-public.pem" "$rsa_pin" 1.2.3 "$work/rsa-output" arch > "$work/rsa-log" 2>&1; then
  echo 'accepted a non-Ed25519 release key' >&2; exit 1
fi
grep -q 'must be an Ed25519 SPKI key' "$work/rsa-log"
cp "$work/ed25519.sig" "$work/bundle.tar.zst.sig"
cp "$work/bundle.tar.zst" "$work/bad&name.tar.zst"
if "$repo/packaging/linux/build-native-packages.sh" "$work/bad&name.tar.zst" "$work/public.pem" "$fingerprint" 1.2.3 "$work/bad-name" arch > "$work/name-log" 2>&1; then
  echo 'accepted unsafe PKGBUILD source filename' >&2; exit 1
fi
grep -q 'invalid bundle basename' "$work/name-log"
# A future-ABI ELF in the library payload must prevent Ubuntu packaging.
printf 'int future(void) { return 0; }\n' > "$work/future.c"
printf 'GLIBC_99.0 { global: future; };\n' > "$work/future.map"
cc -shared -nostdlib -Wl,--version-script="$work/future.map" -o "$payload/engine/libfuture.so" "$work/future.c"
if python3 "$repo/packaging/linux/check-ubuntu-abi.py" "$payload" >/dev/null 2>&1; then
  echo 'accepted an unsupported Ubuntu GLIBC baseline' >&2; exit 1
fi
printf 'tamper' >> "$work/bundle.tar.zst"
if "${build[@]}" "$work/bad" >/dev/null 2>&1; then
  echo 'accepted a tampered bundle' >&2; exit 1
fi
if [[ -n ${PA_TEST_ARTIFACTS:-} ]]; then
  mkdir -p "$PA_TEST_ARTIFACTS"
  cp "$work/out/readyfleet-light_1.2.3_amd64.deb" "$PA_TEST_ARTIFACTS/"
  if [[ -f "$arch/readyfleet-light-1.2.3-1-x86_64.pkg.tar.zst" ]]; then
    cp "$arch/readyfleet-light-1.2.3-1-x86_64.pkg.tar.zst" "$PA_TEST_ARTIFACTS/"
  fi
fi
printf 'native package format, checksum, pin and tamper checks passed\n'
