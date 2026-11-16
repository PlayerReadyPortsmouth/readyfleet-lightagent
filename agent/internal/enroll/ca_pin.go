package enroll

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
)

// internal_ca.pem is the public certificate of ReadyFleet's internal mTLS CA
// (server/internal/ca), the same CA that signs the C2's agent-listener
// certificate. It is not a secret — CA certificates are meant to be
// distributed — and is embedded verbatim, exported by the C2 operator from
// the live ca_material record (see docs/runbooks/ca-backup-and-restore.md
// for how that record is custodied; this file holds only the public half).
//
//go:embed internal_ca.pem
var embeddedCAPEM []byte

// pinnedCAPool is the exclusive trust root for the agent's very first
// connection to the C2 (Enroll's POST to /enroll/v1/redeem) — made before
// the agent has any certificate or trust material of its own, using only
// the bootstrap bearer secret. Before this pin existed, that connection was
// made with an empty tls.Config, which falls back to the OS system trust
// store; the C2's agent-listener cert is signed by this internal CA, not a
// public one, and nothing installs this CA into a fresh machine's Windows
// trust store before enrollment runs (the retired managed-installer .bat
// flow used to do this via `certutil -addstore -f Root`; nothing replaced
// that step when it was removed). Falling back to ambient system trust on a
// venue PC or mentor's BYOD machine means enrollment would silently trust
// whatever CAs that machine already trusts — none of which have any
// business vouching for this C2.
//
// This mirrors the pinning philosophy already used for the bundled agent
// binary itself (agent/internal/bootstrap and agent/internal/lightinstall's
// expectedAgentSHA256 / expectedSignerFingerprint): verify against a known-
// good value baked into the binary at build time, exact-match-or-refuse,
// never best-effort. Enroll uses this pool exclusively as tls.Config.RootCAs
// on its non-Insecure path — not "pinned CA plus system pool as fallback" —
// so a server presenting any certificate not issued by this exact CA is
// refused, full stop.
//
// ROTATION: this is baked in at compile time from a point-in-time export of
// the CA cert, not read live. If the internal CA is ever rotated (root key
// replaced — see docs/runbooks/ca-backup-and-restore.md, which explicitly
// does not cover rotation and would need a new ceremony; the closest
// existing rotation ceremony in this repo is the code-signing leaf rotation
// in docs/runbooks/codesign-cert.md, "To rotate the leaf key" — the same
// shape of problem: rotate, then redistribute the new public material
// before anything relies on it), this embedded cert goes stale and every
// previously-built installer carrying it will fail to connect at
// enrollment with an x509 trust error, even though nothing in the installer
// itself is broken. A new release build embedding the freshly rotated CA
// cert is required before any new device can enroll — the same per-release
// re-bake dependency expectedAgentSHA256 and expectedSignerFingerprint
// already have.
var pinnedCAPool = mustPinnedCAPool()

func mustPinnedCAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	if ok := pool.AppendCertsFromPEM(embeddedCAPEM); !ok {
		// A build with a corrupt or empty embedded cert must not silently
		// fall back to ambient trust — refuse to even start rather than run
		// with a pin that isn't actually pinning anything.
		panic("agent/enroll: embedded internal CA cert (internal_ca.pem) is not valid PEM")
	}
	return pool
}

// bootstrapTLSConfig builds the TLS config for Enroll's un-credentialed
// bootstrap connection to the C2. Trust is pinned exclusively to
// pinnedCAPool — never the OS system trust store — except on the dev-only
// insecure path, which is unchanged: FLEET_DEV=1 local testing needs to
// tolerate a self-signed or mismatched dev cert, and that bypass predates
// and is orthogonal to this pin.
func bootstrapTLSConfig(insecure bool) *tls.Config {
	cfg := &tls.Config{RootCAs: pinnedCAPool}
	if insecure {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}
