// Package lightinstall installs, and later removes, the BYOD lightagent
// on a mentor's personal Windows PC: a per-user, non-admin install — no
// Windows service, no LocalMachine write, no ProgramFiles. It is the
// light counterpart to agent/internal/bootstrap (the managed installer's
// engine), but deliberately simpler: bootstrap's payload/file handling is
// hardened against TOCTOU races on a privileged (SYSTEM+Administrators,
// Program Files/ProgramData) target, which per-user %LocalAppData% isn't
// exposed to the same way (a non-admin user attacking their own profile
// isn't a privilege boundary this needs to defend). Strict field-by-field
// JSON decoding is kept (bootstrap's actual security property against a
// malicious payload); the reparse-point defense and multi-pass secure
// wipe are not reimplemented here — flagged as a real simplification, not
// silently matched.
package lightinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

const maxPayloadBytes = 16 * 1024

// lightTokenPattern matches the two bearers a light install may redeem:
// an rft_ single-use enrollment token (the per-person composed download),
// or an rfc_ pairing code (the unified installer; 9 characters from the
// pairing alphabet, any tier on the wire — the C2 resolves it to the
// byod_solarbeam token it wraps). Only the wire shape is validated here;
// the server's stored credential decides the profile.
var lightTokenPattern = regexp.MustCompile(`^(rft_[A-Za-z0-9_-]{43}|rfc_[A-HJ-NP-Z2-9]{9})$`)

// Payload is the complete on-disk enrollment schema for a light install.
// The legacy schema has one enrolment token. Compatible canary builds may
// also carry report-only scope and expected owner metadata. A payload cannot
// select a controller/report URL or a managed profile.
type Payload struct {
	EnrollmentToken      string `json:"enrollment_token"`
	ResilientInstall     bool   `json:"resilient_install,omitempty"`
	FailureReportToken   string `json:"failure_report_token,omitempty"`
	PairingCodeID        string `json:"pairing_code_id,omitempty"`
	ExpectedOwnerActorID string `json:"expected_owner_actor_id,omitempty"`
}

// LoadPayload reads a bounded payload file and strictly decodes it via
// ParsePayload.
func LoadPayload(path string) (Payload, error) {
	f, err := os.Open(path)
	if err != nil {
		return Payload{}, invalidPayload("open")
	}
	defer f.Close()

	body, err := io.ReadAll(io.LimitReader(f, maxPayloadBytes+1))
	if err != nil {
		return Payload{}, invalidPayload("read")
	}
	return ParsePayload(body)
}

// ParsePayload strictly decodes the one allowed field from an in-memory
// payload body — an unexpected or duplicate key, oversized body, or
// malformed token is rejected outright rather than tolerated. Factored out
// of LoadPayload so light-installer's self-contained build (token embedded
// in the exe's own trailer data rather than a sibling enrollment.json) can
// reuse the exact same validation instead of writing the bytes to a temp
// file just to satisfy a path-based API.
func ParsePayload(body []byte) (Payload, error) {
	if len(body) > maxPayloadBytes {
		return Payload{}, invalidPayload("too large")
	}

	decoder := json.NewDecoder(strings.NewReader(string(body)))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return Payload{}, invalidPayload("invalid JSON")
	}
	var payload Payload
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Payload{}, invalidPayload("invalid JSON")
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return Payload{}, invalidPayload("unexpected or duplicate field")
		}
		seen[key] = true
		var target any
		switch key {
		case "enrollment_token":
			target = &payload.EnrollmentToken
		case "resilient_install":
			target = &payload.ResilientInstall
		case "failure_report_token":
			target = &payload.FailureReportToken
		case "pairing_code_id":
			target = &payload.PairingCodeID
		case "expected_owner_actor_id":
			target = &payload.ExpectedOwnerActorID
		default:
			return Payload{}, invalidPayload("unexpected field")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || string(raw) == "null" {
			return Payload{}, invalidPayload("invalid field type")
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return Payload{}, invalidPayload("invalid field type")
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return Payload{}, invalidPayload("invalid JSON")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return Payload{}, invalidPayload("trailing data")
	}
	if !seen["enrollment_token"] || !lightTokenPattern.MatchString(payload.EnrollmentToken) {
		return Payload{}, invalidPayload("single-use enrollment token required")
	}
	if seen["failure_report_token"] || seen["pairing_code_id"] {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`).MatchString(payload.FailureReportToken) || !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(payload.PairingCodeID) {
			return Payload{}, invalidPayload("invalid reporting scope")
		}
	}
	if seen["expected_owner_actor_id"] && !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(payload.ExpectedOwnerActorID) {
		return Payload{}, invalidPayload("invalid owner")
	}
	return payload, nil
}

// RemovePayload deletes the payload file. Best-effort, simple os.Remove —
// see the package doc comment for why this doesn't reimplement bootstrap's
// secure-wipe.
func RemovePayload(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("lightinstall: remove payload: %w", err)
	}
	return nil
}

func invalidPayload(reason string) error {
	return fmt.Errorf("lightinstall: invalid enrollment payload: %s", reason)
}
