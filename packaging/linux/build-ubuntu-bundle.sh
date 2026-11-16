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
BUILD_VERSION="$version" "$repo/packaging/linux/build-ubuntu-engine.sh" "$engine" "$output"
docker run --rm --cpus="${BUILD_CPUS:-2}" --memory="${BUILD_MEMORY:-6g}" \
  -v "$repo:/fleet:ro" -v "$bridge:/bridge:ro" -v "$go_root:/usr/local/go:ro" -v "$output:/output" \
  -e LOCAL_UID="$(id -u)" -e LOCAL_GID="$(id -g)" -e VERSION="$version" \
  readyfleet-ubuntu24-builder:personal-access bash -euo pipefail -c '
  trap '\''chown -R "$LOCAL_UID:$LOCAL_GID" /output'\'' EXIT
  export PATH=/usr/local/go/bin:$PATH GOFLAGS=-buildvcs=false CGO_ENABLED=0
  mkdir -p /work/fleet /work/bridge
  cp -a /fleet/agent /fleet/proto /fleet/tools /fleet/go.work /fleet/packaging /work/fleet/
  tar -C /bridge --exclude=.git --exclude="./build*" -cf - . | tar -C /work/bridge -xf -
  cd /work/fleet
  public_b64=$(openssl pkey -pubin -in packaging/linux/release-public.pem -outform DER | tail -c 32 | base64 -w0)
  fingerprint=$(openssl pkey -pubin -in packaging/linux/release-public.pem -outform DER | tail -c 32 | sha256sum | cut -d" " -f1)
  go build -trimpath -ldflags "-s -w -X main.agentVersion=$VERSION -X main.lightLinuxPublicKey=$public_b64" -o /output/readyfleet-light ./agent/cmd/lightagent
  agent_sha=$(sha256sum /output/readyfleet-light | cut -d" " -f1)
  go build -trimpath -ldflags "-s -w -X main.serverURL=wss://fleet.player-ready.co.uk:8643 -X github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall.expectedAgentSHA256=$agent_sha -X github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall.expectedSignerFingerprint=$fingerprint" -o /output/readyfleet-light-installer ./agent/cmd/light-installer
  (cd /work/bridge && go build -trimpath -ldflags "-s -w" -o /output/solarbeam-bridge .)
  packaging/linux/build-cachyos-bundle.sh --stage /output/stage --engine-build /output/build \
    --bridge /output/solarbeam-bridge --agent /output/readyfleet-light \
    --installer /output/readyfleet-light-installer --version "$VERSION" --output /output
  '
echo 'Developer bundle only: no release signature was generated and nothing was published.'
