// Package config loads agent configuration from disk.
//
// On Windows the config file lives at:
//
//	%PROGRAMDATA%\fleetmgr\config.json
//
// In dev / fake-agent mode it falls back to ./fleetmgr-config.json next
// to the binary, so a developer can iterate without touching system
// directories.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the on-disk agent configuration.
type Config struct {
	// ServerURL is the wss:// URL of the C2 agent listener.
	ServerURL string `json:"server_url"`

	// EnrollmentURL is the https:// URL of the C2 enrollment endpoint.
	// Falls back to ServerURL with /enroll/v1/redeem appended if empty.
	EnrollmentURL string `json:"enrollment_url,omitempty"`

	// EnrollmentSecret is the bootstrap credential the agent presents to
	// /enroll/v1/redeem as a Bearer token. Accepts either:
	//   - rfk_... a long-lived venue API key (legacy / scripted path), or
	//   - rft_... a single-use enrollment token (the standard flow).
	// The server dispatches by prefix; the agent treats this field as
	// opaque. Cleared from disk after a successful enrollment.
	//
	// JSON key is `enrollment_secret`. The legacy key `venue_api_key` is
	// accepted on unmarshal for one release to preserve existing on-disk
	// configs; UnmarshalJSON below handles the alias.
	EnrollmentSecret string `json:"enrollment_secret,omitempty"`

	// IntendedLabel optionally pre-fills the machine label, set at install
	// time by the IT admin. Cleared after enrollment.
	IntendedLabel string `json:"intended_label,omitempty"`

	// CertPath is the path to the client cert PEM file. Set after
	// enrollment.
	CertPath string `json:"cert_path,omitempty"`

	// KeyPath is the path to the private key. Set after enrollment.
	KeyPath string `json:"key_path,omitempty"`

	// CAPath is the path to the internal CA cert PEM. Set after enrollment.
	CAPath string `json:"ca_path,omitempty"`

	// MachineID is the UUID issued by the server at enrollment.
	MachineID string `json:"machine_id,omitempty"`

	// VenueID is the UUID of the venue this machine belongs to.
	VenueID string `json:"venue_id,omitempty"`
}

// DefaultPath returns the canonical config path for the current platform.
func DefaultPath() string {
	switch runtime.GOOS {
	case "windows":
		programData := os.Getenv("PROGRAMDATA")
		if programData == "" {
			programData = `C:\ProgramData`
		}
		return filepath.Join(programData, "fleetmgr", "config.json")
	default:
		// Dev / fake-agent fallback. Production agents only ship for
		// Windows.
		return "fleetmgr-config.json"
	}
}

// Load reads and parses the config file at path. A missing file is not
// an error — it returns a zero Config and the caller can decide whether
// to enter the enrollment flow.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, fmt.Errorf("agent/config: read %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("agent/config: parse %s: %w", path, err)
	}
	return c, nil
}

// Save writes c to path atomically. The parent directory must exist; on
// Windows it is created during MSI install.
func Save(path string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("agent/config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("agent/config: write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("agent/config: rename: %w", err)
	}
	return nil
}

// IsEnrolled reports whether c contains the post-enrollment material
// needed to connect via mTLS.
func (c Config) IsEnrolled() bool {
	return c.MachineID != "" && c.CertPath != "" && c.KeyPath != ""
}

// UnmarshalJSON implements json.Unmarshaler so Config accepts either
// `enrollment_secret` (preferred) or the legacy `venue_api_key` JSON
// key. When both are present, `enrollment_secret` wins. Drop the
// legacy branch after one release.
func (c *Config) UnmarshalJSON(data []byte) error {
	type configAlias Config // shadow to avoid recursion
	aux := struct {
		LegacyVenueAPIKey string `json:"venue_api_key,omitempty"`
		*configAlias
	}{configAlias: (*configAlias)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Legacy key only wins if the new key was not supplied.
	if c.EnrollmentSecret == "" && aux.LegacyVenueAPIKey != "" {
		c.EnrollmentSecret = aux.LegacyVenueAPIKey
	}
	return nil
}
