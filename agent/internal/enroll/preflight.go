package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Preflight is the C2's answer to POST /enroll/v1/preflight: what a pairing
// code would set this machine up as, WITHOUT spending the code. The unified
// installer (agent/cmd/installer) branches on InstallProfile and shows
// tier-specific consent wording from DeviceTier; nothing here is a
// credential, and the response must never steer where enrollment goes (the
// redeem URL stays compiled in).
//
// Wire contract (tiered installer design, ops/fleet-tiered-installer-2026-11-09,
// phase A1): body {"code":"XXXXXXXXX"}; 200 with this shape, 404 for an
// unknown, expired or already-spent code (enumeration-safe: one answer for
// all three).
type Preflight struct {
	// InstallProfile is "managed" (full agent, Windows service) or
	// "byod_solarbeam" (per-user light agent).
	InstallProfile string `json:"install_profile"`
	// DeviceTier is "venue", "home", "byod" or "none".
	DeviceTier string `json:"device_tier"`
	// VenueName is the display name of the venue the code belongs to.
	VenueName string `json:"venue_name"`
	// AssignedUserDisplay names the person a managed home device is
	// assigned to; empty for venue PCs and personal devices.
	AssignedUserDisplay string `json:"assigned_user_display,omitempty"`
}

// Install profiles and device tiers as the C2 spells them. The installer
// compares against these and refuses anything else rather than guessing.
const (
	ProfileManaged       = "managed"
	ProfileBYODSolarBeam = "byod_solarbeam"

	TierVenue = "venue"
	TierHome  = "home"
	TierBYOD  = "byod"
	// TierNone is what the C2 reports for servers, NAS and unassigned
	// approval-required enrolments: valid on the wire, never a desktop
	// install.
	TierNone = "none"
)

var (
	// ErrCodeNotFound is the 404: the code is unknown, expired or already
	// used. The server deliberately does not say which.
	ErrCodeNotFound = errors.New("agent/enroll: that pairing code was not recognised")
	// ErrPreflightUnavailable covers every other non-2xx answer (rate limit,
	// 5xx) and a malformed body: the code may be fine, the check was not.
	ErrPreflightUnavailable = errors.New("agent/enroll: ReadyFleet could not check the pairing code")
)

// PreflightOptions drives PreflightCode.
type PreflightOptions struct {
	// URL is the https:// preflight endpoint; see PreflightURL.
	URL string
	// Code is the normalised 9-character pairing code, sent bare (no rfk_).
	Code string
	// HTTPClient lets tests inject an httptest client. If nil, a client
	// with the same pinned-CA TLS config as Enroll is used.
	HTTPClient *http.Client
	// Insecure, in dev only, disables server-cert verification.
	Insecure bool
}

// PreflightCode asks the C2 what code would install. It spends nothing.
func PreflightCode(ctx context.Context, opts PreflightOptions) (*Preflight, error) {
	if opts.URL == "" {
		return nil, errors.New("agent/enroll: preflight url is required")
	}
	if opts.Code == "" {
		return nil, errors.New("agent/enroll: pairing code is required")
	}
	body, err := json.Marshal(struct {
		Code string `json:"code"`
	}{Code: opts.Code})
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout:   15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: bootstrapTLSConfig(opts.Insecure)},
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreflightUnavailable, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrCodeNotFound
	case resp.StatusCode/100 != 2:
		return nil, fmt.Errorf("%w: status %d: %s", ErrPreflightUnavailable, resp.StatusCode, summariseBody(respBody))
	}
	var out Preflight
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("%w: parse response: %v", ErrPreflightUnavailable, err)
	}
	if out.InstallProfile == "" || out.DeviceTier == "" {
		return nil, fmt.Errorf("%w: response missing install_profile or device_tier", ErrPreflightUnavailable)
	}
	return &out, nil
}

// PreflightURL derives the preflight endpoint from the agent's wss://
// server URL the same way the light installer derives its redeem URL:
// keep the host, drop scheme and path, use https.
func PreflightURL(serverURL string) string {
	return "https://" + hostOf(serverURL) + "/enroll/v1/preflight"
}

// RedeemURL is the matching redeem endpoint for the same server URL.
func RedeemURL(serverURL string) string {
	return "https://" + hostOf(serverURL) + "/enroll/v1/redeem"
}

func hostOf(serverURL string) string {
	rest := serverURL
	for _, p := range []string{"wss://", "ws://", "https://", "http://"} {
		if strings.HasPrefix(rest, p) {
			rest = rest[len(p):]
			break
		}
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}
