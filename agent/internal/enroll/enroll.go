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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
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
	"runtime"
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
}

// Errors callers may want to inspect.
var (
	ErrNoBearerSecret = errors.New("agent/enroll: no enrollment secret in config")
	ErrServer         = errors.New("agent/enroll: server rejected the request")
)

// Options drives Enroll.
type Options struct {
	// EnrollmentURL is the https:// URL of the C2's redeem endpoint.
	EnrollmentURL string

	// BearerSecret is the bootstrap credential the agent presents as
	// Authorization: Bearer. Accepts either an rfk_ venue API key or
	// an rft_ enrollment token — the server dispatches by prefix.
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

	// Generate keypair + CSR.
	key, err := GenerateKey()
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: keygen: %w", err)
	}
	csr, err := GenerateCSR(key)
	if err != nil {
		return nil, err
	}

	// POST.
	body, err := json.Marshal(Request{
		CSRPEM:       string(csr),
		AgentVersion: opts.AgentVersion,
		Inventory:    opts.Inventory,
	})
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, opts.EnrollmentURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+opts.BearerSecret)
	req.Header.Set("Content-Type", "application/json")

	httpClient := opts.HTTPClient
	if httpClient == nil {
		tlsCfg := &tls.Config{}
		if opts.Insecure {
			tlsCfg.InsecureSkipVerify = true
		}
		httpClient = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agent/enroll: post: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%w: status %d: %s", ErrServer, resp.StatusCode, summariseBody(respBody))
	}

	var out Response
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("agent/enroll: parse response: %w", err)
	}
	if out.CertPEM == "" || out.CAPEM == "" || out.MachineID == "" {
		return nil, fmt.Errorf("agent/enroll: server response missing required fields")
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

	return &Result{
		MachineID:      out.MachineID,
		VenueID:        out.VenueID,
		AgentListenURL: out.AgentListenURL,
		CertPath:       certPath,
		KeyPath:        keyPath,
		CAPath:         caPath,
	}, nil
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
