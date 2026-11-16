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
packaging=$(cd "$(dirname "$0")" && pwd)
image=readyfleet-ubuntu24-builder:personal-access
docker build --tag "$image" "$packaging/ubuntu"
# Only local build output is writable from the container. A copied source tree
# preserves submodules and prevents CMake/npm from mutating the developer repo.
docker run --rm --cpus="${BUILD_CPUS:-2}" --memory="${BUILD_MEMORY:-6g}" \
  -v "$engine:/source:ro" -v "$output:/output" \
  -e LOCAL_UID="$(id -u)" -e LOCAL_GID="$(id -g)" -e BUILD_VERSION -e COMMIT="$(cat "$output/engine-source-commit.txt")" "$image" bash -euo pipefail -c '
  trap '\''chown -R "$LOCAL_UID:$LOCAL_GID" /output'\'' EXIT
  mkdir -p /work/engine
  tar -C /source --exclude=.git --exclude="./build*" --exclude=.worktrees --exclude=node_modules -cf - . | tar -C /work/engine -xf -
  cmake -S /work/engine -B /output/build -G Ninja \
    -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/output/stage \
    -DCMAKE_INSTALL_RPATH='\''$ORIGIN/lib'\'' \
    -DSUNSHINE_INSTALL_SYSTEM_INTEGRATION=OFF -DSUNSHINE_ENABLE_TRAY=OFF \
    -DSUNSHINE_ENABLE_CUDA=OFF -DBUILD_TESTS=OFF -DBUILD_DOCS=OFF
  cmake --build /output/build --parallel 2
  cmake --install /output/build
  '
echo "Ubuntu engine staged at $output/stage; build tree $output/build"
echo 'CUDA was disabled in this lane; NVIDIA capture/encoding requires separate desktop validation.'
