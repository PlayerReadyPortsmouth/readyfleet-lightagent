package solarbeam

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
)

var errLinkCredential = errors.New("solarbeam: protected link credential unavailable")
var errPreviousBridgeStillRunning = errors.New("solarbeam: previous bridge did not stop")

type linkCredential struct {
	env     []string
	cleanup func() error
	mode    string // launch decision shared with the managed engine
}

func (m *Manager) linkAuthMode() (string, error) {
	switch m.paths.LinkAuthMode {
	case "":
		return "warn", nil
	case "warn", "enforce":
		return m.paths.LinkAuthMode, nil
	default:
		return "", errors.New("solarbeam: link auth mode must be warn or enforce")
	}
}

// Provision only after the old bridge has stopped. Both children receive the
// same launch credential; a new bridge always gets a fresh generation.
func (m *Manager) provisionLinkAuth(write func([]byte) (*linkCredential, error)) error {
	mode, err := m.compatibleLinkAuthMode()
	if err != nil {
		return err
	}
	if err := m.cleanupStoppedLinkAuth(); err != nil {
		return err
	}
	// Keep launch cleanup ownership even for legacy: an engine/service failure
	// must still tear down the bridge instead of leaving a mismatched pair.
	m.linkCredential = &linkCredential{mode: mode}
	if mode == "" {
		return nil
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return errLinkCredential
	}
	secret := make([]byte, hex.EncodedLen(len(raw)))
	hex.Encode(secret, raw)
	clear(raw)
	defer clear(secret)
	credential, err := write(secret)
	if err != nil {
		return errLinkCredential
	}
	credential.env = append(credential.env, "SOLARBEAM_LINK_AUTH_MODE="+mode)
	credential.mode = mode
	m.linkCredential = credential
	slog.Info("solarbeam: link authentication provisioned", "mode", mode)
	return nil
}

// Never inherit a credential or mode from the agent's own environment. In
// interactive Windows mode the secret exists only in these two child blocks.
func (m *Manager) linkAuthEnvironment(base []string) []string {
	env := withoutLinkAuth(base)
	if m.linkCredential != nil {
		env = append(env, m.linkCredential.env...)
	}
	return env
}

func withoutLinkAuth(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "SOLARBEAM_LINK_SECRET", "SOLARBEAM_LINK_SECRET_FILE", "SOLARBEAM_LINK_AUTH_MODE":
		default:
			out = append(out, entry)
		}
	}
	return out
}

func (m *Manager) forgetLinkEnvironment() {
	if m.linkCredential != nil {
		m.linkCredential.env = nil
	}
}

func (m *Manager) cleanupLinkAuth() error {
	if m.linkCredential == nil {
		return nil
	}
	credential := m.linkCredential
	credential.env = nil
	if credential.cleanup != nil {
		if err := credential.cleanup(); err != nil {
			return err
		}
	}
	m.linkCredential = nil
	return nil
}

// The service file is public configuration: it must contain only the stable
// credential path and mode, never the launch secret. Reject line injection.
func linkBridgeEnvironment(machineID, path, mode string) (string, error) {
	if strings.ContainsAny(machineID+path, "\r\n\x00") || (mode != "" && mode != "warn" && mode != "enforce") {
		return "", errLinkCredential
	}
	base := "SOLARBEAM_BRIDGE_WS=ws://127.0.0.1:9092/bridge\n" +
		"SOLARBEAM_MACHINE_ID=" + machineID + "\n"
	if mode == "" {
		return base, nil
	}
	return base +
		"SOLARBEAM_LINK_SECRET_FILE=" + path + "\n" +
		"SOLARBEAM_LINK_AUTH_MODE=" + mode + "\n", nil
}

func (m *Manager) cleanupStoppedLinkAuth() error {
	if m.linkCredential != nil {
		return m.cleanupLinkAuth()
	}
	if m.recoveredLinkCleanupFn != nil {
		return m.recoveredLinkCleanupFn()
	}
	return nil
}
