package solarbeam

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const linkCapabilityPrefix = "SolarBeam-Link-Auth: "
const linkCapabilityLimit = 64 * 1024

var errLinkCapabilityProbe = errors.New("link capability probe unavailable")

// Use the existing help path: unknown flags and even --version can start
// legacy engine initialization. No package/version sidecar proves that BOTH
// installed executables implement the provisioned-auth protocol.
func probeLinkCapability(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errLinkCapabilityProbe
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := linkCapabilityCommand(ctx, path)
	return runLinkCapabilityCommand(cmd)
}

func runLinkCapabilityCommand(cmd *exec.Cmd) ([]byte, error) {
	var output linkCapabilityOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return nil, errLinkCapabilityProbe
	}
	return output.data, nil
}

func configureLinkCapabilityCommand(cmd *exec.Cmd, env []string) {
	cmd.Args = append(cmd.Args, "--help")
	cmd.Dir = filepath.Dir(cmd.Path)
	cmd.WaitDelay = time.Second
	cmd.Env = make([]string, 0)
	// Bridge help reflects connection flags' environment defaults. An allowlist
	// prevents both credential disclosure and a reflected fake capability line.
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "SYSTEMROOT", "WINDIR", "PATH", "HOME", "USERPROFILE", "TEMP", "TMP":
			cmd.Env = append(cmd.Env, entry)
		}
	}
}

type linkCapabilityOutput struct{ data []byte }

func (o *linkCapabilityOutput) Write(p []byte) (int, error) {
	if len(p) > linkCapabilityLimit-len(o.data) {
		return 0, errLinkCapabilityProbe
	}
	o.data = append(o.data, p...)
	return len(p), nil
}

func linkCapabilityStatus(output []byte, endpoint string, err error) string {
	if err != nil || len(output) > linkCapabilityLimit {
		return "unavailable"
	}
	found := false
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, linkCapabilityPrefix) {
			continue
		}
		if found || line != linkCapabilityPrefix+endpoint+" provisioned-v1" {
			return "unsupported"
		}
		found = true
	}
	if found {
		return "supported"
	}
	return "unsupported"
}

var errLinkAuthEnforceUnsupported = errors.New("solarbeam: link_auth_mode is enforce but the installed engine and bridge do not both report link authentication support; refusing to start SolarBeam unauthenticated (set solarbeam_link_auth_mode to warn to roll back)")

// An empty mode means unprovisioned legacy on BOTH ends. That fallback is for
// warn only: an enforce host refuses rather than run an unauthenticated link.
// Check afresh on every launch and preload.
func (m *Manager) compatibleLinkAuthMode() (string, error) {
	mode, err := m.linkAuthMode()
	if err != nil {
		return "", err
	}
	engine, engineErr := m.linkCapabilityFn(m.paths.Engine)
	bridge, bridgeErr := m.linkCapabilityFn(m.paths.Bridge)
	engineStatus := linkCapabilityStatus(engine, "engine", engineErr)
	bridgeStatus := linkCapabilityStatus(bridge, "bridge", bridgeErr)
	if engineStatus == "supported" && !m.linkEngineMatchesFn(m.paths) {
		engineStatus = "service_path_unverified"
	}
	if (engineStatus != "supported" || bridgeStatus != "supported") && mode == "enforce" {
		slog.Error("solarbeam: link authentication enforced but not supported by the installed binaries; refusing launch",
			"phase", "link_auth_enforce_refused", "engine", engineStatus, "bridge", bridgeStatus)
		return "", errLinkAuthEnforceUnsupported
	}
	if engineStatus != "supported" || bridgeStatus != "supported" {
		slog.Warn("solarbeam: link authentication not provisioned; keeping both endpoints legacy",
			"phase", "link_auth_legacy", "engine", engineStatus, "bridge", bridgeStatus)
		return "", nil
	}
	return mode, nil
}

// Kept here so all platforms use exactly the same environment allowlist.
func linkCapabilityEnvironment() []string { return os.Environ() }
