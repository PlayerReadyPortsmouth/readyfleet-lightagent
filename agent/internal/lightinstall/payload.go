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

// lightTokenPattern matches an rft_ single-use enrollment token — the
// same shape the managed installer requires, minted for a byod_solarbeam
// enrollment token specifically (server-side, the token's own stored
// agent_profile decides what the redeemed machine becomes; this pattern
// only validates the wire shape, not the profile).
var lightTokenPattern = regexp.MustCompile(`^rft_[A-Za-z0-9_-]{43}$`)

// Payload is the complete on-disk enrollment schema for a light install.
// Deliberately one field — unlike the managed payload's
// requested_name_base, BYOD enrollment always gets a server-generated
// BYOD-###### name and rejects a requested one (server/internal/api/enrollment_tokens.go,
// Task 1 of this build), so there is nothing else for this payload to carry.
type Payload struct {
	EnrollmentToken string `json:"enrollment_token"`
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
	seen := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return Payload{}, invalidPayload("invalid JSON")
		}
		key, ok := token.(string)
		if !ok || key != "enrollment_token" || seen {
			return Payload{}, invalidPayload("unexpected or duplicate field")
		}
		seen = true
		if err := decoder.Decode(&payload.EnrollmentToken); err != nil {
			return Payload{}, invalidPayload("field must be a string")
		}
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return Payload{}, invalidPayload("invalid JSON")
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return Payload{}, invalidPayload("trailing data")
	}
	if !seen || !lightTokenPattern.MatchString(payload.EnrollmentToken) {
		return Payload{}, invalidPayload("single-use enrollment token required")
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
