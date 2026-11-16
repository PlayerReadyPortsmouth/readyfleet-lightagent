#!/usr/bin/env bash
# Local developer bundle. Production signing is a separate release-operator step.
set -euo pipefail
if [[ $# != 4 ]]; then
  echo "usage: $0 VERSION ENGINE_REPO BRIDGE_REPO OUTPUT_DIR" >&2; exit 2
fi
version=$1
engine=$(realpath "$2")
bridge=$(realpath "$3")
output=$(realpath -m "$4")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'version must be x.y.z' >&2; exit 2; }
repo=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
go_root=$(go env GOROOT)
if [[ ${PA_REUSE_ENGINE_BUILD:-0} == 1 ]]; then
  [[ -x "$output/stage/bin/sunshine" && $(cat "$output/engine-build-version.txt") == "$version" && $(cat "$output/engine-source-commit.txt") == $(git -C "$engine" rev-parse HEAD) ]] || {
    echo 'existing engine build version/source does not match' >&2; exit 3;
  }
else
  BUILD_VERSION="$version" "$repo/packaging/linux/build-ubuntu-engine.sh" "$engine" "$output"
fi
git -C "$repo" rev-parse HEAD > "$output/fleet-source-commit.txt"
git -C "$bridge" rev-parse HEAD > "$output/bridge-source-commit.txt"
git -C "$repo" ls-files -z -- agent proto packaging/linux > "$output/fleet-source-files.nul"
git -C "$bridge" ls-files --recurse-submodules -z > "$output/bridge-source-files.nul"
docker run --rm -i --cpus="${BUILD_CPUS:-2}" --memory="${BUILD_MEMORY:-6g}" \
  -v "$repo:/fleet:ro" -v "$bridge:/bridge:ro" -v "$go_root:/usr/local/go:ro" -v "$output:/output" \
  -e LOCAL_UID="$(id -u)" -e LOCAL_GID="$(id -g)" -e VERSION="$version" \
  readyfleet-ubuntu24-builder:personal-access bash -euo pipefail -s <<'CONTAINER'
  trap 'chown -R "$LOCAL_UID:$LOCAL_GID" /output' EXIT
  export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false CGO_ENABLED=0
  mkdir -p /work/fleet /work/bridge
  tar -C /fleet --null --verbatim-files-from -T /output/fleet-source-files.nul -cf - | tar -C /work/fleet -xf -
  awk "/^go / { print }" /fleet/go.work > /work/fleet/go.work
  printf "\nuse (\n ./agent\n ./proto\n)\n" >> /work/fleet/go.work
  tar -C /bridge --null --verbatim-files-from -T /output/bridge-source-files.nul -cf - | tar -C /work/bridge -xf -
  cd /work/fleet
  public_b64=$(openssl pkey -pubin -in packaging/linux/release-public.pem -outform DER | tail -c 32 | base64 -w0)
  fingerprint=$(openssl pkey -pubin -in packaging/linux/release-public.pem -outform DER | tail -c 32 | sha256sum | cut -d" " -f1)
  go build -trimpath -ldflags "-s -w -X main.agentVersion=$VERSION -X main.lightLinuxPublicKey=$public_b64" -o /output/readyfleet-light ./agent/cmd/lightagent
  agent_module=$(go list -f '{{.Module.Path}}' ./agent/cmd/light-installer)
  agent_sha=$(sha256sum /output/readyfleet-light | cut -d" " -f1)
  go build -trimpath -ldflags "-s -w -X main.serverURL=wss://fleet.player-ready.co.uk:8643 -X ${agent_module}/internal/lightinstall.expectedAgentSHA256=$agent_sha -X ${agent_module}/internal/lightinstall.expectedSignerFingerprint=$fingerprint" -o /output/readyfleet-light-installer ./agent/cmd/light-installer
  (cd /work/bridge && go build -trimpath -ldflags "-s -w" -o /output/solarbeam-bridge .)
  packaging/linux/build-cachyos-bundle.sh --stage /output/stage --engine-build /output/build \
    --bridge /output/solarbeam-bridge --agent /output/readyfleet-light \
    --installer /output/readyfleet-light-installer --version "$VERSION" --output /output
  mkdir -p /work/bundle
  artifact="/output/readyfleet-solarbeam-cachyos-x86_64-$VERSION.tar.zst"
  tar --zstd -xf "$artifact" -C /work/bundle
  payload=/work/bundle/readyfleet-solarbeam-cachyos-x86_64
  python3 packaging/linux/check-ubuntu-abi.py "$payload"
  dependencies=$(packaging/linux/ubuntu/runtime-dependencies.sh "$payload")
  [[ -n "$dependencies" ]] || { echo "could not resolve Ubuntu runtime dependencies" >&2; exit 3; }
  printf "%s\n" "$dependencies" | sed -E "s/\([^)]*\)//g;s/ //g" | tr , "\n" | sort -u > "$payload/ubuntu-runtime-packages"
  tar --zstd -C /work/bundle -cf "$artifact" readyfleet-solarbeam-cachyos-x86_64
  (cd /output && sha256sum "$(basename "$artifact")" > "$(basename "$artifact").sha256")
CONTAINER
echo 'Developer bundle only: no release signature was generated and nothing was published.'
