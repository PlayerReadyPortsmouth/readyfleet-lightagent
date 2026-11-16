#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 --version VERSION --engine-stage DIR --engine-build DIR --bridge-repo DIR --signing-key ED25519_PEM --output DIR [--server-url WSS_URL]" >&2
}

version=''
engine_stage=''
engine_build=''
bridge_repo=''
signing_key=''
output=''
# The C2's agent listener, reached DIRECTLY — not through the public reverse
# proxy. The agent pins enrolment TLS to ReadyFleet's internal CA and nothing
# else (agent/internal/enroll/ca_pin.go: RootCAs is the pinned pool, with no
# system-trust fallback, by design). The C2 serves that internal-CA certificate
# itself on this port; readyapp.player-ready.co.uk:443 is Caddy terminating with
# a public Let's Encrypt certificate, which the pin refuses — correctly.
#
# The previous default pointed at that proxy, so every Linux enrolment would
# have died with "x509: certificate signed by unknown authority". It had never
# been run: this is the first Linux profile, and the Windows release path uses
# its own URL.
server_url='wss://fleet.player-ready.co.uk:8643'
while (($#)); do
  case "$1" in
    --version) version=$2; shift 2 ;;
    --engine-stage) engine_stage=$2; shift 2 ;;
    --engine-build) engine_build=$2; shift 2 ;;
    --bridge-repo) bridge_repo=$2; shift 2 ;;
    --signing-key) signing_key=$2; shift 2 ;;
    --output) output=$2; shift 2 ;;
    --server-url) server_url=$2; shift 2 ;;
    *) usage; exit 2 ;;
  esac
done
if [[ -z ${version} || -z ${engine_stage} || -z ${engine_build} || -z ${bridge_repo} || -z ${signing_key} || -z ${output} ]]; then
  usage
  exit 2
fi
[[ -f ${signing_key} ]] || { echo "signing key not found: ${signing_key}" >&2; exit 3; }
[[ -x ${engine_stage%/}/bin/sunshine ]] || { echo "staged sunshine not found" >&2; exit 3; }
[[ -d ${engine_build%/} ]] || { echo "engine build tree not found: ${engine_build}" >&2; exit 3; }
[[ -f ${bridge_repo%/}/go.mod ]] || { echo "bridge repository not found" >&2; exit 3; }

repo_root=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
install -d "${output}"

public_der=${work}/public.der
openssl pkey -in "${signing_key}" -pubout -outform DER -out "${public_der}"
# Ed25519 SubjectPublicKeyInfo ends with the raw 32-byte public key. Pin and
# embed that raw representation, matching verify.Ed25519PublicKeyFingerprint.
public_raw=${work}/public.raw
tail -c 32 "${public_der}" > "${public_raw}"
public_key_b64=$(base64 -w0 "${public_raw}")
signer_fingerprint=$(sha256sum "${public_raw}" | cut -d' ' -f1)

agent_bin=${work}/readyfleet-light
(
  cd "${repo_root}/agent"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.agentVersion=${version} -X main.lightLinuxPublicKey=${public_key_b64}" \
    -o "${agent_bin}" ./cmd/lightagent
)
agent_sha=$(sha256sum "${agent_bin}" | cut -d' ' -f1)

installer_bin=${work}/readyfleet-light-installer
(
  cd "${repo_root}/agent"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.serverURL=${server_url} -X github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall.expectedAgentSHA256=${agent_sha} -X github.com/playerreadyportsmouth/readyfleet/agent/internal/lightinstall.expectedSignerFingerprint=${signer_fingerprint}" \
    -o "${installer_bin}" ./cmd/light-installer
)

bridge_bin=${work}/solarbeam-bridge
(
  cd "${bridge_repo}"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "${bridge_bin}" .
)

cp "${agent_bin}" "${output%/}/readyfleet-light-${version}"
(cd "${output%/}" && sha256sum "readyfleet-light-${version}" > "readyfleet-light-${version}.sha256")
openssl pkeyutl -sign -rawin -inkey "${signing_key}" -in "${output%/}/readyfleet-light-${version}" -out "${output%/}/readyfleet-light-${version}.sig"
base64 -w0 "${output%/}/readyfleet-light-${version}.sig" > "${output%/}/readyfleet-light-${version}.sig.b64"

"${repo_root}/packaging/linux/build-cachyos-bundle.sh" \
  --stage "${engine_stage}" --engine-build "${engine_build}" \
  --bridge "${bridge_bin}" --agent "${agent_bin}" \
  --installer "${installer_bin}" --version "${version}" --output "${output}" \
  --signing-key "${signing_key}"

echo "signer_fingerprint=${signer_fingerprint}"
