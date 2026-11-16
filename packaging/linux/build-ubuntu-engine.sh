#!/usr/bin/env bash
# Build an Ubuntu 24.04 ABI baseline without touching the engine checkout.
# No signing key, network service credentials, release upload or enrolment.
set -euo pipefail
if [[ $# != 2 ]]; then
  echo "usage: $0 SOLARBEAM_ENGINE_V2_REPO OUTPUT_DIR" >&2; exit 2
fi
: "${BUILD_VERSION:?set BUILD_VERSION to the intended engine version, e.g. 0.65.0}"
engine=$(realpath "$1")
output=$(realpath -m "$2")
[[ -f "$engine/CMakeLists.txt" && -d "$engine/third-party" ]] || { echo 'engine checkout missing' >&2; exit 2; }
mkdir -p "$output"
git -C "$engine" rev-parse HEAD > "$output/engine-source-commit.txt"
git -C "$engine" ls-files --recurse-submodules -z > "$output/engine-source-files.nul"
packaging=$(cd "$(dirname "$0")" && pwd)
release_date=${PA_ENGINE_RELEASE_DATE:-$(git -C "$engine" show -s --format=%cI HEAD)}
printf '%s\n' "$release_date" > "$output/engine-release-date.txt"
image=readyfleet-ubuntu24-builder:personal-access
docker build --tag "$image" "$packaging/ubuntu"
# Only local build output is writable from the container. A copied source tree
# preserves submodules and prevents CMake/npm from mutating the developer repo.
docker run --rm --cpus="${BUILD_CPUS:-2}" --memory="${BUILD_MEMORY:-6g}" \
  -v "$engine:/source:ro" -v "$output:/output" -v "$packaging:/recipes:ro" \
  -e LOCAL_UID="$(id -u)" -e LOCAL_GID="$(id -g)" -e BUILD_VERSION -e RELEASE_DATE="$release_date" -e BRANCH="$(git -C "$engine" rev-parse --abbrev-ref HEAD)" -e COMMIT="$(cat "$output/engine-source-commit.txt")" "$image" bash -euo pipefail -c '
  trap '\''chown -R "$LOCAL_UID:$LOCAL_GID" /output'\'' EXIT
  mkdir -p /work/engine
  tar -C /source --null --verbatim-files-from -T /output/engine-source-files.nul -cf - | tar -C /work/engine -xf -
  python3 /recipes/ubuntu/fix-opus-link.py /work/engine
  bash /recipes/ubuntu/build-ffmpeg.sh /work/engine
  # GCC ignores -I precedence if the same directory is also a SYSTEM include.
  # A distinct copy keeps the source-pinned SDK ahead of FFmpeg include paths.
  cp -a /work/engine/third-party/nv-codec-headers/include /work/pinned-nv-codec
  printf "#include <ffnvcodec/nvEncodeAPI.h>\n" | g++-14 -x c++ -H -fsyntax-only -I/work/pinned-nv-codec \
    -isystem /output/build/_deps/ffmpeg/include - 2>&1 | grep /work/pinned-nv-codec
  printf "#include <ffnvcodec/nvEncodeAPI.h>\n" | g++-14 -x c++ -E -dM -I/work/pinned-nv-codec \
    -isystem /output/build/_deps/ffmpeg/include - | grep -E "define NVENCAPI_(MAJOR|MINOR)_VERSION"
  cmake -S /work/engine -B /output/build -G Ninja \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/output/stage \
    -DFFMPEG_PREPARED_BINARIES=/output/ffmpeg-ubuntu \
    -DCMAKE_CXX_FLAGS="-I/work/pinned-nv-codec" \
    -DCMAKE_INSTALL_RPATH='\''$ORIGIN/lib'\'' \
    -DSUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF -DSUNSHINE_ENABLE_TRAY=OFF \
    -DSUNSHINE_ENABLE_CUDA=OFF -DBUILD_TESTS=OFF -DBUILD_DOCS=OFF
  cmake --build /output/build --parallel 2
  cmake --install /output/build
  # miniupnpc changes soname across Ubuntu/Arch releases. Carry this one
  # non-stable ABI rather than requiring Ubuntu 24.04 package names forever.
  install -d /output/stage/lib
  miniupnpc=$(ldd /output/stage/bin/sunshine | awk "\$1 ~ /^libminiupnpc[.]so/ { print \$3 }")
  if [[ -n "$miniupnpc" ]]; then cp -L "$miniupnpc" "/output/stage/lib/$(basename "$miniupnpc")"; fi
  '
printf '%s\n' "$BUILD_VERSION" > "$output/engine-build-version.txt"
echo "Ubuntu engine staged at $output/stage; build tree $output/build"
echo 'CUDA was disabled in this lane; NVIDIA capture/encoding requires separate desktop validation.'
