#!/usr/bin/env bash
# Resolve a verified payload against the Ubuntu builder's installed shlibs data.
set -euo pipefail
[[ $# == 1 ]] || { echo "usage: $0 EXTRACTED_BUNDLE" >&2; exit 2; }
payload=$(realpath "$1")
command -v dpkg-shlibdeps >/dev/null || { echo 'Ubuntu dpkg-shlibdeps is required.' >&2; exit 3; }
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir "$work/debian"
printf 'Source: readyfleet-light\nSection: net\nPriority: optional\nMaintainer: Player Ready <support@player-ready.co.uk>\nStandards-Version: 4.7.0\n\nPackage: readyfleet-light\nArchitecture: amd64\nDescription: Personal SolarBeam host\n' > "$work/debian/control"
elf_args=()
while IFS= read -r -d '' file; do
  if readelf -h "$file" >/dev/null 2>&1; then elf_args+=("-e$file"); fi
done < <(find "$payload" -type f -print0)
# miniupnpc's versioned ABI is bundled for rolling distributions. Do not require
# the old Ubuntu package name when that exact DSO already travels in the bundle.
exclude=()
[[ ! -f "$payload/engine/lib/libminiupnpc.so.17" ]] || exclude=(-xlibminiupnpc17)
cd "$work"
dpkg-shlibdeps -O --ignore-missing-info -l"$payload/engine/lib" "${exclude[@]}" "${elf_args[@]}" | sed -n 's/^shlibs:Depends=//p'
