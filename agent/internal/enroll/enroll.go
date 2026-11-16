// Package enroll runs the agent's bootstrap flow: take an enrollment secret,
// produce a CSR, exchange it for a signed cert, persist everything to
// disk under restrictive ACLs.
//
// The agent writes three PEM files alongside config.json:
//
//   - client.crt — the signed leaf certificate
//   - client.key — the matching RSA private key
//   - ca.crt     — the internal CA's certificate, used to verify the C2
//
// All three are mode 0600 (Unix) or SYSTEM/Administrators-only (Windows;
// applied by the MSI installer rather than this code).
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Result is what the caller persists once enrollment succeeds.
type Result struct {
	MachineID      string
	VenueID        string
	AgentListenURL string
	CertPath       string
	KeyPath        string
	CAPath         string

	// OwnerActorID is the ReadyApp actor this machine belongs to, as the
	// controller recorded it (machines.owner_actor_id). Persisted into
	// config.json so the agent can tell its owner apart from everybody
	// else when someone asks to connect — see agent/internal/consent.
	// Empty when the controller had no attribution for this enrollment,
	// which simply means the agent prompts for every request.
	OwnerActorID string

	// InstallProfile and DeviceTier echo Response's fields; empty when the
	// controller predates them.
	InstallProfile string
	DeviceTier     string
}

// Request is the JSON body we POST to the C2.
type Request struct {
	CSRPEM       string            `json:"csr_pem"`
	AgentVersion string            `json:"agent_version"`
	Inventory    InventorySnapshot `json:"inventory"`
}

// InventorySnapshot mirrors enroll.InventorySnapshot on the server side.
// Kept as a flat map so we don't pull the proto package transitively
// through every consumer of this enrollment client.
type InventorySnapshot struct {
	Hostname            string         `json:"hostname"`
	OSName              string         `json:"os_name"`
	OSVersion           string         `json:"os_version"`
	HardwareFingerprint string         `json:"hardware_fingerprint,omitempty"`
	Hardware            map[string]any `json:"hardware,omitempty"`
	Network             map[string]any `json:"network,omitempty"`
	InstalledSoftware   []any          `json:"installed_software,omitempty"`
}

// Response mirrors the server's RedeemResponse.
type Response struct {
	MachineID      string `json:"machine_id"`
	VenueID        string `json:"venue_id"`
	CertPEM        string `json:"cert_pem"`
	CAPEM          string `json:"ca_pem"`
	AgentListenURL string `json:"agent_listen_url"`
	OwnerActorID   string `json:"owner_actor_id,omitempty"`
	// InstallProfile and DeviceTier are what the controller actually made
	// this machine (tiered installer, A1): "managed"/"byod_solarbeam" and
	// "venue"/"home"/"byod". Older controllers omit them; nothing in the
	// agent requires them, the installer uses them to word its final screen.
	InstallProfile string `json:"install_profile,omitempty"`
	DeviceTier     string `json:"device_tier,omitempty"`
}

// Errors callers may want to inspect.
var (
	ErrNoBearerSecret = errors.New("agent/enroll: no enrollment secret in config")
	ErrServer         = errors.New("agent/enroll: server rejected the request")

	// ErrPendingApprovalRequired is returned when the server deferred this
	// redemption pending a human's confirmation but Options.OnPending was
	// nil — i.e. this caller never opted into waiting. Deliberately an
	// error rather than a silent wait: a caller with no way to tell a
	// human what it is blocked on must not block at all.
	ErrPendingApprovalRequired = errors.New("agent/enroll: this enrollment needs to be confirmed in ReadyApp, and this caller cannot wait for that")

	// ErrEnrollmentDeclined is returned when the owner declined the device
	// in ReadyApp. Terminal: no cert exists and none ever will for this
	// token.
	ErrEnrollmentDeclined = errors.New("agent/enroll: this device was declined in ReadyApp")

	// ErrEnrollmentExpired is returned when the pending window closed
	// before anyone confirmed the device. Terminal: a fresh enrollment
	// token is required.
	ErrEnrollmentExpired = errors.New("agent/enroll: the confirmation request expired before this device was confirmed")

	// ErrEnrollmentUnreachable is returned when the client-side backstop
	// (maxPendingWait) is what ends the wait — as opposed to a genuine
	// server-side verdict (ErrEnrollmentDeclined / ErrEnrollmentExpired).
	// This is not a verdict on the enrollment at all: it means the poll
	// loop could never reach the server for long enough to learn one,
	// most often because the mentor's network was down for the whole
	// window. %w-wrapped around the last retryable transport/5xx error
	// the loop observed, if any, so the real cause survives past this
	// summary rather than being silently discarded tick after tick.
	ErrEnrollmentUnreachable = errors.New("agent/enroll: couldn't reach ReadyFleet while waiting for confirmation")

	// ErrInstallProfileMismatch: the server enrolled the machine under a
	// different install_profile than Options.ExpectInstallProfile. No
	// material was written.
	ErrInstallProfileMismatch = errors.New("agent/enroll: the server's install plan does not match what was confirmed")
)

// Poll pacing for the pending-approval wait. The server currently proposes
// 3000ms; the clamp exists so a broken or hostile response can neither spin
// this into a hot loop nor stretch a poll out past usefulness.
const (
	defaultPollInterval = 3 * time.Second
	minPollInterval     = 100 * time.Millisecond
	maxPollInterval     = 60 * time.Second
)

// maxPendingWait bounds the whole wait client-side. The server's own
// pending window (enroll.PendingApprovalTTL, 10 minutes) normally ends
// this first by answering "expired"; this is the backstop for a server
// that never does — an installer must not sit on a mentor's screen
// forever because of a clock skew or a missed expiry sweep.
//
// A var rather than a const so tests can shrink it to exercise the
// backstop firing without a real 15-minute run.
var maxPendingWait = 15 * time.Minute

// safePendingID matches the uuid.String() form the server actually returns.
// The pending id is interpolated into the poll URL's path, so it is
// validated as a plain UUID before use rather than trusted: a malicious or
// buggy response must not be able to steer the authenticated poll request
// (which carries the poll token) at some other path or host.
var safePendingID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Pending describes a redemption the server accepted but has not completed:
// the CSR is stored and a certificate will only be issued once the device's
// owner confirms it in ReadyApp. Passed to Options.OnPending purely so an
// interactive caller can say what it is waiting for — deliberately without
// the poll token, which is a live credential with no business in a display
// path.
type Pending struct {
	// PendingID identifies the pending row, and is the last path segment
	// of the poll endpoint.
	PendingID string

	// PollInterval is how often Enroll will poll, already clamped to a
	// sane range.
	PollInterval time.Duration
}

// pendingRedeem mirrors the server's RedeemPendingResponse — the 202 body
// returned instead of a certificate when a BYOD enrollment token is
// redeemed.
type pendingRedeem struct {
	Status         string `json:"status"`
	PendingID      string `json:"pending_id"`
	PollToken      string `json:"poll_token"`
	PollIntervalMs int    `json:"poll_interval_ms"`
}

// pollResult mirrors GET /enroll/v1/redeem/{pending_id}: a status plus, once
// approved, the full RedeemResponse the POST would have returned
// synchronously.
type pollResult struct {
	Status string `json:"status"`
	Response
}

// Options drives Enroll.
type Options struct {
	// EnrollmentURL is the https:// URL of the C2's redeem endpoint.
	EnrollmentURL string

	// BearerSecret is the bootstrap credential the agent presents as
	// Authorization: Bearer: an rfk_ venue API key, an rft_ enrollment
	// token, or an rfc_ pairing code — the server dispatches by prefix.
	BearerSecret string

	// AgentVersion identifies this build.
	AgentVersion string

	// Inventory is the initial snapshot to send.
	Inventory InventorySnapshot

	// MaterialDir is the directory to write client.crt / client.key /
	// ca.crt into. Created with mode 0700 if absent.
	MaterialDir string

	// HTTPClient lets tests inject a custom client (httptest, etc).
	// If nil, a sensible default is used.
	HTTPClient *http.Client

	// Insecure, in dev only, disables server-cert verification.
	Insecure bool

	// OnPending opts this caller into the BYOD consent wait. A BYOD
	// enrollment token is redeemed but not completed: the server stores the
	// CSR, answers 202 with a pending id + one-time poll token, and issues
	// nothing until the device's owner confirms it in ReadyApp. When
	// OnPending is non-nil, Enroll calls it once — as soon as the wait
	// begins — and then polls until a terminal status, completing exactly
	// as if the certificate had been returned synchronously.
	//
	// When OnPending is nil, a deferred redemption is ErrPendingApprovalRequired
	// instead. That is the point of the callback being the opt-in rather
	// than a bool: presence of a way to tell a human what we are waiting on
	// is precisely the condition under which waiting is acceptable. A
	// headless caller (the direct lightagent re-enrolment path, tests, CI)
	// fails fast rather than blocking silently for the pending window.
	//
	// Called once, not per poll tick: the only implementation today drives
	// installerui.Wizard's report(), which advances the progress bar and
	// paces each call by 600ms — a per-tick call would march the bar to
	// 100% and pace-limit the poll loop itself.
	OnPending func(Pending)

	// ExpectInstallProfile, if non-empty, must equal the install_profile
	// the server returns (on the 200, or on the approved poll reply). A
	// mismatch is ErrInstallProfileMismatch and nothing is written: the
	// caller showed a consent screen for one plan and must not end up with
	// another. A server that omits the field (pre-tier controllers) passes.
	ExpectInstallProfile string

	// Retry enables bounded safe retries and private consent-wait recovery.
	// Never retries an ambiguous POST: single-use tokens cannot be replayed.
	Retry bool
}

// Enroll performs the redeem call and persists the returned material.
// It does not modify the agent config file; the caller is responsible
// for writing the returned paths back to config.json (so we don't have
// to import config from this lower-level package).
func Enroll(ctx context.Context, opts Options) (*Result, error) {
	if opts.BearerSecret == "" {
		return nil, ErrNoBearerSecret
	}
	if opts.EnrollmentURL == "" {
		return nil, errors.New("agent/enroll: enrollment_url is required")
	}
	if opts.MaterialDir == "" {
		return nil, errors.New("agent/enroll: material_dir is required")
	}

	key, out, err := exchange(ctx, opts)
	if err != nil {
		return nil, err
	}
	if out.CertPEM == "" || out.CAPEM == "" || out.MachineID == "" {
		return nil, fmt.Errorf("agent/enroll: server response missing required fields")
	}
	if opts.ExpectInstallProfile != "" && out.InstallProfile != "" && out.InstallProfile != opts.ExpectInstallProfile {
		return nil, fmt.Errorf("%w: expected %s, server enrolled %s", ErrInstallProfileMismatch, opts.ExpectInstallProfile, out.InstallProfile)
	}

	// Persist material.
	if err := os.MkdirAll(opts.MaterialDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent/enroll: mkdir material dir: %w", err)
	}
	certPath := filepath.Join(opts.MaterialDir, "client.crt")
	keyPath := filepath.Join(opts.MaterialDir, "client.key")
	caPath := filepath.Join(opts.MaterialDir, "ca.crt")

	if err := writeRestricted(certPath, []byte(out.CertPEM)); err != nil {
		return nil, fmt.Errorf("agent/enroll: write cert: %w", err)
	}
	if err := writeRestricted(keyPath, EncodeKeyPEM(key)); err != nil {
		return nil, fmt.Errorf("agent/enroll: write key: %w", err)
	}
	if err := writeRestricted(caPath, []byte(out.CAPEM)); err != nil {
		return nil, fmt.Errorf("agent/enroll: write ca: %w", err)
	}

	if opts.Retry {
		if err := os.Remove(filepath.Join(opts.MaterialDir, checkpointName)); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("agent/enroll: remove recovery checkpoint: %w", err)
		}
	}

	return &Result{
		MachineID:      out.MachineID,
		VenueID:        out.VenueID,
		AgentListenURL: out.AgentListenURL,
		CertPath:       certPath,
		KeyPath:        keyPath,
		CAPath:         caPath,
		OwnerActorID:   out.OwnerActorID,
		InstallProfile: out.InstallProfile,
		DeviceTier:     out.DeviceTier,
	}, nil
}

// waitForApproval polls GET /enroll/v1/redeem/{pending_id} until the
// server reports a terminal status, and returns the RedeemResponse the POST
// would have returned had no consent gate stood in the way.
//
// It lives here, rather than in the installer that owns the UI, because the
// private key this certificate is issued against was generated inside
// Enroll and never leaves it — moving the wait outward would mean handing a
// caller the key material (or splitting Enroll's persist step out behind a
// second exported entry point) purely to relocate an HTTP loop. The
// installer supplies only the part that is genuinely its own: what to tell
// the mentor, via Options.OnPending.
func waitForApproval(ctx context.Context, httpClient *http.Client, opts Options, p pendingRedeem) (*Response, error) {
	if p.Status != "pending" {
		return nil, fmt.Errorf("agent/enroll: server deferred enrollment with unexpected status %q", p.Status)
	}
	if !safePendingID.MatchString(p.PendingID) {
		return nil, fmt.Errorf("agent/enroll: server returned an invalid pending id")
	}
	if p.PollToken == "" {
		return nil, fmt.Errorf("agent/enroll: server deferred enrollment without a poll token")
	}
	if opts.OnPending == nil {
		return nil, ErrPendingApprovalRequired
	}

	interval := time.Duration(p.PollIntervalMs) * time.Millisecond
	switch {
	case p.PollIntervalMs <= 0:
		interval = defaultPollInterval
	case interval < minPollInterval:
		interval = minPollInterval
	case interval > maxPollInterval:
		interval = maxPollInterval
	}

	pollURL := strings.TrimSuffix(opts.EnrollmentURL, "/") + "/" + p.PendingID
	opts.OnPending(Pending{PendingID: p.PendingID, PollInterval: interval})

	waitCtx, cancel := context.WithTimeout(ctx, maxPendingWait)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// lastPollErr remembers the most recent retryable transport/5xx error
	// seen during the loop, so that if the backstop is what ends the wait,
	// the actual cause (the network, not a ReadyApp decision) is still
	// available to report instead of being discarded silently.
	var lastPollErr error
	retryDelay := interval

	for {
		// Sleep first: the POST only just returned, so nobody can have
		// confirmed anything yet, and an immediate poll is a guaranteed
		// wasted round trip.
		select {
		case <-waitCtx.Done():
			return nil, waitEndedErr(ctx, lastPollErr)
		case <-ticker.C:
		}

		result, retryable, err := pollOnce(waitCtx, httpClient, pollURL, p.PollToken)
		if err != nil {
			if retryable {
				// A dropped connection or a 5xx mid-wait is a blip on a
				// mentor's home network, not a verdict — keep waiting, but
				// remember it in case the backstop is what ends this loop.
				lastPollErr = err
				if opts.Retry {
					retryDelay *= 2
					if retryDelay > maxPollInterval {
						retryDelay = maxPollInterval
					}
					ticker.Reset(retryDelay)
				}
				continue
			}
			if waitCtx.Err() != nil {
				// The wait ended underneath an in-flight request; report
				// why it ended, not the transport error that fell out of it.
				return nil, waitEndedErr(ctx, lastPollErr)
			}
			return nil, err
		}
		if opts.Retry {
			retryDelay = interval
			ticker.Reset(interval)
		}
		switch result.Status {
		case "pending":
		case "approved":
			return &result.Response, nil
		case "declined":
			return nil, ErrEnrollmentDeclined
		case "expired":
			return nil, ErrEnrollmentExpired
		default:
			return nil, fmt.Errorf("agent/enroll: unexpected confirmation status %q", result.Status)
		}
	}
}

// waitEndedErr says why the wait stopped: the caller's own cancellation
// verbatim (so errors.Is(err, context.Canceled) still works upstream), or
// — when it is the local backstop deadline that fired, not a genuine
// server-side verdict — ErrEnrollmentUnreachable, wrapped around the last
// retryable poll error seen so the real cause (almost always the network)
// is still visible rather than silently discarded.
func waitEndedErr(ctx context.Context, lastPollErr error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if lastPollErr != nil {
		return fmt.Errorf("%w after %s: %v", ErrEnrollmentUnreachable, maxPendingWait, lastPollErr)
	}
	return fmt.Errorf("%w: gave up after %s with no successful poll", ErrEnrollmentUnreachable, maxPendingWait)
}

// pollOnce performs one poll. retryable distinguishes "try again inside the
// pending window" (transport failure, 5xx) from a verdict this loop must
// stop on (a rejected poll token, a pending id the server no longer knows).
func pollOnce(ctx context.Context, httpClient *http.Client, pollURL, pollToken string) (*pollResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollURL, nil)
	if err != nil {
		return nil, false, fmt.Errorf("agent/enroll: build poll request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+pollToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("agent/enroll: read poll response: %w", readErr)
	}

	if resp.StatusCode/100 == 5 || resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("%w: status %d", ErrServer, resp.StatusCode)
	}
	if resp.StatusCode/100 != 2 {
		return nil, false, fmt.Errorf("%w: status %d: %s", ErrServer, resp.StatusCode, summariseBody(body))
	}
	var result pollResult
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, false, fmt.Errorf("agent/enroll: parse poll response: %w", err)
	}
	return &result, false, nil
}

// GenerateKey creates a fresh 2048-bit RSA private key for an agent.
func GenerateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
}

// GenerateCSR returns a PEM-encoded PKCS#10 certificate signing request.
// The CN is a placeholder; the server overwrites it with the assigned
// machine_id.
func GenerateCSR(key *rsa.PrivateKey) ([]byte, error) {
	tmpl := x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "fleetmgr-agent"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &tmpl, key)
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: create CSR: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// EncodeKeyPEM encodes a private key as a PEM block.
func EncodeKeyPEM(key *rsa.PrivateKey) []byte {
	der := x509.MarshalPKCS1PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
}

// writeRestricted writes the file at 0600. On Windows the MSI installer
// applies the SYSTEM/Administrators ACL; the file mode is honoured by
// the modern Windows runtime even though POSIX ACLs are not.
func writeRestricted(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		// Best-effort tighten in case of umask weirdness.
		_ = os.Chmod(tmp, 0o600)
	}
	return os.Rename(tmp, path)
}

func summariseBody(b []byte) string {
	const max = 256
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
